package allocator

// The retention lease sweeper.
//
// A retained address is capacity nobody else can use, so retention carries a
// lease. This is the thing that enforces it.
//
// # Two thresholds, one clock, no extra state
//
// Expiry happens in two phases — an allocation is marked `Expiring` before it is
// released — because an address vanishing at 3am with no prior signal is exactly
// the event that should have announced itself. But the mark is *derived* rather
// than stored: both phases are thresholds on the same `retained_at` clock.
//
//	expiresAt = retained_at + lease
//	releaseAt = expiresAt   + grace
//
// That is worth stating because storing the mark is the obvious alternative and
// it is worse. A stored mark is a second source of truth that can disagree with
// the row, and a failed mark-write would leave an allocation that never gets
// released. Derived, a failed write costs nothing: the next pass recomputes the
// same answer. The `Expiring` condition on the IPAllocation is a *published
// projection* of a computed fact, not the fact itself.
//
// # Why it holds the pool lock
//
// The dangerous race is not sweeper-vs-sweeper — two sweepers releasing one
// address is idempotent. It is **sweeper-vs-reclaim**: a replacement workload
// re-creating its claim under the deterministic name should inherit the retained
// address, and a sweeper firing at that instant must not delete it out from
// under. Taking `SELECT ... FOR UPDATE` on the pool serialises the sweep against
// allocation in that pool using the lock the allocation path already takes,
// rather than inventing a second discipline. Two sweepers then produce one
// winner and one no-op for free.
//
// # Why one pool per transaction
//
// A sweep that walked many pools in one transaction would hold locks and a
// snapshot for its whole duration — the long-transaction problem that pins the
// Postgres xmin horizon and stalls watch delivery service-wide, which is the
// reason the provisioning cascade is split per level. Discovery is read-only and
// unlocked; each pool is then swept in its own short transaction, in batches.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/metrics"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

const (
	// DefaultLeaseGrace is how long an allocation sits marked `Expiring` before
	// it is released. It is the window in which an operator can see a release
	// coming and stop it by re-binding or extending the lease.
	//
	// Clamped to the lease itself for short leases — see graceFor.
	DefaultLeaseGrace = 24 * time.Hour

	// defaultSweepBatch bounds how many allocations one pool's transaction
	// handles. Small, because the transaction holds that pool's lock and every
	// claim against the pool waits behind it.
	defaultSweepBatch = 50

	// defaultSweepPools bounds how many pools one sweep pass visits, so a
	// backlog is worked through over several passes rather than in one long run.
	defaultSweepPools = 100
)

// SweepOptions configures one pass.
type SweepOptions struct {
	// Now is the instant the sweep evaluates against. Injectable so tests can
	// age a lease without sleeping.
	Now time.Time
	// Grace is the marked-to-released window. Zero uses DefaultLeaseGrace.
	Grace time.Duration
	// Batch bounds allocations per pool transaction. Zero uses the default.
	Batch int
	// MaxPools bounds pools per pass. Zero uses the default.
	MaxPools int
}

// SweepResult reports what a pass did.
//
// Candidates is separate from Marked and Released on purpose, and it is the
// field that makes the metrics honest: a sweep that examined nothing and a sweep
// that examined a hundred and found none due are both "zero released", and they
// mean completely different things. Without the distinction a sweeper whose
// query silently matches nothing is indistinguishable from a healthy one — the
// same shape as every silent failure this service has produced.
type SweepResult struct {
	// Pools visited.
	Pools int
	// Candidates is retained allocations examined — rows a lease could apply to.
	Candidates int
	// Marked is allocations newly past their lease and marked Expiring.
	Marked int
	// Released is allocations past lease + grace and released.
	Released int
	// NoLease is candidates that had no effective lease, and so are held
	// indefinitely. Counted rather than skipped silently: on a scarce range this
	// is the number that explains why capacity is not coming back.
	NoLease int
}

// SweepExpiredLeases runs one pass over the retained set.
//
// db rather than a transaction: each pool is swept in its own, for the reason in
// the package comment above. Errors on an individual pool are logged and the
// pass continues — one pool whose class was deleted must not stop every other
// pool's capacity from being reclaimed.
func (a *PostgresPrefixAllocator) SweepExpiredLeases(ctx context.Context, db TxBeginner, opts SweepOptions) (SweepResult, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultLeaseGrace
	}
	if opts.Batch <= 0 {
		opts.Batch = defaultSweepBatch
	}
	if opts.MaxPools <= 0 {
		opts.MaxPools = defaultSweepPools
	}

	poolKeys, err := discoverRetainedPools(ctx, db, opts.MaxPools)
	if err != nil {
		return SweepResult{}, err
	}

	var total SweepResult
	for _, poolKey := range poolKeys {
		res, err := a.sweepPool(ctx, db, poolKey, opts)
		total.Pools++
		total.Candidates += res.Candidates
		total.Marked += res.Marked
		total.Released += res.Released
		total.NoLease += res.NoLease
		if err != nil {
			// Logged and skipped rather than returned: a pool whose class was
			// deleted, or whose object is mid-write, must not stop the rest of
			// the estate from reclaiming capacity.
			klog.ErrorS(err, "lease sweep failed for pool", "pool", poolKey)
			metrics.RecordLeaseSweepError()
		}
	}

	metrics.RecordLeaseSweep(total.Candidates, total.Marked, total.Released, total.NoLease)
	klog.V(2).InfoS("lease sweep complete",
		"pools", total.Pools, "candidates", total.Candidates,
		"marked", total.Marked, "released", total.Released, "noLease", total.NoLease)
	return total, nil
}

