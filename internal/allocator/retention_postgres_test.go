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
	k8smetrics "k8s.io/component-base/metrics"
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

// The overdue gauges count rows past a finite deadline by the database clock,
// and age the most overdue by its deadline, not by when it was retained. A
// deadline still ahead, 'infinity', an unevaluated lease, and a row retained
// before retention was timestamped are not overdue; a family with nothing due
// reads zero.
func TestTheOverdueRetainedGauges(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "sweep-overdue", "10.78.0.0/16")
	for _, key := range []string{"due-long", "due-recent", "ahead", "forever", "uneval"} {
		retain(t, db, poolKey, "claim-"+key, key)
	}
	for key, expiresAt := range map[string]string{
		"due-long":   `now() - interval '2 hours'`,
		"due-recent": `now() - interval '1 minute'`,
		"ahead":      `now() + interval '1 hour'`,
		"forever":    `'infinity'`,
		"uneval":     `NULL`,
	} {
		if _, err := db.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = `+expiresAt+` WHERE allocation_key = $1`, key); err != nil {
			t.Fatalf("set deadline of %s: %v", key, err)
		}
	}
	// Retained before retention was timestamped, with a past deadline: findDue
	// never releases it, so it is not overdue either.
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, expires_at)
		 VALUES ($1, '10.78.200.0/24', NULL, 'legacy', 'IPv4', 'Claim', now() - interval '3 hours')`, poolKey); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if err := publishRetainedGauges(ctx, db); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got, _ := testutil.GetGaugeMetricValue(metrics.RetainedOverdue.WithLabelValues("IPv4")); got != 2 {
		t.Errorf("ipam_retained_overdue_allocations{ip_family=IPv4} = %v, want 2", got)
	}
	// Two hours past its deadline, give or take the test's own runtime.
	if got, _ := testutil.GetGaugeMetricValue(metrics.RetainedOldestOverdue.WithLabelValues("IPv4")); got < 7200 || got > 7200+600 {
		t.Errorf("ipam_retained_oldest_overdue_seconds{ip_family=IPv4} = %vs, want about 7200s", got)
	}
	for name, gauge := range map[string]*k8smetrics.GaugeVec{
		"ipam_retained_overdue_allocations":    metrics.RetainedOverdue,
		"ipam_retained_oldest_overdue_seconds": metrics.RetainedOldestOverdue,
	} {
		if got, err := testutil.GetGaugeMetricValue(gauge.WithLabelValues("IPv6")); err != nil || got != 0 {
			t.Errorf("%s{ip_family=IPv6} = %v (%v), want 0", name, got, err)
		}
	}

	// An enforcing pass releases both due rows, and the gauges it publishes
	// on the next pass read zero.
	if res := sweep(t, db, SweepEnforce); res.Released != 2 {
		t.Fatalf("enforce released %d, want 2", res.Released)
	}
	if err := publishRetainedGauges(ctx, db); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got, _ := testutil.GetGaugeMetricValue(metrics.RetainedOverdue.WithLabelValues("IPv4")); got != 0 {
		t.Errorf("overdue after enforce = %v, want 0", got)
	}
	if got, _ := testutil.GetGaugeMetricValue(metrics.RetainedOldestOverdue.WithLabelValues("IPv4")); got != 0 {
		t.Errorf("oldest overdue after enforce = %vs, want 0", got)
	}
}

