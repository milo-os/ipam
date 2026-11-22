package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics/testutil"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// retain allocates a /24 for claim under Retain and deletes the claim, leaving
// the row retained with whatever lease the fixtures give it, and an
// IPAllocation object over it as the claim path writes one.
func retain(t *testing.T, db *pgxpool.Pool, poolKey, claim, allocationKey string) string {
	t.Helper()
	cidr := allocateWithPolicy(t, db, poolKey, claim, allocationKey, ipamv1alpha1.ReclaimRetain)
	writeAllocationObject(t, db, allocationKey, cidr)
	releaseRetained(t, db, claim)
	return cidr
}

func writeAllocationObject(t *testing.T, db *pgxpool.Pool, allocationKey, cidr string) {
	t.Helper()
	obj := &ipamv1alpha1.IPAllocation{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPAllocation"},
		ObjectMeta: metav1.ObjectMeta{Name: allocationKey, Namespace: "default"},
		Status:     ipamv1alpha1.IPAllocationStatus{AllocatedCIDR: cidr},
	}
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal allocation: %v", err)
	}
	inTx(t, context.Background(), db.Begin, func(tx pgx.Tx) error {
		_, err := insertObject(context.Background(), tx, allocationKey, "IPAllocation", "default", allocationKey, data)
		return err
	})
}

// makeDue moves a retained row's deadline into the past, as time would.
func makeDue(t *testing.T, db *pgxpool.Pool, allocationKey string) {
	t.Helper()
	tag, err := db.Exec(context.Background(),
		`UPDATE ipam_cidr_allocations SET expires_at = now() - interval '1 minute'
		  WHERE allocation_key = $1 AND claim_key IS NULL`, allocationKey)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("make %s due: %v (%d rows)", allocationKey, err, tag.RowsAffected())
	}
}

func sweep(t *testing.T, db *pgxpool.Pool, mode SweepMode) SweepResult {
	t.Helper()
	res, err := NewSweeper(db, mode).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep (%s): %v", mode, err)
	}
	if res.Errors != 0 {
		t.Fatalf("sweep (%s) carried on past %d error(s)", mode, res.Errors)
	}
	return res
}

func rowExists(t *testing.T, db *pgxpool.Pool, allocationKey string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocationKey).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n == 1
}

func objectExists(t *testing.T, db *pgxpool.Pool, key string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_objects WHERE key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("count objects: %v", err)
	}
	return n == 1
}

func releasesCounted(t *testing.T, mode SweepMode) float64 {
	t.Helper()
	v, err := testutil.GetCounterMetricValue(metrics.RetentionReleases.WithLabelValues(string(mode)))
	if err != nil {
		t.Fatalf("read ipam_retention_releases_total{mode=%s}: %v", mode, err)
	}
	return v
}

