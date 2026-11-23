package serversideapply

import (
	"context"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

const claimNamespace = "default"

// claimApply is the rest.UpdatedObjectInfo the apiserver's apply patcher hands
// the claim registry: the new object is the manifest run through the field
// manager against whatever the store holds, an empty claim when there is none.
// With forceAllowCreate, this is the request kubectl apply --server-side and
// Flux make.
type claimApply struct {
	scheme   *runtime.Scheme
	manifest map[string]any
}

func (a claimApply) Preconditions() *metav1.Preconditions { return nil }

func (a claimApply) UpdatedObject(_ context.Context, old runtime.Object) (runtime.Object, error) {
	gvk := ipamv1alpha1.SchemeGroupVersion.WithKind("IPClaim")
	fm, err := managedfields.NewDefaultFieldManager(
		managedfields.NewDeducedTypeConverter(),
		a.scheme, a.scheme, a.scheme,
		gvk,
		schema.GroupVersion{Group: ipam.GroupName, Version: runtime.APIVersionInternal},
		"", nil,
	)
	if err != nil {
		return nil, err
	}
	return fm.Apply(old, &unstructured.Unstructured{Object: a.manifest}, "kustomize-controller", true)
}

var _ rest.UpdatedObjectInfo = claimApply{}

func claimManifest(name, className string, spec map[string]any) map[string]any {
	s := map[string]any{"className": className}
	for k, v := range spec {
		s[k] = v
	}
	return map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPClaim",
		"metadata":   map[string]any{"name": name, "namespace": claimNamespace},
		"spec":       s,
	}
}

func claimCtx() context.Context { return projectCtx(applyProject, claimNamespace) }

// applyClaim sends manifest the way the apiserver does for an apply patch.
func (r *registries) applyClaim(t *testing.T, manifest map[string]any, dryRun bool) (*ipam.IPClaim, bool, error) {
	t.Helper()
	name := manifest["metadata"].(map[string]any)["name"].(string)
	opts := &metav1.UpdateOptions{FieldManager: "kustomize-controller"}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	obj, created, err := r.claim.Update(claimCtx(), name, claimApply{scheme: r.scheme, manifest: manifest}, nil, nil, true, opts)
	if err != nil {
		return nil, created, err
	}
	return obj.(*ipam.IPClaim), created, nil
}

// strandClaim stores a claim the way the apply path did before the fix: by the
// generic store's create-on-update, with no allocation.
func (r *registries) strandClaim(t *testing.T, manifest map[string]any) {
	t.Helper()
	name := manifest["metadata"].(map[string]any)["name"].(string)
	if _, _, err := r.claim.IPClaimStorage.Update(claimCtx(), name, claimApply{scheme: r.scheme, manifest: manifest}, nil, nil, true, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("strand claim %s: %v", name, err)
	}
	c := r.storedClaim(t, name)
	if c.Status.Phase != ipam.ClaimPending || c.Status.AllocatedCIDR != "" {
		t.Fatalf("stranded claim is not stranded: %+v", c.Status)
	}
}

func (r *registries) storedClaim(t *testing.T, name string) *ipam.IPClaim {
	t.Helper()
	obj, err := r.claim.Get(claimCtx(), name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim %s: %v", name, err)
	}
	return obj.(*ipam.IPClaim)
}

// allocations counts the allocation rows bound to the claim.
func (r *registries) allocations(t *testing.T, name string) int {
	t.Helper()
	key := tenant.Identity{Name: applyProject}.ApplyPrefix("/ipam.miloapis.com/ipclaims/" + claimNamespace + "/" + name)
	var n int
	if err := r.db.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_cidr_allocations WHERE claim_key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("count allocations of %s: %v", name, err)
	}
	return n
}