// Run publishes the mode it was configured with, so alerts that expect
// releases can tell enforce from observe.
func TestTheSweeperPublishesItsMode(t *testing.T) {
	db := testdb.Pool(t)
	for mode, want := range map[SweepMode]float64{SweepEnforce: 1, SweepObserve: 0} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		NewSweeper(db, mode).Run(ctx, time.Minute)
		if got, _ := testutil.GetGaugeMetricValue(metrics.RetentionSweepEnforcing); got != want {
			t.Errorf("ipam_retention_sweep_enforcing in %s mode = %v, want %v", mode, got, want)
		}
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
	due, err := NewSweeper(db, SweepEnforce).findDue(context.Background(), keyset{}, 1000)
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

		released, _, err := releaseDue(ctx, sweepTx, poolKey, pool, due)
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
		if err := NewPostgresPrefixAllocator().ReleaseRetainedAllocation(ctx, release, "alloc-a", nil); err != nil {
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
			if err := NewPostgresPrefixAllocator().ReleaseRetainedAllocation(ctx, tx, "alloc-a", nil); err != nil {
				_ = tx.Rollback(ctx)
				done <- err
				return
			}
			done <- tx.Commit(ctx)
		}()
		testdb.WaitForLockWaiters(t, db, 1)

		if released, _, err := releaseDue(ctx, sweepTx, poolKey, pool, due); err != nil || len(released) != 1 {
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

// holdRow locks one allocation row in its own transaction, so a sweep that
// reaches the row waits there, mid-pass, still holding the sweep lock.
func holdRow(t *testing.T, db *pgxpool.Pool, allocationKey string) pgx.Tx {
	t.Helper()
	h, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = h.Rollback(context.Background()) })
	if _, err := h.Exec(context.Background(),
		`SELECT 1 FROM ipam_cidr_allocations WHERE allocation_key = $1 FOR UPDATE`, allocationKey); err != nil {
		t.Fatalf("hold %s: %v", allocationKey, err)
	}
	return h
}

func sweepLockHolders(t *testing.T, db *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory' AND granted
		    AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & x'ffffffff'::bigint)::oid`,
		sweepLockKey).Scan(&n); err != nil {
		t.Fatalf("count sweep lock holders: %v", err)
	}
	return n
}

// While one replica is mid-pass, another skips its pass rather than sweeping
// alongside it, and once the first finishes the lock is free again.
func TestASecondSweeperSkipsWhileTheFirstIsMidPass(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(10))
	ctx := context.Background()
	poolKey := seedPool(t, db, "race-sweepers", "10.83.0.0/16")
	seedLeaseClass(t, db, dur(time.Hour))
	retain(t, db, poolKey, "claim-a", "alloc-a")
	if _, err := db.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = NULL WHERE allocation_key = 'alloc-a'`); err != nil {
		t.Fatalf("unevaluate: %v", err)
	}
	h := holdRow(t, db, "alloc-a")

	type outcome struct {
		res SweepResult
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		res, err := NewSweeper(db, SweepObserve).Sweep(ctx)
		first <- outcome{res, err}
	}()
	testdb.WaitForLockWaiters(t, db, 1)

	second, err := NewSweeper(db, SweepObserve).Sweep(ctx)
	if err != nil || second.Ran {
		t.Errorf("the second sweeper ran a pass (%+v, %v) while the first held the lock mid-pass", second, err)
	}
	_ = h.Rollback(ctx)
	a := <-first
	if a.err != nil || !a.res.Ran || a.res.Evaluated != 1 {
		t.Errorf("the first sweeper's pass = %+v, %v; want it to finish and evaluate the row", a.res, a.err)
	}
	if n := sweepLockHolders(t, db); n != 0 {
		t.Errorf("%d session(s) hold the sweep lock after the pass, want 0", n)
	}
	if res := sweep(t, db, SweepObserve); !res.Ran {
		t.Error("a pass after the first finished did not get the lock")
	}
}

// Two replicas sweeping one due row at once release it once, however the two
// interleave.
func TestConcurrentSweepersReleaseADueRowOnce(t *testing.T) {
	for i := range 10 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			db := testdb.Pool(t, testdb.MaxConns(8))
			ctx := context.Background()
			poolKey := seedPool(t, db, "race-sweepers", "10.83.0.0/16")
			retain(t, db, poolKey, "claim-a", "alloc-a")
			makeDue(t, db, "alloc-a")

			type outcome struct {
				res SweepResult
				err error
			}
			out := make(chan outcome, 2)
			for range 2 {
				go func() {
					res, err := NewSweeper(db, SweepEnforce).Sweep(ctx)
					out <- outcome{res, err}
				}()
			}
			released := 0
			for range 2 {
				o := <-out
				if o.err != nil || o.res.Errors != 0 {
					t.Errorf("concurrent sweep: %+v, %v", o.res, o.err)
				}
				released += o.res.Released
			}
			if released != 1 || rowExists(t, db, "alloc-a") {
				t.Errorf("two concurrent sweepers released %d, want exactly 1", released)
			}
		})
	}
}

