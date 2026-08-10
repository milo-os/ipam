package allocator

// An operator-created child pool's block against its parent (#66).
//
// Two routes create a child pool and they used to disagree about what one is.
// The cascade called carveFromPool — a carve, purpose PoolCarve, searched
// against every allocation the parent holds. The IPPool registry called
// AllocatePrefix — a claim, purpose Claim, searched against one address space.
// Same operation, same table, two answers.
//
// The disagreement is invisible on any pool whose claims all share one digest,
// which is every pool a `uniqueWithin: []` class backs, and that is most of the
// fixtures. It only shows on a pool serving a class that separates spaces, and
// then it shows in both directions at once: the carve does not withhold its
// block from the claims, and the claims do not withhold theirs from the carve.
// Both are asserted below, because a fix for either one alone reads as complete.
//
// Real Postgres, not a fake transaction: half of what makes the old behaviour
// survivable-looking is that the exclusion constraint on
// (pool_key, scope_digest, allocated_cidr) does not catch it — the two rows
// differ in scope_digest — and a fake has no constraint to be silent.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

const (
	carveParentPool = "carve-parent-v4"
	carveParentCIDR = "10.204.0.0/24"
	carveChildName  = "carve-child-v4"
	carveClass      = "carve-pernet-endpoint-v4"
)

// carveNetworkScope is the scope of every claim below: one network, so the
// claims land in an address space of their own and not in the universal one.
var carveNetworkScope = map[string]ipam.ScopeRef{
	"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
}

// seedCarveParent writes one operator-authored /24 offered to a class whose
// uniqueWithin separates address spaces.
//
// `uniqueWithin: [network]` is load-bearing here rather than incidental. Under
// `uniqueWithin: []` every claim lands in the universal address space, which is
// the digest the registry records a carve under, so a row with the wrong
// purpose is matched by digest instead and the block is withheld anyway —
// correct by accident. The defect needs a second address space to be visible at
// all, which is why most of the existing fixtures cannot see it.
func seedCarveParent(t *testing.T, db *pgxpool.Pool) {
	t.Helper()

	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: carveParentPool},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       carveParentCIDR,
			IPFamily:   ipamv1alpha1.IPv4,
			ClassNames: []string{carveClass},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: carveParentCIDR,
			IPFamily:      ipamv1alpha1.IPv4,
		},
	}
	seedObject(t, db, platformKey("ippools", carveParentPool), "IPPool", carveParentPool, pool)

	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: carveClass},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily:             ipamv1alpha1.IPv4,
			UniqueWithin:         []string{"network"},
			AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 32, Max: 32},
			Visibility:           ipamv1alpha1.VisibilityConsumer,
		},
	}
	seedObject(t, db, platformKey("ipclasses", class.Name), "IPClass", class.Name, class)
}

