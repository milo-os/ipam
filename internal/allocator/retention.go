package allocator

// The retention sweeper.
//
// A retained allocation's deadline is fixed when its claim is deleted and
// stored as expires_at (see lease.go). The sweeper is what acts on it. It runs
// in every apiserver replica, and each pass is taken by whichever replica gets
// a Postgres advisory lock first; the others skip that pass.
//
// # One pass
//
//  1. Apply pool caps: for each pool with a maxRetentionLease, bring forward
//     any deadline the cap cuts short (tightenLocked). An IPPool update does
//     this straight away, best effort; this is what makes it certain.
//  2. Evaluate rows whose lease is still unknown (expires_at NULL).
//  3. Enforce: release due rows, oldest deadline first across all pools, so a
//     pool full of rows with no lease ('infinity') cannot hold back a due row
//     anywhere else. Observe: record each due deadline not yet counted, and
//     release nothing.
//
// Steps 1 to 3 take each pool's lock with SKIP LOCKED, in short transactions
// of at most a chunk of rows, so a live claim is never queued behind the
// sweeper; a pool it holds is left for the next pass. Steps 2 and 3 page
// through their sets by key, under a row budget, and resume on the next pass
// where this one stopped, wrapping round at the end. Rows and pools that fail,
// or are skipped, are paged past rather than retried, so no prefix of bad rows
// can starve the rest. A single row that fails is undone under a savepoint, so
// it costs its own release, not its pool's.
//
// # What the advisory lock is for
//
// Only to stop two replicas doing the same work. It is a session lock, and if
// the session holding it dies mid-pass another replica can start a pass while
// the first finishes its current pool. That is safe: every write is made under
// the pool's row lock and predicated on the row still being retained and due
// (the compare-and-set a reclaim races), exactly as for any other writer of
// allocation rows. The pass checks its session before each pool transaction and
// stops when it has gone. The lock needs a session to live on, so a connection
// pooler between IPAM and Postgres must run in session mode.
//
// # Lock order
//
// A pass never holds two pool locks: every row it writes is in the one pool
// its transaction locked, and a claim row is only ever in its own pool. Within
// a pool it locks the pool, then rows, then objects, as every other writer
// does. The observe ledger is written in a transaction that locks no pool.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// defaultPageSize is how many rows one read of a set returns.
	defaultPageSize = 200
	// defaultRowBudget bounds the rows one pass reads from each set, so a
	// backlog is worked through over several passes rather than in one long
	// one.
	defaultRowBudget = 2000
	// defaultChunk bounds the rows one pool transaction acts on. The
	// transaction holds the pool's lock, so this bounds how long a claim
	// against the pool can find it taken, and how many rows share one
	// republication of the pool's capacity.
	defaultChunk = 25
	// defaultCapPools bounds the capped pools one pass checks.
	defaultCapPools = 200
)

// Sweeper runs retention sweep passes against one database. It is not safe
// for concurrent use; Run drives one.
type Sweeper struct {
	db   *pgxpool.Pool
	mode SweepMode

	pageSize, rowBudget, chunk, capPools int

	// Where the previous pass stopped in each set, so the next resumes there.
	evaluateFrom, dueFrom keyset
	capFrom               string
}

// keyset is a position in a set ordered by (time, allocation key).
type keyset struct {
	set bool
	at  time.Time
	key string
}

// NewSweeper returns a sweeper in mode.
func NewSweeper(db *pgxpool.Pool, mode SweepMode) *Sweeper {
	return &Sweeper{
		db: db, mode: mode,
		pageSize: defaultPageSize, rowBudget: defaultRowBudget, chunk: defaultChunk, capPools: defaultCapPools,
	}
}

