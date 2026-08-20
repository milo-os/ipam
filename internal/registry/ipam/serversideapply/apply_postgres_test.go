// Package serversideapply covers the request path a GitOps deployment uses.
//
// A create and an apply reach the same registry with the same manifest, but
// only apply runs the field manager, and the field manager hands the registry
// an object whose TypeMeta the internal-version conversion has cleared. Every
// other test in this repository creates its fixtures, so nothing else exercises
// the object the storage layer actually receives in staging or production.
package serversideapply

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclaim"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclass"
	"go.miloapis.com/ipam/internal/registry/ipam/ippool"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestMain(m *testing.M) { testdb.TestMain(m) }

const applyProject = "datum-cloud"

type registries struct {
	scheme *runtime.Scheme
	class  *ipclass.IPClassStorage
	pool   *ippool.AllocatingIPPoolREST
	claim  *ipclaim.AllocatingREST
	db     *pgxpool.Pool
}

func newRegistries(t *testing.T) *registries {
	t.Helper()

	db := testdb.Pool(t)
	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion)

	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(codec)

	classStore, _, err := ipclass.NewClassStorage(scheme, getter, nil, db)
	if err != nil {
		t.Fatalf("class storage: %v", err)
	}
	t.Cleanup(classStore.Destroy)

	alloc := allocator.NewPostgresPrefixAllocator()
	poolStore, _, err := ippool.NewIPPoolStorage(scheme, getter, alloc, db, codec)
	if err != nil {
		t.Fatalf("pool storage: %v", err)
	}
	t.Cleanup(poolStore.Destroy)

	claimStore, _, err := ipclaim.NewAllocatingStorage(scheme, getter, alloc, db, codec, nil, nil)
	if err != nil {
		t.Fatalf("claim storage: %v", err)
	}
	t.Cleanup(claimStore.Destroy)

	return &registries{scheme: scheme, class: classStore, pool: poolStore, claim: claimStore, db: db}
}

func projectCtx(project, namespace string) context.Context {
	ctx := genericapirequest.WithUser(context.Background(), &user.DefaultInfo{
		Name: "kustomize-controller",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {tenant.ParentAPIGroupProject},
			tenant.ExtraParentType:     {tenant.ParentTypeProject},
			tenant.ExtraParentName:     {project},
		},
	})
	return genericapirequest.WithNamespace(ctx, namespace)
}

// applied runs a manifest through the same field manager the apiserver's apply
// patcher uses, and returns the object it would hand to the registry's Create.
//
// The manifest is deliberately expressed as the YAML-equivalent map a client
// sends, not as a typed object: the point is the conversion the field manager
// performs on the way to the internal version.
func applied(t *testing.T, scheme *runtime.Scheme, kind string, manifest map[string]any) runtime.Object {
	t.Helper()

	gvk := ipamv1alpha1.SchemeGroupVersion.WithKind(kind)
	fm, err := managedfields.NewDefaultFieldManager(
		managedfields.NewDeducedTypeConverter(),
		scheme,
		scheme,
		scheme,
		gvk,
		schema.GroupVersion{Group: ipam.GroupName, Version: runtime.APIVersionInternal},
		"",
		nil,
	)
	if err != nil {
		t.Fatalf("field manager for %s: %v", kind, err)
	}

	live, err := scheme.New(gvk.GroupVersion().WithKind(kind))
	if err != nil {
		t.Fatalf("new %s: %v", kind, err)
	}
	internalLive, err := scheme.ConvertToVersion(live, schema.GroupVersion{Group: ipam.GroupName, Version: runtime.APIVersionInternal})
	if err != nil {
		t.Fatalf("convert live %s: %v", kind, err)
	}

	out, err := fm.Apply(internalLive, &unstructured.Unstructured{Object: manifest}, "kustomize-controller", true)
	if err != nil {
		t.Fatalf("apply %s: %v", kind, err)
	}
	return out
}

func classManifest(name string) map[string]any {
	return map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPClass",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"ipFamily":            "IPv4",
			"defaultPrefixLength": int64(24),
		},
	}
}

