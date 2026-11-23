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

// applyPatch is the rest.UpdatedObjectInfo the apiserver's apply patcher hands
// the registry: it computes the new object by running the manifest through the
// field manager against whatever the store holds, an empty object when the
// pool does not exist yet. Combined with forceAllowCreate, this is exactly the
// request kubectl apply --server-side and Flux make.
type applyPatch struct {
	t        *testing.T
	scheme   *runtime.Scheme
	manifest map[string]any
}

func (a applyPatch) Preconditions() *metav1.Preconditions { return nil }

func (a applyPatch) UpdatedObject(_ context.Context, old runtime.Object) (runtime.Object, error) {
	gvk := ipamv1alpha1.SchemeGroupVersion.WithKind("IPPool")
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

var _ rest.UpdatedObjectInfo = applyPatch{}

func rootPoolManifest(name, cidr string) map[string]any {
	return map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPPool",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"cidr": cidr, "ipFamily": "IPv4"},
	}
}

func childPoolManifest(name, parent string, prefixLength int64) map[string]any {
	return map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPPool",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"parentPoolRef": map[string]any{"name": parent},
			"prefixLength":  prefixLength,
		},
	}
}

// serverSideApply sends manifest the way the apiserver does for an apply patch.
func (r *registries) serverSideApply(t *testing.T, ctx context.Context, manifest map[string]any, dryRun bool) (*ipam.IPPool, bool, error) {
	t.Helper()
	name := manifest["metadata"].(map[string]any)["name"].(string)
	opts := &metav1.UpdateOptions{FieldManager: "kustomize-controller"}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	obj, created, err := r.pool.Update(ctx, name, applyPatch{t: t, scheme: r.scheme, manifest: manifest}, nil, nil, true, opts)
	if err != nil {
		return nil, created, err
	}
	return obj.(*ipam.IPPool), created, nil
}