// discoverRetainedPools lists pools holding retained allocations, without taking
// any lock.
//
// Unlocked because it decides only where to look. Anything it reports is
// re-checked under the pool's lock before being acted on, so a pool that stops
// qualifying between discovery and sweep simply yields nothing.
func discoverRetainedPools(ctx context.Context, db TxBeginner, limit int) ([]string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin lease discovery transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	defer metrics.ObserveQuery("discover_retained_pools", time.Now())
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT pool_key
		   FROM ipam_cidr_allocations
		  WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NOT NULL
		  LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("discover pools with retained allocations: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan retained pool key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retained pool keys: %w", err)
	}
	return keys, nil
}

// retainedRow is one candidate.
type retainedRow struct {
	allocationKey string
	className     string
	cidr          string
	ownerProject  string
	retainedAt    time.Time
}

// sweepPool handles one pool in one transaction, under that pool's lock.
func (a *PostgresPrefixAllocator) sweepPool(ctx context.Context, db TxBeginner, poolKey string, opts SweepOptions) (SweepResult, error) {
	var res SweepResult

	tx, err := db.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin lease sweep transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// The lock that makes this safe against a concurrent reclaim. Taken before
	// anything is read, so the candidate set cannot change underneath the
	// decisions made from it.
	pool, err := lockAndDecodeIPPool(ctx, tx, poolKey)
	if err != nil {
		return res, err
	}

	candidates, err := loadRetainedAllocations(ctx, tx, poolKey, opts.Batch)
	if err != nil {
		return res, err
	}
	res.Candidates = len(candidates)

	classes := map[string]*ipamv1alpha1.IPClass{}
	for _, row := range candidates {
		class, cerr := sweepClassFor(ctx, tx, classes, row.className)
		if cerr != nil {
			return res, cerr
		}

		lease := EffectiveLease(class, pool)
		expiresAt, ok := LeaseExpiry(row.retainedAt, lease)
		if !ok {
			// Held indefinitely. Counted rather than ignored: on a scarce range
			// this is the number that explains why capacity is not returning.
			res.NoLease++
			continue
		}

		switch {
		case !opts.Now.Before(expiresAt.Add(graceFor(opts.Grace, lease))):
			if rerr := a.releaseExpired(ctx, tx, row, poolKey); rerr != nil {
				return res, rerr
			}
			res.Released++
		case !opts.Now.Before(expiresAt):
			marked, merr := markExpiring(ctx, tx, row, expiresAt.Add(graceFor(opts.Grace, lease)))
			if merr != nil {
				return res, merr
			}
			if marked {
				res.Marked++
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit lease sweep transaction: %w", err)
	}
	committed = true
	return res, nil
}

// graceFor clamps the warning window to the lease itself.
//
// A 24-hour grace on a 30-day lease is a warning. A 24-hour grace on a one-hour
// lease would more than double the hold, which turns the warning into the
// dominant term and makes a short lease not mean what it says. Clamping keeps
// the warning strictly shorter than the thing it warns about.
func graceFor(grace time.Duration, lease *time.Duration) time.Duration {
	if lease != nil && *lease < grace {
		return *lease
	}
	return grace
}

// sweepClassFor reads a class, memoised for the duration of one pool's sweep.
// A pool's retained allocations are overwhelmingly of one or two classes, so
// this turns a per-row read into a per-class one.
//
// A class that no longer exists yields nil, which EffectiveLease reads as "no
// class lease" — so the pool's cap still applies and the allocation is not
// stranded by the class's deletion.
func sweepClassFor(ctx context.Context, tx pgx.Tx, cache map[string]*ipamv1alpha1.IPClass, name string) (*ipamv1alpha1.IPClass, error) {
	if name == "" {
		return nil, nil
	}
	if class, ok := cache[name]; ok {
		return class, nil
	}
	class, err := LoadClass(ctx, tx, name)
	if err != nil {
		if errors.Is(err, ErrClassNotFound) {
			cache[name] = nil
			return nil, nil
		}
		return nil, err
	}
	cache[name] = class
	return class, nil
}

// loadRetainedAllocations reads this pool's retained allocations oldest first.
//
// The WHERE clause mirrors migration 004's partial index exactly, including
// `retained_at IS NOT NULL`, so the scan is index-only and rows with no
// retention timestamp are genuinely absent rather than sorted to one end where
// an ORDER BY would surface them first. Those rows predate the lease feature and
// carry no lease by design; see the migration for why no backfill is correct.
func loadRetainedAllocations(ctx context.Context, tx pgx.Tx, poolKey string, limit int) ([]retainedRow, error) {
	defer metrics.ObserveQuery("load_retained_allocations", time.Now())
	rows, err := tx.Query(ctx,
		`SELECT allocation_key, class_name,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr),
		        owner_project, retained_at
		   FROM ipam_cidr_allocations
		  WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NOT NULL
		    AND pool_key = $1
		  ORDER BY retained_at
		  LIMIT $2`, poolKey, limit)
	if err != nil {
		return nil, fmt.Errorf("load retained allocations for %q: %w", poolKey, err)
	}
	defer rows.Close()

	var out []retainedRow
	for rows.Next() {
		var r retainedRow
		if err := rows.Scan(&r.allocationKey, &r.className, &r.cidr, &r.ownerProject, &r.retainedAt); err != nil {
			return nil, fmt.Errorf("scan retained allocation: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retained allocations: %w", err)
	}
	return out, nil
}

// releaseExpired frees an allocation whose lease and grace have both elapsed,
// removing the row and the object together.
//
// The DELETED changelog entry the object removal writes is the audit record. It
// is the same evidence an operator force-release leaves, which matters: an
// automatic release should not be *less* legible than a deliberate one, and it
// is the more surprising of the two.
func (a *PostgresPrefixAllocator) releaseExpired(ctx context.Context, tx pgx.Tx, row retainedRow, poolKey string) error {
	released, err := a.ForceRelease(ctx, tx, row.allocationKey)
	if err != nil {
		return fmt.Errorf("release expired allocation %q: %w", row.allocationKey, err)
	}
	if !released {
		// The row went between the read and here. Nothing to do, and nothing
		// wrong — but the object must not be deleted on the strength of a
		// release that did not happen.
		return nil
	}
	if _, err := deleteObject(ctx, tx, row.allocationKey); err != nil {
		return fmt.Errorf("delete expired allocation object %q: %w", row.allocationKey, err)
	}

	metrics.RecordLeaseExpiration(row.className, "released")
	// Logged at a level that reaches an operator without a debug flag. This is
	// the line someone reads at 3am, so it names the holder — which is what
	// keeping ownerRef on a retained allocation was for — alongside the address
	// and how long it was held.
	klog.InfoS("released expired retained allocation",
		"allocation", row.allocationKey, "cidr", row.cidr, "class", row.className,
		"ownerProject", row.ownerProject, "pool", poolKey,
		"retainedFor", time.Since(row.retainedAt).Truncate(time.Minute).String())
	return nil
}

// markExpiring publishes the warning: the allocation's object gains a phase and
// a condition naming when it will be released.
//
// Reports whether it changed anything, so a re-mark on every pass is not counted
// as new work — a sweep that reported hundreds of marks per pass would be noise
// hiding the ones that matter.
func markExpiring(ctx context.Context, tx pgx.Tx, row retainedRow, releaseAt time.Time) (bool, error) {
	var data []byte
	if err := tx.QueryRow(ctx,
		`SELECT data FROM ipam_objects WHERE key = $1`, row.allocationKey,
	).Scan(&data); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row outlived its object. Releasing still works — the row is
			// what holds the address — so this is not fatal to the sweep.
			return false, nil
		}
		return false, fmt.Errorf("read allocation object %q: %w", row.allocationKey, err)
	}

	var alloc ipamv1alpha1.IPAllocation
	if err := json.Unmarshal(data, &alloc); err != nil {
		return false, fmt.Errorf("decode allocation object %q: %w", row.allocationKey, err)
	}

	cond := metav1.Condition{
		Type:               "Expiring",
		Status:             metav1.ConditionTrue,
		Reason:             "RetentionLeaseElapsed",
		LastTransitionTime: metav1.NewTime(releaseAt.Add(-time.Since(row.retainedAt))),
		Message: fmt.Sprintf(
			"retention lease elapsed; this address will be released at %s unless the allocation is re-bound or its lease extended",
			releaseAt.UTC().Format(time.RFC3339)),
	}
	if existing := findCondition(alloc.Status.Conditions, "Expiring"); existing != nil &&
		existing.Status == cond.Status && existing.Message == cond.Message {
		return false, nil
	}
	alloc.Status.Conditions = upsertSweepCondition(alloc.Status.Conditions, cond)

	updated, err := json.Marshal(&alloc)
	if err != nil {
		return false, fmt.Errorf("encode allocation object %q: %w", row.allocationKey, err)
	}
	if _, err := updateObject(ctx, tx, row.allocationKey, updated); err != nil {
		return false, fmt.Errorf("mark allocation %q expiring: %w", row.allocationKey, err)
	}

	metrics.RecordLeaseExpiration(row.className, "marked")
	klog.V(2).InfoS("marked retained allocation expiring",
		"allocation", row.allocationKey, "cidr", row.cidr, "class", row.className,
		"releaseAt", releaseAt.UTC().Format(time.RFC3339))
	return true, nil
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// upsertSweepCondition replaces a condition of the same type or appends it.
func upsertSweepCondition(conditions []metav1.Condition, cond metav1.Condition) []metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == cond.Type {
			conditions[i] = cond
			return conditions
		}
	}
	return append(conditions, cond)
}
