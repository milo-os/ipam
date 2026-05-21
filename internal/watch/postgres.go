// Package watch provides a changelog-based watch implementation for PostgreSQL.
//
// The PostgresWatcher polls the ipam_changelog table for new entries and
// converts them into Kubernetes watch.Event objects. This implements the
// watch contract expected by k8s.io/apiserver clients: initial list,
// event stream, and bookmarks.
//
// # The commit-ordered (xmin horizon) cursor
//
// The original implementation advanced its cursor by ipam_changelog's
// resource_version column, which is drawn from a Postgres sequence. A
// sequence value becomes visible to readers the moment nextval() returns,
// BEFORE the transaction that allocated it commits. Two writers that
// interleave nextval()/COMMIT pairs can publish rows out of commit order:
// T1 pulls RV=100, T2 pulls RV=101 and commits, T1 commits later. A watcher
// that polls between the two commits sees only row 101, advances its cursor
// to 101, and never sees row 100 — the cacher's in-memory state diverges
// from the authoritative row set. We observed this bug once on rv=61590 in
// a 60s load run (`Cache consistency check failed`).
//
// The original fix was a global advisory lock serializing every writer's
// nextval()-to-commit window. That lock made the sequence "commit-ordered"
// at the cost of pinning all writers onto one key, capping multi-tenant
// throughput at one tenant's worth of Postgres.
//
// This implementation replaces the lock with a commit-ordered watcher
// cursor based on the 64-bit xact id (xid8) stored on each changelog row.
// Migration 003 adds a commit_xid column defaulted to
// pg_current_xact_id()::text::bigint, so every INSERT captures the
// inserting transaction's xid8 at INSERT time. The watcher then computes
// a per-poll horizon via pg_snapshot_xmin(pg_current_snapshot()), the
// oldest transaction that was still in flight when the poll's snapshot was
// taken. Every row with commit_xid strictly less than that horizon is
// guaranteed committed and visible to every future snapshot. The watcher
// only emits rows below the horizon, and orders ties by the existing
// BIGSERIAL id column — so commit order and scan order are in lockstep
// without any writer-side lock.
//
// Ordering within a transaction is by BIGSERIAL id, which preserves the
// per-transaction write order that was already in the changelog. Across
// transactions, ordering is by (commit_xid, id), which matches true commit
// order for transactions whose xids fall strictly below the horizon.
//
// This is the standard Postgres CDC pattern used by Debezium, Stripe's
// "WAL-G style" changefeeds, and AirByte. See:
//   - https://github.com/debezium/debezium/blob/main/debezium-connector-postgres/
//   - https://stripe.com/blog/online-migrations
//
// # Initial list / watch handoff
//
// The cacher and watchlist clients need an initial LIST followed by a
// WATCH with no gap and no duplicates. Gap-free handoff is achieved with a
// single REPEATABLE READ transaction that reads the current state of
// ipam_objects AND captures pg_snapshot_xmin of the same snapshot. After
// the LIST tx commits, the watch goroutine starts with the cursor set
// so that its first poll picks up exactly the set of rows whose commit_xid
// is >= the LIST snapshot's xmin. That is, any row that could have been
// inserted by a transaction in flight during the LIST — whether or not it
// was visible inside the LIST snapshot — will be re-emitted from the
// changelog on the first WATCH poll as soon as its xid falls below the
// current horizon. Since objects have a monotonic resource_version and
// the cacher de-duplicates by RV, benign duplicates on the LIST→WATCH
// boundary do not cause divergence.
package watch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/metrics"
)

const (
	// defaultPollInterval is the safety-net poll interval. With LISTEN/NOTIFY
	// providing real-time push notifications, this timer exists ONLY to catch
	// notifications lost during a LISTEN-connection reconnect window — the
	// steady-state case is driven entirely by NOTIFY kicks.
	//
	// Historically this was 1s, which was overly aggressive for a backstop:
	// round-7 profiling observed pollChanges running 246 times/sec on a
	// 30 writes/sec workload (13% of apiserver CPU). 5s is plenty: the
	// cacher's stale-data window is much larger, and any genuinely missed
	// NOTIFY will still be picked up well within the cacher's tolerance.
	defaultPollInterval = 5 * time.Second
	// notifyCoalesceDelay is the debounce window applied to LISTEN/NOTIFY
	// kicks. When a kick arrives the watcher waits this long before polling,
	// collapsing every kick that arrives during the window into a single
	// poll. This is safe because the (commit_xid, id) cursor with the xmin
	// horizon filter guarantees no committed row is ever skipped — delaying
	// a poll by a few tens of milliseconds only affects latency, never
	// correctness. Keep this short enough that end-to-end watch latency
	// stays well under 100ms.
	notifyCoalesceDelay = 50 * time.Millisecond
	// defaultBookmarkInterval is the default interval between bookmark events.
	defaultBookmarkInterval = 30 * time.Second
	// defaultChangelogRetention is how far back the changelog is kept. Any
	// client disconnected longer than this window will receive a compaction
	// error and must re-list — standard Kubernetes behaviour. 5 minutes
	// covers the longest LISTEN reconnect backoff (30s) with a large margin,
	// keeps the table small under sustained load, and avoids the index-bloat
	// write amplification seen with the original 24h window.
	defaultChangelogRetention = 5 * time.Minute
	// defaultCleanupInterval is how often the cleanup loop runs. Frequent
	// enough that the table never accumulates more than one interval's worth
	// of rows above the retention window.
	defaultCleanupInterval = 1 * time.Minute
	// notifyChannelName is the Postgres NOTIFY channel name we LISTEN on.
	// Must match the channel name used in the trigger function in
	// migrations/002_listen_notify.sql.
	notifyChannelName = "ipam_changes"
	// listenerMinReconnect/MaxReconnect bound the reconnection backoff for
	// the dedicated LISTEN connection.
	listenerMinReconnect = 1 * time.Second
	listenerMaxReconnect = 30 * time.Second
	// horizonStallWarnInterval is how long the snapshot horizon may remain
	// frozen before we log a WARN. A frozen horizon means some transaction
	// has been in flight longer than this interval, blocking the watcher
	// from emitting newer rows. This is an observability signal only; the
	// watcher continues to wait rather than skip rows (skipping would
	// reintroduce the lost-event bug the whole scheme exists to avoid).
	horizonStallWarnInterval = 5 * time.Minute
)