// SweepResult is what one pass did.
type SweepResult struct {
	// Ran is false when another replica held the pass's lock and this one
	// did nothing but publish gauges.
	Ran bool
	// Tightened is deadlines a pool's cap brought forward.
	Tightened int
	// Evaluated is rows whose lease this pass resolved.
	Evaluated int
	// Unevaluable is rows whose class could not be read, left unevaluated.
	Unevaluable int
	// Due is due rows the pass read: in observe mode, those not counted yet.
	Due int
	// Released is rows released (enforce).
	Released int
	// WouldRelease is deadlines counted as would-be releases (observe).
	WouldRelease int
	// PoolsSkipped is pools left for the next pass because a claim held them.
	PoolsSkipped int
	// Errors is pools whose transaction failed; the pass carried on.
	Errors int
	// RowErrors is single rows the pass could not act on; their pools were
	// swept regardless.
	RowErrors int
}

// Run sweeps every interval until ctx is done, starting with a pass at once.
//
// A pass that fails is logged and counted, and the loop carries on: a sweep
// that cannot run is a capacity problem, not a serving one. A pass that panics
// is recovered for the same reason, and the loop backs off before the next,
// doubling up to ten intervals while it keeps panicking.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	metrics.RetentionSweepInterval.Set(interval.Seconds())
	klog.InfoS("Retention sweeper started", "interval", interval.String(), "mode", string(s.mode))
	backoff := interval
	for {
		wait := interval
		if s.safePass(ctx, interval) {
			wait = backoff
			backoff = min(backoff*2, 10*interval)
		} else {
			backoff = interval
		}
		select {
		case <-ctx.Done():
			klog.InfoS("Retention sweeper stopped")
			return
		case <-time.After(wait):
		}
	}
}

// safePass runs one pass, and reports whether it panicked.
func (s *Sweeper) safePass(ctx context.Context, interval time.Duration) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			metrics.RetentionSweepErrors.Inc()
			klog.ErrorS(fmt.Errorf("%v", r), "Retention sweep pass panicked; the apiserver carries on and the sweeper retries after a backoff")
		}
	}()
	passCtx, cancel := context.WithTimeout(ctx, max(interval, time.Minute))
	defer cancel()
	if _, err := s.Sweep(passCtx); err != nil && ctx.Err() == nil {
		klog.ErrorS(err, "Retention sweep pass failed")
	}
	return false
}