// A pass whose lock session dies stops before its next pool transaction,
// rather than carrying on as though it still held the lock. The pool it was in
// finishes under that pool's lock, which is what keeps it safe.
func TestAPassStopsWhenItsLockSessionDies(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(10))
	ctx := context.Background()
	seedLeaseClass(t, db, dur(time.Hour))
	first := seedPool(t, db, "a-lock-first", "10.84.0.0/16")
	second := seedPool(t, db, "b-lock-second", "10.85.0.0/16")
	retain(t, db, first, "claim-a", "alloc-a")
	retain(t, db, second, "claim-b", "alloc-b")
	if _, err := db.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = NULL`); err != nil {
		t.Fatalf("unevaluate: %v", err)
	}
	h := holdRow(t, db, "alloc-a")

	type outcome struct {
		res SweepResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := NewSweeper(db, SweepObserve).Sweep(ctx)
		done <- outcome{res, err}
	}()
	testdb.WaitForLockWaiters(t, db, 1)
	if _, err := db.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_locks
		  WHERE locktype = 'advisory' AND granted
		    AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & x'ffffffff'::bigint)::oid`,
		sweepLockKey); err != nil {
		t.Fatalf("terminate the lock session: %v", err)
	}
	_ = h.Rollback(ctx)
	o := <-done
	if o.err == nil {
		t.Fatalf("a pass that lost its lock session carried on: %+v", o.res)
	}
	if retainedRetention(t, db, "alloc-b").Lease != LeaseUnevaluated {
		t.Error("the pass went on to the next pool after losing its lock")
	}
}