// PostgresWatcher manages watch streams backed by the ipam_changelog table.
//
// Two mechanisms feed the watch goroutines:
//
//  1. LISTEN/NOTIFY (primary): A single dedicated pgx connection per
//     PostgresWatcher holds a LISTEN on the "ipam_changes" Postgres channel.
//     The migration installs an AFTER INSERT trigger on ipam_changelog that
//     fires NOTIFY on every mutation, so the apiserver receives a wake-up
//     within milliseconds. The watcher fans out a kick to every active
//     watch goroutine, which immediately drains the changelog. Reconnect is
//     hand-rolled (pgx has no drop-in equivalent to lib/pq's NewListener)
//     with backoff bounded by listenerMinReconnect/listenerMaxReconnect.
//
//  2. Periodic safety poll (backstop): Every postgresWatch ticks every
//     defaultPollInterval (5s) and drains the changelog regardless of
//     whether a NOTIFY was received. This catches notifications missed
//     during the LISTEN connection's reconnect window. NOTIFY kicks are
//     coalesced via a short debounce window (notifyCoalesceDelay) so
//     bursty write workloads collapse into a single poll.
type PostgresWatcher struct {
	db    *sql.DB
	codec runtime.Codec
	// dsn is needed to create the dedicated LISTEN connection. The watcher
	// opens a single pgx.Conn via pgx.Connect for LISTEN/NOTIFY, entirely
	// separate from the *sql.DB pool used for queries.
	dsn string

	// excludedKeyPrefixes lists key prefixes that this watcher should NOT
	// emit events for. The postgres-native AllocatingREST claim layer
	// serves its own watch via per-handler LISTEN connections, so the
	// claim key prefix is added here to stop the polled watcher from
	// wasting CPU decoding claim rows the native layer already serves.
	excludedKeyPrefixes []string

	// cleanupOnce ensures changelog cleanup is started only once.
	cleanupOnce sync.Once
	// listenerOnce ensures the LISTEN/NOTIFY goroutine is started only once.
	listenerOnce sync.Once
	// cleanupDone signals cleanup and listener goroutines to stop.
	cleanupDone chan struct{}

	// active tracks all live watch streams so RequestWatchProgress and the
	// LISTEN/NOTIFY listener can fan out signals to each one.
	mu     sync.RWMutex
	active map[*postgresWatch]struct{}
}

// New creates a new PostgresWatcher. The dsn is used to create a dedicated
// connection for LISTEN/NOTIFY (separate from the pooled *sql.DB used for
// regular queries). If dsn is empty the watcher falls back to polling-only.
func New(db *sql.DB, codec runtime.Codec, dsn string) *PostgresWatcher {
	return NewWithExclusions(db, codec, dsn, nil)
}

// NewWithExclusions creates a PostgresWatcher that skips emitting events for
// keys matching any of the supplied prefixes. The postgres-native QuotaClaim
// layer uses this to keep the polled watcher from duplicating work it already
// serves via per-handler LISTEN connections.
func NewWithExclusions(db *sql.DB, codec runtime.Codec, dsn string, excludedKeyPrefixes []string) *PostgresWatcher {
	return &PostgresWatcher{
		db:                  db,
		codec:               codec,
		dsn:                 dsn,
		cleanupDone:         make(chan struct{}),
		active:              make(map[*postgresWatch]struct{}),
		excludedKeyPrefixes: append([]string(nil), excludedKeyPrefixes...),
	}
}

// NotifyProgress is called by Store.RequestWatchProgress to push a fresh
// resource version to every active watch. Each watch will emit a bookmark
// event with this RV on its result channel as soon as the polling loop
// picks it up. The cacher relies on this to determine when its in-memory
// cache is consistent up to a given RV (used by ConsistentListFromCache).
func (pw *PostgresWatcher) NotifyProgress(rv uint64) {
	pw.mu.RLock()
	defer pw.mu.RUnlock()
	for w := range pw.active {
		select {
		case w.progress <- rv:
		default:
			// Channel full — a previous progress signal is still pending.
			// That's fine; the watcher will pick up the latest RV when it
			// drains the channel.
		}
	}
}