// An enforced sweep frees a due allocation the way an explicit release does:
// the row and the IPAllocation go, with a DELETED event, capacity comes back,
// and the next claim is handed the same address.
func TestEnforceReleasesADueAllocationAndReissuesItsAddress(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	const cidr = "10.70.0.0/16"
	poolKey := seedPool(t, db, "sweep-enforce", cidr)
	held := retain(t, db, poolKey, "claim-a", "alloc-a")
	makeDue(t, db, "alloc-a")
	before := releasesCounted(t, SweepEnforce)

	res := sweep(t, db, SweepEnforce)
	if !res.Ran || res.Due != 1 || res.Released != 1 {
		t.Fatalf("sweep = %+v, want one due row released", res)
	}
	if rowExists(t, db, "alloc-a") || objectExists(t, db, "alloc-a") {
		t.Error("the due row or its IPAllocation survived an enforced sweep")
	}
	var deleted int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_changelog WHERE key = 'alloc-a' AND event_type = 'DELETED'`).Scan(&deleted); err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	if deleted != 1 {
		t.Errorf("%d DELETED changelog rows for the released IPAllocation, want 1", deleted)
	}
	if got, want := storedConsumption(t, db, poolKey), measureNow(t, db, poolKey, cidr); got != want || got != "0" {
		t.Errorf("consumption = %s after the release (fresh measurement %s), want 0", got, want)
	}
	if got := releasesCounted(t, SweepEnforce) - before; got != 1 {
		t.Errorf("ipam_retention_releases_total{mode=enforce} moved by %v, want 1", got)
	}
	if got := allocateWithPolicy(t, db, poolKey, "claim-b", "alloc-b", ipamv1alpha1.ReclaimDelete); got != held {
		t.Errorf("next claim got %q, want the released %q", got, held)
	}
}

// Observe mode releases nothing, and counts each deadline once as it passes,
// however many passes see it due.
func TestObserveNeverReleasesAndCountsADeadlineOnce(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "sweep-observe", "10.71.0.0/16")
	retain(t, db, poolKey, "claim-a", "alloc-a")
	makeDue(t, db, "alloc-a")
	before := releasesCounted(t, SweepObserve)

	first := sweep(t, db, SweepObserve)
	second := sweep(t, db, SweepObserve)
	if first.WouldRelease != 1 || second.WouldRelease != 0 || first.Released+second.Released != 0 {
		t.Errorf("passes = %+v then %+v, want one would-release in the first and none after", first, second)
	}
	if !rowExists(t, db, "alloc-a") || !objectExists(t, db, "alloc-a") {
		t.Fatal("observe mode released a due allocation")
	}

	// A second deadline passing is counted once more, and only once.
	retain(t, db, poolKey, "claim-b", "alloc-b")
	makeDue(t, db, "alloc-b")
	if got := sweep(t, db, SweepObserve).WouldRelease; got != 1 {
		t.Errorf("the pass after a second deadline counted %d, want 1", got)
	}
	if got := releasesCounted(t, SweepObserve) - before; got != 2 {
		t.Errorf("ipam_retention_releases_total{mode=observe} moved by %v, want 2", got)
	}
	if !rowExists(t, db, "alloc-b") {
		t.Error("observe mode released the second due allocation")
	}
}

// Only a retained claim row with a timestamped retention can expire. A
// reservation or a pool carve has no claim either, and releasing one would hand
// out a gateway or the block a child pool allocates from; a row retained before
// retention was timestamped has no moment a lease could be measured from.
func TestOnlyTimestampedRetainedClaimRowsAreSwept(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "sweep-purpose", "10.72.0.0/16")
	for _, r := range []struct{ key, cidr, purpose, retainedAt string }{
		{"reservation", "10.72.0.0/32", "Reservation", "now() - interval '2 days'"},
		{"carve", "10.72.1.0/24", "PoolCarve", "now() - interval '2 days'"},
		{"untimestamped", "10.72.2.0/24", "Claim", "NULL"},
	} {
		if _, err := db.Exec(ctx, fmt.Sprintf(
			`INSERT INTO ipam_cidr_allocations
			    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, retained_at, expires_at)
			 VALUES ($1, $2, NULL, $3, 'IPv4', $4, %s, now() - interval '1 day')`, r.retainedAt),
			poolKey, r.cidr, r.key, r.purpose); err != nil {
			t.Fatalf("seed %s row: %v", r.key, err)
		}
	}

	if res := sweep(t, db, SweepEnforce); res.Due != 0 || res.Released != 0 {
		t.Errorf("sweep = %+v, want nothing due", res)
	}
	for _, key := range []string{"reservation", "carve", "untimestamped"} {
		if !rowExists(t, db, key) {
			t.Errorf("the %s row was swept", key)
		}
	}
}

// The due set is one scan ordered by deadline across every pool, so a pool
// full of rows that never expire cannot hold back a due row elsewhere: the
// per-pool, retained_at-ordered scan it replaced could be stuck on them.
func TestRowsWithNoLeaseCannotStarveADueRow(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	crowded := seedPool(t, db, "a-crowded", "10.73.0.0/16")
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, retained_at, expires_at)
		 SELECT $1, ('10.73.' || (g / 256) || '.' || (g % 256) || '/32')::cidr, NULL,
		        'held-' || g, 'IPv4', 'Claim', now() - interval '30 days', 'infinity'
		   FROM generate_series(0, 499) g`, crowded); err != nil {
		t.Fatalf("seed rows with no lease: %v", err)
	}
	poolKey := seedPool(t, db, "b-due", "10.74.0.0/16")
	retain(t, db, poolKey, "claim-a", "alloc-a")
	makeDue(t, db, "alloc-a")

	res := sweep(t, db, SweepEnforce)
	if res.Released != 1 || rowExists(t, db, "alloc-a") {
		t.Errorf("sweep = %+v; the due row behind 500 rows with no lease was not released in one pass", res)
	}
	var left int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1`, crowded).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 500 {
		t.Errorf("%d rows with no lease left, want all 500", left)
	}
}

// A row whose lease could not be evaluated at retention is evaluated by the
// next pass, against the class and pool as they are then, and its IPAllocation
// is told.
func TestTheSweeperEvaluatesALeaseRetentionCouldNot(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "sweep-evaluate", "10.75.0.0/16")
	classKey := classStorageKey(testProject, "standard")
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1, 'IPClass', 'standard', '{"spec": "not a spec"}')`,
		classKey); err != nil {
		t.Fatalf("seed unreadable class: %v", err)
	}
	retain(t, db, poolKey, "claim-a", "alloc-a")
	if retainedRetention(t, db, "alloc-a").Lease != LeaseUnevaluated {
		t.Fatal("setup: the lease was evaluated despite the unreadable class")
	}
	if _, err := db.Exec(ctx, `DELETE FROM ipam_objects WHERE key = $1`, classKey); err != nil {
		t.Fatalf("drop class: %v", err)
	}
	seedLeaseClass(t, db, dur(3*time.Hour))

	if res := sweep(t, db, SweepObserve); res.Evaluated != 1 {
		t.Fatalf("sweep = %+v, want one row evaluated", res)
	}
	stored := retainedRetention(t, db, "alloc-a")
	if stored.Lease != LeaseFinite || leaseOf(stored) != 3*time.Hour {
		t.Errorf("stored retention = %+v, want a 3h lease from retention", stored)
	}
	a := readAllocationObject(t, db, "alloc-a")
	if a.Status.ExpiresAt == nil || !a.Status.ExpiresAt.Time.Equal(stored.ExpiresAt) {
		t.Errorf("IPAllocation expiresAt = %v, want %s", a.Status.ExpiresAt, stored.ExpiresAt)
	}
	if c := meta.FindStatusCondition(a.Status.Conditions, ipamv1alpha1.AllocationConditionRetained); c == nil ||
		c.Reason != ipamv1alpha1.RetainedReasonLeaseActive {
		t.Errorf("Retained condition = %+v, want LeaseActive", c)
	}
}

