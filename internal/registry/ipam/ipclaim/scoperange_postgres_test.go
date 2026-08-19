package ipclaim

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/scope"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// The tenant IPv6 plan, as three classes: a /48 per network, a /64 per network
// and location, a /96 per endpoint, all carved out of one operator /36.
func newTenantREST(t *testing.T) (*AllocatingREST, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Pool(t)
	ctx := context.Background()

	seedObject := func(kind, name string, obj any) {
		t.Helper()
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", kind, name, err)
		}
		resource := "ipclasses"
		if kind == "IPPool" {
			resource = "ippools"
		}
		if _, err := db.Exec(ctx,
			`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,$2,$3,$4)`,
			tenant.Identity{Name: testProject}.ResourceKey(resource, name), kind, name, data,
		); err != nil {
			t.Fatalf("seed %s %s: %v", kind, name, err)
		}
	}

	class := func(name string, spec ipamv1alpha1.IPClassSpec) {
		seedObject("IPClass", name, &ipamv1alpha1.IPClass{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		})
	}

	class("tenant-vpc", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, DefaultPrefixLength: 48,
		PoolPer: []string{"network", scope.ReservedRoleProject},
	})
	class("tenant-subnet", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-vpc", DefaultPrefixLength: 64,
		PoolPer: []string{"network", "location", scope.ReservedRoleProject},
	})
	class("tenant-endpoint", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-subnet", DefaultPrefixLength: 96,
	})

	seedObject("IPPool", "tenant-root", &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-root"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: "fd20:1000::/36", IPFamily: ipamv1alpha1.IPv6, ClassNames: []string{"tenant-vpc"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "fd20:1000::/36", IPFamily: ipamv1alpha1.IPv6,
		},
	})

	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion)

	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(codec)

	rest, _, err := NewAllocatingStorage(scheme, getter, allocator.NewPostgresPrefixAllocator(), db, codec, nil, nil)
	if err != nil {
		t.Fatalf("claim storage: %v", err)
	}
	t.Cleanup(rest.Destroy)
	return rest, db
}

func networkRef(name string) ipam.ScopeRef {
	return ipam.ScopeRef{APIGroup: "networking.datumapis.com", Kind: "Network", Name: name}
}

func locationRef(name string) ipam.ScopeRef {
	return ipam.ScopeRef{APIGroup: "networking.datumapis.com", Kind: "Location", Name: name}
}

func rangeClaim(name, network string) *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ipam.IPClaimSpec{
			ClassName: "tenant-vpc",
			Target:    ipam.TargetScopeRange,
			Scope:     map[string]ipam.ScopeRef{"network": networkRef(network)},
		},
	}
}

func endpointClaim(name, network, location string) *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ipam.IPClaimSpec{
			ClassName: "tenant-endpoint",
			Scope: map[string]ipam.ScopeRef{
				"network":  networkRef(network),
				"location": locationRef(location),
			},
		},
	}
}

func create(t *testing.T, rest *AllocatingREST, claim *ipam.IPClaim) *ipam.IPClaim {
	t.Helper()
	obj, err := rest.Create(claimCtx(testProject), claim, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create %s: %v", claim.Name, err)
	}
	return obj.(*ipam.IPClaim)
}