// carveChild runs the carve with exactly the arguments
// AllocatingIPPoolREST.Create passes for a pool with spec.parentPoolRef set:
// the child pool's own key as the allocation key, no class (an operator
// authored it, so no class built it), and the universal address space digest.
//
// It goes through the exported entry point rather than carveFromPool so the
// test covers what the registry can actually reach. A test written against the
// unexported helper would pass while the registry called something else, which
// is the defect verbatim.
func carveChild(t *testing.T, db *pgxpool.Pool, alloc *PostgresPrefixAllocator, childKey string, prefixLen int) (string, error) {
	t.Helper()
	ctx := platformCtx()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cidr, err := alloc.CarveChildPool(ctx, tx, platformKey("ippools", carveParentPool), prefixLen, PoolCarveRecord{
		AllocationKey: childKey,
		IPFamily:      "IPv4",
		ScopeDigest:   scope.EmptyAddressSpaceDigest(),
		OwnerProject:  testPlatformProject,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit carve: %v", err)
	}
	return cidr, nil
}

// TestClaimAgainstParentAvoidsAnOperatorChildPoolsBlock is the test #66 names.
//
// A child pool's block is space that has left the parent. It must be withheld
// from every claim the parent serves, whatever address space that claim belongs
// to — which is what `purpose <> 'Claim'` says, and what recording the row as a
// Claim took away. Recorded as a Claim, the block was compared by digest, so it
// withheld itself only from claims that happened to share the universal digest;
// a `uniqueWithin: [network]` claim digests differently and was handed an
// address inside a live sub-pool.
func TestClaimAgainstParentAvoidsAnOperatorChildPoolsBlock(t *testing.T) {
	db := newMigratedPool(t)
	seedCarveParent(t, db)
	alloc := NewPostgresPrefixAllocator()

	parentKey := platformKey("ippools", carveParentPool)
	childKey := platformKey("ippools", carveChildName)

	childCIDR, err := carveChild(t, db, alloc, childKey, 28)
	if err != nil {
		t.Fatalf("carve child pool: %v", err)
	}
	// FirstFit over an empty /24, so the child takes the bottom /28. Asserted
	// rather than assumed: every address assertion below is relative to it.
	if childCIDR != "10.204.0.0/28" {
		t.Fatalf("child pool CIDR = %s, want 10.204.0.0/28", childCIDR)
	}

	class := loadClassFromDB(t, db, carveClass)
	claimCIDR, err := allocateAs(t, db, alloc, parentKey, "project-alpha", "a1", class, carveNetworkScope)
	if err != nil {
		t.Fatalf("claim against parent: %v", err)
	}

	child := mustParseCIDR(t, childCIDR)
	claim := mustParseCIDR(t, claimCIDR)
	if child.Contains(claim.IP) {
		t.Fatalf("claim got %s, inside child pool %s: a child pool's block is space that has "+
			"left the parent and must be withheld from every address space the parent serves, "+
			"not only from the one the carve happened to be recorded under", claimCIDR, childCIDR)
	}

	// Positive control on the mechanism, not merely on the outcome. The claim
	// must land at the first address ABOVE the child's block — that is the
	// search having seen the carve. A claim placed anywhere else would satisfy
	// the containment check for some other reason.
	if claimCIDR != "10.204.0.16/32" {
		t.Errorf("claim = %s, want 10.204.0.16/32: the first free address after the child's /28",
			claimCIDR)
	}

	// And the row itself. The containment check above passes for any
	// implementation that happens to place the two apart; this is the property
	// the fix is actually about, and it is what the pool delete guard and the
	// lease sweeper's index predicate read.
	var purpose string
	if err := db.QueryRow(platformCtx(),
		`SELECT purpose FROM ipam_cidr_allocations WHERE allocation_key = $1`, childKey).Scan(&purpose); err != nil {
		t.Fatalf("read carve row: %v", err)
	}
	if purpose != string(ipamv1alpha1.PurposePoolCarve) {
		t.Errorf("carve purpose = %q, want %q: a child pool's block is not a claim, and the "+
			"search skips it by purpose rather than matching it by digest",
			purpose, ipamv1alpha1.PurposePoolCarve)
	}
}

// TestOperatorChildPoolAvoidsClaimsInEveryAddressSpace is the same defect from
// the other side, and the half that is not visible in the purpose column.
//
// AllocatePrefix searches one address space because that is what a claim needs.
// A carve needs the opposite: a sub-pool is real space, not a view of it, so it
// must avoid every block the parent holds. Carving through AllocatePrefix under
// the universal digest hid every `uniqueWithin: [network]` claim from the
// search, and the child pool was placed on top of addresses already in use —
// with the exclusion constraint silent, because the two rows differ in
// scope_digest.
//
// Ordering is what separates this from the test above: here the claim exists
// first, so only the carve's own search can prevent the overlap.
func TestOperatorChildPoolAvoidsClaimsInEveryAddressSpace(t *testing.T) {
	db := newMigratedPool(t)
	seedCarveParent(t, db)
	alloc := NewPostgresPrefixAllocator()

	parentKey := platformKey("ippools", carveParentPool)
	class := loadClassFromDB(t, db, carveClass)

	claimCIDR, err := allocateAs(t, db, alloc, parentKey, "project-alpha", "a1", class, carveNetworkScope)
	if err != nil {
		t.Fatalf("claim against parent: %v", err)
	}
	if claimCIDR != "10.204.0.0/32" {
		t.Fatalf("claim = %s, want 10.204.0.0/32 in an empty pool", claimCIDR)
	}

	childCIDR, err := carveChild(t, db, alloc, platformKey("ippools", carveChildName), 28)
	if err != nil {
		t.Fatalf("carve child pool: %v", err)
	}

	child := mustParseCIDR(t, childCIDR)
	claim := mustParseCIDR(t, claimCIDR)
	if child.Contains(claim.IP) {
		t.Fatalf("child pool %s swallowed live claim %s: a carve must search every allocation "+
			"the parent holds, not the one address space it is recorded under",
			childCIDR, claimCIDR)
	}

	// Positive control: the carve has to be pushed to the next aligned /28, not
	// merely somewhere else. 10.204.0.0/28 is the block it would take if the
	// claim were invisible to it.
	if childCIDR != "10.204.0.16/28" {
		t.Errorf("child pool = %s, want 10.204.0.16/28: the first aligned /28 clear of the claim",
			childCIDR)
	}
}
