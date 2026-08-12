package ipclaim

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// newScopedREST wires the real allocator against a real database and seeds one
// class with the supplied uniqueWithin, plus a pool offering it.
func newScopedREST(t *testing.T, uniqueWithin []string) (*AllocatingREST, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Pool(t)

	seedClass(t, db, testProject, &ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24, UniqueWithin: uniqueWithin,
	})
	seedOfferingPool(t, db, testProject, "10.0.0.0/16")

	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	return &AllocatingREST{
		allocator: allocator.NewPostgresPrefixAllocator(),
		db:        db,
		strategy:  NewStrategy(scheme),
		codec:     serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion),
	}, db
}

func seedClass(t *testing.T, db *pgxpool.Pool, project string, spec *ipamv1alpha1.IPClassSpec) {
	t.Helper()
	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "standard"},
		Spec:       *spec,
	}
	data, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal class: %v", err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPClass',$2,$3)`,
		tenant.Identity{Name: project}.ResourceKey("ipclasses", "standard"), "standard", data); err != nil {
		t.Fatalf("seed class in %s: %v", project, err)
	}
}

func seedOfferingPool(t *testing.T, db *pgxpool.Pool, project, cidr string) {
	t.Helper()
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "us-east"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: cidr, IPFamily: ipamv1alpha1.IPv4, ClassNames: []string{"standard"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: ipamv1alpha1.IPv4,
		},
	}
	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		tenant.Identity{Name: project}.ResourceKey("ippools", "us-east"), "us-east", data); err != nil {
		t.Fatalf("seed pool in %s: %v", project, err)
	}
}

// networkScope is the scope a claim carries for the `network` role.
func networkScope(name string) map[string]ipam.ScopeRef {
	return map[string]ipam.ScopeRef{
		"network": {APIGroup: "networking.miloapis.com", Kind: "Network", Name: name},
	}
}

func scopedClaim(name string, scope map[string]ipam.ScopeRef) *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       ipam.IPClaimSpec{ClassName: "standard", IPFamily: ipam.IPv4, Scope: scope},
	}
}

func bind(t *testing.T, r *AllocatingREST, ctx context.Context, claim *ipam.IPClaim) string {
	t.Helper()
	obj, err := r.Create(ctx, claim, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create %s: %v", claim.Name, err)
	}
	return obj.(*ipam.IPClaim).Status.AllocatedCIDR
}

// The feature: a class that separates address spaces by network hands the same
// address to two networks out of one pool.
func TestTwoAddressSpacesGetTheSameAddress(t *testing.T) {
	r, _ := newScopedREST(t, []string{"network"})
	ctx := claimCtx(testProject)

	a := bind(t, r, ctx, scopedClaim("claim-a", networkScope("net-a")))
	b := bind(t, r, ctx, scopedClaim("claim-b", networkScope("net-b")))

	if a != b {
		t.Errorf("two networks got %s and %s; each address space starts at the base of the pool", a, b)
	}
}

// Within one address space the pool behaves as it always did.
func TestOneAddressSpaceGetsDistinctAddresses(t *testing.T) {
	r, _ := newScopedREST(t, []string{"network"})
	ctx := claimCtx(testProject)

	a := bind(t, r, ctx, scopedClaim("claim-a", networkScope("shared")))
	b := bind(t, r, ctx, scopedClaim("claim-b", networkScope("shared")))

	if a == b {
		t.Errorf("two claims in one network both got %s", a)
	}
}