func countPoolsProvisionedBy(t *testing.T, db *pgxpool.Pool, className string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ipam_objects
		  WHERE kind = 'IPPool'
		    AND ipam_data_to_jsonb(data) -> 'metadata' -> 'labels' ->> 'ipam.miloapis.com/provisioned-by' = $1`,
		className).Scan(&n); err != nil {
		t.Fatalf("count pools provisioned by %s: %v", className, err)
	}
	return n
}

func mustContain(t *testing.T, outer, inner string) {
	t.Helper()
	_, outerNet, err := net.ParseCIDR(outer)
	if err != nil {
		t.Fatalf("parse %q: %v", outer, err)
	}
	innerIP, innerNet, err := net.ParseCIDR(inner)
	if err != nil {
		t.Fatalf("parse %q: %v", inner, err)
	}
	outerOnes, _ := outerNet.Mask.Size()
	innerOnes, _ := innerNet.Mask.Size()
	if !outerNet.Contains(innerIP) || innerOnes < outerOnes {
		t.Fatalf("%s does not lie inside %s", inner, outer)
	}
}

// THE ADOPTION GUARANTEE. A range provisioned up front is the same pool the
// cascade would have built under the first endpoint claim, so the endpoint is
// addressed out of it rather than out of a second /48 provisioned beside it.
//
// Without it a network would publish one prefix and its endpoints would live in
// another, which is worse than not publishing one at all.
func TestAScopeRangeIsAdoptedByALaterEndpointClaim(t *testing.T) {
	rest, db := newTenantREST(t)

	network := create(t, rest, rangeClaim("vpc-a", "net-a"))
	if network.Status.Phase != ipam.ClaimBound {
		t.Fatalf("range claim phase = %q, want Bound", network.Status.Phase)
	}
	if network.Status.PoolRef == nil || network.Status.PoolRef.Name == "" {
		t.Fatal("range claim names no pool")
	}
	if network.Status.BoundAllocationRef != nil {
		t.Errorf("range claim bound an IPAllocation %q; a range is a pool, not a block",
			network.Status.BoundAllocationRef.Name)
	}
	if _, ipnet, err := net.ParseCIDR(network.Status.AllocatedCIDR); err != nil {
		t.Fatalf("range CIDR %q: %v", network.Status.AllocatedCIDR, err)
	} else if ones, _ := ipnet.Mask.Size(); ones != 48 {
		t.Fatalf("range is /%d, want the class's /48", ones)
	}

	endpoint := create(t, rest, endpointClaim("ep-1", "net-a", "lon1"))
	mustContain(t, network.Status.AllocatedCIDR, endpoint.Status.AllocatedCIDR)

	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 1 {
		t.Errorf("tenant-vpc provisioned %d pools for one network, want 1", n)
	}
	var identities int
	if err := db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ipam_pool_identity WHERE class_name = 'tenant-vpc'`).Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if identities != 1 {
		t.Errorf("tenant-vpc holds %d identity rows, want 1", identities)
	}
}

// The order does not matter: a range claim arriving after the endpoints adopts
// the pool the cascade already built and reports the range they are in.
func TestAScopeRangeAdoptsTheCascadesOwnPool(t *testing.T) {
	rest, db := newTenantREST(t)

	endpoint := create(t, rest, endpointClaim("ep-1", "net-a", "lon1"))
	network := create(t, rest, rangeClaim("vpc-a", "net-a"))

	mustContain(t, network.Status.AllocatedCIDR, endpoint.Status.AllocatedCIDR)
	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 1 {
		t.Errorf("tenant-vpc provisioned %d pools for one network, want 1", n)
	}
}

// Two networks are two ranges. Nothing here is subtle; it fails loudly if the
// scope digest stops separating them.
func TestTwoNetworksHoldDifferentRanges(t *testing.T) {
	rest, _ := newTenantREST(t)

	a := create(t, rest, rangeClaim("vpc-a", "net-a"))
	b := create(t, rest, rangeClaim("vpc-b", "net-b"))
	if a.Status.AllocatedCIDR == b.Status.AllocatedCIDR {
		t.Fatalf("both networks were handed %s", a.Status.AllocatedCIDR)
	}
}

// A second claim under the same name is refused rather than handed the range a
// live claim holds, so a controller that retries a create reads its own claim
// back instead of taking a second hold on it.
func TestASecondClaimForOneRangeIsRefused(t *testing.T) {
	rest, _ := newTenantREST(t)

	create(t, rest, rangeClaim("vpc-a", "net-a"))

	_, err := rest.Create(claimCtx(testProject), rangeClaim("vpc-a-again", "net-a"), nil, &metav1.CreateOptions{})
	if !apierrors.IsConflict(err) {
		t.Fatalf("second claim for one range returned %v, want a conflict", err)
	}

	_, err = rest.Create(claimCtx(testProject), rangeClaim("vpc-a", "net-a"), nil, &metav1.CreateOptions{})
	if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("recreating the same claim returned %v, want a conflict", err)
	}
}

