package ipclaim

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/ipallocation"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// newLifecycleREST wires claim and allocation storage over one test database,
// through the same Postgres RESTOptionsGetter the server uses. Delete needs a
// real store behind it: it reads the object before releasing it.
func newLifecycleREST(t *testing.T, classPolicy ipamv1alpha1.ReclaimPolicy) (*AllocatingREST, *ipallocation.ReleasingREST, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Pool(t)
	ctx := context.Background()

	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "standard"},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24, ReclaimPolicy: classPolicy,
		},
	}
	classData, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal class: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPClass',$2,$3)`,
		tenant.Identity{Name: testProject}.ResourceKey("ipclasses", "standard"), "standard", classData); err != nil {
		t.Fatalf("seed class: %v", err)
	}

	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "us-east"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: "10.0.0.0/16", IPFamily: ipamv1alpha1.IPv4, ClassNames: []string{"standard"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "10.0.0.0/16", IPFamily: ipamv1alpha1.IPv4,
		},
	}
	poolData, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", "us-east"), "us-east", poolData); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion)

	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(codec)

	alloc := allocator.NewPostgresPrefixAllocator()
	claimREST, _, err := NewAllocatingStorage(scheme, getter, alloc, db, codec, nil)
	if err != nil {
		t.Fatalf("claim storage: %v", err)
	}
	t.Cleanup(claimREST.Destroy)
	allocREST, _, err := ipallocation.NewAllocationStorage(scheme, getter, alloc, db)
	if err != nil {
		t.Fatalf("allocation storage: %v", err)
	}
	t.Cleanup(allocREST.Destroy)

	return claimREST, allocREST, db
}

func retainClaim(name string, policy ipam.ReclaimPolicy) *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ipam.IPClaimSpec{
			ClassName: "standard", IPFamily: ipam.IPv4, ReclaimPolicy: policy,
		},
	}
}

// allocationRow is the state of the ipam_cidr_allocations row behind a claim.
type allocationRow struct {
	claimKey   *string
	retainedAt *string
	cidr       string
}