// Sweep runs one pass. The error is a failure that stopped the pass; failures
// it carried on past are counted in the result and the metrics.
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
		metrics.RetentionSweepRowErrors.Add(float64(res.RowErrors))
		metrics.RetentionLeaseUnevaluable.Add(float64(res.Unevaluable))
		metrics.RetentionSweepPoolsSkipped.Add(float64(res.PoolsSkipped))
		if res.Ran {
			metrics.RetentionSweepDuration.Observe(time.Since(start).Seconds())
			if err == nil {
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

	held := func() error {
		if _, err := conn.Exec(ctx, `SELECT 1`); err != nil {
			return fmt.Errorf("lost the sweep lock's session: %w", err)
		}
		return nil
	}
	if err := s.applyCaps(ctx, held, &res); err != nil {
		return res, err
	}
	if err := s.evaluate(ctx, held, &res); err != nil {
		return res, err
	}
	if s.mode == SweepEnforce {
		err = s.release(ctx, held, &res)
	} else {
		err = s.observe(ctx, &res)
	}
	if err != nil {
		return res, err
	}
	metrics.RecordRetentionReleases(string(SweepEnforce), res.Released)

	klog.V(2).InfoS("Retention sweep pass complete", "mode", string(s.mode),
		"tightened", res.Tightened, "evaluated", res.Evaluated, "unevaluable", res.Unevaluable,
		"due", res.Due, "released", res.Released, "wouldRelease", res.WouldRelease,
		"poolsSkipped", res.PoolsSkipped, "errors", res.Errors, "rowErrors", res.RowErrors)
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

// chunks splits rows into runs of at most n.
func chunks(rows []candidate, n int) [][]candidate {
	var out [][]candidate
	for len(rows) > n {
		out = append(out, rows[:n])
		rows = rows[n:]
	}
	if len(rows) > 0 {
		out = append(out, rows)
	}
	return out
}

// pageThrough reads a set a page at a time from *from, under the pass's row
// budget, handing each page to act, and leaves *from where the next pass should
// start: after the last row read, or at the beginning once the end is reached.
func (s *Sweeper) pageThrough(from *keyset, read func(after keyset, limit int) ([]candidate, error), at func(candidate) time.Time, act func([]candidate) error) error {
	for budget := s.rowBudget; budget > 0; {
		page, err := read(*from, min(s.pageSize, budget))
		if err != nil {
			return err
		}
		if len(page) == 0 {
			*from = keyset{}
			return nil
		}
		last := page[len(page)-1]
		*from = keyset{set: true, at: at(last), key: last.allocationKey}
		budget -= len(page)
		if err := act(page); err != nil {
			return err
		}
		if len(page) < s.pageSize {
			*from = keyset{}
			return nil
		}
	}
	return nil
}

// afterClause is the keyset predicate continuing a scan ordered by (col,
// allocation_key) from k, and its arguments, numbered from n.
func afterClause(col string, k keyset, n int) (string, []any) {
	if !k.set {
		return "", nil
	}
	return fmt.Sprintf(" AND (%s, allocation_key) > ($%d, $%d)", col, n, n+1), []any{k.at, k.key}
}

// applyCaps brings retained deadlines in every capped pool forward to the cap.
// Pools are found by an unlocked read and tightened one transaction each.
func (s *Sweeper) applyCaps(ctx context.Context, held func() error, res *SweepResult) error {
	defer metrics.ObserveQuery("find_capped_pools", time.Now())
	rows, err := s.db.Query(ctx,
		`SELECT o.key FROM ipam_objects o
		  WHERE o.kind = 'IPPool' AND o.key > $1
		    AND ipam_data_to_jsonb(o.data) -> 'spec' ? 'maxRetentionLease'
		    AND EXISTS (SELECT 1 FROM ipam_cidr_allocations a
		                 WHERE a.pool_key = o.key AND a.claim_key IS NULL AND a.purpose = 'Claim'
		                   AND a.retained_at IS NOT NULL AND a.expires_at > now())
		  ORDER BY o.key
		  LIMIT $2`, s.capFrom, s.capPools)
	if err != nil {
		return fmt.Errorf("find capped pools: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read capped pools: %w", err)
	}
	if len(keys) < s.capPools {
		s.capFrom = ""
	} else {
		s.capFrom = keys[len(keys)-1]
	}
	for _, poolKey := range keys {
		if err := held(); err != nil {
			return err
		}
		s.inPool(ctx, poolKey, "cap", res, func(tx pgx.Tx, pool *ipamv1alpha1.IPPool) (func(), error) {
			n, err := tightenLocked(ctx, tx, poolKey, pool)
			if err != nil {
				return nil, err
			}
			return func() { res.Tightened += n }, nil
		})
	}
	return nil
}

// leaseMemo remembers each class's lease, and each failure to read one, for
// one pass: a pass's rows are overwhelmingly of a few classes, and a class
// that cannot be read should cost one read, not one per row.
type leaseMemo map[string]memoLease

type memoLease struct {
	lease *metav1.Duration
	err   error
}

func (m leaseMemo) read(ctx context.Context, tx pgx.Tx, project, name string) (*metav1.Duration, error) {
	key := project + "/" + name
	if v, ok := m[key]; ok {
		return v.lease, v.err
	}
	lease, err := classRetentionLease(ctx, tx, project, name)
	m[key] = memoLease{lease, err}
	return lease, err
}

// evaluate resolves the lease of retained rows that do not have one yet.
//
// A row whose class cannot be read is counted and paged past, as is a pool
// that is skipped or fails, for the rest of the pass; neither holds back the
// rows after it.
func (s *Sweeper) evaluate(ctx context.Context, held func() error, res *SweepResult) error {
	memo := leaseMemo{}
	passedOver := map[string]bool{}
	read := func(after keyset, limit int) ([]candidate, error) {
		defer metrics.ObserveQuery("find_unevaluated_retained", time.Now())
		clause, args := afterClause("retained_at", after, 2)
		rows, err := s.db.Query(ctx,
			`SELECT allocation_key, pool_key, class_name, class_project, retained_at
			   FROM ipam_cidr_allocations
			  WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NOT NULL AND expires_at IS NULL`+clause+`
			  ORDER BY retained_at, allocation_key
			  LIMIT $1`, append([]any{limit}, args...)...)
		if err != nil {
			return nil, fmt.Errorf("find retained allocations to evaluate: %w", err)
		}
		found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
			var c candidate
			err := row.Scan(&c.allocationKey, &c.poolKey, &c.className, &c.classProject, &c.retainedAt)
			return c, err
		})
		if err != nil {
			return nil, fmt.Errorf("read retained allocations to evaluate: %w", err)
		}
		return found, nil
	}
	return s.pageThrough(&s.evaluateFrom, read, func(c candidate) time.Time { return c.retainedAt }, func(page []candidate) error {
		keys, groups := byPool(page)
		for _, poolKey := range keys {
			if passedOver[poolKey] {
				continue
			}
			for _, chunk := range chunks(groups[poolKey], s.chunk) {
				if err := held(); err != nil {
					return err
				}
				if !s.inPool(ctx, poolKey, "evaluate", res, func(tx pgx.Tx, pool *ipamv1alpha1.IPPool) (func(), error) {
					evaluated, unevaluable, rowErrors := evaluateChunk(ctx, tx, poolKey, pool, chunk, memo)
					return func() {
						res.Evaluated += evaluated
						res.Unevaluable += unevaluable
						res.RowErrors += rowErrors
					}, nil
				}) {
					passedOver[poolKey] = true
					break
				}
			}
		}
		return nil
	})
}