// Evaluation pages past rows whose class cannot be read, so they cannot keep a
// row behind them from ever being evaluated, and a pass that carried on past
// them still counts as having run.
func TestUnreadableClassesDoNotStarveEvaluation(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "starve-eval", "10.90.0.0/16")
	seedLeaseClass(t, db, dur(time.Hour))
	retain(t, db, poolKey, "claim-good", "alloc-good")
	if _, err := db.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = NULL WHERE allocation_key = 'alloc-good'`); err != nil {
		t.Fatalf("unevaluate: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1, 'IPClass', 'broken', '{"spec": "x"}')`,
		classStorageKey(testProject, "broken")); err != nil {
		t.Fatalf("seed unreadable class: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, class_name, class_project, retained_at, expires_at)
		 SELECT $1, ('10.90.' || (100 + g / 256) || '.' || (g % 256) || '/32')::cidr, NULL,
		        'bad-' || g, 'IPv4', 'Claim', 'broken', $2, now() - interval '30 days', NULL
		   FROM generate_series(0, 499) g`, poolKey, testProject); err != nil {
		t.Fatalf("seed rows of the unreadable class: %v", err)
	}
	before, _ := testutil.GetCounterMetricValue(metrics.RetentionLeaseUnevaluable)
	metrics.RetentionSweepLastSuccess.Set(0)

	res := sweep(t, db, SweepEnforce)
	if res.Evaluated != 1 || retainedRetention(t, db, "alloc-good").Lease != LeaseFinite {
		t.Errorf("pass = %+v; the readable row behind 500 unreadable ones was not evaluated", res)
	}
	if res.Unevaluable != 500 {
		t.Errorf("unevaluable = %d, want 500", res.Unevaluable)
	}
	if got, _ := testutil.GetCounterMetricValue(metrics.RetentionLeaseUnevaluable); got-before != 500 {
		t.Errorf("ipam_retention_lease_unevaluable_total moved by %v, want 500", got-before)
	}
	if v, _ := testutil.GetGaugeMetricValue(metrics.RetentionSweepLastSuccess); v == 0 {
		t.Error("a pass that completed past unevaluable rows did not advance the last-success timestamp")
	}
}

// Due rows in a pool a claim holds, or whose pool is gone, are paged past, so
// they cannot keep a due row in another pool from being released.
func TestDueRowsInAnUnavailablePoolDoNotStarveOthers(t *testing.T) {
	for _, tc := range []struct{ name, why string }{{"held", "held by a claim"}, {"gone", "pool object gone"}} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Pool(t)
			ctx := context.Background()
			hot := seedPool(t, db, "a-hot", "10.91.0.0/16")
			if _, err := db.Exec(ctx,
				`INSERT INTO ipam_cidr_allocations
				    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, retained_at, expires_at)
				 SELECT $1, ('10.91.' || (g / 256) || '.' || (g % 256) || '/32')::cidr, NULL,
				        'hot-' || g, 'IPv4', 'Claim', now() - interval '30 days', now() - interval '20 days'
				   FROM generate_series(0, 249) g`, hot); err != nil {
				t.Fatalf("seed due rows: %v", err)
			}
			other := seedPool(t, db, "b-other", "10.92.0.0/16")
			retain(t, db, other, "claim-a", "alloc-a")
			makeDue(t, db, "alloc-a")

			if tc.name == "held" {
				holder, err := db.Begin(ctx)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				defer func() { _ = holder.Rollback(ctx) }()
				if _, err := lockAndDecodeIPPool(ctx, holder, hot); err != nil {
					t.Fatalf("hold the pool: %v", err)
				}
			} else {
				// The foreign key keeps a pool with rows from being deleted, so
				// drop it for the length of the test to make the rows orphans.
				if _, err := db.Exec(ctx, `ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_allocations_pool_key_fkey`); err != nil {
					t.Fatalf("drop the foreign key: %v", err)
				}
				if _, err := db.Exec(ctx, `DELETE FROM ipam_objects WHERE key = $1`, hot); err != nil {
					t.Fatalf("delete the pool: %v", err)
				}
			}
			res := sweep(t, db, SweepEnforce)
			if res.Released != 1 || rowExists(t, db, "alloc-a") {
				t.Errorf("pass = %+v; a due row was not released behind 250 due rows in a pool %s", res, tc.why)
			}
		})
	}
}

// One row whose release fails, here because its IPAllocation cannot be
// deleted, is undone alone: it stays held and is counted, and the rest of its
// pool is released.
func TestARowThatCannotBeReleasedDoesNotHoldUpItsPool(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	const cidr = "10.86.0.0/16"
	poolKey := seedPool(t, db, "bad-row", cidr)
	retain(t, db, poolKey, "claim-a", "alloc-a")
	retain(t, db, poolKey, "claim-b", "alloc-b")
	makeDue(t, db, "alloc-a")
	makeDue(t, db, "alloc-b")
	if _, err := db.Exec(ctx, `
		CREATE FUNCTION refuse_delete() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'refused'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER refuse_alloc_a BEFORE DELETE ON ipam_objects
		    FOR EACH ROW WHEN (OLD.key = 'alloc-a') EXECUTE FUNCTION refuse_delete();`); err != nil {
		t.Fatalf("make alloc-a's object undeletable: %v", err)
	}

	res, err := NewSweeper(db, SweepEnforce).Sweep(ctx)
	if err != nil || res.Errors != 0 {
		t.Fatalf("pass = %+v, %v; want it to complete", res, err)
	}
	if res.Released != 1 || res.RowErrors != 1 {
		t.Errorf("pass = %+v; want one released and one row error", res)
	}
	if !rowExists(t, db, "alloc-a") || !objectExists(t, db, "alloc-a") {
		t.Error("the row whose release failed was half released")
	}
	if rowExists(t, db, "alloc-b") {
		t.Error("the healthy row in the same pool was not released")
	}
	if got, want := storedConsumption(t, db, poolKey), measureNow(t, db, poolKey, cidr); got != want {
		t.Errorf("consumption = %s after a partly failed release, fresh measurement %s", got, want)
	}
}

// Releasing a pool's due rows republishes its capacity once per chunk, not once
// per row: 200 releases are a handful of pool writes, not 200.
func TestReleasingAPoolsDueRowsRepublishesItsCapacityPerChunk(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	const cidr = "10.101.0.0/16"
	poolKey := seedPool(t, db, "hold", cidr)
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, retained_at, expires_at)
		 SELECT $1, ('10.101.' || g || '.0/24')::cidr, NULL, 'a-' || g, 'IPv4', 'Claim',
		        now() - interval '2 days', now() - interval '1 day'
		   FROM generate_series(0, 199) g`, poolKey); err != nil {
		t.Fatalf("seed due rows: %v", err)
	}
	var before int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ipam_changelog WHERE key = $1`, poolKey).Scan(&before); err != nil {
		t.Fatalf("count changelog: %v", err)
	}
	res := sweep(t, db, SweepEnforce)
	var after int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ipam_changelog WHERE key = $1`, poolKey).Scan(&after); err != nil {
		t.Fatalf("count changelog: %v", err)
	}
	if res.Released != 200 {
		t.Fatalf("released %d, want 200", res.Released)
	}
	if writes, want := after-before, 200/defaultChunk; writes != want {
		t.Errorf("the pool was rewritten %d times for 200 releases, want %d (once per chunk)", writes, want)
	}
	if got := storedConsumption(t, db, poolKey); got != "0" || got != measureNow(t, db, poolKey, cidr) {
		t.Errorf("consumption = %s after releasing everything, want 0", got)
	}
}