// Releasing a range gives it back: the pool, its identity and the carve it held
// in the operator's pool all go, and the next network is handed the same block.
func TestReleasingARangeFreesIt(t *testing.T) {
	rest, db := newTenantREST(t)
	ctx := claimCtx(testProject)

	first := create(t, rest, rangeClaim("vpc-a", "net-a"))

	if _, _, err := rest.Delete(ctx, "vpc-a", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete range claim: %v", err)
	}

	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 0 {
		t.Errorf("%d pools survived the release", n)
	}
	var identities, carves int
	if err := db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ipam_pool_identity WHERE class_name = 'tenant-vpc'`).Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if identities != 0 {
		t.Errorf("%d identity rows survived the release", identities)
	}
	if err := db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ipam_cidr_allocations WHERE purpose = 'PoolCarve'`).Scan(&carves); err != nil {
		t.Fatalf("count carves: %v", err)
	}
	if carves != 0 {
		t.Errorf("%d carves survived the release", carves)
	}

	second := create(t, rest, rangeClaim("vpc-b", "net-b"))
	if second.Status.AllocatedCIDR != first.Status.AllocatedCIDR {
		t.Errorf("after release the next network got %s, want the freed %s",
			second.Status.AllocatedCIDR, first.Status.AllocatedCIDR)
	}
}

// A range with anything inside it is not released. Freeing it would hand the
// next tenant a block another tenant's endpoints are still addressed out of.
func TestReleasingAnOccupiedRangeIsRefused(t *testing.T) {
	rest, db := newTenantREST(t)
	ctx := claimCtx(testProject)

	before := create(t, rest, rangeClaim("vpc-a", "net-a"))
	create(t, rest, endpointClaim("ep-1", "net-a", "lon1"))

	_, _, err := rest.Delete(ctx, "vpc-a", nil, &metav1.DeleteOptions{})
	if !apierrors.IsConflict(err) {
		t.Fatalf("deleting an occupied range returned %v, want a conflict", err)
	}

	// Refused means untouched, not half-released.
	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 1 {
		t.Errorf("%d pools after the refused release, want the range to survive", n)
	}
	obj, err := rest.Get(ctx, "vpc-a", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the refused claim is gone: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != before.Status.AllocatedCIDR {
		t.Errorf("claim now reports %s, want the unchanged %s", got, before.Status.AllocatedCIDR)
	}

	// Emptying it makes the release succeed.
	if _, _, err := rest.Delete(ctx, "ep-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete endpoint claim: %v", err)
	}
	if _, _, err := rest.Delete(ctx, "vpc-a", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete emptied range: %v", err)
	}
	// The per-region pool the endpoint conjured goes with the range. It is
	// derived space, so leaving it would make a range that ever held one
	// endpoint impossible to give back.
	if n := countPoolsProvisionedBy(t, db, "tenant-subnet"); n != 0 {
		t.Errorf("%d subnet pools survived the release of the range above them", n)
	}
	if n := countPoolsProvisionedBy(t, db, "tenant-vpc"); n != 0 {
		t.Errorf("%d ranges survived their own release", n)
	}
}

// poolPer is what makes a class provision pools. A class without it holds no
// range, and a request for one is refused rather than answered with a pool no
// allocation would ever be served from — which is the orphan the caller was
// trying to avoid in the first place.
func TestAClassThatProvisionsNoPoolsHoldsNoRange(t *testing.T) {
	rest, _ := newTenantREST(t)

	claim := rangeClaim("orphan", "net-a")
	claim.Spec.ClassName = "tenant-endpoint"
	claim.Spec.Scope["location"] = locationRef("lon1")

	_, err := rest.Create(claimCtx(testProject), claim, nil, &metav1.CreateOptions{})
	if err == nil {
		t.Fatal("a range was handed out for a class that provisions no pools")
	}
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("refusal was %v, want a bad request", err)
	}
}

// A range claim short the role its class provisions pools per cannot resolve to
// a range at all, and says which role is missing.
func TestARangeClaimMissingItsScopeRoleIsRefused(t *testing.T) {
	rest, _ := newTenantREST(t)

	claim := rangeClaim("vpc-a", "net-a")
	claim.Spec.Scope = nil

	if _, err := rest.Create(claimCtx(testProject), claim, nil, &metav1.CreateOptions{}); err == nil {
		t.Fatal("a range was handed out for a scope naming no network")
	}
}
