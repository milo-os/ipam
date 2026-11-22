package allocator

// The retention sweeper.
//
// A retained allocation's deadline is fixed when its claim is deleted and
// stored as expires_at (see lease.go). The sweeper is what acts on it. It runs
// in every apiserver replica, and each pass is taken by whichever replica gets
// a Postgres advisory lock first; the others skip that pass. The lock is only
// there so two replicas do not do the same work: correctness rests on the pool
// lock, as it does for every other writer of allocation rows.
//
// # One pass
//
//  1. Evaluate rows whose lease is still unknown (expires_at NULL), a bounded
//     batch per pass, one short transaction per pool.
//  2. Find the due set: one scan of the expiry index, oldest deadline first,
//     across all pools. A pool full of rows with no lease ('infinity') sorts
//     last and cannot hold back a due row anywhere else.
//  3. Enforce: per pool, one transaction. Take the pool row with SKIP LOCKED,
//     so a live claim is never queued behind the sweeper and the pool is left
//     for the next pass instead; then delete exactly the due rows still
//     retained and still due. That predicate is the compare-and-set against a
//     reclaim that won the pool lock first. Capacity is republished and the
//     IPAllocation objects deleted, with DELETED changelog entries, in the
//     same transaction.
//
//     Observe: count what enforce would release, once per deadline, and
//     release nothing.
//
// The retained-set gauges are read on every replica every pass, whether or not
// it holds the lock, so each replica exports current values.
//
// # Lock order
//
// A pass never holds two pool locks: every row it writes is in the one pool
// its transaction locked, and a claim row is only ever in its own pool. The
// advisory lock is a session lock on a connection of its own, which no
// transaction waits on.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tracing"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// SweepMode is whether the sweeper releases what is due or only counts it.
type SweepMode string

const (
	// SweepObserve counts would-be releases and releases nothing.
	SweepObserve SweepMode = "observe"
	// SweepEnforce releases retained allocations whose lease has run out.
	SweepEnforce SweepMode = "enforce"
)

// ParseSweepMode reads a --retention-sweep-mode value.
func ParseSweepMode(s string) (SweepMode, error) {
	switch m := SweepMode(s); m {
	case SweepObserve, SweepEnforce:
		return m, nil
	}
	return "", fmt.Errorf("retention sweep mode %q is not one of %q, %q", s, SweepObserve, SweepEnforce)
}

// sweepLockKey is the pg_try_advisory_lock key one pass is taken under. Any
// fixed value works, as long as nothing else in the database uses it; this one
// spells "ipam_rsw".
const sweepLockKey int64 = 0x6970616d5f727377

const (
	// defaultEvaluateBatch bounds the rows of unknown lease one pass evaluates.
	defaultEvaluateBatch = 500
	// defaultDueBatch bounds the due rows one pass reads. A pool's share is
	// deleted in one transaction holding that pool's lock, so this also bounds
	// how long a claim against the pool can be made to wait.
	defaultDueBatch = 200
)

// Sweeper runs retention sweep passes against one database.
type Sweeper struct {
	db   *pgxpool.Pool
	mode SweepMode

	evaluateBatch int
	dueBatch      int
}

// NewSweeper returns a sweeper in mode.
func NewSweeper(db *pgxpool.Pool, mode SweepMode) *Sweeper {
	return &Sweeper{db: db, mode: mode, evaluateBatch: defaultEvaluateBatch, dueBatch: defaultDueBatch}
}

// SweepResult is what one pass did.
type SweepResult struct {
	// Ran is false when another replica held the pass's lock and this one
	// did nothing but publish gauges.
	Ran bool
	// Evaluated is rows whose lease this pass resolved.
	Evaluated int
	// Due is due rows the pass found: in observe mode, those that fell due
	// since the last count.
	Due int
	// Released is rows released (enforce).
	Released int
	// WouldRelease is deadlines counted as would-be releases (observe).
	WouldRelease int
	// PoolsSkipped is pools left for the next pass because a claim held them.
	PoolsSkipped int
	// Errors is failures the pass carried on past.
	Errors int
}

