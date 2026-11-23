package serversideapply

import (
	"context"
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

func statusCode(err error) int32 {
	if s, ok := err.(apierrors.APIStatus); ok {
		return s.Status().Code
	}
	return 0
}
