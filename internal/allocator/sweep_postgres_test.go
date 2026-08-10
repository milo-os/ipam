package allocator

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// seedLeasePool writes a pool and a class with the given lease settings, and
// returns the pool key.
func seedLeasePool(t *testing.T, db *pgxpool.Pool, cidr string, classLease, poolCap *metav1.Duration) (poolKey, className string) {
	t.Helper()
	poolKey = platformKey("ippools", "lease-pool")
	className = "lease-class"

	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "lease-pool"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: cidr, IPFamily: ipamv1alpha1.IPv4,
			ClassNames: []string{className}, MaxRetentionLease: poolCap,
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: ipamv1alpha1.IPv4,
		},
	}
	seedObject(t, db, poolKey, "IPPool", "lease-pool", pool)

	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: className},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily:             ipamv1alpha1.IPv4,
			AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 32, Max: 32},
			ReclaimPolicy:        ipamv1alpha1.ReclaimRetain,
			RetentionLease:       classLease,
		},
	}
	seedObject(t, db, platformKey("ipclasses", className), "IPClass", className, class)
	return poolKey, className
}

// allocateAndRetain allocates an address, writes its IPAllocation object, then
// deletes the claim so the allocation enters the retained state through the
// real release path — which is what sets retained_at via migration 004's trigger.
func allocateAndRetain(t *testing.T, db *pgxpool.Pool, poolKey, className, name string) string {
	t.Helper()
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()
	allocKey := "/ipam.miloapis.com/ipallocations/default/" + name

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cidr, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
		PoolKey: poolKey, AllocationKey: allocKey, ClaimKey: "/claim/" + name,
		ClassName: className, ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32,
		IPFamily: "IPv4", ReclaimPolicy: string(ipamv1alpha1.ReclaimRetain),
		OwnerProject: "acme",
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("allocate %s: %v", name, err)
	}
	obj := &ipamv1alpha1.IPAllocation{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPAllocation"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ipamv1alpha1.IPAllocationSpec{
			IPFamily: ipamv1alpha1.IPv4, PoolRef: ipamv1alpha1.LocalRef{Name: "lease-pool"},
			ClassName: className, Purpose: ipamv1alpha1.PurposeClaim,
			ClaimRef: &ipamv1alpha1.LocalRef{Name: name},
		},
		Status: ipamv1alpha1.IPAllocationStatus{
			Phase: ipamv1alpha1.AllocationReady, AllocatedCIDR: cidr,
		},
	}
	data, err := encodeObject(obj)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("encode: %v", err)
	}
	if _, err := insertObject(ctx, tx, allocKey, "IPAllocation", "default", name, data); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert allocation object: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Retain it by releasing the claim — the real path, so the trigger fires.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	outcomes, err := alloc.Release(ctx, tx, "/claim/"+name)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("release: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Retained {
		_ = tx.Rollback(ctx)
		t.Fatalf("outcomes = %+v, want one retained", outcomes)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit release: %v", err)
	}
	return cidr
}