// Run sweeps every interval until ctx is done, starting with a pass at once. A
// pass that fails is logged and counted; the loop carries on, because a sweep
// that cannot run is a capacity problem, not a serving one.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	metrics.RetentionSweepInterval.Set(interval.Seconds())
	klog.InfoS("Retention sweeper started", "interval", interval.String(), "mode", string(s.mode))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, max(interval, time.Minute))
		if _, err := s.Sweep(passCtx); err != nil && ctx.Err() == nil {
			klog.ErrorS(err, "Retention sweep pass failed")
		}
		cancel()
		select {
		case <-ctx.Done():
			klog.InfoS("Retention sweeper stopped")
			return
		case <-ticker.C:
		}
	}
}

// Sweep runs one pass. The error is a failure that stopped the pass; failures
// it carried on past are counted in the result and in the errors metric.
func (s *Sweeper) Sweep(ctx context.Context) (res SweepResult, err error) {
	start := time.Now()
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanRetentionSweep)
	defer func() {
		span.SetAttributes(
			attribute.String(tracing.AttrSweepMode, string(s.mode)),
			attribute.Bool(tracing.AttrSweepRan, res.Ran),
			attribute.Int(tracing.AttrSweepEvaluated, res.Evaluated),
			attribute.Int(tracing.AttrSweepDue, res.Due),
			attribute.Int(tracing.AttrSweepReleased, res.Released),
			attribute.Int(tracing.AttrSweepSkipped, res.PoolsSkipped),
		)
		if err != nil {
			res.Errors++
			span.SetStatus(codes.Error, err.Error())
		}
		metrics.RetentionSweepErrors.Add(float64(res.Errors))
		metrics.RetentionSweepPoolsSkipped.Add(float64(res.PoolsSkipped))
		if res.Ran {
			metrics.RetentionSweepDuration.Observe(time.Since(start).Seconds())
			if res.Errors == 0 {
				metrics.RetentionSweepLastSuccess.Set(float64(time.Now().Unix()))
			}
		}
		span.End()
	}()

	if gerr := publishRetainedGauges(ctx, s.db); gerr != nil {
		klog.ErrorS(gerr, "Could not read the retained set; keeping the previous gauges")
		res.Errors++
	}

	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return res, fmt.Errorf("acquire a connection for the sweep lock: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, sweepLockKey).Scan(&locked); err != nil {
		conn.Release()
		return res, fmt.Errorf("take the sweep lock: %w", err)
	}
	if !locked {
		conn.Release()
		return res, nil
	}
	defer releaseSweepLock(conn)
	res.Ran = true

	state, err := s.readState(ctx)
	if err != nil {
		return res, err
	}
	if err := s.evaluate(ctx, state, &res); err != nil {
		return res, err
	}
	if s.mode == SweepEnforce {
		err = s.release(ctx, &res)
	} else {
		err = s.observe(ctx, &res)
	}
	if err != nil {
		return res, err
	}
	metrics.RecordRetentionReleases(string(SweepEnforce), res.Released)
	metrics.RecordRetentionReleases(string(SweepObserve), res.WouldRelease)

	klog.V(2).InfoS("Retention sweep pass complete", "mode", string(s.mode),
		"evaluated", res.Evaluated, "due", res.Due, "released", res.Released,
		"wouldRelease", res.WouldRelease, "poolsSkipped", res.PoolsSkipped, "errors", res.Errors)
	return res, nil
}

// releaseSweepLock lets go of the pass's advisory lock. If that fails the
// session is closed rather than returned to the pool still holding it, which
// would stop every replica from sweeping until the connection was recycled.
func releaseSweepLock(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, sweepLockKey); err != nil {
		klog.ErrorS(err, "Could not release the retention sweep lock; closing its session")
		_ = conn.Hijack().Close(ctx)
		return
	}
	conn.Release()
}

// sweepState is the observing sweeper's progress: the last deadline it counted.
type sweepState struct {
	countedThrough    pgtype.Timestamptz
	countedThroughKey string
}

// readState writes this pass's mode and reads where counting stopped. The row
// is created on first use, with nothing counted yet.
func (s *Sweeper) readState(ctx context.Context) (sweepState, error) {
	var st sweepState
	err := s.db.QueryRow(ctx,
		`INSERT INTO ipam_retention_sweep_state (id, mode, counted_through, counted_through_key)
		 VALUES (1, $1, '-infinity', '')
		 ON CONFLICT (id) DO UPDATE SET mode = EXCLUDED.mode, updated_at = NOW()
		 RETURNING counted_through, counted_through_key`, string(s.mode),
	).Scan(&st.countedThrough, &st.countedThroughKey)
	if err != nil {
		return st, fmt.Errorf("read sweep state: %w", err)
	}
	return st, nil
}

