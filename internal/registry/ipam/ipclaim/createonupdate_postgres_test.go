package ipclaim

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
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

// strandAt stores claim the way the apply path did before the fix: by the
// generic store's create-on-update, with no allocation.
func strandAt(t *testing.T, r *AllocatingREST, claim *ipam.IPClaim) *ipam.IPClaim {
	t.Helper()
	obj, _, err := r.IPClaimStorage.Update(claimCtx(testProject), claim.Name, rest.DefaultUpdatedObjectInfo(claim.DeepCopy()), nil, nil, true, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("strand claim %s: %v", claim.Name, err)
	}
	stranded := obj.(*ipam.IPClaim)
	if !isStranded(stranded) {
		t.Fatalf("stranded claim is not stranded: %+v", stranded.Status)
	}
	return stranded
}

func getClaim(t *testing.T, r *AllocatingREST, name string) *ipam.IPClaim {
	t.Helper()
	obj, err := r.Get(claimCtx(testProject), name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim %s: %v", name, err)
	}
	return obj.(*ipam.IPClaim)
}

// A dry-run update of a stranded scope-range claim places nothing: no pool is
// provisioned and the claim stays as it was.
func TestADryRunUpdateOfAStrandedScopeRangeClaimPlacesNothing(t *testing.T) {
	r, db := newTenantREST(t)
	stranded := strandAt(t, r, rangeClaim("vpc-a", "net-a"))

	upsert(t, r, stranded, true)
	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 0 {
		t.Errorf("dry-run provisioned %d pools, want 0", n)
	}
	if got := getClaim(t, r, stranded.Name); !isStranded(got) {
		t.Errorf("dry-run changed the stranded claim: %+v", got.Status)
	}
}

// A change made to a stranded claim after the update that read it, and before
// the bind writes it, survives: the bind finds the row moved on, rolls back,
// and the next update binds the claim with the change kept. Without the check
// the bind rewrote the row from the update's copy and dropped the change.
func TestABindDoesNotOverwriteAChangeMadeDuringIt(t *testing.T) {
	r, _, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	ctx := claimCtx(testProject)
	stranded := strandAt(t, r, retainClaim("web", ""))

	changed := false
	beforeBind = func(ctx context.Context, _ *ipam.IPClaim) {
		if changed {
			return
		}
		changed = true
		current := getClaim(t, r, "web")
		current.Labels = map[string]string{"team": "a"}
		if _, _, err := r.IPClaimStorage.Update(ctx, "web", rest.DefaultUpdatedObjectInfo(current), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
			t.Errorf("concurrent label change: %v", err)
		}
	}
	t.Cleanup(func() { beforeBind = nil })

	if _, _, err := r.Update(ctx, "web", rest.DefaultUpdatedObjectInfo(stranded.DeepCopy()), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update stranded claim: %v", err)
	}
	if !changed {
		t.Fatal("the bind never ran")
	}
	after := getClaim(t, r, "web")
	if after.Labels["team"] != "a" {
		t.Fatalf("the label set during the bind was lost: labels = %v", after.Labels)
	}
	if !isStranded(after) {
		t.Fatalf("the bind wrote over a row that had moved on: %+v", after.Status)
	}

	got, _, err := r.Update(ctx, "web", rest.DefaultUpdatedObjectInfo(after.DeepCopy()), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	bound := got.(*ipam.IPClaim)
	if bound.Status.Phase != ipam.ClaimBound || bound.Labels["team"] != "a" {
		t.Errorf("second update: phase = %q labels = %v, want Bound with the label kept", bound.Status.Phase, bound.Labels)
	}
}
