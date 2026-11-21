package ipclaim

import (
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

func subnetRangeClaim(name, network, location, address string) *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ipam.IPClaimSpec{
			ClassName: "tenant-subnet",
			Target:    ipam.TargetScopeRange,
			Address:   address,
			Scope: map[string]ipam.ScopeRef{
				"network":  networkRef(network),
				"location": locationRef(location),
			},
		},
	}
}

// The NSO flow with the chain placed where the caller names it: the network's
// range, then a range per network and location inside it, then an endpoint
// address inside that, each bound exactly.
func TestAnAddressedRangePlacesTheChainAndAnEndpointBindsInsideIt(t *testing.T) {
	rest, db := newTenantREST(t)

	vpc := rangeClaim("vpc-a", "net-a")
	vpc.Spec.Address = "fd20:1000:a::/48"
	if got := create(t, rest, vpc).Status.AllocatedCIDR; got != "fd20:1000:a::/48" {
		t.Fatalf("network range = %s, want fd20:1000:a::/48", got)
	}

	subnet := create(t, rest, subnetRangeClaim("subnet-a1", "net-a", "loc-1", "fd20:1000:a:10::/64"))
	if subnet.Status.AllocatedCIDR != "fd20:1000:a:10::/64" {
		t.Fatalf("subnet range = %s, want fd20:1000:a:10::/64", subnet.Status.AllocatedCIDR)
	}
	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 1 {
		t.Errorf("%d tenant-vpc pools, want the one the network range placed", n)
	}

	ep := endpointClaim("ep-a1", "net-a", "loc-1")
	ep.Spec.Address = "fd20:1000:a:10:0:5::/96"
	got := create(t, rest, ep)
	if got.Status.AllocatedCIDR != "fd20:1000:a:10:0:5::/96" {
		t.Errorf("endpoint = %s, want fd20:1000:a:10:0:5::/96", got.Status.AllocatedCIDR)
	}
	if got.Status.PoolRef == nil || got.Status.PoolRef.Name != subnet.Status.PoolRef.Name {
		t.Errorf("endpoint drew from %v, want the placed subnet %s", got.Status.PoolRef, subnet.Status.PoolRef.Name)
	}
}

func TestAnAddressedRangeIsRefusedWithAReason(t *testing.T) {
	rest, db := newTenantREST(t)
	vpc := rangeClaim("vpc-a", "net-a")
	vpc.Spec.Address = "fd20:1000:a::/48"
	create(t, rest, vpc)

	withAddress := func(c *ipam.IPClaim, address string) *ipam.IPClaim {
		c.Spec.Address = address
		return c
	}
	tests := []struct {
		name       string
		claim      *ipam.IPClaim
		wantCode   int32
		wantReason ipamerrors.Reason
	}{
		{"outside the network's existing range", subnetRangeClaim("s-out", "net-a", "loc-2", "fd20:1000:b:10::/64"),
			http.StatusBadRequest, ipamerrors.ReasonAddressOutsidePool},
		{"the network's range moved", withAddress(rangeClaim("vpc-moved", "net-a"), "fd20:1000:d::/48"),
			http.StatusBadRequest, ipamerrors.ReasonAddressOutsidePool},
		{"carved to another network", withAddress(rangeClaim("vpc-c", "net-c"), "fd20:1000:a::/48"),
			http.StatusConflict, ipamerrors.ReasonAddressInUse},
		{"unaligned", withAddress(rangeClaim("vpc-u", "net-u"), "fd20:1000:e:1::/48"),
			http.StatusBadRequest, ipamerrors.ReasonAddressUnaligned},
		{"not the class's size", withAddress(rangeClaim("vpc-l", "net-l"), "fd20:1000:e::/56"),
			http.StatusBadRequest, ipamerrors.ReasonPrefixLengthRejected},
		{"wrong family", withAddress(rangeClaim("vpc-f", "net-f"), "10.0.0.0/16"),
			http.StatusBadRequest, ipamerrors.ReasonFamilyMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := countRows(t, db, `SELECT COUNT(*) FROM ipam_cidr_allocations`)
			pools := countRows(t, db, `SELECT COUNT(*) FROM ipam_objects WHERE kind = 'IPPool'`)
			_, err := rest.Create(claimCtx(testProject), tc.claim, nil, &metav1.CreateOptions{})
			if err == nil {
				t.Fatal("Create succeeded")
			}
			status, ok := err.(apierrors.APIStatus)
			if !ok || status.Status().Code != tc.wantCode {
				t.Fatalf("error %v, want HTTP %d", err, tc.wantCode)
			}
			if got := ipamerrors.ReasonFor(err); got != tc.wantReason {
				t.Errorf("reason = %q, want %q (%v)", got, tc.wantReason, err)
			}
			if after := countRows(t, db, `SELECT COUNT(*) FROM ipam_cidr_allocations`); after != before {
				t.Errorf("refusal wrote %d allocation rows", after-before)
			}
			if after := countRows(t, db, `SELECT COUNT(*) FROM ipam_objects WHERE kind = 'IPPool'`); after != pools {
				t.Errorf("refusal left %d pools", after-pools)
			}
			if n := countRows(t, db, `SELECT COUNT(*) FROM ipam_objects WHERE kind = 'IPClaim' AND name = $1`, tc.claim.Name); n != 0 {
				t.Errorf("refused claim %q was stored", tc.claim.Name)
			}
		})
	}
}

// Dry-run provisions nothing, for a range claim with or without an address and
// for a block claim whose chain does not exist yet.
func TestADryRunProvisionsNoPools(t *testing.T) {
	rest, db := newTenantREST(t)
	dry := &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}

	claims := []*ipam.IPClaim{
		subnetRangeClaim("dry-addressed", "net-d", "loc-1", "fd20:1000:f:10::/64"),
		rangeClaim("dry-range", "net-e"),
		endpointClaim("dry-endpoint", "net-f", "loc-1"),
	}
	for i, c := range claims {
		obj, err := rest.Create(claimCtx(testProject), c, nil, dry)
		if err != nil {
			t.Fatalf("dry-run %s: %v", c.Name, err)
		}
		got := obj.(*ipam.IPClaim)
		if got.Status.Phase != ipam.ClaimBound || got.Status.AllocatedCIDR == "" {
			t.Errorf("dry-run %s reported %q %q, want it bound", c.Name, got.Status.Phase, got.Status.AllocatedCIDR)
		}
		if i == 0 && got.Status.AllocatedCIDR != "fd20:1000:f:10::/64" {
			t.Errorf("addressed dry-run reported %s, want fd20:1000:f:10::/64", got.Status.AllocatedCIDR)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM ipam_objects WHERE kind <> 'IPClass' AND key NOT LIKE '%tenant-root'`); n != 0 {
		t.Errorf("dry-runs left %d objects behind", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM ipam_cidr_allocations`); n != 0 {
		t.Errorf("dry-runs left %d allocation rows", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM ipam_pool_identity`); n != 0 {
		t.Errorf("dry-runs left %d pool identities", n)
	}
}
