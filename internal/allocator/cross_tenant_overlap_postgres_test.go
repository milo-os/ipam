package allocator

// Cross-tenant overlapping root pools (#100).
//
// #87 refuses a root pool overlapping another root OF THE SAME TENANT, at
// create time. That is deliberate and it is same-tenant only, because private
// space is tenant-scoped: two tenants both using 10.0.0.0/8 is normal and
// correct. So cross-tenant overlap is not "a case to add later" — it is the
// only remaining shape of the hazard, and #87's guard actively refuses the
// shape a same-tenant test would use.
//
// The question is whether two tenants' overlapping roots can hand the same
// address to unrelated claims, and it is MEASURED here rather than read off
// internal/scope. The tenancy model rests on that claim, and #64 was a
// regression in exactly this mechanism: `uniqueWithin: []` had meant "one space
// per tenant", and two projects were measured holding the same /32 out of one
// pool before it was fixed.
//
// # There are two mechanisms, not one, and they cover different shapes
//
// It is tempting to say "the tenant is in the digest, so tenants are separate".
// That is true of ONE of the two ways a tenant reaches a pool, and the
// distinction decides which test covers what:
//
//	OPERATOR-AUTHORED ROOTS — separation is by CONSENT, not by digest.
//	  DiscoverPool only considers pools whose key matches the class's backing
//	  projects (backingKeyPatterns). Two tenants with their own roots are served
//	  from different pools entirely, and the digest never enters into it.
//
//	CASCADE-PROVISIONED POOLS — separation is by PoolDigest's tenant field.
//	  Two tenants claiming one class in one scope must derive different pool
//	  identities or they would share a pool. That is the #55/#64 mechanism and
//	  is tested by TestFlatClassIsOneSpaceAcrossTenants and friends.
//
// Both are tier-2 in #91's sense — enforced by application code alone, with
// nothing in the schema behind them. The exclusion constraint cannot help with
// either: it keys on `pool_key WITH =`, so two rows in DIFFERENT pools are
// never compared at all.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// overlapCIDR is used by BOTH tenants' root pools. Identical on purpose: this
// is the configuration #87 refuses within one tenant and permits across two.
const overlapCIDR = "10.250.0.0/24"

// TestTwoTenantsWithOverlappingRootsAreServedFromTheirOwnPool is the primary
// measurement.
//
// Expected: both tenants get the SAME address, from DIFFERENT pools, and that
// is correct rather than a collision — 10.250.0.0/32 in project-alpha's own
// /24 is a different address from 10.250.0.0/32 in project-beta's, because they
// are different address spaces belonging to different tenants.
//
// The failure this would catch is one tenant being served out of the OTHER
// tenant's pool, which is address-space theft rather than legitimate reuse. The
// assertion is therefore on the pool each allocation came from, not merely on
// the addresses being equal.
func TestTwoTenantsWithOverlappingRootsAreServedFromTheirOwnPool(t *testing.T) {
	db := newMigratedPool(t)

	// One class per tenant, each consenting ONLY to that tenant's project.
	alphaClass := seedFlatClass(t, db, "xt-alpha-v4", []string{"project-alpha"})
	betaClass := seedFlatClass(t, db, "xt-beta-v4", []string{"project-beta"})

	alphaPool := seedPoolIn(t, db, "project-alpha", "xt-alpha-root", overlapCIDR, "xt-alpha-v4")
	betaPool := seedPoolIn(t, db, "project-beta", "xt-beta-root", overlapCIDR, "xt-beta-v4")

	// Positive control on the premise: the two pools really do overlap. If a
	// future edit made them disjoint, every assertion below would pass while
	// testing nothing about overlap.
	if alphaPool == betaPool {
		t.Fatal("the two pools have the same key; they must be distinct objects in distinct projects")
	}
	assertSameCIDR(t, db, alphaPool, betaPool)

	gotAlpha := discoverFor(t, db, alphaClass)
	gotBeta := discoverFor(t, db, betaClass)

	if gotAlpha != alphaPool {
		t.Errorf("project-alpha's class resolved to %q, want its own pool %q", gotAlpha, alphaPool)
	}
	if gotBeta != betaPool {
		t.Errorf("project-beta's class resolved to %q, want its own pool %q", gotBeta, betaPool)
	}

	// And the allocations themselves: same address, different pools.
	alloc := NewPostgresPrefixAllocator()
	alphaCIDR, err := allocateAs(t, db, alloc, alphaPool, "project-alpha", "a1", alphaClass, nil)
	if err != nil {
		t.Fatalf("project-alpha allocate: %v", err)
	}
	betaCIDR, err := allocateAs(t, db, alloc, betaPool, "project-beta", "b1", betaClass, nil)
	if err != nil {
		t.Fatalf("project-beta allocate: %v", err)
	}

	if alphaCIDR != betaCIDR {
		t.Errorf("addresses = (%s, %s); two tenants drawing the first address from their own "+
			"overlapping /24s should BOTH get the pool's first address. Different addresses "+
			"would mean the two pools are somehow sharing state", alphaCIDR, betaCIDR)
	}

	// The rows must be in different pools. Same address in one pool would be
	// the collision; same address in two pools is the model working.
	var pools int
	if err := db.QueryRow(platformCtx(),
		`SELECT count(DISTINCT pool_key) FROM ipam_cidr_allocations
		  WHERE allocated_cidr = $1::inet AND purpose = 'Claim'`, alphaCIDR).Scan(&pools); err != nil {
		t.Fatalf("count pools holding %s: %v", alphaCIDR, err)
	}
	if pools != 2 {
		t.Errorf("distinct pools holding %s = %d, want 2: each tenant holds it in its own pool",
			alphaCIDR, pools)
	}
}

