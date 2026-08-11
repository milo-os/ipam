package allocator

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// allocateWithPolicy allocates for claim under the given reclaim policy and
// returns the CIDR it was handed.
func allocateWithPolicy(t *testing.T, db *pgxpool.Pool, poolKey, claim, allocationKey string, policy ipamv1alpha1.ReclaimPolicy) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cidr, err := NewPostgresPrefixAllocator().AllocatePrefix(ctx, tx, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 24, IPFamily: "IPv4",
		ClaimKey: claim, AllocationKey: allocationKey,
		OwnerProject: testProject, ClassName: "standard", ReclaimPolicy: policy,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("allocate %s: %v", claim, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return cidr
}

func releaseReturningRetained(t *testing.T, db *pgxpool.Pool, claim string) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	retained, err := NewPostgresPrefixAllocator().Release(ctx, tx, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("release %s: %v", claim, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit release: %v", err)
	}
	return retained
}

// The default policy still frees the address, and the next claim reuses it.
func TestReleaseUnderDeleteFreesTheAddress(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "retain-delete", "10.50.0.0/16")

	first := allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimDelete)
	if retained := releaseReturningRetained(t, db, "claim-a"); len(retained) != 0 {
		t.Fatalf("Release reported %v retained under policy Delete", retained)
	}

	var rows int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1`, poolKey).Scan(&rows); err != nil {
		t.Fatalf("count allocations: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d allocation rows survive a Delete release, want 0", rows)
	}
	if got := allocateWithPolicy(t, db, poolKey, "claim-b", "alloc-b", ipamv1alpha1.ReclaimDelete); got != first {
		t.Errorf("next claim got %q, want the freed %q", got, first)
	}
}

// Retain leaves the row in place, unbound and stamped, and the address stays
// held: the next claim gets a different block.
func TestReleaseUnderRetainHoldsTheAddress(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	const cidr = "10.51.0.0/16"
	poolKey := seedPool(t, db, "retain-hold", cidr)

	first := allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)
	consumedBefore := storedConsumption(t, db, poolKey)

	retained := releaseReturningRetained(t, db, "claim-a")
	if len(retained) != 1 || retained[0] != "alloc-a" {
		t.Fatalf("Release reported retained = %v, want [alloc-a]", retained)
	}

	var claimKey *string
	var retainedAt *string
	var allocated string
	if err := db.QueryRow(ctx,
		`SELECT claim_key, retained_at::text,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations WHERE allocation_key = 'alloc-a'`,
	).Scan(&claimKey, &retainedAt, &allocated); err != nil {
		t.Fatalf("read retained row: %v", err)
	}
	if claimKey != nil {
		t.Errorf("claim_key = %q after retention, want NULL", *claimKey)
	}
	if retainedAt == nil {
		t.Error("retained_at is NULL; the trigger did not stamp the retention")
	}
	if allocated != first {
		t.Errorf("retained row holds %q, want %q", allocated, first)
	}

	// Nothing was freed, so nothing about the pool's consumption changed.
	if got := storedConsumption(t, db, poolKey); got != consumedBefore {
		t.Errorf("consumption = %s after retention, want the unchanged %s", got, consumedBefore)
	}
	if want := measureNow(t, db, poolKey, cidr); storedConsumption(t, db, poolKey) != want {
		t.Errorf("consumption %s disagrees with a fresh measurement %s", storedConsumption(t, db, poolKey), want)
	}

	if got := allocateWithPolicy(t, db, poolKey, "claim-b", "alloc-b", ipamv1alpha1.ReclaimDelete); got == first {
		t.Errorf("next claim was handed the retained %q", got)
	}
}

// A retained allocation has no claim left to release through, so its own key
// is the release path — and it frees the address for reuse.
func TestReleaseAllocationFreesARetainedAddress(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "retain-release", "10.52.0.0/16")

	first := allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)
	releaseReturningRetained(t, db, "claim-a")

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := NewPostgresPrefixAllocator().ReleaseAllocation(ctx, tx, "alloc-a"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ReleaseAllocation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var rows int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = 'alloc-a'`).Scan(&rows); err != nil {
		t.Fatalf("count allocations: %v", err)
	}
	if rows != 0 {
		t.Errorf("retained row survives its own release")
	}
	if got := allocateWithPolicy(t, db, poolKey, "claim-b", "alloc-b", ipamv1alpha1.ReclaimDelete); got != first {
		t.Errorf("next claim got %q, want the freed %q", got, first)
	}
}

// The row records what it was handed out under, because a retained row is read
// after the claim that chose those values is gone.
func TestAllocationRowRecordsItsClassAndPolicy(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "retain-columns", "10.53.0.0/16")
	allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)

	var className, policy, purpose string
	if err := db.QueryRow(context.Background(),
		`SELECT class_name, reclaim_policy, purpose
		   FROM ipam_cidr_allocations WHERE allocation_key = 'alloc-a'`,
	).Scan(&className, &policy, &purpose); err != nil {
		t.Fatalf("read allocation row: %v", err)
	}
	if className != "standard" {
		t.Errorf("class_name = %q, want standard", className)
	}
	if policy != string(ipamv1alpha1.ReclaimRetain) {
		t.Errorf("reclaim_policy = %q, want Retain", policy)
	}
	if purpose != "Claim" {
		t.Errorf("purpose = %q, want Claim", purpose)
	}
}