func assertBound(t *testing.T, c *ipam.IPClaim, wantCIDR string) {
	t.Helper()
	if c.Status.Phase != ipam.ClaimBound {
		t.Errorf("phase = %q, want %q", c.Status.Phase, ipam.ClaimBound)
	}
	if c.Status.AllocatedCIDR != wantCIDR {
		t.Errorf("allocatedCIDR = %q, want %q", c.Status.AllocatedCIDR, wantCIDR)
	}
	if c.Status.BoundAllocationRef == nil {
		t.Error("boundAllocationRef is nil")
	}
}

// claimSetup creates the class and the pool that offers it.
func (r *registries) claimSetup(t *testing.T, classSpec map[string]any) {
	t.Helper()
	ctx := projectCtx(applyProject, metav1.NamespaceNone)
	class := classManifest("standard")
	for k, v := range classSpec {
		class["spec"].(map[string]any)[k] = v
	}
	if _, err := r.class.Create(ctx, applied(t, r.scheme, "IPClass", class), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("create class: %v", err)
	}
	if _, err := r.pool.Create(ctx, applied(t, r.scheme, "IPPool", poolManifest("us-east", "standard")), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
}

// Issue 210: a claim created by server-side apply must be bound exactly as one
// created by POST is. Before the fix it was stored at Pending with no address
// and nothing ever bound it.
func TestApplyingANewClaimBindsIt(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, nil)

	got, created, err := r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil {
		t.Fatalf("apply claim: %v", err)
	}
	if !created {
		t.Error("created = false, want true for an apply that creates the claim")
	}
	assertBound(t, got, "10.0.0.0/24")
	assertBound(t, r.storedClaim(t, "web"), "10.0.0.0/24")
	if n := r.allocations(t, "web"); n != 1 {
		t.Errorf("claim holds %d allocations, want 1", n)
	}
	if len(got.ManagedFields) == 0 {
		t.Error("applied claim has no managedFields; the apply's field ownership was lost")
	}

	// Applying the same manifest again keeps the address and allocates nothing.
	again, created, err := r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil {
		t.Fatalf("reapply claim: %v", err)
	}
	if created {
		t.Error("reapply reported created = true")
	}
	assertBound(t, again, "10.0.0.0/24")
	if n := r.allocations(t, "web"); n != 1 {
		t.Errorf("claim holds %d allocations after reapply, want 1", n)
	}
}

// The plain create path still binds, and lands where an apply would.
func TestCreatingAClaimStillBindsIt(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, nil)

	obj, err := r.claim.Create(claimCtx(), applied(t, r.scheme, "IPClaim", claimManifest("web", "standard", nil)), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create claim: %v", err)
	}
	assertBound(t, obj.(*ipam.IPClaim), "10.0.0.0/24")
}

// A server-side dry-run of an apply that would create a claim reports the
// address it would get and keeps none of it.
func TestDryRunApplyOfAClaimKeepsNothing(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, nil)

	got, _, err := r.applyClaim(t, claimManifest("web", "standard", nil), true)
	if err != nil {
		t.Fatalf("dry-run apply: %v", err)
	}
	if got.Status.AllocatedCIDR != "10.0.0.0/24" {
		t.Errorf("dry-run allocatedCIDR = %q, want the would-be 10.0.0.0/24", got.Status.AllocatedCIDR)
	}
	if _, err := r.claim.Get(claimCtx(), "web", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("dry-run stored the claim: get err = %v", err)
	}
	if n := r.allocations(t, "web"); n != 0 {
		t.Errorf("dry-run left %d allocations, want 0", n)
	}
}

// A claim naming its address gets that address through apply.
func TestApplyingAClaimForAFixedAddress(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, nil)

	got, _, err := r.applyClaim(t, claimManifest("fixed", "standard", map[string]any{"address": "10.0.5.0/24"}), false)
	if err != nil {
		t.Fatalf("apply fixed-address claim: %v", err)
	}
	assertBound(t, got, "10.0.5.0/24")

	// A second claim for the same address is refused, as a create is.
	if _, _, err := r.applyClaim(t, claimManifest("fixed-2", "standard", map[string]any{"address": "10.0.5.0/24"}), false); err == nil {
		t.Error("apply of a second claim for a held address succeeded; want it refused")
	}
	if _, err := r.claim.Get(claimCtx(), "fixed-2", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("refused claim was stored anyway: get err = %v", err)
	}
}