// kickAll signals every active watch to immediately drain the changelog.
// Used by the LISTEN/NOTIFY goroutine when a Postgres notification arrives.
// Non-blocking: if a watch's kick channel is full, the kick is dropped
// because there's already a pending drain request that will catch the same
// changes.
func (pw *PostgresWatcher) kickAll() {
	pw.mu.RLock()
	defer pw.mu.RUnlock()
	for w := range pw.active {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

func (pw *PostgresWatcher) register(w *postgresWatch) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.active[w] = struct{}{}
}

func (pw *PostgresWatcher) unregister(w *postgresWatch) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	delete(pw.active, w)
}

// startListener launches a single goroutine that holds a dedicated pgx
// connection LISTENing on the ipam_changes channel. When a notification
// arrives it kicks every active watch. pgx has no built-in reconnect for
// LISTEN/NOTIFY (unlike lib/pq's pq.NewListener), so the loop is responsible
// for reopening the connection with exponential backoff after any failure.
// The per-watch safety poll (defaultPollInterval) covers any gap during a
// reconnect — no committed row can be lost because the (commit_xid, id)
// cursor skips nothing below the horizon.
func (pw *PostgresWatcher) startListener() {
	if pw.dsn == "" {
		klog.V(2).InfoS("PostgresWatcher: no DSN provided, LISTEN/NOTIFY disabled, falling back to polling only")
		return
	}

	go func() {
		klog.V(2).InfoS("PostgresWatcher: starting LISTEN/NOTIFY loop", "channel", notifyChannelName)
		defer klog.V(2).InfoS("PostgresWatcher: LISTEN/NOTIFY loop stopped")

		backoff := listenerMinReconnect
		for {
			// Fast-exit if shutdown happened while we were backing off.
			select {
			case <-pw.cleanupDone:
				return
			default:
			}

			err := pw.runListener()
			if err == nil || errors.Is(err, context.Canceled) {
				// Clean shutdown: cleanupDone fired.
				return
			}

			klog.ErrorS(err, "PostgresWatcher: LISTEN connection lost, reconnecting", "backoff", backoff)
			select {
			case <-pw.cleanupDone:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > listenerMaxReconnect {
				backoff = listenerMaxReconnect
			}
		}
	}()
}

// runListener opens a single pgx connection, issues LISTEN, and blocks in
// WaitForNotification until either the context is cancelled (clean shutdown)
// or a connection error forces a reconnect. On successful LISTEN it kicks
// every active watch once so they immediately drain anything missed during
// the (re)connect window, and resets the caller's backoff on return is
// handled by the caller observing nil error only on shutdown. A returned
// non-nil error always means "reconnect"; a nil error means "shutdown".
func (pw *PostgresWatcher) runListener() error {
	// Tie the pgx connection's lifetime to cleanupDone so a Stop() during a
	// blocking WaitForNotification tears the connection down promptly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-pw.cleanupDone:
			cancel()
		case <-ctx.Done():
		}
	}()

	conn, err := pgx.Connect(ctx, pw.dsn)
	if err != nil {
		return fmt.Errorf("pgx connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannelName); err != nil {
		return fmt.Errorf("LISTEN %s: %w", notifyChannelName, err)
	}
	klog.V(2).InfoS("PostgresWatcher: LISTEN connection established", "channel", notifyChannelName)

	// Hand-off between push and the periodic poll: kick every active watch
	// so it immediately drains anything that may have been NOTIFY'd while
	// we were disconnected. Safe to call even on the first connect.
	pw.kickAll()

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			// Context cancelled means clean shutdown; anything else is a
			// real connection error that should trigger a reconnect.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("WaitForNotification: %w", err)
		}
		if n == nil {
			continue
		}
		pw.kickAll()
	}
}

// Watch starts a new watch stream for the given key prefix starting from the
// resource version specified in opts. newFunc should return a zero-value object
// of the watched type; it is used to construct bookmark events. If nil,
// bookmark events will be skipped.
func (pw *PostgresWatcher) Watch(ctx context.Context, key string, opts storage.ListOptions, newFunc func() runtime.Object) (watch.Interface, error) {
	// Start background cleanup and the LISTEN/NOTIFY loop on the first watch.
	pw.cleanupOnce.Do(func() {
		go pw.cleanupLoop()
	})
	pw.listenerOnce.Do(func() {
		pw.startListener()
	})

	startRV := int64(0)
	if opts.ResourceVersion != "" {
		_, err := fmt.Sscanf(opts.ResourceVersion, "%d", &startRV)
		if err != nil {
			return nil, storage.NewInternalError(fmt.Errorf("invalid resource version %q: %w", opts.ResourceVersion, err))
		}
	}

	// sendInitialEvents signals that the watch should synthesize ADDED events
	// for all existing objects matching the key prefix, followed by a bookmark
	// event with the k8s.io/initial-events-end annotation. This is required by
	// the k8s.io/client-go WatchList client (v0.32+).
	sendInitialEvents := opts.SendInitialEvents != nil && *opts.SendInitialEvents

	w := &postgresWatch{
		db:                  pw.db,
		codec:               pw.codec,
		key:                 key,
		predicate:           opts.Predicate,
		newFunc:             newFunc,
		startRV:             startRV,
		result:              make(chan watch.Event, 100),
		done:                make(chan struct{}),
		progress:            make(chan uint64, 1),
		kick:                make(chan struct{}, 1),
		sendInitialEvents:   sendInitialEvents,
		parent:              pw,
		excludedKeyPrefixes: pw.excludedKeyPrefixes,
	}

	// If the client is resuming from a specific RV, seed the (xid, id)
	// cursor from the changelog row that carried that RV. A mismatch means
	// the changelog has been compacted past the client's resume point;
	// callers expect an error in that case rather than silent gap-skipping.
	if startRV > 0 {
		if err := w.seedCursorFromRV(ctx, startRV); err != nil {
			return nil, err
		}
	}

	pw.register(w)
	go w.poll(ctx)
	return w, nil
}