func readAllocationObject(t *testing.T, db *pgxpool.Pool, key string) ipamv1alpha1.IPAllocation {
	t.Helper()
	var data []byte
	if err := db.QueryRow(context.Background(), `SELECT data FROM ipam_objects WHERE key = $1`, key).Scan(&data); err != nil {
		t.Fatalf("read IPAllocation %s: %v", key, err)
	}
	var a ipamv1alpha1.IPAllocation
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatalf("decode IPAllocation %s: %v", key, err)
	}
	return a
}

// The gauges read the whole retained set, by family and lease state, counting
// a row retained before retention was timestamped as having no lease.
func TestTheRetainedSetGauges(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "sweep-gauges", "10.77.0.0/16")
	retain(t, db, poolKey, "claim-finite", "finite")
	setDeadline(t, db, "finite", ptr(time.Hour))
	retain(t, db, poolKey, "claim-none", "none")
	retain(t, db, poolKey, "claim-uneval", "uneval")
	if _, err := db.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = NULL WHERE allocation_key = 'uneval'`); err != nil {
		t.Fatalf("unevaluate: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose)
		 VALUES ($1, '10.77.200.0/24', NULL, 'legacy', 'IPv4', 'Claim')`, poolKey); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	allocateWithPolicy(t, db, poolKey, "claim-bound", "bound", ipamv1alpha1.ReclaimRetain)

	if err := publishRetainedGauges(ctx, db); err != nil {
		t.Fatalf("publish: %v", err)
	}
	for lease, want := range map[string]float64{"finite": 1, "none": 2, "unevaluated": 1} {
		got, err := testutil.GetGaugeMetricValue(metrics.RetainedAllocations.WithLabelValues("IPv4", lease))
		if err != nil {
			t.Fatalf("read gauge: %v", err)
		}
		if got != want {
			t.Errorf("ipam_retained_allocations{ip_family=IPv4,lease=%s} = %v, want %v", lease, got, want)
		}
	}
	if got, _ := testutil.GetGaugeMetricValue(metrics.RetainedAllocations.WithLabelValues("IPv6", "none")); got != 0 {
		t.Errorf("IPv6 none = %v, want 0", got)
	}
	if age, _ := testutil.GetGaugeMetricValue(metrics.RetainedOldestAge.WithLabelValues("IPv4")); age < 0 || age > 600 {
		t.Errorf("oldest retained age = %vs, want the few seconds since the fixtures retained", age)
	}
}