func poolManifest(name, className string) map[string]any {
	return map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPPool",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"cidr":       "10.0.0.0/16",
			"ipFamily":   "IPv4",
			"classNames": []any{className},
		},
	}
}

// A pool deployed by apply must serve claims exactly as one deployed by create
// does. This is the whole of issue 122: the manifests are identical and only
// the verb differs.
func TestApplyingAPoolLetsItsClassAllocate(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, err := r.class.Create(ctx, applied(t, r.scheme, "IPClass", classManifest("standard")), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("apply class: %v", err)
	}
	if _, err := r.pool.Create(ctx, applied(t, r.scheme, "IPPool", poolManifest("us-east", "standard")), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("apply pool: %v", err)
	}

	var kind string
	if err := r.db.QueryRow(context.Background(),
		`SELECT kind FROM ipam_objects WHERE key = $1`,
		tenant.Identity{Name: applyProject}.ResourceKey("ippools", "us-east"),
	).Scan(&kind); err != nil {
		t.Fatalf("read stored kind: %v", err)
	}
	if kind != "IPPool" {
		t.Errorf("stored kind = %q, want IPPool", kind)
	}

	var offers int
	if err := r.db.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_pool_class_offer WHERE class_name = 'standard'`).Scan(&offers); err != nil {
		t.Fatalf("count offers: %v", err)
	}
	if offers != 1 {
		t.Errorf("class 'standard' has %d offers, want 1", offers)
	}

	claim := &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"},
		Spec:       ipam.IPClaimSpec{ClassName: "standard", IPFamily: ipam.IPv4},
	}
	obj, err := r.claim.Create(projectCtx(applyProject, "default"), claim, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("claim against an applied pool: %v", err)
	}
	bound := obj.(*ipam.IPClaim)
	if bound.Status.Phase != ipam.ClaimBound {
		t.Errorf("phase = %q, want %q", bound.Status.Phase, ipam.ClaimBound)
	}
	if bound.Status.AllocatedCIDR != "10.0.0.0/24" {
		t.Errorf("allocatedCIDR = %q, want 10.0.0.0/24", bound.Status.AllocatedCIDR)
	}
}

// The blanking is a property of apply, not of create: an object created by POST
// and then edited by apply must keep its kind through the update path too.
func TestApplyingOverACreatedPoolKeepsItsKind(t *testing.T) {
	r := newRegistries(t)
	ctx := projectCtx(applyProject, metav1.NamespaceNone)

	if _, err := r.class.Create(ctx, applied(t, r.scheme, "IPClass", classManifest("standard")), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("apply class: %v", err)
	}

	created := &ipam.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "us-east"},
		Spec:       ipam.IPPoolSpec{CIDR: "10.0.0.0/16", IPFamily: ipam.IPv4},
	}
	if _, err := r.pool.Create(ctx, created, nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	edited := applied(t, r.scheme, "IPPool", poolManifest("us-east", "standard"))
	stored, err := r.pool.Get(ctx, "us-east", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	next := edited.(*ipam.IPPool)
	next.ResourceVersion = stored.(*ipam.IPPool).ResourceVersion
	next.Status = stored.(*ipam.IPPool).Status
	if _, _, err := r.pool.Update(ctx, "us-east", updateTo(next), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("apply over created pool: %v", err)
	}

	var kind string
	if err := r.db.QueryRow(context.Background(),
		`SELECT kind FROM ipam_objects WHERE key = $1`,
		tenant.Identity{Name: applyProject}.ResourceKey("ippools", "us-east"),
	).Scan(&kind); err != nil {
		t.Fatalf("read stored kind: %v", err)
	}
	if kind != "IPPool" {
		t.Errorf("stored kind after apply = %q, want IPPool", kind)
	}

	var offers int
	if err := r.db.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_pool_class_offer WHERE class_name = 'standard'`).Scan(&offers); err != nil {
		t.Fatalf("count offers: %v", err)
	}
	if offers != 1 {
		t.Errorf("class 'standard' has %d offers after apply, want 1", offers)
	}
}

func updateTo(obj runtime.Object) rest.UpdatedObjectInfo {
	return rest.DefaultUpdatedObjectInfo(obj)
}