// A retention that commits after an observe pass has counted past its
// deadline is still counted, once: the ledger records deadlines, not a point
// in the due set.
func TestObserveCountsADeadlineThatCommitsLate(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(8))
	ctx := context.Background()
	poolKey := seedPool(t, db, "obs-late", "10.95.0.0/16")
	setPoolCap(t, db, poolKey, dur(time.Nanosecond))
	allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)
	allocateWithPolicy(t, db, poolKey, "claim-x", "alloc-x", ipamv1alpha1.ReclaimRetain)

	// claim-a's retention is timestamped now (NOW() is its transaction's start)
	// and commits only after a pass has counted a later deadline.
	late, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := late.Exec(ctx, `SELECT now()`); err != nil {
		t.Fatalf("start the late retention: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	releaseRetained(t, db, "claim-x")
	time.Sleep(1100 * time.Millisecond)
	before := releasesCounted(t, SweepObserve)
	if res := sweep(t, db, SweepObserve); res.WouldRelease != 1 {
		t.Fatalf("first observe pass = %+v, want claim-x's deadline counted", res)
	}
	if _, err := NewPostgresPrefixAllocator().Release(ctx, late, "claim-a"); err != nil {
		t.Fatalf("late retention: %v", err)
	}
	if err := late.Commit(ctx); err != nil {
		t.Fatalf("commit late retention: %v", err)
	}
	sweep(t, db, SweepObserve)
	sweep(t, db, SweepObserve)
	if got := releasesCounted(t, SweepObserve) - before; got != 2 {
		t.Errorf("observe counted %v would-be releases; enforce would release 2", got)
	}
	if res := sweep(t, db, SweepEnforce); res.Released != 2 {
		t.Errorf("enforce released %d, want 2", res.Released)
	}
}

// A deadline is stored to the whole second, rounded up, so the deadline the
// IPAllocation shows is exactly the one the sweeper acts on, and not before
// retention plus the full lease.
func TestTheShownDeadlineIsTheStoredOne(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "precision", "10.97.0.0/16")
	lease := 90*time.Minute + 500*time.Millisecond
	setPoolCap(t, db, poolKey, dur(lease))
	retain(t, db, poolKey, "claim-a", "alloc-a")
	if _, err := db.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = NULL WHERE allocation_key = 'alloc-a'`); err != nil {
		t.Fatalf("unevaluate: %v", err)
	}
	sweep(t, db, SweepObserve)

	stored := retainedRetention(t, db, "alloc-a")
	shown := readAllocationObject(t, db, "alloc-a").Status.ExpiresAt
	if shown == nil || !shown.Time.Equal(stored.ExpiresAt) {
		t.Errorf("status.expiresAt = %v, stored deadline %s; want them equal", shown, stored.ExpiresAt)
	}
	if stored.ExpiresAt.Before(stored.RetainedAt.Add(lease)) {
		t.Errorf("deadline %s is before retention plus the lease, %s", stored.ExpiresAt, stored.RetainedAt.Add(lease))
	}
	if got := stored.ExpiresAt.Sub(stored.RetainedAt); got >= lease+time.Second {
		t.Errorf("deadline is %s after retention, want less than a second past the %s lease", got, lease)
	}
}

// A pass that panics is recovered, counted, and followed by another after a
// backoff; the process carries on.
func TestTheSweepLoopSurvivesAPanickingPass(t *testing.T) {
	before, _ := testutil.GetCounterMetricValue(metrics.RetentionSweepErrors)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// A sweeper with no database panics on its first query.
	NewSweeper(nil, SweepObserve).Run(ctx, 10*time.Millisecond)
	if got, _ := testutil.GetCounterMetricValue(metrics.RetentionSweepErrors); got-before < 2 {
		t.Errorf("errors moved by %v; want the loop to have recovered and retried at least twice", got-before)
	}
}

// A lease whose publication fails is not kept: the row stays unevaluated, so
// its status cannot lag it, and a later pass evaluates and publishes it once
// the IPAllocation can be written.
func TestALeaseThatCannotBePublishedIsEvaluatedOnceItCanBe(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "eval-publish", "10.67.0.0/16")
	seedLeaseClass(t, db, dur(time.Hour))
	retain(t, db, poolKey, "claim-a", "alloc-a")
	if _, err := db.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = NULL WHERE allocation_key = 'alloc-a'`); err != nil {
		t.Fatalf("unevaluate: %v", err)
	}
	allow := refuseUpdatesOf(t, db, "alloc-a")

	res, err := NewSweeper(db, SweepObserve).Sweep(ctx)
	if err != nil || res.Evaluated != 0 || res.RowErrors != 1 {
		t.Fatalf("pass while alloc-a cannot be published = %+v, %v; want one row error and nothing evaluated", res, err)
	}
	if retainedRetention(t, db, "alloc-a").Lease != LeaseUnevaluated {
		t.Fatal("the lease was kept though its IPAllocation could not show it")
	}

	allow()
	if res := sweep(t, db, SweepObserve); res.Evaluated != 1 {
		t.Fatalf("pass once alloc-a can be published = %+v, want it evaluated", res)
	}
	stored := retainedRetention(t, db, "alloc-a")
	a := readAllocationObject(t, db, "alloc-a")
	if a.Status.ExpiresAt == nil || !a.Status.ExpiresAt.Time.Equal(stored.ExpiresAt) {
		t.Errorf("alloc-a shows %v, stored %s; want the status to have caught up", a.Status.ExpiresAt, stored.ExpiresAt)
	}
}

// A page cut short by the pass's row budget is not mistaken for the end of the
// set: the next pass resumes after it instead of starting over.
func TestABudgetShortOfAPageResumesWhereItStopped(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "budget", "10.68.0.0/16")
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, retained_at, expires_at)
		 SELECT $1, ('10.68.' || g || '.0/24')::cidr, NULL, 'b-' || lpad(g::text, 2, '0'), 'IPv4', 'Claim',
		        now() - interval '2 days', NULL
		   FROM generate_series(0, 9) g`, poolKey); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewSweeper(db, SweepObserve)
	s.pageSize, s.rowBudget = 4, 6
	first, err := s.Sweep(ctx)
	if err != nil || first.Evaluated != 6 {
		t.Fatalf("first pass = %+v, %v; want the budget's 6 rows evaluated", first, err)
	}
	if !s.evaluateFrom.set {
		t.Fatal("a pass that stopped on its budget forgot where it stopped")
	}
	if second, err := s.Sweep(ctx); err != nil || second.Evaluated != 4 {
		t.Errorf("second pass = %+v, %v; want the remaining 4", second, err)
	}
}