func retainedAtOf(t *testing.T, db *pgxpool.Pool, allocKey string) time.Time {
	t.Helper()
	var ts *time.Time
	if err := db.QueryRow(platformCtx(),
		`SELECT retained_at FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocKey).Scan(&ts); err != nil {
		t.Fatalf("read retained_at: %v", err)
	}
	if ts == nil {
		t.Fatalf("retained_at is NULL for %s; the release path did not record retention", allocKey)
	}
	return *ts
}

// The whole lifecycle: retained, warned, released — and the capacity comes back.
func TestSweepMarksThenReleases(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	lease := 30 * 24 * time.Hour
	poolKey, _ := seedLeasePool(t, db, "10.220.0.0/30", &metav1.Duration{Duration: lease}, nil)
	className := "lease-class"
	cidr := allocateAndRetain(t, db, poolKey, className, "a")
	allocKey := "/ipam.miloapis.com/ipallocations/default/a"
	retainedAt := retainedAtOf(t, db, allocKey)

	// Before the lease elapses: examined, and deliberately untouched.
	res, err := alloc.SweepExpiredLeases(ctx, db, SweepOptions{Now: retainedAt.Add(time.Hour)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Candidates != 1 {
		t.Errorf("candidates = %d, want 1 — a healthy sweep must show it looked", res.Candidates)
	}
	if res.Marked != 0 || res.Released != 0 {
		t.Errorf("a lease that has not elapsed must not act: %+v", res)
	}

	// Past the lease, inside the grace window: marked, not released.
	res, err = alloc.SweepExpiredLeases(ctx, db, SweepOptions{Now: retainedAt.Add(lease + time.Hour)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Marked != 1 || res.Released != 0 {
		t.Fatalf("expected one mark and no release, got %+v", res)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocKey); n != 1 {
		t.Error("a marked allocation must still hold its address")
	}
	// The warning must be visible on the object, not merely in a log.
	var condCount int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_objects
		  WHERE key = $1
		    AND ipam_data_to_jsonb(data) -> 'status' -> 'conditions' @> '[{"type":"Expiring"}]'`,
		allocKey).Scan(&condCount); err != nil {
		t.Fatalf("read condition: %v", err)
	}
	if condCount != 1 {
		t.Error("the allocation object must publish an Expiring condition before release")
	}

	// Re-marking is idempotent and must not be counted as new work.
	res, err = alloc.SweepExpiredLeases(ctx, db, SweepOptions{Now: retainedAt.Add(lease + 2*time.Hour)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Marked != 0 {
		t.Errorf("re-marking must not count as new work, got %+v", res)
	}

	// Past lease + grace: released, object and row together.
	res, err = alloc.SweepExpiredLeases(ctx, db, SweepOptions{
		Now: retainedAt.Add(lease + DefaultLeaseGrace + time.Minute)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Released != 1 {
		t.Fatalf("expected one release, got %+v", res)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocKey); n != 0 {
		t.Error("the allocation row must be gone")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE key = $1`, allocKey); n != 0 {
		t.Error("the allocation object must be gone")
	}

	// The property that matters: the capacity actually returned.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	reissued, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
		PoolKey: poolKey, AllocationKey: "/alloc/next", ClaimKey: "/claim/next",
		ClassName: className, ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32,
		IPFamily: "IPv4", ReclaimPolicy: string(ipamv1alpha1.ReclaimDelete),
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-allocate: %v", err)
	}
	_ = tx.Commit(ctx)
	if reissued != cidr {
		t.Errorf("re-issued %s, want the expired address %s back in circulation", reissued, cidr)
	}
}

// An allocation with no lease is examined and held. This is the default, and it
// is what makes shipping the feature safe: nothing expires until someone opts in.
func TestSweepHoldsAllocationsWithNoLease(t *testing.T) {
	db := newMigratedPool(t)
	poolKey, className := seedLeasePool(t, db, "10.220.0.0/30", nil, nil)
	allocateAndRetain(t, db, poolKey, className, "a")

	res, err := NewPostgresPrefixAllocator().SweepExpiredLeases(platformCtx(), db,
		SweepOptions{Now: time.Now().Add(10 * 365 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Candidates != 1 {
		t.Errorf("candidates = %d, want 1", res.Candidates)
	}
	if res.NoLease != 1 {
		t.Errorf("noLease = %d, want 1 — held-forever addresses must be counted, not skipped silently", res.NoLease)
	}
	if res.Marked != 0 || res.Released != 0 {
		t.Errorf("no lease means no expiry, even a decade on: %+v", res)
	}
}

// The pool's cap must bind a class that asks for longer, because the pool is the
// thing that runs out.
func TestSweepAppliesThePoolCap(t *testing.T) {
	db := newMigratedPool(t)
	poolKey, className := seedLeasePool(t, db,
		"10.220.0.0/30",
		&metav1.Duration{Duration: 365 * 24 * time.Hour}, // class asks for a year
		&metav1.Duration{Duration: time.Hour},            // pool allows an hour
	)
	allocateAndRetain(t, db, poolKey, className, "a")
	retainedAt := retainedAtOf(t, db, "/ipam.miloapis.com/ipallocations/default/a")

	// Grace is clamped to the lease, so an hour's lease means release at 2h.
	res, err := NewPostgresPrefixAllocator().SweepExpiredLeases(platformCtx(), db,
		SweepOptions{Now: retainedAt.Add(2*time.Hour + time.Minute)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Released != 1 {
		t.Fatalf("the pool's cap must bind a longer class lease, got %+v", res)
	}
}

// A gateway reservation must never expire. It has a NULL claim_key like a
// retained allocation, and only `purpose = 'Claim'` separates them — one word
// between a working sweeper and one that hands out subnet gateways.
func TestSweepNeverTouchesReservationsOrCarves(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	poolKey, _ := seedLeasePool(t, db, "10.220.0.0/24", &metav1.Duration{Duration: time.Nanosecond}, nil)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := ProvisionReservations(ctx, tx, poolKey, "IPv4", "acme",
		[]net.IPNet{mustCIDR(t, "10.220.0.0/24")},
		Reservation{Leading: 1, Trailing: 1, UnitPrefixLength: 32}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ProvisionReservations: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	before := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1`, poolKey)

	// A lease of one nanosecond: everything a lease *could* apply to is overdue.
	res, err := NewPostgresPrefixAllocator().SweepExpiredLeases(ctx, db,
		SweepOptions{Now: time.Now().Add(365 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Candidates != 0 {
		t.Errorf("candidates = %d, want 0 — reservations must not even be examined", res.Candidates)
	}
	if res.Released != 0 {
		t.Fatalf("the sweeper released %d reservation(s); a gateway reservation must never expire", res.Released)
	}
	after := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1`, poolKey)
	if after != before {
		t.Errorf("allocation rows went from %d to %d; reservations and carves are not the sweeper's to take", before, after)
	}
}

// The dangerous race, demonstrated rather than reasoned about.
//
// A replacement workload re-creating its claim should inherit the retained
// address. If a sweeper fires at that instant, exactly one of them must win:
// either the reclaim gets the address and the sweeper finds nothing, or the
// sweeper releases and the reclaim allocates fresh. What must never happen is
// both — a claim holding an address whose row the sweeper deleted.
//
// It really interleaves: both run concurrently against a real database, released
// together from a barrier, with no ordering imposed. The pool lock is what
// arbitrates, and it is the same lock the allocation path already takes.
func TestSweepDoesNotRaceAReclaim(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	lease := time.Hour
	poolKey, className := seedLeasePool(t, db, "10.220.0.0/24", &metav1.Duration{Duration: lease}, nil)

	const rounds = 12
	for i := range rounds {
		name := fmt.Sprintf("r%d", i)
		allocKey := "/ipam.miloapis.com/ipallocations/default/" + name
		cidr := allocateAndRetain(t, db, poolKey, className, name)
		retainedAt := retainedAtOf(t, db, allocKey)
		sweepAt := retainedAt.Add(lease + lease + time.Minute) // past lease + clamped grace

		var wg sync.WaitGroup
		start := make(chan struct{})
		var reclaimErr, sweepErr error
		var reclaimed string

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			tx, err := db.Begin(ctx)
			if err != nil {
				reclaimErr = err
				return
			}
			// A replacement claim asking for that exact address, which is what
			// re-binding a retained allocation looks like.
			reclaimed, reclaimErr = alloc.AllocatePrefix(ctx, tx, AllocateRequest{
				PoolKey: poolKey, AllocationKey: allocKey + "-reclaim",
				ClaimKey: "/claim/" + name + "-reclaim", ClassName: className,
				ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32, Address: cidr,
				IPFamily: "IPv4", ReclaimPolicy: string(ipamv1alpha1.ReclaimRetain),
			})
			if reclaimErr != nil {
				_ = tx.Rollback(ctx)
				return
			}
			reclaimErr = tx.Commit(ctx)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, sweepErr = alloc.SweepExpiredLeases(ctx, db, SweepOptions{Now: sweepAt})
		}()
		close(start)
		wg.Wait()

		if sweepErr != nil {
			t.Fatalf("round %d: sweep failed: %v", i, sweepErr)
		}

		// The invariant is about who holds the address, not about how many
		// rows exist at the end — the sweeper legitimately releases *after* a
		// reclaim has already been refused, so "refused" does not imply
		// "something still holds it". What must never happen is a claim
		// believing it holds an address no row backs, or two rows holding one.
		rows := countRows(t, db,
			`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1 AND host(allocated_cidr) = host($2::cidr)`,
			poolKey, cidr)
		if rows > 1 {
			t.Fatalf("round %d: %d rows hold %s — the sweeper and the reclaim both took it", i, rows, cidr)
		}

		reclaimRows := countRows(t, db,
			`SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocKey+"-reclaim")
		switch {
		case reclaimErr == nil:
			// The reclaim won, so its binding must be backed by a row. The
			// failure this guards is the serious one: a live claim holding an
			// address the sweeper deleted out from under it.
			if reclaimed != cidr {
				t.Fatalf("round %d: reclaim got %s, asked for %s", i, reclaimed, cidr)
			}
			if reclaimRows != 1 {
				t.Fatalf("round %d: the reclaim succeeded but %d rows back it — the sweeper released an address a live claim holds", i, reclaimRows)
			}
		case errors.Is(reclaimErr, ErrAddressTaken):
			// The address was still held when the reclaim looked. Whether the
			// sweeper has since released it is timing, not correctness — but
			// the refused reclaim must have left nothing behind.
			if reclaimRows != 0 {
				t.Fatalf("round %d: the reclaim was refused yet left %d row(s)", i, reclaimRows)
			}
		default:
			t.Fatalf("round %d: unexpected reclaim error: %v", i, reclaimErr)
		}
	}
}

// Migration 004's trigger clears retained_at when an allocation is re-bound, so
// an allocation retained, re-bound, then released again starts a *fresh* lease
// rather than inheriting its first retention timestamp.
//
// Without that, the original bug reappears one level down: the second retention
// would be measured from the first, and an allocation that had been retained
// once long ago would expire almost immediately the next time. This was in
// neither the design proposal nor the migration spec — `engine-core` added it
// and asked for a test, and the sweeper is what would be wrong without it.
func TestReBindingRestartsTheLeaseClock(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	lease := time.Hour
	poolKey, className := seedLeasePool(t, db, "10.220.0.0/30", &metav1.Duration{Duration: lease}, nil)
	allocateAndRetain(t, db, poolKey, className, "a")
	allocKey := "/ipam.miloapis.com/ipallocations/default/a"

	// Age the first retention deliberately. Without this the two retentions are
	// microseconds apart and the test cannot tell a restarted clock from an
	// inherited one — every sweep instant past the first is past the second too.
	// The first version of this test asserted the right property and could not
	// have failed for the right reason, which is the same defect as a
	// concurrency test that never contends.
	oldRetention := time.Now().Add(-72 * time.Hour).UTC()
	if _, err := db.Exec(ctx,
		`UPDATE ipam_cidr_allocations SET retained_at = $2 WHERE allocation_key = $1`,
		allocKey, oldRetention); err != nil {
		t.Fatalf("age the retention: %v", err)
	}

	// Confirm the setup really is past its lease, so the negative assertion
	// below means something: at this instant an inherited clock acts.
	//
	// The instant is inside the grace window — past the lease, before release —
	// deliberately. A confirming step that released the allocation would leave
	// nothing to re-bind, which is what the first attempt at this test did.
	overdueAt := oldRetention.Add(lease + 30*time.Minute)
	res, err := alloc.SweepExpiredLeases(ctx, db, SweepOptions{Now: overdueAt})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Marked != 1 {
		t.Fatalf("the aged allocation should be past its lease at %s, got %+v", overdueAt, res)
	}

	// Re-bind: the row gets a claim again, and the trigger must clear
	// retained_at, because a bound allocation is not retained at all.
	if _, err := db.Exec(ctx,
		`UPDATE ipam_cidr_allocations SET claim_key = $2 WHERE allocation_key = $1`,
		allocKey, "/claim/a-again"); err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	var afterBind *time.Time
	if err := db.QueryRow(ctx,
		`SELECT retained_at FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocKey).Scan(&afterBind); err != nil {
		t.Fatalf("read retained_at after re-bind: %v", err)
	}
	if afterBind != nil {
		t.Fatalf("retained_at = %s after re-binding; a bound allocation is not retained", afterBind)
	}

	// A bound allocation is invisible to the sweeper regardless of age.
	res, err = alloc.SweepExpiredLeases(ctx, db, SweepOptions{Now: overdueAt})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Candidates != 0 {
		t.Errorf("candidates = %d, want 0 — a re-bound allocation is not in the retained set", res.Candidates)
	}

	// Release it again. The clock restarts from now, not from the aged value.
	if _, err := db.Exec(ctx,
		`UPDATE ipam_cidr_allocations SET claim_key = NULL WHERE allocation_key = $1`, allocKey); err != nil {
		t.Fatalf("second release: %v", err)
	}
	secondRetention := retainedAtOf(t, db, allocKey)
	if !secondRetention.After(oldRetention.Add(time.Hour)) {
		t.Fatalf("second retention %s did not restart from the aged first retention %s", secondRetention, oldRetention)
	}

	// The consequence: at the instant that *was* past the lease, it no longer
	// is. Measured from the first retention this allocation is well overdue;
	// measured from the second it has barely started.
	res, err = alloc.SweepExpiredLeases(ctx, db, SweepOptions{Now: overdueAt})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Released != 0 || res.Marked != 0 {
		t.Fatalf("the lease was measured from the first retention, not the second: %+v", res)
	}
	if res.Candidates != 1 {
		t.Errorf("candidates = %d, want 1 — it is retained again, just not due", res.Candidates)
	}

	// And it still expires on its own schedule.
	res, err = alloc.SweepExpiredLeases(ctx, db, SweepOptions{
		Now: secondRetention.Add(lease + lease + time.Minute)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Released != 1 {
		t.Fatalf("expected release on the second lease, got %+v", res)
	}
}

// Pre-004 retained rows carry no retained_at, which means no lease applies. They
// must be examined and held, never treated as epoch-old and swept — that being
// the whole reason no backfill was written.
func TestSweepTreatsNullRetainedAtAsNeverDue(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	poolKey, className := seedLeasePool(t, db, "10.220.0.0/30", &metav1.Duration{Duration: time.Nanosecond}, nil)
	allocateAndRetain(t, db, poolKey, className, "a")
	allocKey := "/ipam.miloapis.com/ipallocations/default/a"

	// Simulate a row retained before the lease feature existed.
	if _, err := db.Exec(ctx,
		`UPDATE ipam_cidr_allocations SET retained_at = NULL WHERE allocation_key = $1`, allocKey); err != nil {
		t.Fatalf("clear retained_at: %v", err)
	}

	res, err := NewPostgresPrefixAllocator().SweepExpiredLeases(ctx, db,
		SweepOptions{Now: time.Now().Add(10 * 365 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Released != 0 || res.Marked != 0 {
		t.Fatalf("a row with no retention timestamp must never be acted on, got %+v", res)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocKey); n != 1 {
		t.Error("the allocation must still hold its address")
	}
}

// --- reclaiming a retained address -------------------------------------------

// The property #34 exists for: the replacement gets the **same** address, not
// merely an address.
//
// Asserted by re-issue identity rather than by Bound, because a test asserting
// Bound would pass against release-and-reallocate — which is precisely the
// behaviour retention was chosen to replace.
func TestReclaimReturnsTheSameAddress(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	poolKey, className := seedLeasePool(t, db, "10.220.0.0/24", nil, nil)
	original := allocateAndRetain(t, db, poolKey, className, "a")
	allocKey := "/ipam.miloapis.com/ipallocations/default/a"

	// Somebody else claims in between, so a fresh allocation would not return
	// the same address by luck.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
		PoolKey: poolKey, AllocationKey: "/alloc/other", ClaimKey: "/claim/other",
		ClassName: className, ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32,
		IPFamily: "IPv4", ReclaimPolicy: string(ipamv1alpha1.ReclaimDelete),
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("interleaving allocation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The replacement, deriving the same allocation key from the same claim
	// identity.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	recovered, ok, err := alloc.ReclaimRetained(ctx, tx, ReclaimRequest{
		AllocationKey: allocKey, ClaimKey: "/claim/a-replacement",
		ClassName: className, ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ReclaimRetained: %v", err)
	}
	if !ok {
		_ = tx.Rollback(ctx)
		t.Fatal("nothing was reclaimed; the replacement did not find its predecessor's address")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit reclaim: %v", err)
	}

	if recovered != original {
		t.Fatalf("recovered %s, want the retained address %s — an address that survives a redeploy is the same address", recovered, original)
	}

	// It is bound again, and the lease clock has stopped.
	var claimKey *string
	var retainedAt *time.Time
	if err := db.QueryRow(ctx,
		`SELECT claim_key, retained_at FROM ipam_cidr_allocations WHERE allocation_key = $1`,
		allocKey).Scan(&claimKey, &retainedAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if claimKey == nil || *claimKey != "/claim/a-replacement" {
		t.Errorf("claim_key = %v, want the replacement claim", claimKey)
	}
	if retainedAt != nil {
		t.Errorf("retained_at = %s; a re-bound allocation is not retained and its lease must not keep running", retainedAt)
	}
}

// Nothing retained is the ordinary case and must not be an error.
func TestReclaimFindsNothingForAFirstClaim(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	seedLeasePool(t, db, "10.220.0.0/30", nil, nil)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, ok, err := NewPostgresPrefixAllocator().ReclaimRetained(ctx, tx, ReclaimRequest{
		AllocationKey: "/ipam.miloapis.com/ipallocations/default/never-existed",
		ClaimKey:      "/claim/x", ClassName: "lease-class",
		ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32,
	})
	if err != nil {
		t.Fatalf("a first claim must not error: %v", err)
	}
	if ok {
		t.Error("reclaimed something that was never allocated")
	}
}

// A claim that disagrees with what was retained is refused, not quietly given
// something else. Silently allocating a different address would leave the
// retained one held by nobody and invisible, while the caller believed it had
// recovered.
func TestReclaimRefusesAMismatch(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	poolKey, className := seedLeasePool(t, db, "10.220.0.0/24", nil, nil)
	allocateAndRetain(t, db, poolKey, className, "a")
	allocKey := "/ipam.miloapis.com/ipallocations/default/a"

	base := ReclaimRequest{
		AllocationKey: allocKey, ClaimKey: "/claim/a-replacement",
		ClassName: className, ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32,
	}

	tests := []struct {
		name    string
		mutate  func(*ReclaimRequest)
		wantMsg string
	}{
		{
			name:    "a different class is not the same claim wearing the same name",
			mutate:  func(r *ReclaimRequest) { r.ClassName = "some-other-class" },
			wantMsg: "class",
		},
		{
			// The subtle one: a network deleted and recreated under the same name
			// is a different address space, so a claim in the new one derives an
			// identical name and must still not inherit the old space's address.
			name: "a different address space does not inherit",
			mutate: func(r *ReclaimRequest) {
				r.ScopeDigest = scope.AddressSpaceDigest("", map[string]ipam.ScopeRef{
					"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "recreated", UID: "new-uid"},
				})
			},
			wantMsg: "address space",
		},
		{
			name:    "a different size is refused rather than reinterpreted",
			mutate:  func(r *ReclaimRequest) { r.PrefixLength = 30 },
			wantMsg: "asks for a /30",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)

			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			_, ok, err := alloc.ReclaimRetained(ctx, tx, req)
			_ = tx.Rollback(ctx)

			if !errors.Is(err, ErrRetainedMismatch) {
				t.Fatalf("expected ErrRetainedMismatch, got ok=%v err=%v", ok, err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("the error must name what differs; got %v", err)
			}
		})
	}

	// After every refusal the address is still retained and still held — a
	// refused reclaim must not have consumed it.
	var claimKey *string
	if err := db.QueryRow(ctx,
		`SELECT claim_key FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocKey).Scan(&claimKey); err != nil {
		t.Fatalf("the allocation was consumed by a refused reclaim: %v", err)
	}
	if claimKey != nil {
		t.Errorf("claim_key = %v after refusals; it must still be retained", *claimKey)
	}
}

// An allocation somebody already holds is not reclaimable, even by a claim whose
// identity derives the same key — that would be one claim taking another's
// address.
func TestReclaimRefusesAnAllocationAlreadyBound(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	poolKey, className := seedLeasePool(t, db, "10.220.0.0/30", nil, nil)
	allocateAndRetain(t, db, poolKey, className, "a")
	allocKey := "/ipam.miloapis.com/ipallocations/default/a"

	// Re-bind it once.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, ok, err := alloc.ReclaimRetained(ctx, tx, ReclaimRequest{
		AllocationKey: allocKey, ClaimKey: "/claim/first",
		ClassName: className, ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32,
	}); err != nil || !ok {
		_ = tx.Rollback(ctx)
		t.Fatalf("first reclaim: ok=%v err=%v", ok, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// A second claim deriving the same key must be refused.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _, err = alloc.ReclaimRetained(ctx, tx, ReclaimRequest{
		AllocationKey: allocKey, ClaimKey: "/claim/second",
		ClassName: className, ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32,
	})
	if !errors.Is(err, ErrRetainedMismatch) {
		t.Fatalf("expected a bound allocation to be unreclaimable, got %v", err)
	}
	if !strings.Contains(err.Error(), "/claim/first") {
		t.Errorf("the error should name who holds it, got %v", err)
	}
}