// An apply that cannot bind the claim is refused with the error a create gets,
// and stores nothing.
func TestApplyingAClaimThatCannotBeBoundIsRefused(t *testing.T) {
	r := newRegistries(t)

	if _, _, err := r.applyClaim(t, claimManifest("web", "missing", nil), false); err == nil {
		t.Fatal("apply against a missing class succeeded; want it refused")
	}
	if _, err := r.claim.Get(claimCtx(), "web", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("refused claim was stored anyway: get err = %v", err)
	}
}

// A claim recreated by apply under the name of one whose address was retained
// takes that address back, as a create does.
func TestApplyingAClaimReclaimsItsRetainedAddress(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, map[string]any{"reclaimPolicy": "Retain"})

	// Hold the first /24 elsewhere, so the reclaimed address is not also the
	// one a fresh allocation would have picked.
	if _, _, err := r.applyClaim(t, claimManifest("other", "standard", nil), false); err != nil {
		t.Fatalf("apply other claim: %v", err)
	}
	first, _, err := r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil {
		t.Fatalf("apply claim: %v", err)
	}
	if _, _, err := r.claim.Delete(claimCtx(), "web", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete claim: %v", err)
	}
	if _, _, err := r.claim.Delete(claimCtx(), "other", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete other claim: %v", err)
	}

	again, created, err := r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil || !created {
		t.Fatalf("reapply after delete: created=%v err=%v", created, err)
	}
	assertBound(t, again, first.Status.AllocatedCIDR)
}

// A claim stored unbound by the old apply path is bound by its next apply.
func TestReapplyingAStrandedClaimBindsIt(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, nil)
	r.strandClaim(t, claimManifest("web", "standard", nil))
	uid := r.storedClaim(t, "web").UID

	// A dry-run binds nothing.
	if _, _, err := r.applyClaim(t, claimManifest("web", "standard", nil), true); err != nil {
		t.Fatalf("dry-run reapply: %v", err)
	}
	if n := r.allocations(t, "web"); n != 0 {
		t.Fatalf("dry-run bound the stranded claim (%d allocations)", n)
	}

	got, created, err := r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil {
		t.Fatalf("reapply stranded claim: %v", err)
	}
	if created {
		t.Error("reapply of a stored claim reported created = true")
	}
	assertBound(t, got, "10.0.0.0/24")
	stored := r.storedClaim(t, "web")
	assertBound(t, stored, "10.0.0.0/24")
	if stored.UID != uid {
		t.Errorf("binding replaced the claim: uid %s -> %s", uid, stored.UID)
	}
	if got.ResourceVersion != stored.ResourceVersion {
		t.Errorf("response resourceVersion %s does not match the stored %s", got.ResourceVersion, stored.ResourceVersion)
	}
	if n := r.allocations(t, "web"); n != 1 {
		t.Errorf("claim holds %d allocations, want 1", n)
	}
}

// A stranded claim that still cannot be bound says why in its Bound condition,
// and is bound once the cause is fixed.
func TestAStrandedClaimReportsWhyItIsNotBound(t *testing.T) {
	r := newRegistries(t)
	r.strandClaim(t, claimManifest("web", "standard", nil))

	got, _, err := r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil {
		t.Fatalf("reapply stranded claim: %v", err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, "Bound")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ClassNotFound" {
		t.Fatalf("Bound condition = %+v, want False/ClassNotFound", c)
	}
	if got.Status.Phase != ipam.ClaimPending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}

	// Reapplying while the cause stands does not rewrite the claim each time.
	// The first reapply after the condition is written may store the row once
	// more: a row written outside the generic store embeds a different
	// resourceVersion, so the store's no-op check sees a change.
	if _, _, err := r.applyClaim(t, claimManifest("web", "standard", nil), false); err != nil {
		t.Fatalf("second reapply: %v", err)
	}
	rv := r.storedClaim(t, "web").ResourceVersion
	if _, _, err := r.applyClaim(t, claimManifest("web", "standard", nil), false); err != nil {
		t.Fatalf("third reapply: %v", err)
	}
	if now := r.storedClaim(t, "web").ResourceVersion; now != rv {
		t.Errorf("unchanged failure rewrote the claim: resourceVersion %s -> %s", rv, now)
	}

	r.claimSetup(t, nil)
	got, _, err = r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil {
		t.Fatalf("reapply once the class exists: %v", err)
	}
	assertBound(t, got, "10.0.0.0/24")
}

