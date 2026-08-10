package ippool

// Overlapping root pools are refused; nested child pools are not.
//
// Both halves matter, and the second is the one that catches the mistake this
// fix was written to avoid. Dropping pool_key from the database's overlap
// EXCLUDE is the intuitive way to close #87 and it destroys the cascade:
// measured against real data it would have rejected 104 existing legitimate row
// pairs — a child pool's allocations sit *inside* the carve that created it — to
// catch 3 genuinely-wrong ones. A test that only asserted the refusal would pass
// against that fix and the cascade would break in production.
//
// These exercise the overlap decision itself. The SQL that gathers a tenant's
// existing roots is covered by the e2e suite, which is where a query belongs.

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// The overlap predicate itself, on the shapes that matter for root pools.
func TestRootOverlapPredicate(t *testing.T) {
	cases := []struct {
		name     string
		a, b     string
		overlaps bool
	}{
		{"identical — the pair that exists on the dev cluster today", "fd00::/20", "fd00::/20", true},
		{"contained, shared base — the pair that handed out duplicates", "10.171.0.0/16", "10.171.0.0/24", true},
		{"contained, offset base", "10.171.0.0/16", "10.171.5.0/24", true},
		{"container stated second", "10.171.5.0/24", "10.171.0.0/16", true},
		{"adjacent but disjoint", "10.171.0.0/24", "10.171.1.0/24", false},
		{"unrelated v4", "10.0.0.0/8", "192.168.0.0/16", false},
		{"unrelated v6", "fd00::/20", "fd20::/20", false},
		// A v4 pool and a v6 pool cannot collide, and must not be refused as if
		// they could — dual-stack is two pools by design.
		{"different families never overlap", "10.0.0.0/8", "fd00::/20", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := allocation.CIDRsOverlap(mustCIDR(t, tc.a), mustCIDR(t, tc.b))
			if got != tc.overlaps {
				t.Errorf("CIDRsOverlap(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.overlaps)
			}
		})
	}
}

// A child pool must never be considered, whatever its range. This is the half
// that fails against a fix reaching for the EXCLUDE without a purpose
// discriminator: a child's CIDR is carved *from* its parent, so it overlaps by
// construction and always would.
func TestChildPoolIsExemptFromRootOverlap(t *testing.T) {
	r := &AllocatingIPPoolREST{}

	child := &ipam.IPPool{
		Spec: ipam.IPPoolSpec{
			// Deliberately identical to a range a parent would hold. If the check
			// looked at children at all, this is what it would reject.
			CIDR:          "10.171.0.0/24",
			ParentPoolRef: &ipam.LocalRef{Name: "some-parent"},
		},
	}
	// A nil db would panic if the check ever reached the query, so reaching the
	// end without one proves the exemption short-circuits before any lookup.
	if err := r.validateNoRootOverlap(t.Context(), child, tenant.Identity{Name: "proj-1"}); err != nil {
		t.Fatalf("a child pool must be exempt from the root-overlap check, got: %v", err)
	}
}

// A pool with no CIDR is a cascade-shaped or malformed object; the check has
// nothing to compare and must not invent a refusal.
func TestPoolWithoutCIDRIsExempt(t *testing.T) {
	r := &AllocatingIPPoolREST{}
	pool := &ipam.IPPool{Spec: ipam.IPPoolSpec{}}
	if err := r.validateNoRootOverlap(t.Context(), pool, tenant.Identity{Name: "proj-1"}); err != nil {
		t.Fatalf("a pool with no CIDR must be exempt, got: %v", err)
	}
}

// The refusal has to be actionable: a 409 that names the pool it collided with
// and both ranges. "Conflict" with no subject sends the operator to the
// database.
func TestOverlapConflictNamesTheOtherPool(t *testing.T) {
	err := overlapConflict("new-pool", "10.171.0.0/24",
		rootPool{name: "plat-ula-infra", cidr: mustCIDR(t, "10.171.0.0/16")})

	if !apierrors.IsConflict(err) {
		t.Fatalf("expected 409 Conflict, got %#v", err)
	}
	msg := err.Error()
	for _, want := range []string{"plat-ula-infra", "10.171.0.0/24", "10.171.0.0/16", "parentPoolRef"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q, so the operator cannot act on it:\n%s", want, msg)
		}
	}
}