// Stop terminates background goroutines. Call when shutting down.
func (pw *PostgresWatcher) Stop() {
	close(pw.cleanupDone)
}

// cleanupLoop periodically removes old changelog entries.
func (pw *PostgresWatcher) cleanupLoop() {
	ticker := time.NewTicker(defaultCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-pw.cleanupDone:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-defaultChangelogRetention)
			result, err := pw.db.Exec(
				`DELETE FROM ipam_changelog WHERE created_at < $1`, cutoff,
			)
			if err != nil {
				klog.ErrorS(err, "Failed to clean up changelog entries")
				continue
			}
			if rows, err := result.RowsAffected(); err == nil && rows > 0 {
				klog.V(2).InfoS("Cleaned up old changelog entries", "count", rows)
			}
		}
	}
}

// postgresWatch implements watch.Interface by polling the changelog table.
//
// Cursor semantics: lastXid and lastID form a lexicographic pair tracking
// the last changelog row emitted to the client. A poll emits all rows
// satisfying (commit_xid > lastXid OR (commit_xid = lastXid AND id > lastID))
// AND commit_xid < horizon, ordered by (commit_xid, id). lastRV is the
// per-object Kubernetes resource version of the last emitted row, used for
// bookmark events (kubectl sees resource_version, not commit_xid).
type postgresWatch struct {
	db        *sql.DB
	codec     runtime.Codec
	key       string
	predicate storage.SelectionPredicate
	newFunc   func() runtime.Object
	// startRV is the client-supplied resource version from the Watch call.
	// Used only to seed the (xid, id) cursor via seedCursorFromRV before
	// the polling loop begins.
	startRV int64
	// lastXid is the highest commit_xid we have emitted so far, and
	// together with lastID forms the secondary sort key for the changelog
	// poll. Both start at 0 (equivalent to "nothing emitted yet").
	lastXid int64
	// lastID is the BIGSERIAL id of the last changelog row emitted at
	// commit_xid == lastXid. Within a single xid tie, polls order by id
	// ascending; across xids they order by commit_xid ascending.
	lastID int64
	// lastRV tracks the most recent resource_version emitted or observed
	// from an authoritative source (initial LIST, RequestWatchProgress).
	// It's the value we put in bookmarks and the value we use to detect
	// "nothing new" short-circuits.
	lastRV int64
	// horizonLastAdvance records when we last observed the snapshot
	// horizon advance. If it stays frozen for longer than
	// horizonStallWarnInterval we log a WARN and keep waiting.
	horizonLastAdvance time.Time
	// horizonAtLastAdvance is the last horizon value we observed, used to
	// detect when the horizon actually moves forward.
	horizonAtLastAdvance int64
	result               chan watch.Event
	done                 chan struct{}
	// progress receives RVs pushed by PostgresWatcher.NotifyProgress (called
	// from Store.RequestWatchProgress). When a value arrives, the watch
	// emits a bookmark event so the cacher knows it's caught up to that RV.
	progress chan uint64
	// kick is signaled by the LISTEN/NOTIFY listener whenever a Postgres
	// notification arrives. The watch immediately drains the changelog
	// instead of waiting for the next periodic safety poll.
	kick              chan struct{}
	closeOnce         sync.Once
	sendInitialEvents bool
	parent            *PostgresWatcher
	// excludedKeyPrefixes mirrors PostgresWatcher.excludedKeyPrefixes at
	// watch construction time, so later changes to the parent do not
	// affect in-flight watchers.
	excludedKeyPrefixes []string
}

// ResultChan returns the channel of watch events.
func (w *postgresWatch) ResultChan() <-chan watch.Event {
	return w.result
}

// Stop stops the watch and releases resources.
func (w *postgresWatch) Stop() {
	w.closeOnce.Do(func() {
		if w.parent != nil {
			w.parent.unregister(w)
		}
		close(w.done)
	})
}

