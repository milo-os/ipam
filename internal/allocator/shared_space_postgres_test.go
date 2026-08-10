package allocator

// Two tenants drawing from one operator-authored pool.
//
// This is the pair of properties that decide whether a shared pool is safe, and
// they pull in opposite directions — which is why they are asserted together in
// one file rather than left to two suites that could be fixed independently:
//
//   - `uniqueWithin: []` is the STRICTEST setting. The class says nothing
//     separates two allocations, so the pool is one address space and no two
//     claims may hold the same block, whoever made them. A public-unicast class
//     is spelled this way, and two tenants holding one public address is the
//     failure that must be impossible.
//
//   - `uniqueWithin: [network]` is the shared-IPv4 case. Two tenants' networks
//     both named `default` are two networks, so both may hold the same address
//     out of the same pool. Refusing the second is the defect #55 fixed.
//
// A change that satisfies one and breaks the other looks correct from wherever
// the author was standing. Both run against real Postgres because the
// exclusion constraint on (pool_key, scope_digest, allocated_cidr) is half of
// the enforcement and a fake transaction has none of it.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// claimSpaceDigest computes the digest a claim's allocation is recorded under,
// by the same rule the claim registry applies at
// internal/registry/ipam/ipclaim/storage.go — the claim's scope projected onto
// the class's uniqueWithin, for the claiming tenant.
//
// The test goes through the production rule rather than hand-building a digest
// so that it tracks the code under test. If that rule changes, this call
// changes with it; the assertions below must not.
func claimSpaceDigest(t *testing.T, tenant string, claimScope map[string]ipam.ScopeRef, class *ipamv1alpha1.IPClass) string {
	t.Helper()
	digest, err := scope.ProjectAddressSpaceDigest(tenant, claimScope, class.Spec.UniqueWithin, "uniqueWithin")
	if err != nil {
		t.Fatalf("project claim scope onto uniqueWithin of %q: %v", class.Name, err)
	}
	return digest
}

// seedSharedPools writes two operator-authored IPv4 pools at the platform root,
// one per uniqueWithin shape. They are separate pools on purpose: a pool
// offered to two classes whose uniqueWithin differs is rejected at write time
// (validateUniqueWithinAgreement), because the two would not see each other's
// allocations.
func seedSharedPools(t *testing.T, db *pgxpool.Pool) {
	t.Helper()

	pools := []struct {
		name, cidr, class string
	}{
		{"flat-v4", "10.202.0.0/24", "flat-endpoint-v4"},
		{"pernet-v4", "10.203.0.0/24", "pernet-endpoint-v4"},
	}
	for _, p := range pools {
		obj := &ipamv1alpha1.IPPool{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
			ObjectMeta: metav1.ObjectMeta{Name: p.name},
			Spec: ipamv1alpha1.IPPoolSpec{
				CIDR:       p.cidr,
				IPFamily:   ipamv1alpha1.IPv4,
				ClassNames: []string{p.class},
			},
			Status: ipamv1alpha1.IPPoolStatus{
				Phase:         ipamv1alpha1.PoolReady,
				AllocatedCIDR: p.cidr,
				IPFamily:      ipamv1alpha1.IPv4,
			},
		}
		seedObject(t, db, platformKey("ippools", p.name), "IPPool", p.name, obj)
	}

	classes := []*ipamv1alpha1.IPClass{
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
			ObjectMeta: metav1.ObjectMeta{Name: "flat-endpoint-v4"},
			Spec: ipamv1alpha1.IPClassSpec{
				IPFamily:             ipamv1alpha1.IPv4,
				UniqueWithin:         []string{},
				AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 32, Max: 32},
				Visibility:           ipamv1alpha1.VisibilityConsumer,
			},
		},
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
			ObjectMeta: metav1.ObjectMeta{Name: "pernet-endpoint-v4"},
			Spec: ipamv1alpha1.IPClassSpec{
				IPFamily:             ipamv1alpha1.IPv4,
				UniqueWithin:         []string{"network"},
				AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 32, Max: 32},
				Visibility:           ipamv1alpha1.VisibilityConsumer,
			},
		},
	}
	for _, c := range classes {
		seedObject(t, db, platformKey("ipclasses", c.Name), "IPClass", c.Name, c)
	}
}

// allocateAs runs one claim's allocation as a given tenant, in its own
// committed transaction, exactly as the claim path does.
func allocateAs(t *testing.T, db *pgxpool.Pool, alloc *PostgresPrefixAllocator,
	poolKey, tenant, name string, class *ipamv1alpha1.IPClass, claimScope map[string]ipam.ScopeRef,
) (string, error) {
	t.Helper()
	ctx := platformCtx()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cidr, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
		PoolKey:       poolKey,
		AllocationKey: "project/" + tenant + "/alloc/" + name,
		ClaimKey:      "project/" + tenant + "/claim/" + name,
		ClassName:     class.Name,
		ScopeDigest:   claimSpaceDigest(t, tenant, claimScope, class),
		PrefixLength:  32,
		IPFamily:      "IPv4",
		ReclaimPolicy: string(ipamv1alpha1.ReclaimDelete),
		OwnerProject:  tenant,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", name, err)
	}
	return cidr, nil
}

