package allocator

// IPClass.status.offeringPools (#57).
//
// The field is what tells an operator "you created a class and nothing backs
// it" before a consumer discovers it as a failed claim. It read zero for every
// class in the service, which is not a missing signal but a wrong one — zero is
// a specific assertion, and it was being made about classes that were fully
// backed.
//
// The hard part is not counting rows. It is that only the ROOT of a class chain
// is discovered through ipam_pool_class_offer; every class below it is served by
// the cascade out of the level above, and no operator ever offers a pool to it.
// So the obvious implementation — count the offers naming this class — returns
// zero for `tenant-subnet-ipv6` and `tenant-endpoint-ipv6` while every claim
// naming them succeeds, which reproduces the defect it was written to fix on
// two classes out of three.
//
// Real Postgres because the count is a join against ipam_pool_class_offer and
// the key-pattern filter that carries the class's backing consent.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/tenant"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func countOffers(t *testing.T, db *pgxpool.Pool, names ...string) map[string]int32 {
	t.Helper()
	ctx := platformCtx()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	counts, err := OfferingPoolCounts(ctx, tx, names)
	if err != nil {
		t.Fatalf("count offering pools for %v: %v", names, err)
	}
	return counts
}

// TestOfferingPoolsCountsTheChainRoot is the property the field's own doc
// promises: zero means every claim naming this class fails.
//
// seedTenantChain is the design doc's three-deep IPv6 chain behind one
// operator-authored pool. Exactly one class is named in that pool's
// spec.classNames, so an implementation that counts offers naming the class
// itself reports 1, 0, 0 — and the two zeros are claims that work.
func TestOfferingPoolsCountsTheChainRoot(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)

	counts := countOffers(t, db, "tenant-network-ipv6", "tenant-subnet-ipv6", "tenant-endpoint-ipv6")

	for _, name := range []string{"tenant-network-ipv6", "tenant-subnet-ipv6", "tenant-endpoint-ipv6"} {
		if counts[name] != 1 {
			t.Errorf("%s: offeringPools = %d, want 1. Every class in a chain is backed by "+
				"whatever backs its root — if nothing offers the root, ResolvePool fails at its "+
				"first step and every claim naming any class in the chain fails with it. "+
				"Counting offers that name the class itself reports zero here, about a class "+
				"whose claims all succeed.", name, counts[name])
		}
	}
}

// TestOfferingPoolsIsZeroOnlyWhenNothingBacksTheChain is the other half, and
// the one the field exists for. Without it the test above passes against an
// implementation that returns a constant.
func TestOfferingPoolsIsZeroOnlyWhenNothingBacksTheChain(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)

	// A second chain root that no pool offers. Same family, same shape, no
	// backing — this is the state an operator lands in after authoring a class
	// and before authoring its pool.
	unbacked := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-network-ipv6"},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily:             ipamv1alpha1.IPv6,
			PoolPer:              []string{"network"},
			AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 48, Max: 48},
		},
	}
	seedObject(t, db, platformKey("ipclasses", unbacked.Name), "IPClass", unbacked.Name, unbacked)

	// And a child of it, so the zero has to propagate down a chain rather than
	// only being read off the class that was queried.
	orphanChild := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-endpoint-ipv6"},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily:             ipamv1alpha1.IPv6,
			ParentClassName:      "orphan-network-ipv6",
			UniqueWithin:         []string{"network"},
			AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 96, Max: 96},
		},
	}
	seedObject(t, db, platformKey("ipclasses", orphanChild.Name), "IPClass", orphanChild.Name, orphanChild)

	counts := countOffers(t, db,
		"tenant-endpoint-ipv6", "orphan-network-ipv6", "orphan-endpoint-ipv6")

	if counts["orphan-network-ipv6"] != 0 {
		t.Errorf("orphan-network-ipv6: offeringPools = %d, want 0 — no pool names it",
			counts["orphan-network-ipv6"])
	}
	if counts["orphan-endpoint-ipv6"] != 0 {
		t.Errorf("orphan-endpoint-ipv6: offeringPools = %d, want 0 — its chain root is backed "+
			"by nothing, so every claim naming it fails", counts["orphan-endpoint-ipv6"])
	}
	// The backed chain in the same call must still read 1, so neither result can
	// come from a constant.
	if counts["tenant-endpoint-ipv6"] != 1 {
		t.Errorf("tenant-endpoint-ipv6: offeringPools = %d, want 1 — a backed chain and an "+
			"unbacked one must not report the same number", counts["tenant-endpoint-ipv6"])
	}
}