// countedBefore reports whether a deadline at expiresAt, under allocationKey,
// sorts at or before where counting stopped: written too late for a scan to
// count it.
func (st sweepState) countedBefore(expiresAt time.Time, allocationKey string) bool {
	switch st.countedThrough.InfinityModifier {
	case pgtype.NegativeInfinity:
		return false
	case pgtype.Infinity:
		return true
	}
	through := st.countedThrough.Time
	return expiresAt.Before(through) || (expiresAt.Equal(through) && allocationKey <= st.countedThroughKey)
}

// candidate is a row the pass found to act on.
type candidate struct {
	retainedRow
	ipFamily     string
	cidr         string
	ownerProject string
	expiresAt    time.Time
}

// byPool groups rows by pool, in key order, keeping each pool's rows in the
// order they were read.
func byPool(rows []candidate) ([]string, map[string][]candidate) {
	groups := map[string][]candidate{}
	var keys []string
	for _, r := range rows {
		if _, ok := groups[r.poolKey]; !ok {
			keys = append(keys, r.poolKey)
		}
		groups[r.poolKey] = append(groups[r.poolKey], r)
	}
	slices.Sort(keys)
	return keys, groups
}

// evaluate resolves the lease of retained rows that do not have one yet.
func (s *Sweeper) evaluate(ctx context.Context, state sweepState, res *SweepResult) error {
	defer metrics.ObserveQuery("find_unevaluated_retained", time.Now())
	rows, err := s.db.Query(ctx,
		`SELECT allocation_key, pool_key, class_name, class_project, retained_at
		   FROM ipam_cidr_allocations
		  WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NOT NULL AND expires_at IS NULL
		  ORDER BY retained_at
		  LIMIT $1`, s.evaluateBatch)
	if err != nil {
		return fmt.Errorf("find retained allocations to evaluate: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.allocationKey, &c.poolKey, &c.className, &c.classProject, &c.retainedAt)
		return c, err
	})
	if err != nil {
		return fmt.Errorf("read retained allocations to evaluate: %w", err)
	}

	keys, groups := byPool(found)
	for _, poolKey := range keys {
		s.inPool(ctx, poolKey, "evaluate", res, func(tx pgx.Tx, pool *ipamv1alpha1.IPPool) (func(), error) {
			evaluated, late := 0, 0
			for _, c := range groups[poolKey] {
				lease, ok, err := leaseFor(ctx, tx, pool, poolKey, c.className, c.classProject)
				if !ok {
					klog.ErrorS(err, "Could not evaluate a retained allocation's lease", "allocation", c.allocationKey, "class", c.className)
					res.Errors++
					continue
				}
				r, changed, err := writeLease(ctx, tx, c.allocationKey, lease)
				if err != nil {
					return nil, err
				}
				if !changed {
					continue
				}
				if err := writeRetentionStatus(ctx, tx, r); err != nil {
					return nil, err
				}
				evaluated++
				if s.mode == SweepObserve && r.Lease == LeaseFinite && state.countedBefore(r.ExpiresAt, r.AllocationKey) {
					late++
				}
			}
			return func() {
				res.Evaluated += evaluated
				res.WouldRelease += late
			}, nil
		})
	}
	return nil
}

// findDue reads the due set. The predicate is the expiry index's, plus the
// deadline, so the scan starts at the oldest deadline and stops at now.
func (s *Sweeper) findDue(ctx context.Context, after string, args ...any) ([]candidate, error) {
	defer metrics.ObserveQuery("find_due_retained", time.Now())
	rows, err := s.db.Query(ctx,
		`SELECT allocation_key, pool_key, class_name, retained_at, ip_family,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr), owner_project, expires_at
		   FROM ipam_cidr_allocations
		  WHERE claim_key IS NULL AND purpose = 'Claim' AND expires_at IS NOT NULL
		    AND retained_at IS NOT NULL AND expires_at <= now()`+after, args...)
	if err != nil {
		return nil, fmt.Errorf("find due retained allocations: %w", err)
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.allocationKey, &c.poolKey, &c.className, &c.retainedAt, &c.ipFamily,
			&c.cidr, &c.ownerProject, &c.expiresAt)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read due retained allocations: %w", err)
	}
	return due, nil
}