func (r *registries) carves(t *testing.T, childName string) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = $1`,
		tenant.Identity{Name: applyProject}.ResourceKey("ippools", childName),
	).Scan(&n); err != nil {
		t.Fatalf("count carves of %s: %v", childName, err)
	}
	return n
}

func (r *registries) storedPool(t *testing.T, ctx context.Context, name string) *ipam.IPPool {
	t.Helper()
	obj, err := r.pool.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pool %s: %v", name, err)
	}
	return obj.(*ipam.IPPool)
}

func assertCarved(t *testing.T, p *ipam.IPPool, wantCIDR string) {
	t.Helper()
	if p.Status.Phase != ipam.PoolReady {
		t.Errorf("phase = %q, want %q", p.Status.Phase, ipam.PoolReady)
	}
	if p.Status.AllocatedCIDR != wantCIDR {
		t.Errorf("allocatedCIDR = %q, want %q", p.Status.AllocatedCIDR, wantCIDR)
	}
	c := meta.FindStatusCondition(p.Status.Conditions, "Allocated")
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Allocated condition = %+v, want True", c)
	}
}

// Issue 208: a child pool created by server-side apply must be carved exactly
// as one created by POST is. Before the fix it was stored at Pending with no
// allocatedCIDR and no conditions, and nothing ever carved it.
func TestApplyingANewChildPoolCarvesIt(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, created, err := r.serverSideApply(t, ctx, rootPoolManifest("parent", "10.0.0.0/16"), false); err != nil || !created {
		t.Fatalf("apply root pool: created=%v err=%v", created, err)
	}

	got, created, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false)
	if err != nil {
		t.Fatalf("apply child pool: %v", err)
	}
	if !created {
		t.Error("created = false, want true for an apply that creates the pool")
	}
	assertCarved(t, got, "10.0.0.0/24")
	assertCarved(t, r.storedPool(t, ctx, "child"), "10.0.0.0/24")
	if n := r.carves(t, "child"); n != 1 {
		t.Errorf("child holds %d carves in its parent, want 1", n)
	}
	if len(got.ManagedFields) == 0 {
		t.Error("applied child pool has no managedFields; the apply's field ownership was lost")
	}

	// Applying the same manifest again is a no-op update, not a second carve.
	again, created, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false)
	if err != nil {
		t.Fatalf("reapply child pool: %v", err)
	}
	if created {
		t.Error("reapply reported created = true")
	}
	assertCarved(t, again, "10.0.0.0/24")
	if n := r.carves(t, "child"); n != 1 {
		t.Errorf("child holds %d carves after reapply, want 1", n)
	}
}

// The plain create path still carves, and lands where an apply would.
func TestCreatingAChildPoolStillCarvesIt(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("parent", "10.0.0.0/16"), false); err != nil {
		t.Fatalf("apply root pool: %v", err)
	}
	obj, err := r.pool.Create(ctx, applied(t, r.scheme, "IPPool", childPoolManifest("child", "parent", 24)), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create child pool: %v", err)
	}
	assertCarved(t, obj.(*ipam.IPPool), "10.0.0.0/24")
}

// An apply that cannot carve the pool is refused with the error a create gets,
// and leaves nothing behind.
func TestApplyingAChildPoolThatCannotBeCarvedIsRefused(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	_, _, err := r.serverSideApply(t, ctx, childPoolManifest("orphan", "missing", 24), false)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("apply with a missing parent: err = %v, want 400", err)
	}
	if _, err := r.pool.Get(ctx, "orphan", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("refused child pool was stored anyway: get err = %v", err)
	}

	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("small", "10.1.0.0/24"), false); err != nil {
		t.Fatalf("apply root pool: %v", err)
	}
	_, _, err = r.serverSideApply(t, ctx, childPoolManifest("too-big", "small", 16), false)
	if code := statusCode(err); code != 507 {
		t.Fatalf("apply larger than the parent: err = %v (code %d), want 507", err, code)
	}
	if _, err := r.pool.Get(ctx, "too-big", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("refused child pool was stored anyway: get err = %v", err)
	}
}

// A server-side dry-run of an apply that would create a child pool reports
// the carve and keeps none of it.
func TestDryRunApplyOfAChildPoolKeepsNothing(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("parent", "10.0.0.0/16"), false); err != nil {
		t.Fatalf("apply root pool: %v", err)
	}
	got, _, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), true)
	if err != nil {
		t.Fatalf("dry-run apply child pool: %v", err)
	}
	if got.Status.AllocatedCIDR != "10.0.0.0/24" {
		t.Errorf("dry-run allocatedCIDR = %q, want the would-be carve 10.0.0.0/24", got.Status.AllocatedCIDR)
	}
	if _, err := r.pool.Get(ctx, "child", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("dry-run stored the child pool: get err = %v", err)
	}
	if n := r.carves(t, "child"); n != 0 {
		t.Errorf("dry-run left %d carves in the parent, want 0", n)
	}
}

// A root pool created by apply gets the same overlap check a create does.
// Before the fix the apply path skipped it.
func TestApplyingAnOverlappingRootPoolIsRefused(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("a", "10.0.0.0/16"), false); err != nil {
		t.Fatalf("apply first root pool: %v", err)
	}
	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("b", "10.0.128.0/17"), false); err == nil {
		t.Fatal("apply of an overlapping root pool succeeded; want it refused as a create is")
	}
}

// strand stores a child pool the way the apply path did before the fix: by the
// generic store's create-on-update, with no carve.
func (r *registries) strand(t *testing.T, ctx context.Context, name, parent string, prefixLength int64) {
	t.Helper()
	if _, _, err := r.pool.Store.Update(ctx, name, applyPatch{t: t, scheme: r.scheme, manifest: childPoolManifest(name, parent, prefixLength)}, nil, nil, true, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("strand child pool %s: %v", name, err)
	}
	p := r.storedPool(t, ctx, name)
	if p.Status.AllocatedCIDR != "" || p.Status.Phase != ipam.PoolPending {
		t.Fatalf("stranded pool is not stranded: %+v", p.Status)
	}
}

// A child pool stored uncarved by the old apply path is carved by its next
// apply.
func TestReapplyingAStrandedChildPoolCarvesIt(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("parent", "10.0.0.0/16"), false); err != nil {
		t.Fatalf("apply root pool: %v", err)
	}
	r.strand(t, ctx, "child", "parent", 24)

	// A dry-run changes nothing.
	if _, _, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), true); err != nil {
		t.Fatalf("dry-run reapply: %v", err)
	}
	if n := r.carves(t, "child"); n != 0 {
		t.Fatalf("dry-run carved the stranded pool (%d carves)", n)
	}

	got, created, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false)
	if err != nil {
		t.Fatalf("reapply stranded child pool: %v", err)
	}
	if created {
		t.Error("reapply of a stored pool reported created = true")
	}
	assertCarved(t, got, "10.0.0.0/24")
	assertCarved(t, r.storedPool(t, ctx, "child"), "10.0.0.0/24")
	if got.ResourceVersion != r.storedPool(t, ctx, "child").ResourceVersion {
		t.Errorf("response resourceVersion %s does not match the stored %s", got.ResourceVersion, r.storedPool(t, ctx, "child").ResourceVersion)
	}
	if n := r.carves(t, "child"); n != 1 {
		t.Errorf("child holds %d carves, want 1", n)
	}
}

// A stranded child pool that still cannot be carved says why in its Allocated
// condition instead of sitting at Pending with nothing to explain it, and is
// carved once the cause is fixed.
func TestAStrandedChildPoolReportsWhyItIsNotCarved(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	r.strand(t, ctx, "child", "parent", 24)

	got, _, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false)
	if err != nil {
		t.Fatalf("reapply stranded child pool: %v", err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, "Allocated")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ParentNotFound" {
		t.Fatalf("Allocated condition = %+v, want False/ParentNotFound", c)
	}
	if got.Status.Phase != ipam.PoolPending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}

	// Reapplying while the cause stands does not rewrite the pool each time.
	// The first reapply after the condition is written may store the row once
	// more: a row written outside the generic store embeds a different
	// resourceVersion, so the store's no-op check sees a change. Every created
	// child pool has the same one-time rewrite.
	if _, _, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false); err != nil {
		t.Fatalf("second reapply: %v", err)
	}
	rv := r.storedPool(t, ctx, "child").ResourceVersion
	if _, _, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false); err != nil {
		t.Fatalf("third reapply: %v", err)
	}
	if now := r.storedPool(t, ctx, "child").ResourceVersion; now != rv {
		t.Errorf("unchanged failure rewrote the pool: resourceVersion %s -> %s", rv, now)
	}

	// A parent too small for the child is reported as exhausted.
	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("parent", "10.0.0.0/25"), false); err != nil {
		t.Fatalf("apply small parent: %v", err)
	}
	got, _, err = r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false)
	if err != nil {
		t.Fatalf("reapply against a small parent: %v", err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Allocated"); c == nil || c.Reason != "ParentExhausted" {
		t.Fatalf("Allocated condition = %+v, want ParentExhausted", c)
	}

	// An uncarved child pool can still be deleted and applied again.
	if _, _, err := r.pool.Delete(ctx, "child", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete stranded child pool: %v", err)
	}
	if _, _, err := r.pool.Delete(ctx, "parent", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete small parent: %v", err)
	}
	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("parent", "10.0.0.0/16"), false); err != nil {
		t.Fatalf("apply parent: %v", err)
	}
	got, created, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false)
	if err != nil || !created {
		t.Fatalf("apply child after recreate: created=%v err=%v", created, err)
	}
	assertCarved(t, got, "10.0.0.0/24")
}

// createChild POSTs a child pool, as kubectl create does.
func (r *registries) createChild(t *testing.T, ctx context.Context, name, parent string, prefixLength int64, dryRun bool) (*ipam.IPPool, error) {
	t.Helper()
	opts := &metav1.CreateOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	obj, err := r.pool.Create(ctx, applied(t, r.scheme, "IPPool", childPoolManifest(name, parent, prefixLength)), nil, opts)
	if err != nil {
		return nil, err
	}
	return obj.(*ipam.IPPool), nil
}

func assertNotStored(t *testing.T, r *registries, ctx context.Context, name string) {
	t.Helper()
	if _, err := r.pool.Get(ctx, name, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("pool %s was stored: get err = %v", name, err)
	}
	if n := r.carves(t, name); n != 0 {
		t.Errorf("pool %s holds %d carves, want 0", name, n)
	}
}

func assertUncarvedForMissingParent(t *testing.T, p *ipam.IPPool) {
	t.Helper()
	if p.Status.Phase != ipam.PoolPending || p.Status.AllocatedCIDR != "" {
		t.Errorf("dry-run status = phase %q cidr %q, want Pending and no range", p.Status.Phase, p.Status.AllocatedCIDR)
	}
	c := meta.FindStatusCondition(p.Status.Conditions, "Allocated")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ParentNotFound" {
		t.Errorf("Allocated condition = %+v, want False/ParentNotFound", c)
	}
}

// Flux dry-runs every object in a set before applying any. A server-side
// dry-run of a child pool whose parent does not exist yet must therefore
// succeed, or a set holding a new parent and its child fails on every
// reconcile and the parent is never applied. It answers with the pool
// uncarved and stores nothing. This holds for apply and for a plain POST.
func TestDryRunOfAChildPoolWithoutItsParentSucceeds(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	got, _, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), true)
	if err != nil {
		t.Fatalf("dry-run apply with a missing parent: %v", err)
	}
	assertUncarvedForMissingParent(t, got)
	assertNotStored(t, r, ctx, "child")

	got, err = r.createChild(t, ctx, "child", "parent", 24, true)
	if err != nil {
		t.Fatalf("dry-run create with a missing parent: %v", err)
	}
	assertUncarvedForMissingParent(t, got)
	assertNotStored(t, r, ctx, "child")
}

// The dry-run is lenient; the real request is not. A real create of a child
// pool whose parent is missing is still refused, whichever verb sends it, so
// the caller retries once the parent exists.
func TestARealCreateWithoutItsParentIsStillRefused(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, _, err := r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false); !apierrors.IsBadRequest(err) {
		t.Errorf("apply with a missing parent: err = %v, want 400", err)
	}
	if _, err := r.createChild(t, ctx, "child", "parent", 24, false); !apierrors.IsBadRequest(err) {
		t.Errorf("create with a missing parent: err = %v, want 400", err)
	}
	assertNotStored(t, r, ctx, "child")
}

// A dry-run against a parent that exists but is full is still refused: the
// real create would be, and no ordering of the set changes that.
func TestDryRunAgainstAFullParentIsStillRefused(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("small", "10.1.0.0/24"), false); err != nil {
		t.Fatalf("apply root pool: %v", err)
	}
	if _, _, err := r.serverSideApply(t, ctx, childPoolManifest("too-big", "small", 16), true); statusCode(err) != 507 {
		t.Errorf("dry-run larger than the parent: err = %v, want 507", err)
	}
}

// A plain POST with dryRun=All of a child pool reports the carve it would
// make and keeps none of it.
func TestDryRunCreateOfAChildPoolLeavesNoCarve(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("parent", "10.0.0.0/16"), false); err != nil {
		t.Fatalf("apply root pool: %v", err)
	}
	got, err := r.createChild(t, ctx, "child", "parent", 24, true)
	if err != nil {
		t.Fatalf("dry-run create: %v", err)
	}
	if got.Status.AllocatedCIDR != "10.0.0.0/24" {
		t.Errorf("dry-run allocatedCIDR = %q, want the would-be carve 10.0.0.0/24", got.Status.AllocatedCIDR)
	}
	assertNotStored(t, r, ctx, "child")

	// The space the dry-run reported is still free for a real create.
	real, err := r.createChild(t, ctx, "child", "parent", 24, false)
	if err != nil {
		t.Fatalf("create after dry-run: %v", err)
	}
	assertCarved(t, real, "10.0.0.0/24")
}

// fluxApply does what Flux's apply of a set of custom objects does: dry-run
// every object, stop on the first refusal, then apply each in turn and stop on
// the first refusal. Flux sorts a set of one kind by name, so the caller passes
// the manifests in name order.
func (r *registries) fluxApply(t *testing.T, ctx context.Context, manifests ...map[string]any) error {
	t.Helper()
	for _, m := range manifests {
		if _, _, err := r.serverSideApply(t, ctx, m, true); err != nil {
			return err
		}
	}
	for _, m := range manifests {
		if _, _, err := r.serverSideApply(t, ctx, m, false); err != nil {
			return err
		}
	}
	return nil
}

// A first install holding a new parent and its child in one set goes through
// in one reconcile when the parent comes first. Before dry-runs of a child
// with a missing parent were accepted, the child's dry-run failed and the set
// failed on every reconcile, so the parent was never applied.
func TestASetWithANewParentAndChildEndsCarved(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	// "env" sorts before "region", so Flux applies the parent first.
	if err := r.fluxApply(t, ctx, rootPoolManifest("env", "10.0.0.0/16"), childPoolManifest("region", "env", 24)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertCarved(t, r.storedPool(t, ctx, "region"), "10.0.0.0/24")
	if n := r.carves(t, "region"); n != 1 {
		t.Errorf("child holds %d carves, want 1", n)
	}
}

// Applies one at a time end with the child carved in either order. Parent
// then child goes through at once. Child then parent has the child refused
// until the parent exists, and a retry of the child then carves it.
func TestApplyingAParentAndChildInEitherOrderEndsCarved(t *testing.T) {
	for _, tc := range []struct {
		name       string
		childFirst bool
	}{{"parent then child", false}, {"child then parent", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistries(t)
			ctx := projectCtx(applyProject, metav1.NamespaceNone)
			parent := rootPoolManifest("parent", "10.0.0.0/16")
			child := childPoolManifest("child", "parent", 24)

			if tc.childFirst {
				if _, _, err := r.serverSideApply(t, ctx, child, false); !apierrors.IsBadRequest(err) {
					t.Fatalf("apply child before its parent: err = %v, want 400", err)
				}
				assertNotStored(t, r, ctx, "child")
			}
			if _, _, err := r.serverSideApply(t, ctx, parent, false); err != nil {
				t.Fatalf("apply parent: %v", err)
			}
			got, created, err := r.serverSideApply(t, ctx, child, false)
			if err != nil || !created {
				t.Fatalf("apply child: created=%v err=%v", created, err)
			}
			assertCarved(t, got, "10.0.0.0/24")
			if n := r.carves(t, "child"); n != 1 {
				t.Errorf("child holds %d carves, want 1", n)
			}
		})
	}
}

// Two reapplies of a stranded child pool racing each other carve it once.
func TestConcurrentReappliesOfAStrandedChildPoolCarveOnce(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, _, err := r.serverSideApply(t, ctx, rootPoolManifest("parent", "10.0.0.0/16"), false); err != nil {
		t.Fatalf("apply root pool: %v", err)
	}
	r.strand(t, ctx, "child", "parent", 24)

	const racers = 2
	var wg sync.WaitGroup
	results := make([]*ipam.IPPool, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], _, errs[i] = r.serverSideApply(t, ctx, childPoolManifest("child", "parent", 24), false)
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
	if n := r.carves(t, "child"); n != 1 {
		t.Errorf("child holds %d carves after %d racing reapplies, want 1", n, racers)
	}
	assertCarved(t, r.storedPool(t, ctx, "child"), "10.0.0.0/24")
}

func statusCode(err error) int32 {
	if s, ok := err.(apierrors.APIStatus); ok {
		return s.Status().Code
	}
	return 0
}