// A pool a live claim holds is skipped, not waited on, and released on the next
// pass once the claim is done with it.
func TestASweepYieldsToAClaimHoldingThePool(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "sweep-yield", "10.78.0.0/16")
	retain(t, db, poolKey, "claim-a", "alloc-a")
	makeDue(t, db, "alloc-a")

	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := lockAndDecodeIPPool(ctx, holder, poolKey); err != nil {
		t.Fatalf("hold the pool: %v", err)
	}
	if res := sweep(t, db, SweepEnforce); res.PoolsSkipped != 1 || res.Released != 0 {
		t.Errorf("sweep while the pool is held = %+v, want it skipped", res)
	}
	_ = holder.Rollback(ctx)
	if res := sweep(t, db, SweepEnforce); res.Released != 1 {
		t.Errorf("sweep after the pool is free = %+v, want the row released", res)
	}
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// holdForSweep opens the transaction an enforced sweep releases one pool's due
// rows in, up to the point the rows are deleted, so a test can queue a rival
// behind its lock.
func holdForSweep(t *testing.T, db *pgxpool.Pool, poolKey string) (pgx.Tx, *ipamv1alpha1.IPPool) {
	t.Helper()
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	pool, err := lockPoolNoWait(context.Background(), tx, poolKey)
	if err != nil || pool == nil {
		t.Fatalf("lock pool for the sweep: %v (pool %v)", err, pool)
	}
	return tx, pool
}

func dueCandidates(t *testing.T, db *pgxpool.Pool) []candidate {
	t.Helper()
	due, err := NewSweeper(db, SweepEnforce).findDue(context.Background(), ` ORDER BY expires_at`)
	if err != nil {
		t.Fatalf("find due: %v", err)
	}
	return due
}