// release frees what is due, pool by pool.
func (s *Sweeper) release(ctx context.Context, res *SweepResult) error {
	due, err := s.findDue(ctx, ` ORDER BY expires_at LIMIT $1`, s.dueBatch)
	if err != nil {
		return err
	}
	res.Due = len(due)

	keys, groups := byPool(due)
	for _, poolKey := range keys {
		s.inPool(ctx, poolKey, "release", res, func(tx pgx.Tx, pool *ipamv1alpha1.IPPool) (func(), error) {
			released, err := releaseDue(ctx, tx, poolKey, pool, groups[poolKey])
			if err != nil {
				return nil, err
			}
			return func() {
				res.Released += len(released)
				logReleased(poolKey, released, groups[poolKey])
			}, nil
		})
	}
	return nil
}

// releaseDue frees the rows of one pool, which tx holds locked, that were due
// when read and still are. A row a reclaim bound, or something released, since
// it was read is not matched by the delete and is left alone.
func releaseDue(ctx context.Context, tx pgx.Tx, poolKey string, pool *ipamv1alpha1.IPPool, due []candidate) ([]releasedAllocation, error) {
	keys := make([]string, 0, len(due))
	for _, c := range due {
		keys = append(keys, c.allocationKey)
	}
	released, err := deleteLockedAllocations(ctx, tx, map[string]*ipamv1alpha1.IPPool{poolKey: pool},
		`allocation_key = ANY($1::text[]) AND claim_key IS NULL AND purpose = 'Claim'
		 AND retained_at IS NOT NULL AND expires_at <= now()`, keys)
	if err != nil {
		return nil, err
	}
	for _, r := range released {
		if _, err := deleteObject(ctx, tx, r.allocationKey); err != nil {
			return nil, fmt.Errorf("delete IPAllocation %q: %w", r.allocationKey, err)
		}
	}
	return released, nil
}

// logReleased records each release at a level an operator sees without a debug
// flag: an address leaving its holder on a schedule is the event someone asks
// about later, so the line names the holder and the deadline.
func logReleased(poolKey string, released []releasedAllocation, due []candidate) {
	byKey := make(map[string]candidate, len(due))
	for _, c := range due {
		byKey[c.allocationKey] = c
	}
	for _, r := range released {
		c := byKey[r.allocationKey]
		klog.InfoS("Released a retained allocation whose lease ran out",
			"allocation", r.allocationKey, "cidr", r.cidr, "pool", poolKey,
			"class", c.className, "ownerProject", c.ownerProject,
			"retainedAt", c.retainedAt.UTC().Format(time.RFC3339),
			"expiresAt", c.expiresAt.UTC().Format(time.RFC3339))
	}
}