// TestFlatClassIsOneSpaceAcrossTenants is the regression guard for #64.
//
// Measured on a live cluster before the fix: a platform-authored class with
// `uniqueWithin: []` over a platform-authored 10.202.0.0/24 handed
// 10.202.0.0/32 to project-alpha AND to project-beta. Both Bound, one pool,
// nothing logged — the success path, which is the shape that gets believed.
//
// The assertion is on the second tenant getting a DIFFERENT address, not on
// both merely succeeding. Both succeeding is what the defect does.
func TestFlatClassIsOneSpaceAcrossTenants(t *testing.T) {
	db := newMigratedPool(t)
	seedSharedPools(t, db)
	alloc := NewPostgresPrefixAllocator()

	poolKey := platformKey("ippools", "flat-v4")
	class := loadClassFromDB(t, db, "flat-endpoint-v4")

	// No scope at all: uniqueWithin is empty, so a claim's scope is projected
	// onto nothing and the class's own rule is the whole story.
	alphaCIDR, err := allocateAs(t, db, alloc, poolKey, "project-alpha", "a1", class, nil)
	if err != nil {
		t.Fatalf("project-alpha allocate: %v", err)
	}
	betaCIDR, err := allocateAs(t, db, alloc, poolKey, "project-beta", "b1", class, nil)
	if err != nil {
		t.Fatalf("project-beta allocate: %v", err)
	}

	if alphaCIDR == betaCIDR {
		t.Fatalf("two tenants were handed the same address %s out of one pool; "+
			"uniqueWithin: [] is the strictest setting and means one space, not one space per tenant",
			alphaCIDR)
	}

	// And the positive control for the same mechanism: it must be one space
	// because the second tenant's search SAW the first tenant's allocation,
	// not because something else moved the address. The pool is empty and
	// FirstFit, so the two addresses have to be consecutive.
	if alphaCIDR != "10.202.0.0/32" || betaCIDR != "10.202.0.1/32" {
		t.Errorf("addresses = (%s, %s), want (10.202.0.0/32, 10.202.0.1/32): "+
			"the second tenant must take the next free address in the same space",
			alphaCIDR, betaCIDR)
	}

	// Both rows must carry the SAME scope digest. Equal-but-different addresses
	// could also be produced by two spaces that happened not to collide, and
	// that would pass the check above while leaving the defect in place.
	var digests int
	if err := db.QueryRow(platformCtx(),
		`SELECT count(DISTINCT scope_digest) FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose = 'Claim'`,
		poolKey).Scan(&digests); err != nil {
		t.Fatalf("count digests: %v", err)
	}
	if digests != 1 {
		t.Errorf("distinct scope digests = %d, want 1: a flat class is one address space, "+
			"so both tenants' allocations must be recorded in it", digests)
	}
}

// TestPerNetworkClassKeepsTenantsApart is the guard on the other direction —
// the property #55 established, which the fix for #64 must not undo.
//
// Two projects each have a network named `default`. They are two networks, so
// both may hold the same address out of one shared pool. This is the whole
// point of uniqueWithin: [network] and is what shared tenant IPv4 requires.
func TestPerNetworkClassKeepsTenantsApart(t *testing.T) {
	db := newMigratedPool(t)
	seedSharedPools(t, db)
	alloc := NewPostgresPrefixAllocator()

	poolKey := platformKey("ippools", "pernet-v4")
	class := loadClassFromDB(t, db, "pernet-endpoint-v4")

	defaultNetwork := map[string]ipam.ScopeRef{
		"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
	}

	alphaCIDR, err := allocateAs(t, db, alloc, poolKey, "project-alpha", "a1", class, defaultNetwork)
	if err != nil {
		t.Fatalf("project-alpha allocate: %v", err)
	}
	betaCIDR, err := allocateAs(t, db, alloc, poolKey, "project-beta", "b1", class, defaultNetwork)
	if err != nil {
		t.Fatalf("project-beta allocate: %v", err)
	}

	if alphaCIDR != betaCIDR {
		t.Errorf("addresses = (%s, %s), want both 10.203.0.0/32: two projects' networks named "+
			"`default` are two networks and must be two address spaces, each starting at the "+
			"pool's first address", alphaCIDR, betaCIDR)
	}
	if alphaCIDR != "10.203.0.0/32" {
		t.Errorf("first address = %s, want 10.203.0.0/32", alphaCIDR)
	}

	// Two spaces, so two distinct digests — the inverse of the flat case, and
	// the reason a single fix cannot satisfy both by making every digest equal
	// or every digest distinct.
	var digests int
	if err := db.QueryRow(platformCtx(),
		`SELECT count(DISTINCT scope_digest) FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose = 'Claim'`,
		poolKey).Scan(&digests); err != nil {
		t.Fatalf("count digests: %v", err)
	}
	if digests != 2 {
		t.Errorf("distinct scope digests = %d, want 2: each project's `default` network is its own space", digests)
	}

	// A second claim in one project's own network must still be refused the
	// address that project already holds. Without this the test would pass
	// against an implementation that gave every CLAIM its own space.
	second, err := allocateAs(t, db, alloc, poolKey, "project-alpha", "a2", class, defaultNetwork)
	if err != nil {
		t.Fatalf("project-alpha second allocate: %v", err)
	}
	if second == alphaCIDR {
		t.Errorf("two claims in one network got the same address %s", second)
	}
}