// TestOfferingPoolsCountAgreesWithDiscovery is what stops the count and the
// claim path drifting.
//
// The count is only worth having if it describes the set DiscoverPool actually
// searches. Two queries against ipam_pool_class_offer would be free to disagree
// — a class reading "3 pools" whose every claim fails, or the reverse — and
// neither would error. Both now go through offeringPools, and this asserts the
// consequence rather than the refactor: the family filter is applied on both
// sides.
func TestOfferingPoolsCountAgreesWithDiscovery(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)

	// An IPv4 pool offering the IPv6 root class. Nothing forbids the offer row;
	// the family filter is what makes it uncountable and undiscoverable, and it
	// has to do both.
	wrongFamily := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "wrong-family-v4"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       "10.210.0.0/20",
			IPFamily:   ipamv1alpha1.IPv4,
			ClassNames: []string{"tenant-network-ipv6"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: "10.210.0.0/20",
			IPFamily:      ipamv1alpha1.IPv4,
		},
	}
	seedObject(t, db, platformKey("ippools", "wrong-family-v4"), "IPPool", "wrong-family-v4", wrongFamily)

	// Positive control on the fixture: the offer row really is there, so a
	// count of 1 below is the family filter working and not the row missing.
	var offers int
	if err := db.QueryRow(platformCtx(),
		`SELECT count(*) FROM ipam_pool_class_offer WHERE class_name = 'tenant-network-ipv6'`,
	).Scan(&offers); err != nil {
		t.Fatalf("count offer rows: %v", err)
	}
	if offers != 2 {
		t.Fatalf("offer rows = %d, want 2: the fixture must publish both pools to the class, "+
			"or this test proves nothing about the family filter", offers)
	}

	if got := countOffers(t, db, "tenant-network-ipv6")["tenant-network-ipv6"]; got != 1 {
		t.Errorf("offeringPools = %d, want 1: an IPv4 pool cannot back an IPv6 class, and the "+
			"count must apply the same family filter DiscoverPool does", got)
	}

	// And DiscoverPool agrees — it resolves to the IPv6 pool, not the IPv4 one
	// that also offers the class.
	ctx := platformCtx()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	class := loadClassFromDB(t, db, "tenant-network-ipv6")
	key, err := DiscoverPool(ctx, tx, class, scopeFor("net-a", "us-central-1"))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if key != platformKey("ippools", "tenant-v6") {
		t.Errorf("DiscoverPool = %s, want the IPv6 pool: the count and discovery must select "+
			"the same set", key)
	}
}

// TestOfferingPoolCountRespectsBackingConsent guards the filter that moved.
//
// The single-class lister pre-filters the pool key in SQL (`obj.key LIKE ANY`).
// The batched count for a LIST cannot: one statement serves several classes and
// consent is per class, so the key filter moved into Go, in offerEligible.
//
// That is a security-relevant filter — it is what stops a tenant creating an
// IPPool in their own project, listing a popular class name on it, and being
// counted as backing that class. Moving it from SQL to Go must not weaken it,
// and nothing else in the suite would notice if it had: every other count test
// uses pools in the platform project, which pass the filter either way.
func TestOfferingPoolCountRespectsBackingConsent(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)

	// A pool in a project the class does not name, offering the chain's root.
	//
	// Written out rather than via seedPoolIn, which hardcodes IPv4: against this
	// IPv6 chain such a pool is excluded by the FAMILY filter, so the test would
	// pass with the consent check deleted. Caught by deleting it and watching
	// this test still pass — the fixture has to leave consent as the ONLY thing
	// standing between the squatter and the count.
	squatter := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "squatter-v6"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       "fd90::/20",
			IPFamily:   ipamv1alpha1.IPv6,
			ClassNames: []string{"tenant-network-ipv6"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: "fd90::/20",
			IPFamily:      ipamv1alpha1.IPv6,
		},
	}
	seedObject(t, db, tenant.Identity{Name: "attacker-project"}.ResourceKey("ippools", "squatter-v6"),
		"IPPool", "squatter-v6", squatter)

	// Positive control on the fixture: the offer row is really there, so a count
	// of 1 below is the consent filter working rather than a missing row.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_pool_class_offer WHERE class_name = 'tenant-network-ipv6'`); n != 2 {
		t.Fatalf("offer rows = %d, want 2: the squatting pool must be a real candidate or "+
			"this test proves nothing", n)
	}

	for _, name := range []string{"tenant-network-ipv6", "tenant-endpoint-ipv6"} {
		if got := countOffers(t, db, name)[name]; got != 1 {
			t.Errorf("%s: offeringPools = %d, want 1 — a pool in a project the class does not "+
				"consent to must not be counted as backing it", name, got)
		}
	}
}