// observe counts the deadlines that have passed since the last count, and
// moves the count on past them. It releases nothing, so a due row stays due;
// counting it once, as its deadline passes, is what keeps the would-release
// rate the rate enforce will have.
//
// The state row is locked for the count, so a pool's retention cap tightened
// meanwhile (TightenRetention) either commits before the count reads, or sees
// where this count stopped.
func (s *Sweeper) observe(ctx context.Context, res *SweepResult) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin observe transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var st sweepState
	if err := tx.QueryRow(ctx,
		`SELECT counted_through, counted_through_key FROM ipam_retention_sweep_state WHERE id = 1 FOR UPDATE`,
	).Scan(&st.countedThrough, &st.countedThroughKey); err != nil {
		return fmt.Errorf("lock sweep state: %w", err)
	}

	var (
		lastAt  pgtype.Timestamptz
		lastKey string
	)
	n := 0
	queryStart := time.Now()
	rows, err := tx.Query(ctx,
		`SELECT allocation_key, expires_at,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr), pool_key
		   FROM ipam_cidr_allocations
		  WHERE claim_key IS NULL AND purpose = 'Claim' AND expires_at IS NOT NULL
		    AND retained_at IS NOT NULL AND expires_at <= now()
		    AND (expires_at, allocation_key) > ($1, $2)
		  ORDER BY expires_at, allocation_key
		  LIMIT $3`, st.countedThrough, st.countedThroughKey, s.dueBatch)
	if err != nil {
		return fmt.Errorf("find newly due retained allocations: %w", err)
	}
	for rows.Next() {
		var key, cidr, poolKey string
		if err := rows.Scan(&key, &lastAt, &cidr, &poolKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan newly due retained allocation: %w", err)
		}
		lastKey = key
		n++
		klog.V(2).InfoS("A retained allocation's lease ran out; observe mode leaves it held",
			"allocation", key, "cidr", cidr, "pool", poolKey, "expiresAt", lastAt.Time.UTC().Format(time.RFC3339))
	}
	rows.Close()
	metrics.ObserveQuery("find_newly_due_retained", queryStart)
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate newly due retained allocations: %w", err)
	}
	res.Due = n
	if n == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ipam_retention_sweep_state
		    SET counted_through = $1, counted_through_key = $2, updated_at = NOW()
		  WHERE id = 1`, lastAt, lastKey); err != nil {
		return fmt.Errorf("record observe progress: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit observe progress: %w", err)
	}
	res.WouldRelease += n
	return nil
}

// inPool runs fn in one transaction holding poolKey's lock, taken without
// waiting. A pool a live claim holds is skipped and left for the next pass.
// A failure is logged and counted, and the pass carries on with the next
// pool: one pool that cannot be swept must not stop the rest.
//
// fn may return a func to run once the transaction has committed, for what
// must only be said of work that happened.
func (s *Sweeper) inPool(ctx context.Context, poolKey, phase string, res *SweepResult, fn func(pgx.Tx, *ipamv1alpha1.IPPool) (func(), error)) {
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanRetentionSweepPool, trace.WithAttributes(
		attribute.String(tracing.AttrPoolName, poolNameOf(poolKey)),
		attribute.String(tracing.AttrSweepPhase, phase),
	))
	defer span.End()

	err := func() error {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin sweep transaction: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		pool, err := lockPoolNoWait(ctx, tx, poolKey)
		if err != nil {
			return err
		}
		if pool == nil {
			res.PoolsSkipped++
			span.SetAttributes(attribute.Bool(tracing.AttrSweepSkipped, true))
			return nil
		}
		afterCommit, err := fn(tx, pool)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit sweep transaction: %w", err)
		}
		if afterCommit != nil {
			afterCommit()
		}
		return nil
	}()
	if err != nil {
		res.Errors++
		span.SetStatus(codes.Error, err.Error())
		klog.ErrorS(err, "Retention sweep of a pool failed", "pool", poolKey, "phase", phase)
	}
}

// lockPoolNoWait locks and decodes a pool row if no one else holds it. It
// returns nil, and no error, when the row is locked, which is the sweeper's
// cue to yield. A pool with rows cannot be deleted, so a missing row is a
// locked one in practice, and is skipped the same way.
func lockPoolNoWait(ctx context.Context, tx pgx.Tx, poolKey string) (*ipamv1alpha1.IPPool, error) {
	defer metrics.ObserveQuery("select_pool_for_update_skip_locked", time.Now())
	var data []byte
	err := tx.QueryRow(ctx,
		`SELECT data FROM ipam_objects WHERE key = $1 FOR UPDATE SKIP LOCKED`, poolKey,
	).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock pool row: %w", err)
	}
	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, fmt.Errorf("decode pool object: %w", err)
	}
	return &pool, nil
}

// publishRetainedGauges reads the retained set in one aggregate query and
// replaces the gauges with it. Rows retained before retention was timestamped
// count as no lease: nothing will ever release them on a schedule.
func publishRetainedGauges(ctx context.Context, db Querier) error {
	defer metrics.ObserveQuery("retained_set_gauges", time.Now())
	rows, err := db.Query(ctx,
		`SELECT ip_family,
		        count(*) FILTER (WHERE retained_at IS NOT NULL AND expires_at IS NOT NULL AND expires_at <> 'infinity'),
		        count(*) FILTER (WHERE retained_at IS NULL OR expires_at = 'infinity'),
		        count(*) FILTER (WHERE retained_at IS NOT NULL AND expires_at IS NULL),
		        COALESCE(EXTRACT(EPOCH FROM now() - min(retained_at)), 0)::float8
		   FROM ipam_cidr_allocations
		  WHERE claim_key IS NULL AND purpose = 'Claim'
		  GROUP BY ip_family`)
	if err != nil {
		return fmt.Errorf("read retained set: %w", err)
	}
	families, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (metrics.RetainedFamily, error) {
		var f metrics.RetainedFamily
		err := row.Scan(&f.IPFamily, &f.Finite, &f.None, &f.Unevaluated, &f.OldestAgeSeconds)
		return f, err
	})
	if err != nil {
		return fmt.Errorf("read retained set: %w", err)
	}
	metrics.PublishRetained(families)
	return nil
}
