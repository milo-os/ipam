package ipclaim

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// upsert sends claim the way the apiserver sends a server-side apply: an
// update that may create.
func upsert(t *testing.T, r *AllocatingREST, claim *ipam.IPClaim, dryRun bool) (*ipam.IPClaim, bool) {
	t.Helper()
	opts := &metav1.UpdateOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	obj, created, err := r.Update(claimCtx(testProject), claim.Name, rest.DefaultUpdatedObjectInfo(claim.DeepCopy()), nil, nil, true, opts)
	if err != nil {
		t.Fatalf("update %s: %v", claim.Name, err)
	}
	return obj.(*ipam.IPClaim), created
}

// A scope-range claim created by an update that may create is placed exactly
// as one created by POST is, and a dry-run of it places nothing.
func TestAScopeRangeClaimCreatedByApplyIsPlaced(t *testing.T) {
	r, db := newTenantREST(t)

	dry, _ := upsert(t, r, rangeClaim("vpc-a", "net-a"), true)
	if dry.Status.AllocatedCIDR == "" {
		t.Error("dry-run reported no range")
	}
	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 0 {
		t.Fatalf("dry-run provisioned %d pools, want 0", n)
	}

	got, created := upsert(t, r, rangeClaim("vpc-a", "net-a"), false)
	if !created {
		t.Error("created = false, want true")
	}
	if got.Status.Phase != ipam.ClaimBound || got.Status.AllocatedCIDR == "" {
		t.Fatalf("range claim status = %+v, want Bound with a range", got.Status)
	}
	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 1 {
		t.Errorf("tenant-vpc provisioned %d pools, want 1", n)
	}
}

// A scope-range claim stored unplaced by the old apply path is placed by its
// next update, keeping its identity.
func TestAStrandedScopeRangeClaimIsPlacedOnUpdate(t *testing.T) {
	r, db := newTenantREST(t)

	claim := rangeClaim("vpc-a", "net-a")
	if _, _, err := r.IPClaimStorage.Update(claimCtx(testProject), claim.Name, rest.DefaultUpdatedObjectInfo(claim.DeepCopy()), nil, nil, true, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("strand range claim: %v", err)
	}
	obj, err := r.Get(claimCtx(testProject), claim.Name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get stranded claim: %v", err)
	}
	stranded := obj.(*ipam.IPClaim)
	if stranded.Status.AllocatedCIDR != "" {
		t.Fatalf("stranded claim already holds %s", stranded.Status.AllocatedCIDR)
	}

	got, created := upsert(t, r, stranded, false)
	if created {
		t.Error("update of a stored claim reported created = true")
	}
	if got.Status.Phase != ipam.ClaimBound || got.Status.AllocatedCIDR == "" {
		t.Fatalf("range claim status = %+v, want Bound with a range", got.Status)
	}
	if got.UID != stranded.UID {
		t.Errorf("placing replaced the claim: uid %s -> %s", stranded.UID, got.UID)
	}
	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 1 {
		t.Errorf("tenant-vpc provisioned %d pools, want 1", n)
	}
}