// The projection is recorded, so an allocation says which space it is in
// without recomputing it from the class.
func TestTheAllocationRecordsTheProjectedScope(t *testing.T) {
	r, db := newScopedREST(t, []string{"network"})
	ctx := claimCtx(testProject)

	claim := scopedClaim("claim-a", map[string]ipam.ScopeRef{
		"network":  {APIGroup: "networking.miloapis.com", Kind: "Network", Name: "net-a"},
		"location": {APIGroup: "networking.miloapis.com", Kind: "Location", Name: "dfw"},
	})
	bind(t, r, ctx, claim)

	var data []byte
	if err := db.QueryRow(context.Background(),
		`SELECT data FROM ipam_objects WHERE kind = 'IPAllocation'`).Scan(&data); err != nil {
		t.Fatalf("read allocation object: %v", err)
	}
	var alloc ipamv1alpha1.IPAllocation
	if err := json.Unmarshal(data, &alloc); err != nil {
		t.Fatalf("decode allocation object: %v", err)
	}

	// location is not in uniqueWithin, so it is not part of the space and must
	// not be recorded as though it were.
	if _, ok := alloc.Spec.Scope["location"]; ok {
		t.Errorf("spec.scope = %v, want only the uniqueWithin roles", alloc.Spec.Scope)
	}
	if got := alloc.Spec.Scope["network"].Name; got != "net-a" {
		t.Errorf("spec.scope[network] = %q, want net-a", got)
	}

	var rowDigest string
	if err := db.QueryRow(context.Background(),
		`SELECT scope_digest FROM ipam_cidr_allocations`).Scan(&rowDigest); err != nil {
		t.Fatalf("read allocation row: %v", err)
	}
	// The search ran under the row's digest, so status must report that one and
	// not a second projection that could differ from it.
	if alloc.Status.ScopeDigest != rowDigest {
		t.Errorf("status.scopeDigest = %q but the row was written under %q",
			alloc.Status.ScopeDigest, rowDigest)
	}
}

// A claim short a role its class requires is a bad request naming the role, and
// it allocates nothing.
func TestAClaimMissingAUniqueWithinRoleIsRefused(t *testing.T) {
	r, db := newScopedREST(t, []string{"network"})

	_, err := r.Create(claimCtx(testProject), scopedClaim("claim-a", nil), nil, &metav1.CreateOptions{})
	if err == nil {
		t.Fatal("Create succeeded for a claim carrying no network")
	}
	if !apierrors.IsBadRequest(err) {
		t.Errorf("err = %v, want a 400", err)
	}
	if !strings.Contains(err.Error(), "network") || !strings.Contains(err.Error(), "standard") {
		t.Errorf("err = %q, want it to name the missing role and the class", err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`); n != 0 {
		t.Errorf("refused claim left %d allocation rows, want 0", n)
	}
}

// A role present but nameless is a missing field wearing the shape of a
// present one; it must not digest to a space of its own.
func TestANamelessScopeRefIsAMissingRole(t *testing.T) {
	r, _ := newScopedREST(t, []string{"network"})

	claim := scopedClaim("claim-a", map[string]ipam.ScopeRef{
		"network": {APIGroup: "networking.miloapis.com", Kind: "Network"},
	})
	if _, err := r.Create(claimCtx(testProject), claim, nil, &metav1.CreateOptions{}); err == nil {
		t.Fatal("Create succeeded for a claim whose network has no name")
	}
}

// `uniqueWithin: []` separates nothing: the scope a claim carries is not part
// of its address space, so two claims still cannot hold one address.
func TestEmptyUniqueWithinSeparatesNothing(t *testing.T) {
	r, _ := newScopedREST(t, nil)
	ctx := claimCtx(testProject)

	a := bind(t, r, ctx, scopedClaim("claim-a", networkScope("net-a")))
	b := bind(t, r, ctx, scopedClaim("claim-b", networkScope("net-b")))

	if a == b {
		t.Errorf("two networks both got %s from a class that separates nothing", a)
	}
}

// The same, across tenants. Two projects sharing one class through a reference
// reach one pool and one address space, so neither may be handed the other's
// address.
func TestEmptyUniqueWithinSeparatesNothingAcrossProjects(t *testing.T) {
	db := testdb.Pool(t)

	const other = "other-project"
	seedClass(t, db, testProject, &ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24,
	})
	seedOfferingPool(t, db, testProject, "10.0.0.0/16")
	seedClass(t, db, other, &ipamv1alpha1.IPClassSpec{
		Source: &ipamv1alpha1.ClassSourceRef{Project: testProject, Name: "standard"},
	})

	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	r := &AllocatingREST{
		allocator: allocator.NewPostgresPrefixAllocator(),
		db:        db,
		strategy:  NewStrategy(scheme),
		codec:     serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion),
	}

	a := bind(t, r, claimCtx(testProject), scopedClaim("claim-a", nil))
	b := bind(t, r, claimCtx(other), scopedClaim("claim-b", nil))

	if a == b {
		t.Errorf("two projects both got %s from a class that separates nothing", a)
	}
}