func readAllocationRow(t *testing.T, db *pgxpool.Pool, allocationKey string) (allocationRow, bool) {
	t.Helper()
	var row allocationRow
	err := db.QueryRow(context.Background(),
		`SELECT claim_key, retained_at::text,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocationKey,
	).Scan(&row.claimKey, &row.retainedAt, &row.cidr)
	if err != nil {
		return allocationRow{}, false
	}
	return row, true
}

func allocationKeyFor(claim *ipam.IPClaim) string {
	id := tenant.Identity{Name: testProject}
	return allocationObjectKey(id, claim.Namespace, allocationNameFor(claim.Namespace, claim.Name))
}

// Deleting a claim under the default policy frees its address, and the next
// claim is handed the same block back.
func TestDeleteUnderDeletePolicyFreesTheAddress(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := created.(*ipam.IPClaim).Status.AllocatedCIDR

	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, ok := readAllocationRow(t, db, allocationKeyFor(retainClaim("claim-1", ""))); ok {
		t.Error("allocation row survives a Delete-policy release")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE kind = 'IPAllocation'`); n != 0 {
		t.Errorf("%d IPAllocation objects survive, want 0", n)
	}

	next, err := r.Create(ctx, retainClaim("claim-2", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create after delete: %v", err)
	}
	if got := next.(*ipam.IPClaim).Status.AllocatedCIDR; got != first {
		t.Errorf("next claim got %q, want the freed %q", got, first)
	}
}

// Retain keeps the address and the object that records it: the row is unbound
// and stamped, the IPAllocation survives with no claimRef, and the next claim
// is handed a different block.
func TestDeleteUnderRetainHoldsTheAddress(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claim := created.(*ipam.IPClaim)
	first := claim.Status.AllocatedCIDR
	allocationName := claim.Status.BoundAllocationRef.Name

	var poolBefore string
	if err := db.QueryRow(context.Background(),
		`SELECT ipam_data_to_jsonb(data) -> 'status' -> 'capacity' ->> 'allocated'
		   FROM ipam_objects WHERE kind = 'IPPool'`).Scan(&poolBefore); err != nil {
		t.Fatalf("read pool capacity: %v", err)
	}

	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	row, ok := readAllocationRow(t, db, allocationKeyFor(claim))
	if !ok {
		t.Fatal("allocation row was deleted despite reclaimPolicy Retain")
	}
	if row.claimKey != nil {
		t.Errorf("claim_key = %q, want NULL", *row.claimKey)
	}
	if row.retainedAt == nil {
		t.Error("retained_at is NULL; the trigger did not stamp the retention")
	}
	if row.cidr != first {
		t.Errorf("retained row holds %q, want %q", row.cidr, first)
	}

	// The claim is gone; the allocation it left behind is not, and it reads as
	// unbound rather than as still belonging to a claim that no longer exists.
	if _, err := r.Get(ctx, "claim-1", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get after delete returned %v, want NotFound", err)
	}
	obj, err := allocREST.Get(ctx, allocationName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get retained IPAllocation: %v", err)
	}
	retained := obj.(*ipam.IPAllocation)
	if retained.Spec.ClaimRef != nil {
		t.Errorf("spec.claimRef = %v on a retained allocation, want nil", retained.Spec.ClaimRef)
	}
	if retained.Spec.ReclaimPolicy != ipam.ReclaimRetain {
		t.Errorf("spec.reclaimPolicy = %q, want Retain", retained.Spec.ReclaimPolicy)
	}
	if retained.Spec.ClassName != "standard" {
		t.Errorf("spec.className = %q, want standard", retained.Spec.ClassName)
	}

	var poolAfter string
	if err := db.QueryRow(context.Background(),
		`SELECT ipam_data_to_jsonb(data) -> 'status' -> 'capacity' ->> 'allocated'
		   FROM ipam_objects WHERE kind = 'IPPool'`).Scan(&poolAfter); err != nil {
		t.Fatalf("read pool capacity: %v", err)
	}
	if poolAfter != poolBefore {
		t.Errorf("pool allocated capacity moved from %s to %s; retention frees nothing", poolBefore, poolAfter)
	}

	next, err := r.Create(ctx, retainClaim("claim-2", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create after retain: %v", err)
	}
	if got := next.(*ipam.IPClaim).Status.AllocatedCIDR; got == first {
		t.Errorf("next claim was handed the retained %q", got)
	}
}

// A claim overrides its class's policy, so Retain is reachable without a class
// that defaults to it.
func TestClaimOverridesTheClassReclaimPolicy(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ipam.ReclaimRetain), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	row, ok := readAllocationRow(t, db, allocationKeyFor(created.(*ipam.IPClaim)))
	if !ok {
		t.Fatal("allocation row was deleted despite the claim's Retain override")
	}
	if row.claimKey != nil {
		t.Errorf("claim_key = %q, want NULL", *row.claimKey)
	}
}

// Deleting a retained allocation is the only way to give its address back, and
// it gives it back: the next claim is handed the same block.
func TestDeletingARetainedAllocationFreesTheAddress(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claim := created.(*ipam.IPClaim)
	first := claim.Status.AllocatedCIDR
	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete claim: %v", err)
	}

	if _, _, err := allocREST.Delete(ctx, claim.Status.BoundAllocationRef.Name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete retained allocation: %v", err)
	}
	if _, ok := readAllocationRow(t, db, allocationKeyFor(claim)); ok {
		t.Error("allocation row survives the deletion of the IPAllocation")
	}

	next, err := r.Create(ctx, retainClaim("claim-2", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create after release: %v", err)
	}
	if got := next.(*ipam.IPClaim).Status.AllocatedCIDR; got != first {
		t.Errorf("next claim got %q, want the freed %q", got, first)
	}
}

// A bound allocation still belongs to its claim, and deleting it directly
// would strand the claim on an address nothing records.
func TestDeletingABoundAllocationIsRefused(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claim := created.(*ipam.IPClaim)

	if _, _, err := allocREST.Delete(ctx, claim.Status.BoundAllocationRef.Name, nil, &metav1.DeleteOptions{}); err == nil {
		t.Fatal("deleted an allocation that is still bound to a claim")
	}
	if _, ok := readAllocationRow(t, db, allocationKeyFor(claim)); !ok {
		t.Error("the refused delete freed the allocation row anyway")
	}
}

// A claim recreated under the name of one whose address was retained collides
// with that retained allocation. Rebinding it is a separate feature; refusing
// it clearly is the requirement here.
func TestRecreatingAClaimOverARetainedAllocationConflicts(t *testing.T) {
	r, _, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	if _, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err == nil {
		t.Fatal("recreating the claim succeeded; it must not silently take a second address under a retained identity")
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("Create returned %v (%T), want a 409 Conflict", err, err)
	}
}