// poll continuously queries the changelog table for new events.
func (w *postgresWatch) poll(ctx context.Context) {
	defer close(w.result)

	w.horizonLastAdvance = time.Now()

	// If the client requested initial events, synthesize ADDED events for all
	// existing objects matching the key prefix, then send a bookmark with the
	// k8s.io/initial-events-end annotation to signal end-of-initial-list.
	if w.sendInitialEvents {
		if err := w.sendInitialEventList(ctx); err != nil {
			klog.ErrorS(err, "Failed to send initial events", "key", w.key)
			return
		}
		if !w.sendInitialEventsEndBookmark() {
			return
		}
	}

	pollTicker := time.NewTicker(defaultPollInterval)
	defer pollTicker.Stop()

	bookmarkTicker := time.NewTicker(defaultBookmarkInterval)
	defer bookmarkTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.done:
			return
		case rv := <-w.progress:
			// RequestWatchProgress nudge from the cacher. Drain ALL pending
			// changelog rows (not just one batch) so the bookmark we emit
			// truly reflects state at or after `rv`. Without draining to
			// completion, the cacher's ConsistentListFromCache wait would see
			// a bookmark RV below the requested RV and keep waiting, hitting
			// the 3-second timeout every time under post-burst backlog.
			if err := w.drainChangelog(ctx); err != nil {
				klog.ErrorS(err, "Error draining changelog before progress bookmark", "key", w.key)
			}
			if rv > uint64(w.lastRV) {
				w.lastRV = int64(rv)
			}
			w.sendBookmarkAt(uint64(w.lastRV))
		case <-w.kick:
			// LISTEN/NOTIFY push: a Postgres notification told us
			// there's new data. Wait briefly so any additional kicks
			// arriving in the same window collapse into a single
			// drainChangelog call — with many concurrent writers this
			// collapses hundreds of kicks/sec down to ~20/sec without
			// changing visible latency. Correctness is preserved by
			// the xmin horizon filter on pollChanges: a delayed poll
			// only defers emission, it never skips committed rows.
			coalesceTimer := time.NewTimer(notifyCoalesceDelay)
			select {
			case <-coalesceTimer.C:
			case <-ctx.Done():
				coalesceTimer.Stop()
				return
			case <-w.done:
				coalesceTimer.Stop()
				return
			}
			// Drain any kicks that arrived during the coalesce window
			// so we don't immediately re-enter this branch.
			select {
			case <-w.kick:
			default:
			}
			if err := w.drainChangelog(ctx); err != nil {
				klog.ErrorS(err, "Error draining changelog after NOTIFY kick", "key", w.key)
			}
		case <-bookmarkTicker.C:
			// Periodic bookmark on a timer
			w.sendBookmark()
		case <-pollTicker.C:
			// Safety poll: catches notifications missed during
			// LISTEN-connection reconnect windows. Runs at
			// defaultPollInterval (5s) — the steady-state path is
			// driven by NOTIFY kicks, so this is just a backstop.
			if _, err := w.pollChanges(ctx); err != nil {
				klog.ErrorS(err, "Error polling changelog", "key", w.key, "lastXid", w.lastXid, "lastID", w.lastID)
			}
		}
	}
}

// seedCursorFromRV translates a client-supplied resource version into the
// internal (commit_xid, id) cursor. The client says "resume at or after RV
// N"; we find the changelog row whose resource_version is N and take its
// (commit_xid, id) as the inclusive lower bound — i.e. the next emitted
// row is strictly greater than (that xid, that id).
//
// If no row exists for the requested RV (e.g. the changelog has been
// compacted past it) we fall back to using it as a directional hint: pick
// the row with the largest resource_version <= startRV. If even that
// returns nothing the cursor stays at (0, 0) which emits everything from
// the beginning — the compaction case would be surfaced by the cacher's
// own consistency checks rather than here.
func (w *postgresWatch) seedCursorFromRV(ctx context.Context, startRV int64) error {
	var xid, id sql.NullInt64
	err := w.db.QueryRowContext(ctx,
		`SELECT commit_xid, id FROM ipam_changelog WHERE resource_version = $1 ORDER BY id DESC LIMIT 1`,
		startRV,
	).Scan(&xid, &id)
	if err != nil && err != sql.ErrNoRows {
		return storage.NewInternalError(fmt.Errorf("seed cursor from resource version %d: %w", startRV, err))
	}
	if err == sql.ErrNoRows {
		err = w.db.QueryRowContext(ctx,
			`SELECT commit_xid, id FROM ipam_changelog
			  WHERE resource_version <= $1
			  ORDER BY resource_version DESC, id DESC
			  LIMIT 1`,
			startRV,
		).Scan(&xid, &id)
		if err != nil && err != sql.ErrNoRows {
			return storage.NewInternalError(fmt.Errorf("seed cursor from resource version <=%d: %w", startRV, err))
		}
	}
	if xid.Valid && id.Valid {
		w.lastXid = xid.Int64
		w.lastID = id.Int64
	}
	w.lastRV = startRV
	return nil
}