// TestATenantIsNeverServedFromANonConsentingOverlappingPool is the failure the
// test above is really guarding against, isolated.
//
// The two pools overlap and offer classes of the same shape, so the ONLY thing
// keeping project-alpha's claim out of project-beta's pool is the class's
// backing consent. Nothing in the database enforces it: the exclusion
// constraint keys on pool_key, so it never compares rows across pools at all.
//
// This is #91 tier 2 — application code alone — protecting the property the
// service exists to provide.
//
// # Falsifying this test requires breaking TWO things, and that is a finding
//
// Consent is enforced twice on the DiscoverPool path: once in SQL, by
// `obj.key LIKE ANY ($2::text[])` in offeringPools, and once in Go, by the
// prefix loop in offerEligible. Measured: removing EITHER one alone leaves this
// test passing, because the other still refuses the pool. Removing both makes
// it fail, reporting the beta pool by name.
//
// So the redundancy is real defence in depth, and the cost is that **no test
// can catch the removal of one layer**. Anyone deleting the Go check because
// "the SQL already does it" would see a green suite — and would be wrong,
// because the batched count path in OfferingPoolCounts has no SQL key filter at
// all and relies on the Go check alone (see offerEligible's own comment).
//
// Stated here rather than left to be rediscovered: if you are verifying a
// change to consent, break both layers or you have verified nothing.
func TestATenantIsNeverServedFromANonConsentingOverlappingPool(t *testing.T) {
	db := newMigratedPool(t)

	// A class that consents ONLY to project-alpha...
	class := seedFlatClass(t, db, "xt-consent-v4", []string{"project-alpha"})
	// ...while the only pool offering it lives in project-beta.
	seedPoolIn(t, db, "project-beta", "xt-beta-only", overlapCIDR, "xt-consent-v4")

	// Positive control: the offer row exists, so a refusal below is consent
	// doing the work rather than the pool being invisible for another reason.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_pool_class_offer WHERE class_name = 'xt-consent-v4'`); n != 1 {
		t.Fatalf("offer rows = %d, want 1: the pool must be a real candidate", n)
	}

	if got, err := discover(t, db, class); err == nil {
		t.Fatalf("resolved to %q: a pool in a project the class does not consent to must not "+
			"serve it, and overlapping CIDRs make that address-space theft rather than a "+
			"harmless mistake", got)
	}
}

// TestOverlappingRootsAcrossTenantsArePermittedAtCreate records the asymmetry
// deliberately, because it is the thing someone "harmonising" the two paths
// would break.
//
// Within one tenant, overlapping roots are refused at create (#87). Across
// tenants they are permitted, and must be: private space is tenant-scoped, and
// refusing them would make it impossible for two tenants to both use 10/8.
//
// Two different mechanisms reaching compatible answers. A single rule applied
// to both would either permit same-tenant overlap (reopening #87) or forbid the
// cross-tenant case (breaking the model).
func TestOverlappingRootsAcrossTenantsArePermittedAtCreate(t *testing.T) {
	db := newMigratedPool(t)

	seedFlatClass(t, db, "xt-perm-a-v4", []string{"project-alpha"})
	seedFlatClass(t, db, "xt-perm-b-v4", []string{"project-beta"})

	// Both land. The storage layer has no opinion about overlap across pools —
	// #87's refusal lives in the IPPool registry's create path and is scoped to
	// one tenant's own roots.
	a := seedPoolIn(t, db, "project-alpha", "xt-perm-a", overlapCIDR, "xt-perm-a-v4")
	b := seedPoolIn(t, db, "project-beta", "xt-perm-b", overlapCIDR, "xt-perm-b-v4")

	assertSameCIDR(t, db, a, b)

	var roots int
	if err := db.QueryRow(platformCtx(),
		`SELECT count(*) FROM ipam_objects
		  WHERE kind = 'IPPool'
		    AND ipam_data_to_jsonb(data)->'spec'->>'cidr' = $1`, overlapCIDR).Scan(&roots); err != nil {
		t.Fatalf("count roots on %s: %v", overlapCIDR, err)
	}
	if roots != 2 {
		t.Errorf("root pools on %s = %d, want 2 — cross-tenant overlap must be permitted",
			overlapCIDR, roots)
	}
}

// discoverFor resolves the pool a class's claim would draw from.
func discoverFor(t *testing.T, db *pgxpool.Pool, class *ipamv1alpha1.IPClass) string {
	t.Helper()
	key, err := discover(t, db, class)
	if err != nil {
		t.Fatalf("discover for %s: %v", class.Name, err)
	}
	return key
}

// assertSameCIDR is the premise every assertion in this file depends on: the
// two pools really do cover the same addresses. Checked rather than assumed,
// because a fixture edit that made them disjoint would leave every test here
// passing while testing nothing about overlap.
func assertSameCIDR(t *testing.T, db *pgxpool.Pool, keyA, keyB string) {
	t.Helper()
	read := func(key string) string {
		var cidr string
		if err := db.QueryRow(platformCtx(),
			`SELECT ipam_data_to_jsonb(data)->'spec'->>'cidr' FROM ipam_objects WHERE key = $1`,
			key).Scan(&cidr); err != nil {
			t.Fatalf("read cidr of %s: %v", key, err)
		}
		return cidr
	}
	a, b := read(keyA), read(keyB)
	if a != b {
		t.Fatalf("pools do not overlap: %s has %s, %s has %s — the premise of this file is "+
			"that they cover the same addresses", keyA, a, keyB, b)
	}
}