// evaluateChunk fixes the leases of rows in one pool, which tx holds locked.
// Each lease is written under a savepoint, so one that fails is undone alone
// and the row is left for a later pass.
func evaluateChunk(ctx context.Context, tx pgx.Tx, poolKey string, pool *ipamv1alpha1.IPPool, rows []candidate, memo leaseMemo) (evaluated, unevaluable, rowErrors int) {
	for _, c := range rows {
		lease, ok, err := leaseWith(ctx, tx, pool, poolKey, c.className, c.classProject, memo.read)
		if !ok {
			klog.ErrorS(err, "Could not evaluate a retained allocation's lease", "allocation", c.allocationKey, "class", c.className)
			unevaluable++
			continue
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			klog.ErrorS(err, "Could not open a savepoint to evaluate a lease", "allocation", c.allocationKey)
			rowErrors++
			continue
		}
		r, changed, err := writeLease(ctx, sp, c.allocationKey, lease)
		if err == nil {
			err = sp.Commit(ctx)
		} else {
			_ = sp.Rollback(ctx)
		}
		if err != nil {
			klog.ErrorS(err, "Could not write a retained allocation's lease", "allocation", c.allocationKey)
			rowErrors++
			continue
		}
		if !changed {
			continue
		}
		evaluated++
		// A publication that fails is counted by publishRetention; the lease
		// stands.
		publishRetention(ctx, tx, r)
	}
	return evaluated, unevaluable, rowErrors
}