// A reclaim and a sweep of one retained row serialise on the pool, whichever
// gets there first, and exactly one of them has it: the sweep's delete is
// predicated on the row still being retained and due, and the reclaim's bind
// on the row still existing unbound.
func TestASweepAndAReclaimOfOneRowHaveOneWinner(t *testing.T) {
	t.Run("reclaim first", func(t *testing.T) {
		db := testdb.Pool(t, testdb.MaxConns(8))
		ctx := context.Background()
		poolKey := seedPool(t, db, "race-reclaim-1", "10.79.0.0/16")
		retain(t, db, poolKey, "claim-a", "alloc-a")
		makeDue(t, db, "alloc-a")

		a := NewPostgresPrefixAllocator()
		reclaim, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if row, err := a.LockClaimAllocation(ctx, reclaim, "alloc-a"); err != nil || row == nil {
			t.Fatalf("lock for reclaim: %v (row %v)", err, row)
		}
		if res := sweep(t, db, SweepEnforce); res.Released != 0 || res.PoolsSkipped != 1 {
			t.Errorf("sweep during the reclaim = %+v, want the pool skipped", res)
		}
		if err := a.ReclaimAllocation(ctx, reclaim, "alloc-a", "claim-a2", ipamv1alpha1.ReclaimRetain); err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if err := reclaim.Commit(ctx); err != nil {
			t.Fatalf("commit reclaim: %v", err)
		}
		if res := sweep(t, db, SweepEnforce); res.Due != 0 || !rowExists(t, db, "alloc-a") {
			t.Errorf("sweep after the reclaim = %+v; the reclaimed row must not be due", res)
		}
	})

	t.Run("sweep first", func(t *testing.T) {
		db := testdb.Pool(t, testdb.MaxConns(8))
		ctx := context.Background()
		poolKey := seedPool(t, db, "race-reclaim-2", "10.80.0.0/16")
		retain(t, db, poolKey, "claim-a", "alloc-a")
		makeDue(t, db, "alloc-a")
		due := dueCandidates(t, db)

		sweepTx, pool := holdForSweep(t, db, poolKey)
		type outcome struct {
			row *ClaimAllocation
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			tx, err := db.Begin(ctx)
			if err != nil {
				done <- outcome{err: err}
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			row, err := NewPostgresPrefixAllocator().LockClaimAllocation(ctx, tx, "alloc-a")
			done <- outcome{row, err}
		}()
		testdb.WaitForLockWaiters(t, db, 1)

		released, err := releaseDue(ctx, sweepTx, poolKey, pool, due)
		if err != nil || len(released) != 1 {
			t.Fatalf("sweep release = %v, %v; want one row", released, err)
		}
		if err := sweepTx.Commit(ctx); err != nil {
			t.Fatalf("commit sweep: %v", err)
		}
		o := <-done
		if isDeadlock(o.err) {
			t.Fatalf("the reclaim deadlocked: %v", o.err)
		}
		if o.err != nil || o.row != nil {
			t.Errorf("the reclaim queued behind the sweep found %+v, %v; want nothing to reclaim", o.row, o.err)
		}
	})
}

// A sweep and an explicit release of one retained row serialise on the pool
// the same way, and the row is freed once.
func TestASweepAndAnExplicitReleaseOfOneRowHaveOneWinner(t *testing.T) {
	t.Run("release first", func(t *testing.T) {
		db := testdb.Pool(t, testdb.MaxConns(8))
		ctx := context.Background()
		const cidr = "10.81.0.0/16"
		poolKey := seedPool(t, db, "race-release-1", cidr)
		retain(t, db, poolKey, "claim-a", "alloc-a")
		makeDue(t, db, "alloc-a")

		release, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := NewPostgresPrefixAllocator().ReleaseRetainedAllocation(ctx, release, "alloc-a"); err != nil {
			t.Fatalf("release: %v", err)
		}
		if res := sweep(t, db, SweepEnforce); res.Released != 0 || res.PoolsSkipped != 1 {
			t.Errorf("sweep during the release = %+v, want the pool skipped", res)
		}
		if err := release.Commit(ctx); err != nil {
			t.Fatalf("commit release: %v", err)
		}
		if res := sweep(t, db, SweepEnforce); res.Due != 0 {
			t.Errorf("sweep after the release = %+v, want nothing due", res)
		}
		if got := storedConsumption(t, db, poolKey); got != measureNow(t, db, poolKey, cidr) {
			t.Errorf("consumption %s disagrees with a fresh measurement", got)
		}
	})

	t.Run("sweep first", func(t *testing.T) {
		db := testdb.Pool(t, testdb.MaxConns(8))
		ctx := context.Background()
		const cidr = "10.82.0.0/16"
		poolKey := seedPool(t, db, "race-release-2", cidr)
		retain(t, db, poolKey, "claim-a", "alloc-a")
		makeDue(t, db, "alloc-a")
		due := dueCandidates(t, db)

		sweepTx, pool := holdForSweep(t, db, poolKey)
		done := make(chan error, 1)
		go func() {
			tx, err := db.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			if err := NewPostgresPrefixAllocator().ReleaseRetainedAllocation(ctx, tx, "alloc-a"); err != nil {
				_ = tx.Rollback(ctx)
				done <- err
				return
			}
			done <- tx.Commit(ctx)
		}()
		testdb.WaitForLockWaiters(t, db, 1)

		if released, err := releaseDue(ctx, sweepTx, poolKey, pool, due); err != nil || len(released) != 1 {
			t.Fatalf("sweep release = %v, %v; want one row", released, err)
		}
		if err := sweepTx.Commit(ctx); err != nil {
			t.Fatalf("commit sweep: %v", err)
		}
		if err := <-done; err != nil {
			if isDeadlock(err) {
				t.Fatalf("the release deadlocked: %v", err)
			}
			t.Errorf("the release queued behind the sweep returned %v, want nil (nothing left to free)", err)
		}
		if got := storedConsumption(t, db, poolKey); got != "0" || got != measureNow(t, db, poolKey, cidr) {
			t.Errorf("consumption = %s after one release, want 0 and a fresh measurement to agree", got)
		}
	})
}

// One pass at a time: a replica that finds the advisory lock held skips the
// pass, and two replicas sweeping at once release a due row once.
func TestTwoSweepersReleaseADueRowOnce(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(8))
	ctx := context.Background()
	poolKey := seedPool(t, db, "race-sweepers", "10.83.0.0/16")
	retain(t, db, poolKey, "claim-a", "alloc-a")
	makeDue(t, db, "alloc-a")

	other, err := db.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_lock($1)`, sweepLockKey); err != nil {
		t.Fatalf("hold the sweep lock: %v", err)
	}
	if res := sweep(t, db, SweepEnforce); res.Ran || res.Released != 0 {
		t.Errorf("sweep while another replica holds the lock = %+v, want it skipped", res)
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock($1)`, sweepLockKey); err != nil {
		t.Fatalf("release the sweep lock: %v", err)
	}
	other.Release()

	results := make(chan SweepResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			res, err := NewSweeper(db, SweepEnforce).Sweep(ctx)
			results <- res
			errs <- err
		}()
	}
	released := 0
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent sweep: %v", err)
		}
		res := <-results
		if res.Errors != 0 {
			t.Errorf("concurrent sweep carried on past %d error(s)", res.Errors)
		}
		released += res.Released
	}
	if released != 1 || rowExists(t, db, "alloc-a") {
		t.Errorf("two concurrent sweepers released %d, want exactly 1", released)
	}
}