// sendInitialEventList queries the ipam_objects table for all objects
// matching the key prefix and sends them as ADDED events. To avoid gaps
// between the initial LIST and the subsequent changelog polling, the LIST
// runs inside a REPEATABLE READ transaction that ALSO captures the
// snapshot's xmin (via pg_snapshot_xmin(pg_current_snapshot())) and the
// highest changelog (commit_xid, id) currently below that xmin. The watch
// cursor is then seeded so that the first poll picks up exactly the rows
// whose transactions were in flight during the LIST, once they commit and
// fall below the horizon.
//
// The resource_version on each listed object is still used as lastRV for
// bookmark events — kubectl sees resource_version, not commit_xid.
func (w *postgresWatch) sendInitialEventList(ctx context.Context) error {
	keyPrefix := w.key
	if !strings.HasSuffix(keyPrefix, "/") {
		keyPrefix += "/"
	}

	tx, err := w.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("failed to begin snapshot tx for initial list: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			klog.ErrorS(rbErr, "failed to rollback initial-list snapshot tx", "key", w.key)
		}
	}()

	// Pin a snapshot for this transaction. Any subsequent statement on tx
	// sees exactly the rows visible to this snapshot. Capturing xmin here
	// gives us the boundary between "definitely-committed before LIST" and
	// "possibly-in-flight during LIST".
	var snapshotXmin int64
	if err := tx.QueryRowContext(ctx,
		`SELECT pg_snapshot_xmin(pg_current_snapshot())::text::bigint`,
	).Scan(&snapshotXmin); err != nil {
		return fmt.Errorf("failed to capture snapshot xmin: %w", err)
	}

	// Within the same snapshot, capture the lexicographic max
	// (commit_xid, id) pair that is strictly below the snapshot xmin. Any
	// row strictly below the xmin and strictly greater (in the
	// lexicographic sense) than this pair does not exist, so seeding the
	// cursor to this pair means the first poll starts exactly where the
	// LIST left off.
	var cursorXid, cursorID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT commit_xid, id
		   FROM ipam_changelog
		  WHERE commit_xid < $1
		  ORDER BY commit_xid DESC, id DESC
		  LIMIT 1`,
		snapshotXmin,
	).Scan(&cursorXid, &cursorID); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to capture initial cursor: %w", err)
	}

	listArgs := []any{w.key, keyPrefix + "%"}
	listQuery := `SELECT key, resource_version, data
		 FROM ipam_objects
		 WHERE (key = $1 OR key LIKE $2)`
	for _, excl := range w.excludedKeyPrefixes {
		listArgs = append(listArgs, excl+"%")
		listQuery += fmt.Sprintf(" AND key NOT LIKE $%d", len(listArgs))
	}
	listQuery += " ORDER BY resource_version ASC"

	rows, err := tx.QueryContext(ctx, listQuery, listArgs...)
	if err != nil {
		return fmt.Errorf("failed to query objects for initial events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key string
		var rv int64
		var data []byte

		if err := rows.Scan(&key, &rv, &data); err != nil {
			return fmt.Errorf("failed to scan object row: %w", err)
		}

		event, err := w.toWatchEvent("ADDED", data, rv)
		if err != nil {
			klog.ErrorS(err, "Failed to convert object to initial event", "key", key, "rv", rv)
			continue
		}

		// Apply predicate filtering
		if !w.predicate.Empty() {
			matches, err := w.predicate.Matches(event.Object)
			if err != nil || !matches {
				if rv > w.lastRV {
					w.lastRV = rv
				}
				continue
			}
		}

		select {
		case w.result <- *event:
			if rv > w.lastRV {
				w.lastRV = rv
			}
		case <-w.done:
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Commit the snapshot tx. After this point we're no longer pinning the
	// snapshot's xmin so new transactions can advance the horizon, letting
	// our subsequent polls see everything committed after LIST started.
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit initial list snapshot: %w", err)
	}

	w.lastXid = cursorXid.Int64
	w.lastID = cursorID.Int64
	return nil
}

// sendInitialEventsEndBookmark sends a bookmark event with the
// k8s.io/initial-events-end annotation, signaling the end of the initial
// event stream to watchlist clients. Returns false if the watch was stopped.
func (w *postgresWatch) sendInitialEventsEndBookmark() bool {
	if w.newFunc == nil {
		// Without a newFunc we cannot construct a bookmark object; the client
		// will hang. This is a programmer error.
		klog.ErrorS(nil, "Cannot send initial-events-end bookmark: newFunc is nil", "key", w.key)
		return false
	}

	// Use the highest committed resource version as the bookmark RV: the
	// max over rows whose commit_xid is strictly below the current
	// snapshot horizon. Never read last_value from the sequence here —
	// that's observable before the owning tx commits and would let the
	// cacher advertise a resume point whose underlying row is still
	// in flight.
	var maxRV int64
	if err := w.db.QueryRow(
		`SELECT COALESCE(MAX(resource_version), 0)
		   FROM ipam_changelog
		  WHERE commit_xid < pg_snapshot_xmin(pg_current_snapshot())::text::bigint`,
	).Scan(&maxRV); err != nil {
		klog.ErrorS(err, "Failed to get committed max resource version for initial-events-end bookmark")
		return false
	}
	if maxRV > w.lastRV {
		w.lastRV = maxRV
	}

	obj := w.newFunc()
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(obj, uint64(w.lastRV)); err != nil {
		klog.ErrorS(err, "Failed to set resource version on bookmark object")
		return false
	}

	// Add the k8s.io/initial-events-end annotation.
	accessor, err := meta.Accessor(obj)
	if err != nil {
		klog.ErrorS(err, "Failed to get meta accessor for bookmark object")
		return false
	}
	annotations := accessor.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["k8s.io/initial-events-end"] = "true"
	accessor.SetAnnotations(annotations)

	select {
	case w.result <- watch.Event{Type: watch.Bookmark, Object: obj}:
		return true
	case <-w.done:
		return false
	}
}

// pollBatchSize is the maximum number of changelog rows fetched per
// pollChanges call. When a batch is full (n == pollBatchSize) there are
// likely more rows available — callers should re-poll immediately.
const pollBatchSize = 500

// drainChangelog calls pollChanges in a tight loop until fewer than
// pollBatchSize rows are returned, meaning the watcher is fully caught up.
// This is used by the progress and kick paths so that a post-burst backlog
// (e.g. 70K changelog rows left after a throughput test) is drained in
// seconds rather than the minutes it would take at the 5-second poll-ticker
// rate.
func (w *postgresWatch) drainChangelog(ctx context.Context) error {
	kind := kindFromKey(w.key)
	batches := 0
	for {
		n, err := w.pollChanges(ctx)
		batches++
		if err != nil {
			metrics.RecordDrainCycle(kind, batches > 1)
			return err
		}
		if n < pollBatchSize {
			metrics.RecordDrainCycle(kind, batches > 1)
			return nil
		}
		// Full batch returned — check cancellation before re-polling.
		select {
		case <-ctx.Done():
			metrics.RecordDrainCycle(kind, batches > 1)
			return ctx.Err()
		case <-w.done:
			metrics.RecordDrainCycle(kind, batches > 1)
			return nil
		default:
		}
	}
}

// pollChanges fetches new changelog entries since the last emitted
// (commit_xid, id) cursor. The horizon is the snapshot-xmin taken fresh on
// every poll — any row with commit_xid strictly less than the horizon is
// guaranteed to have committed and be visible to every future snapshot.
// We never emit a row whose commit_xid >= horizon: doing so would risk
// ordering violations with earlier, still-in-flight transactions.
//
// Ordering is (commit_xid, id) ascending. Within a single transaction,
// rows share the same commit_xid and are ordered by the BIGSERIAL id
// column, preserving the write order they were inserted in.

func (w *postgresWatch) pollChanges(ctx context.Context) (int, error) {
	keyPrefix := w.key
	if !strings.HasSuffix(keyPrefix, "/") {
		keyPrefix += "/"
	}

	var horizon int64
	if err := w.db.QueryRowContext(ctx,
		`SELECT pg_snapshot_xmin(pg_current_snapshot())::text::bigint`,
	).Scan(&horizon); err != nil {
		return 0, fmt.Errorf("failed to read snapshot horizon: %w", err)
	}

	w.maybeWarnHorizonStall(horizon)

	args := []any{horizon, w.lastXid, w.lastID, w.key, keyPrefix + "%"}
	query := `SELECT key, resource_version, event_type, data, commit_xid, id, created_at
		 FROM ipam_changelog
		 WHERE commit_xid < $1
		   AND (commit_xid > $2 OR (commit_xid = $2 AND id > $3))
		   AND (key = $4 OR key LIKE $5)`
	for _, excl := range w.excludedKeyPrefixes {
		args = append(args, excl+"%")
		query += fmt.Sprintf(" AND key NOT LIKE $%d", len(args))
	}
	query += fmt.Sprintf(" ORDER BY commit_xid ASC, id ASC LIMIT %d", pollBatchSize)

	rows, err := w.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to query changelog: %w", err)
	}
	defer rows.Close()

	var n int
	for rows.Next() {
		var key string
		var rv int64
		var eventType string
		var data []byte
		var xid, id int64
		var createdAt time.Time

		if err := rows.Scan(&key, &rv, &eventType, &data, &xid, &id, &createdAt); err != nil {
			return n, fmt.Errorf("failed to scan changelog row: %w", err)
		}

		event, err := w.toWatchEvent(eventType, data, rv)
		if err != nil {
			klog.ErrorS(err, "Failed to convert changelog entry to watch event",
				"key", key, "rv", rv, "eventType", eventType)
			w.advanceCursor(xid, id, rv)
			n++
			continue
		}

		// Apply predicate filtering
		if !w.predicate.Empty() {
			matches, err := w.predicate.Matches(event.Object)
			if err != nil || !matches {
				w.advanceCursor(xid, id, rv)
				n++
				continue
			}
		}

		select {
		case w.result <- *event:
			// Watch lag = time between row INSERT (changelog created_at)
			// and event hand-off to the result channel. Observed only on
			// the dispatch path so a slow consumer doesn't pollute the
			// histogram with channel-backpressure time.
			metrics.ObserveWatchLag(createdAt)
			// Per-event counter: bookmark events bypass this path entirely
			// (sendInitialEventsEndBookmark sends them directly), so this
			// only counts user-visible Add/Modify/Delete dispatches. kindFromKey
			// extracts the lowercase plural resource from the storage key prefix.
			metrics.RecordWatchEvent(kindFromKey(w.key), eventType)
			w.advanceCursor(xid, id, rv)
			n++
		case <-w.done:
			return n, nil
		}
	}
	err = rows.Err()
	metrics.RecordPollBatch(kindFromKey(w.key), n)
	return n, err
}

// kindFromKey returns the lowercase plural resource name embedded in a
// storage key, used as the `kind` label on the watch_events_total counter.
//
// Storage key shapes (see internal/tenant/tenant.go):
//   - platform-scoped:        /ipam.miloapis.com/<resource>[/<name>]
//   - project-scoped:         project/<id>/ipam.miloapis.com/<resource>[/<name>]
//
// The watcher's key is either a fully-qualified key (single-object watch)
// or a prefix (list watch). Both share the same /ipam.miloapis.com/<resource>
// segment; we return the segment that immediately follows it. If the key
// does not match the expected layout (which would be a bug, not user input),
// we return "unknown" so the metric still emits a series.
func kindFromKey(key string) string {
	const marker = "/ipam.miloapis.com/"
	_, rest, ok := strings.Cut(key, marker)
	if !ok || rest == "" {
		return "unknown"
	}
	kind, _, _ := strings.Cut(rest, "/")
	return kind
}

// advanceCursor moves the emitted-so-far cursor to (xid, id). lastRV is
// also updated if the row's resource_version is higher than any previously
// emitted row, so bookmarks reflect the latest RV we have seen.
func (w *postgresWatch) advanceCursor(xid, id, rv int64) {
	w.lastXid = xid
	w.lastID = id
	if rv > w.lastRV {
		w.lastRV = rv
	}
}

// maybeWarnHorizonStall logs a WARN if the snapshot horizon has not moved
// forward in horizonStallWarnInterval. A frozen horizon means some
// transaction has been in flight longer than the warn interval, which
// blocks the watcher from emitting any row whose commit_xid is >= that
// transaction's xid. We only log; we never skip rows. Skipping would
// reintroduce the lost-event bug this entire scheme is designed to avoid.
func (w *postgresWatch) maybeWarnHorizonStall(horizon int64) {
	now := time.Now()
	if horizon > w.horizonAtLastAdvance {
		w.horizonAtLastAdvance = horizon
		w.horizonLastAdvance = now
		return
	}
	if now.Sub(w.horizonLastAdvance) >= horizonStallWarnInterval {
		klog.Warningf("PostgresWatcher: snapshot horizon frozen at xid8 %d for %s; a long-running transaction is blocking newer events for key=%q",
			horizon, now.Sub(w.horizonLastAdvance).Round(time.Second), w.key)
		// Reset timer so we log again periodically, not every poll.
		w.horizonLastAdvance = now
	}
}

// toWatchEvent converts a changelog row into a watch.Event.
func (w *postgresWatch) toWatchEvent(eventType string, data []byte, rv int64) (*watch.Event, error) {
	var watchType watch.EventType
	switch eventType {
	case "ADDED":
		watchType = watch.Added
	case "MODIFIED":
		watchType = watch.Modified
	case "DELETED":
		watchType = watch.Deleted
	default:
		return nil, fmt.Errorf("unknown event type: %s", eventType)
	}

	if data == nil {
		return nil, fmt.Errorf("changelog entry has nil data")
	}

	obj, _, err := w.codec.Decode(data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decode changelog data: %w", err)
	}

	// Set the resource version on the decoded object
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(obj, uint64(rv)); err != nil {
		return nil, fmt.Errorf("failed to set resource version: %w", err)
	}

	return &watch.Event{
		Type:   watchType,
		Object: obj,
	}, nil
}

// sendBookmark sends a periodic bookmark event reflecting the latest RV
// that is guaranteed committed. We use only changelog rows whose commit_xid
// is strictly below the current snapshot xmin so the advertised RV can be
// safely handed back as a resume point — resume-from a not-yet-committed RV
// would confuse seedCursorFromRV into skipping rows.
func (w *postgresWatch) sendBookmark() {
	var maxRV int64
	err := w.db.QueryRow(
		`SELECT COALESCE(MAX(resource_version), 0)
		   FROM ipam_changelog
		  WHERE commit_xid < pg_snapshot_xmin(pg_current_snapshot())::text::bigint`,
	).Scan(&maxRV)
	if err != nil {
		klog.ErrorS(err, "Failed to get max resource version for bookmark")
		return
	}
	if maxRV <= w.lastRV {
		return
	}
	w.sendBookmarkAt(uint64(maxRV))
}

// sendBookmarkAt emits a bookmark event with the supplied resource version.
// Used both by the periodic bookmark ticker and by RequestWatchProgress to
// signal "the storage is at least at this RV".
func (w *postgresWatch) sendBookmarkAt(rv uint64) {
	if w.newFunc == nil {
		return
	}
	obj := w.newFunc()
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(obj, rv); err != nil {
		klog.ErrorS(err, "Failed to set resource version on bookmark object")
		return
	}
	event := watch.Event{Type: watch.Bookmark, Object: obj}
	select {
	case w.result <- event:
		if int64(rv) > w.lastRV {
			w.lastRV = int64(rv)
		}
	case <-w.done:
	default:
		// Channel full — caller will retry
	}
}