// findDue reads a page of the due set from after. The predicate is the expiry
// index's, plus the deadline, so the scan starts at the oldest deadline and
// stops at now.
func (s *Sweeper) findDue(ctx context.Context, after keyset, limit int) ([]candidate, error) {
	defer metrics.ObserveQuery("find_due_retained", time.Now())
	clause, args := afterClause("expires_at", after, 2)
	rows, err := s.db.Query(ctx,
		`SELECT allocation_key, pool_key, class_name, retained_at, ip_family,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr), owner_project, expires_at
		   FROM ipam_cidr_allocations
		  WHERE claim_key IS NULL AND purpose = 'Claim' AND expires_at IS NOT NULL
		    AND retained_at IS NOT NULL AND expires_at <= now()`+clause+`
		  ORDER BY expires_at, allocation_key
		  LIMIT $1`, append([]any{limit}, args...)...)
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

// release frees what is due, pool by pool, a chunk per transaction. A pool
// that is skipped or fails is left alone for the rest of the pass, and its
// rows are paged past.
func (s *Sweeper) release(ctx context.Context, held func() error, res *SweepResult) error {
	passedOver := map[string]bool{}
	read := func(after keyset, limit int) ([]candidate, error) { return s.findDue(ctx, after, limit) }
	return s.pageThrough(&s.dueFrom, read, func(c candidate) time.Time { return c.expiresAt }, func(page []candidate) error {
		res.Due += len(page)
		keys, groups := byPool(page)
		for _, poolKey := range keys {
			if passedOver[poolKey] {
				continue
			}
			for _, chunk := range chunks(groups[poolKey], s.chunk) {
				if err := held(); err != nil {
					return err
				}
				if !s.inPool(ctx, poolKey, "release", res, func(tx pgx.Tx, pool *ipamv1alpha1.IPPool) (func(), error) {
					released, rowErrors, err := releaseDue(ctx, tx, poolKey, pool, chunk)
					if err != nil {
						return nil, err
					}
					return func() {
						res.Released += len(released)
						res.RowErrors += rowErrors
						logReleased(poolKey, released, chunk)
					}, nil
				}) {
					passedOver[poolKey] = true
					break
				}
			}
		}
		return nil
	})
}

// releaseDue frees the rows of one pool, which tx holds locked, that were due
// when read and still are, and republishes the pool's capacity once for all of
// them. A row a reclaim bound, or something released, since it was read is not
// matched and is left alone.
//
// Each row is released under a savepoint, so one whose release fails (its
// object cannot be deleted, say) is undone alone, left held, and counted; the
// rest of the chunk goes ahead. Rows are released one at a time because the
// pool's running consumption is adjusted per block, against what still holds
// its addresses after it goes.
func releaseDue(ctx context.Context, tx pgx.Tx, poolKey string, pool *ipamv1alpha1.IPPool, due []candidate) ([]releasedAllocation, int, error) {
	parents, err := parsePoolCIDR(pool)
	if err != nil {
		return nil, 0, err
	}
	var (
		released  []releasedAllocation
		consumed  *big.Int
		rowErrors int
	)
	for _, c := range due {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("open savepoint: %w", err)
		}
		r, total, err := releaseOne(ctx, sp, poolKey, parents, c.allocationKey)
		if err == nil {
			err = sp.Commit(ctx)
		} else {
			_ = sp.Rollback(ctx)
		}
		if err != nil {
			klog.ErrorS(err, "Could not release a retained allocation whose lease ran out; it stays held", "allocation", c.allocationKey, "pool", poolKey)
			rowErrors++
			continue
		}
		if r != nil {
			released = append(released, *r)
			consumed = total
		}
	}
	if len(released) > 0 {
		if err := persistPoolCapacity(ctx, tx, pool, poolKey, parents, consumed); err != nil {
			return nil, 0, fmt.Errorf("update pool capacity after release: %w", err)
		}
	}
	return released, rowErrors, nil
}

