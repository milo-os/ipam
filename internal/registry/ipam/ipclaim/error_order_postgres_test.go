package ipclaim

// A create reads whether an allocation is retained under the claim's identity
// in the same round trip as the first read of its class chain. These pin the
// order the refusals come in: the duplicate's 409 first, then the checks that
// read nothing, then the chain.

import (
	"context"
	"net/netip"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// A second create of a bound claim is ClaimExists even when the chain it was
// bound under can no longer be read: the duplicate is decided before any
// ancestry is.
func TestADuplicateIsClaimExistsWhenItsClassChainIsBroken(t *testing.T) {
	r, db := newTenantREST(t)
	create(t, r, endpointClaim("dup", "net-a", "ams"))

	if _, err := db.Exec(context.Background(), `DELETE FROM ipam_objects WHERE key = $1`,
		tenant.Identity{Name: testProject}.ResourceKey("ipclasses", "tenant-subnet")); err != nil {
		t.Fatalf("delete parent class: %v", err)
	}

	// The chain really is broken: a claim of another name is refused for it.
	if _, err := r.Create(claimCtx(testProject), endpointClaim("other", "net-a", "ams"), nil, &metav1.CreateOptions{}); err == nil ||
		ipamerrors.ReasonFor(err) == ipamerrors.ReasonClaimExists {
		t.Fatalf("a new claim under a chain missing its parent returned %v, want a refusal that is not ClaimExists", err)
	}

	_, err := r.Create(claimCtx(testProject), endpointClaim("dup", "net-a", "ams"), nil, &metav1.CreateOptions{})
	if ipamerrors.ReasonFor(err) != ipamerrors.ReasonClaimExists {
		t.Fatalf("a duplicate under a broken chain returned %v, want ClaimExists", err)
	}
}

// A second create of a bound claim naming an unaligned address is ClaimExists,
// not AddressUnaligned: the address is checked after the duplicate is.
func TestADuplicateNamingAnUnalignedAddressIsClaimExists(t *testing.T) {
	r, _ := newTenantREST(t)
	bound := create(t, r, endpointClaim("dup", "net-a", "ams"))

	block := netip.MustParsePrefix(bound.Status.AllocatedCIDR)
	unaligned := block.Addr().Next().String()

	fresh := endpointClaim("fresh", "net-a", "ams")
	fresh.Spec.Address = unaligned
	if _, err := r.Create(claimCtx(testProject), fresh, nil, &metav1.CreateOptions{}); ipamerrors.ReasonFor(err) != ipamerrors.ReasonAddressUnaligned {
		t.Fatalf("a new claim naming %s returned %v, want AddressUnaligned", unaligned, err)
	}

	dup := endpointClaim("dup", "net-a", "ams")
	dup.Spec.Address = unaligned
	if _, err := r.Create(claimCtx(testProject), dup, nil, &metav1.CreateOptions{}); ipamerrors.ReasonFor(err) != ipamerrors.ReasonClaimExists {
		t.Fatalf("a duplicate naming %s returned %v, want ClaimExists", unaligned, err)
	}
}

// A claim is refused once the root pool stops offering the chain's class, even
// though every pool of its chain still exists, and nothing is allocated.
func TestAClaimIsRefusedOnceTheRootStopsOfferingItsClass(t *testing.T) {
	r, db := newTenantREST(t)
	create(t, r, endpointClaim("first", "net-a", "ams"))

	setClassNames(t, db, "tenant-root", []string{"withdrawn"})
	before := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`)

	_, err := r.Create(claimCtx(testProject), endpointClaim("second", "net-a", "ams"), nil, &metav1.CreateOptions{})
	if ipamerrors.ReasonFor(err) != ipamerrors.ReasonNoOfferingPool {
		t.Fatalf("a claim under a withdrawn root returned %v, want NoOfferingPool", err)
	}
	if after := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`); after != before {
		t.Errorf("allocation rows went from %d to %d; a refused claim must allocate nothing", before, after)
	}
}