// Two reapplies of a stranded claim racing each other bind it once.
func TestConcurrentReappliesOfAStrandedClaimBindOnce(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, nil)
	r.strandClaim(t, claimManifest("web", "standard", nil))

	const racers = 2
	var wg sync.WaitGroup
	results := make([]*ipam.IPClaim, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], _, errs[i] = r.applyClaim(t, claimManifest("web", "standard", nil), false)
		}()
	}
	close(start)
	wg.Wait()

	for i := range racers {
		if errs[i] != nil {
			t.Errorf("reapply %d: %v", i, errs[i])
			continue
		}
		if results[i].Status.AllocatedCIDR != "10.0.0.0/24" {
			t.Errorf("reapply %d returned allocatedCIDR %q, want 10.0.0.0/24", i, results[i].Status.AllocatedCIDR)
		}
	}
	if n := r.allocations(t, "web"); n != 1 {
		t.Errorf("claim holds %d allocations after %d racing reapplies, want 1", n, racers)
	}
	assertBound(t, r.storedClaim(t, "web"), "10.0.0.0/24")
}

// A stranded claim whose name holds a retained address takes that address
// back when it is bound, as a create under that name does.
func TestHealingAStrandedClaimReclaimsItsRetainedAddress(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, map[string]any{"reclaimPolicy": "Retain"})

	if _, _, err := r.applyClaim(t, claimManifest("other", "standard", nil), false); err != nil {
		t.Fatalf("apply other claim: %v", err)
	}
	first, _, err := r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil {
		t.Fatalf("apply claim: %v", err)
	}
	for _, name := range []string{"web", "other"} {
		if _, _, err := r.claim.Delete(claimCtx(), name, nil, &metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}

	r.strandClaim(t, claimManifest("web", "standard", nil))
	got, _, err := r.applyClaim(t, claimManifest("web", "standard", nil), false)
	if err != nil {
		t.Fatalf("reapply stranded claim: %v", err)
	}
	assertBound(t, got, first.Status.AllocatedCIDR)
}

// A stranded claim naming its address is bound to that address.
func TestHealingAStrandedFixedAddressClaim(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, nil)

	manifest := claimManifest("fixed", "standard", map[string]any{"address": "10.0.5.0/24"})
	r.strandClaim(t, manifest)
	got, _, err := r.applyClaim(t, manifest, false)
	if err != nil {
		t.Fatalf("reapply stranded claim: %v", err)
	}
	assertBound(t, got, "10.0.5.0/24")
}

// The create check the apiserver passes (authorization and admission, quota
// among them) runs exactly once when an apply creates a claim.
func TestApplyingANewClaimRunsTheCreateCheckOnce(t *testing.T) {
	r := newRegistries(t)
	r.claimSetup(t, nil)

	calls := 0
	check := func(context.Context, runtime.Object) error {
		calls++
		return nil
	}
	if _, _, err := r.claim.Update(claimCtx(), "web", claimApply{scheme: r.scheme, manifest: claimManifest("web", "standard", nil)}, check, nil, true, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("apply claim: %v", err)
	}
	if calls != 1 {
		t.Errorf("create check ran %d times, want 1", calls)
	}
}