// releaseOne frees one due row and its IPAllocation, and returns the pool's
// consumption once it is gone; nil when the row is no longer due.
func releaseOne(ctx context.Context, tx pgx.Tx, poolKey string, parents []net.IPNet, allocationKey string) (*releasedAllocation, *big.Int, error) {
	r := releasedAllocation{allocationKey: allocationKey, poolKey: poolKey}
	err := tx.QueryRow(ctx,
		`DELETE FROM ipam_cidr_allocations
		  WHERE allocation_key = $1 AND pool_key = $2 AND claim_key IS NULL AND purpose = 'Claim'
		    AND retained_at IS NOT NULL AND expires_at <= now()
		 RETURNING ip_family, host(allocated_cidr) || '/' || masklen(allocated_cidr)`, allocationKey, poolKey,
	).Scan(&r.ipFamily, &r.cidr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("release %q: %w", allocationKey, err)
	}
	_, block, err := net.ParseCIDR(r.cidr)
	if err != nil {
		return nil, nil, fmt.Errorf("parse released cidr %q: %w", r.cidr, err)
	}
	// After the delete, so the overlap query sees what still holds these
	// addresses.
	consumed, err := consumptionAfterRelease(ctx, tx, poolKey, parents, *block)
	if err != nil {
		return nil, nil, err
	}
	if err := writeConsumed(ctx, tx, poolKey, consumed); err != nil {
		return nil, nil, err
	}
	if _, err := deleteObject(ctx, tx, allocationKey); err != nil {
		return nil, nil, fmt.Errorf("delete IPAllocation %q: %w", allocationKey, err)
	}
	return &r, consumed, nil
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

// observe counts each due deadline once, by recording it in the observe ledger
// in the transaction that counts it, and releases nothing. A due row stays due
// in observe mode, so counting the due set itself would count one row on every
// pass; the ledger is what makes the would-release rate the rate enforce will
// have. A deadline counts the first time a pass sees it due, however it came
// to be due: a retention that committed late, an evaluation, a tightened cap.
//
// The insert takes no pool lock, and its primary key makes the count exact
// even if two passes ever ran at once.
func (s *Sweeper) observe(ctx context.Context, res *SweepResult) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin observe transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	start := time.Now()
	rows, err := tx.Query(ctx,
		`INSERT INTO ipam_retention_observed (allocation_key, expires_at)
		 SELECT a.allocation_key, a.expires_at
		   FROM ipam_cidr_allocations a
		  WHERE a.claim_key IS NULL AND a.purpose = 'Claim' AND a.expires_at IS NOT NULL
		    AND a.retained_at IS NOT NULL AND a.expires_at <= now()
		    AND NOT EXISTS (SELECT 1 FROM ipam_retention_observed o
		                     WHERE o.allocation_key = a.allocation_key AND o.expires_at = a.expires_at)
		  ORDER BY a.expires_at, a.allocation_key
		  LIMIT $1
		 ON CONFLICT DO NOTHING
		 RETURNING allocation_key, expires_at`, s.rowBudget)
	if err != nil {
		return fmt.Errorf("record newly due retained allocations: %w", err)
	}
	type counted struct {
		key string
		at  time.Time
	}
	newly, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (counted, error) {
		var c counted
		err := row.Scan(&c.key, &c.at)
		return c, err
	})
	metrics.ObserveQuery("record_newly_due_retained", start)
	if err != nil {
		return fmt.Errorf("read newly due retained allocations: %w", err)
	}

	// Entries whose row is no longer retained under that deadline count
	// nothing any more.
	if _, err := tx.Exec(ctx,
		`DELETE FROM ipam_retention_observed o
		  WHERE NOT EXISTS (SELECT 1 FROM ipam_cidr_allocations a
		                     WHERE a.allocation_key = o.allocation_key AND a.expires_at = o.expires_at
		                       AND a.claim_key IS NULL)`); err != nil {
		return fmt.Errorf("prune the observe ledger: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit observe ledger: %w", err)
	}

	res.Due = len(newly)
	res.WouldRelease = len(newly)
	metrics.RecordRetentionReleases(string(SweepObserve), len(newly))
	for _, c := range newly {
		klog.V(2).InfoS("A retained allocation's lease ran out; observe mode leaves it held",
			"allocation", c.key, "expiresAt", c.at.UTC().Format(time.RFC3339))
	}
	return nil
}

// inPool runs fn in one transaction holding poolKey's lock, taken without
// waiting, and reports whether it ran and committed. A pool a live claim holds
// is skipped and left for the next pass. A failure is logged and counted, and
// the pass carries on with the next pool: one pool that cannot be swept must
// not stop the rest.
//
// fn may return a func to run once the transaction has committed, for what
// must only be said of work that happened.
func (s *Sweeper) inPool(ctx context.Context, poolKey, phase string, res *SweepResult, fn func(pgx.Tx, *ipamv1alpha1.IPPool) (func(), error)) bool {
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanRetentionSweepPool, trace.WithAttributes(
		attribute.String(tracing.AttrPoolName, poolNameOf(poolKey)),
		attribute.String(tracing.AttrSweepPhase, phase),
	))
	defer span.End()

	committed := false
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
		committed = true
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
	return committed
}

// lockPoolNoWait locks and decodes a pool row if no one else holds it. It
// returns nil, and no error, when the row is locked, which is the sweeper's
// cue to yield, or when there is no pool row at all: rows whose pool is gone
// have nothing to serialise against, and are left alone.
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
