package allocator

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// spaceOf is the digest of the address space a claim carrying network `name`
// lands in, for a class whose uniqueWithin is [network].
func spaceOf(name string) string {
	return scope.AddressSpaceDigest(testProject, map[string]ipam.ScopeRef{
		"network": {APIGroup: "networking.miloapis.com", Kind: "Network", Name: name},
	})
}

// allocateInSpace reserves a block in one address space and returns its CIDR.
func allocateInSpace(t *testing.T, db *pgxpool.Pool, poolKey, claim, digest string, prefixLen int) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cidr, err := NewPostgresPrefixAllocator().AllocatePrefix(ctx, tx, PrefixRequest{
		PoolKey: poolKey, PrefixLen: prefixLen, IPFamily: "IPv4",
		ClaimKey: claim, AllocationKey: claim, OwnerProject: testProject,
		ClassName: "standard", ScopeDigest: digest,
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

// seedPoolWithStrategy seeds a pool whose allocation strategy is not FirstFit,
// which takes the whole-set search path rather than the paged one.
func seedPoolWithStrategy(t *testing.T, db *pgxpool.Pool, name, cidr string, strategy ipamv1alpha1.AllocationStrategy) string {
	t.Helper()
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: cidr, IPFamily: ipamv1alpha1.IPv4,
			Allocation: ipamv1alpha1.AllocationSpec{Strategy: strategy},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: ipamv1alpha1.IPv4,
		},
	}
	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	key := tenant.Identity{Name: testProject}.ResourceKey("ippools", name)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		key, name, data); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return key
}

// withhold inserts a row that is not a claim — a reservation, or the carve
// backing a child pool — holding block in every address space.
func withhold(t *testing.T, db *pgxpool.Pool, poolKey, key, block, purpose string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, allocation_key, ip_family, purpose,
		     class_name, scope_digest, reclaim_policy, owner_project)
		 VALUES ($1, $2, $3, 'IPv4', $4, 'standard', $5, 'Delete', $6)`,
		poolKey, block, key, purpose, scope.EmptyAddressSpaceDigest(), testProject); err != nil {
		t.Fatalf("withhold %s as %s: %v", block, purpose, err)
	}
}

// The search and the insert must agree. Two claims in different spaces get one
// address, and each row records the space its search ran under.
func TestTwoSpacesShareAnAddress(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "spaces-share", "10.70.0.0/16")

	a := allocateInSpace(t, db, poolKey, "claim-a", spaceOf("net-a"), 24)
	b := allocateInSpace(t, db, poolKey, "claim-b", spaceOf("net-b"), 24)

	if a != "10.70.0.0/24" || b != "10.70.0.0/24" {
		t.Errorf("got %s and %s, want both spaces to start at the base of the pool", a, b)
	}

	var digest string
	if err := db.QueryRow(context.Background(),
		`SELECT scope_digest FROM ipam_cidr_allocations WHERE claim_key = 'claim-a'`).Scan(&digest); err != nil {
		t.Fatalf("read allocation row: %v", err)
	}
	if digest != spaceOf("net-a") {
		t.Errorf("row recorded space %q, but the search ran in %q", digest, spaceOf("net-a"))
	}
}

// The other half of the same agreement: a search that ran under one digest and
// a row recorded under another would stop blocking anything.
func TestOneSpaceDoesNotShareAnAddress(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "spaces-distinct", "10.75.0.0/16")

	a := allocateInSpace(t, db, poolKey, "claim-a", spaceOf("shared"), 24)
	b := allocateInSpace(t, db, poolKey, "claim-b", spaceOf("shared"), 24)

	if a == b {
		t.Errorf("two claims in one space both got %s", a)
	}
}

// The whole-set path and the paged path must agree on what a space contains,
// or a pool reports itself exhausted on another space's allocations.
func TestTheWholeSetSearchIsAlsoPerSpace(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPoolWithStrategy(t, db, "spaces-bestfit", "10.72.0.0/16", ipamv1alpha1.BestFit)

	a := allocateInSpace(t, db, poolKey, "claim-a", spaceOf("net-a"), 24)
	b := allocateInSpace(t, db, poolKey, "claim-b", spaceOf("net-b"), 24)

	if a != b {
		t.Errorf("under BestFit two spaces got %s and %s; each space is searched alone", a, b)
	}
}

// A reservation and a carve are space that has left the pool, so no space may
// allocate inside them.
func TestWithheldBlocksHoldEverySpace(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "spaces-withheld", "10.71.0.0/16")

	withhold(t, db, poolKey, "reservation-1", "10.71.0.0/24", "Reservation")
	withhold(t, db, poolKey, "carve-1", "10.71.1.0/24", "PoolCarve")

	for _, space := range []string{"net-a", "net-b"} {
		got := allocateInSpace(t, db, poolKey, "claim-"+space, spaceOf(space), 24)
		if got != "10.71.2.0/24" {
			t.Errorf("space %s got %s, want the first block below the withheld ones", space, got)
		}
	}
}

// A carve takes space out of the pool entirely, so it must clear every space's
// claims and not only the one it happens to be recorded under.
func TestACarveAvoidsClaimsInEverySpace(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "spaces-carve", "10.73.0.0/16")

	if got := allocateInSpace(t, db, poolKey, "claim-a", spaceOf("net-a"), 24); got != "10.73.0.0/24" {
		t.Fatalf("seed claim took %s, want the base of the pool", got)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	block, err := carveFromPool(ctx, tx, poolKey, 20, poolCarve{
		AllocationKey: "child-pool", ClassName: "child",
		ScopeDigest: scope.PoolDigest(testProject, nil),
		IPFamily:    "IPv4", OwnerProject: testProject,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("carve: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if block.String() == "10.73.0.0/20" {
		t.Error("the carve covers a claim held in another address space")
	}
	if block.String() != "10.73.16.0/20" {
		t.Errorf("carve took %s, want the first /20 clear of every space", block)
	}
}

// Consumption is the union across spaces. An address two spaces hold is one
// address the pool cannot hand to a third.
func TestSharedAddressesAreCountedOnce(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "spaces-consumption", "10.74.0.0/16")

	allocateInSpace(t, db, poolKey, "claim-a", spaceOf("net-a"), 24)
	allocateInSpace(t, db, poolKey, "claim-b", spaceOf("net-b"), 24)

	var consumed string
	if err := db.QueryRow(ctx,
		`SELECT consumed::text FROM ipam_pool_consumption WHERE pool_key = $1`,
		poolKey).Scan(&consumed); err != nil {
		t.Fatalf("read consumption: %v", err)
	}
	if consumed != "256" {
		t.Errorf("consumed = %s, want the 256 addresses of one /24", consumed)
	}

	var data []byte
	if err := db.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, poolKey).Scan(&data); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	if pool.Status.Capacity.Allocated != "256" {
		t.Errorf("status.capacity.allocated = %s, want 256", pool.Status.Capacity.Allocated)
	}
	// 256 of 65536 is 0.3906%. Counting the shared block twice reads as 0.7812%.
	if pool.Status.UtilizationPercent != 0.3906 {
		t.Errorf("utilizationPercent = %v, want 0.3906", pool.Status.UtilizationPercent)
	}
}
