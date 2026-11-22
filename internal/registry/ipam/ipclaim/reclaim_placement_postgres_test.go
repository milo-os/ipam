package ipclaim

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// expectMismatch fails unless err is a RetainedAllocationMismatch naming field.
func expectMismatch(t *testing.T, err error, field string) {
	t.Helper()
	if ipamerrors.ReasonFor(err) != ipamerrors.ReasonRetainedAllocationMismatch {
		t.Fatalf("recreate returned %v (reason %q), want RetainedAllocationMismatch on %s", err, ipamerrors.ReasonFor(err), field)
	}
	if _, got, _ := ipamerrors.RetainedAllocationMismatch(err); got != field {
		t.Errorf("mismatch names field %q, want %q: %v", got, field, err)
	}
}

func allocationKeyIn(project string, claim *ipam.IPClaim) string {
	return allocationObjectKey(tenant.Identity{Name: project}, claim.Namespace, allocationNameFor(claim.Namespace, claim.Name))
}

func locationScope(name string) map[string]ipam.ScopeRef {
	return map[string]ipam.ScopeRef{"location": locationRef(name)}
}

// A chained class with poolPer: [location] holds one pool per location. A
// recreate naming another location resolves to another pool, and is refused
// rather than handed an address from the first.
func TestReclaimRefusesAnotherPoolPerLocation(t *testing.T) {
	r, db := newTenantREST(t)
	// The endpoint class defaults to Delete, so the claim's Retain needs a
	// lease to retain under.
	seedNamedClass(t, db, "tenant-endpoint", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-subnet", DefaultPrefixLength: 96,
		RetentionLease: &metav1.Duration{Duration: 24 * time.Hour},
	})
	ctx := claimCtx(testProject)

	retained := endpointClaim("ep-1", "net-a", "ams")
	retained.Spec.ReclaimPolicy = ipam.ReclaimRetain
	created := create(t, r, retained.DeepCopy())
	if _, _, err := r.Delete(ctx, "ep-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// The lhr subnet exists too, so the refusal is about which pool, not about
	// one that is missing.
	create(t, r, endpointClaim("ep-lhr", "net-a", "lhr"))

	_, err := r.Create(ctx, endpointClaim("ep-1", "net-a", "lhr"), nil, &metav1.CreateOptions{})
	expectMismatch(t, err, "spec.scope")
	assertStillRetained(t, db, retained)

	// The same location still reclaims.
	again := endpointClaim("ep-1", "net-a", "ams")
	obj, err := r.Create(ctx, again, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("recreate in the retained location: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != created.Status.AllocatedCIDR {
		t.Errorf("got %q, want the retained %q", got, created.Status.AllocatedCIDR)
	}
}

// newPlacementREST is the lifecycle fixture with the seeded pool replaced by
// one declaring location ams, under a class that separates nothing, so only
// the pool's own scope tells two locations apart.
func newPlacementREST(t *testing.T) (*AllocatingREST, *pgxpool.Pool) {
	t.Helper()
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	seedPoolIn(t, db, testProject, "us-east", "10.0.0.0/16", map[string]ipamv1alpha1.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "ams"},
	}, "standard")
	seedPoolIn(t, db, testProject, "lhr-pool", "10.3.0.0/16", map[string]ipamv1alpha1.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "lhr"},
	}, "standard")
	return r, db
}

func locatedClaim(name, location string) *ipam.IPClaim {
	c := retainClaim(name, "")
	c.Spec.Scope = locationScope(location)
	return c
}

// A pool states the scope it serves. A recreate whose scope that pool does not
// serve would never have been placed there, so it is refused, even where the
// class's uniqueWithin makes the two scopes one address space.
func TestReclaimRefusesAPoolThatDoesNotServeTheScope(t *testing.T) {
	r, db := newPlacementREST(t)
	ctx := claimCtx(testProject)
	retainAndDelete(t, r, ctx, locatedClaim("claim-1", "ams"))

	_, err := r.Create(ctx, locatedClaim("claim-1", "lhr"), nil, &metav1.CreateOptions{})
	expectMismatch(t, err, "spec.scope")
	assertStillRetained(t, db, locatedClaim("claim-1", "ams"))
}

// A pool that has stopped offering the class is not one a claim of the class
// is placed in.
func TestReclaimRefusesAPoolThatNoLongerOffersTheClass(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	seedNamedClass(t, db, "other", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24})
	retainAndDelete(t, r, ctx, retainClaim("claim-1", ""))

	seedPoolIn(t, db, testProject, "us-east", "10.0.0.0/16", nil, "other")

	_, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	expectMismatch(t, err, "spec.className")
	assertStillRetained(t, db, retainClaim("claim-1", ""))
}

// A class may gain an address scope it did not have. An address handed out
// before it is not one handed out as that scope.
func TestReclaimRefusesAnAddressScopeTheAllocationWasNotHandedOutUnder(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	retainAndDelete(t, r, ctx, retainClaim("claim-1", ""))

	seedNamedClass(t, db, "standard", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
		AddressScope: ipamv1alpha1.AddressScopePublic,
	})

	_, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	expectMismatch(t, err, "spec.addressScope")
	assertStillRetained(t, db, retainClaim("claim-1", ""))
}

// A class is its defining project and its name. A consumer's own "standard",
// replaced by a reference to the platform's "standard", is another class: the
// address held under the first is not handed back under the second. The same
// holds for a row written before the defining project was recorded, where the
// pool is what tells them apart.
func TestReclaimRefusesAnotherProjectsClassOfTheSameName(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "recorded"
		if legacy {
			name = "unrecorded"
		}
		t.Run(name, func(t *testing.T) {
			const consumer = "consumer"
			r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
			ctx := claimCtx(consumer)
			seedClassIn(t, db, consumer, "standard", ipamv1alpha1.IPClassSpec{
				IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
			})
			seedPoolIn(t, db, consumer, "own", "10.4.0.0/16", nil, "standard")

			claim := retainClaim("claim-1", "")
			if _, err := r.Create(ctx, claim.DeepCopy(), nil, &metav1.CreateOptions{}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			key := allocationKeyIn(consumer, claim)
			if legacy {
				if _, err := db.Exec(context.Background(),
					`UPDATE ipam_cidr_allocations SET class_project = NULL, address_scope = NULL WHERE allocation_key = $1`, key); err != nil {
					t.Fatalf("unrecord the class: %v", err)
				}
			}

			// The consumer's class is replaced by a reference to the platform's,
			// which newLifecycleREST defines and backs with its own pool.
			if _, err := db.Exec(context.Background(), `DELETE FROM ipam_objects WHERE key = $1`,
				tenant.Identity{Name: consumer}.ResourceKey("ipclasses", "standard")); err != nil {
				t.Fatalf("drop the consumer's definition: %v", err)
			}
			seedReference(t, db, consumer, "standard", false)

			_, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
			expectMismatch(t, err, "spec.className")
			row, ok := readAllocationRow(t, db, key)
			if !ok || row.claimKey != nil {
				t.Error("the refused recreate bound or freed the retained row")
			}
		})
	}
}

// A row written before the class's project was recorded still reclaims under a
// class with no parent: its pool can only be in the defining project, so the
// pool proves the class.
func TestAnUnrecordedRowReclaimsUnderAClassWithNoParent(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	held := retainAndDelete(t, r, ctx, retainClaim("claim-1", ""))
	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_cidr_allocations SET class_project = NULL, address_scope = NULL WHERE allocation_key = $1`,
		allocationKeyFor(retainClaim("claim-1", ""))); err != nil {
		t.Fatalf("unrecord the class: %v", err)
	}

	obj, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("recreate over an unrecorded row: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
		t.Errorf("got %q, want the retained %q", got, held)
	}
}

// A generated name that lands on a retained identity was chosen by nobody, so
// it is refused as a collision — which the generateName loop draws again on —
// rather than handed an unrelated claim's address.
func TestAGeneratedNameOnARetainedIdentityIsRedrawnNotReclaimed(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	retainAndDelete(t, r, ctx, retainClaim("dns-abcde", ""))

	drawn := retainClaim("dns-abcde", "")
	drawn.GenerateName = "dns-"
	_, err := r.create(ctx, drawn, nil, &metav1.CreateOptions{}, true)
	if ipamerrors.ReasonFor(err) != ipamerrors.ReasonAllocationRetained {
		t.Fatalf("a generated name on a retained identity returned %v, want AllocationRetained", err)
	}
	if !isGeneratedNameCollision(err) {
		t.Error("the refusal is not a generated-name collision; Create would return it instead of drawing again")
	}
	assertStillRetained(t, db, retainClaim("dns-abcde", ""))
}

// The identity is the namespace and name in one project. The same name in
// another namespace, or in another project, is another identity: it cannot see
// the retained row, and is handed a fresh address.
func TestARetainedAllocationIsInvisibleOutsideItsNamespaceAndProject(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	held := retainAndDelete(t, r, ctx, retainClaim("claim-1", ""))

	otherNS := retainClaim("claim-1", "")
	otherNS.Namespace = "other"
	obj, err := r.Create(genericapirequest.WithNamespace(ctx, "other"), otherNS, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create in another namespace: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got == held {
		t.Errorf("a claim in another namespace was handed the retained %q", held)
	}

	const elsewhere = "elsewhere"
	seedClassIn(t, db, elsewhere, "standard", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
	})
	// The same range, so an address equal to the retained one would prove
	// nothing about which row it came from; the row is checked instead.
	seedPoolIn(t, db, elsewhere, "us-east", "10.0.0.0/16", nil, "standard")
	if _, err := r.Create(claimCtx(elsewhere), retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create in another project: %v", err)
	}
	if row, ok := readAllocationRow(t, db, allocationKeyIn(elsewhere, retainClaim("claim-1", ""))); !ok || row.claimKey == nil {
		t.Error("the other project's claim holds no allocation of its own")
	}
	assertStillRetained(t, db, retainClaim("claim-1", ""))
}

// A recreate racing the delete that retains the address, queued on the pool in
// a known order. Delete first: the address is retained and the recreate takes
// it back. Recreate first: the name is still held, so it is refused, and the
// delete then retains. Neither deadlocks.
func TestARecreateRacingTheRetainingDeleteSerialisesOnThePool(t *testing.T) {
	for _, deleteFirst := range []bool{true, false} {
		name := "delete first"
		if !deleteFirst {
			name = "recreate first"
		}
		t.Run(name, func(t *testing.T) {
			r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
			ctx := claimCtx(testProject)
			poolKey := tenant.Identity{Name: testProject}.ResourceKey("ippools", "us-east")
			created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			held := created.(*ipam.IPClaim).Status.AllocatedCIDR

			holder, err := db.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin holder: %v", err)
			}
			if _, err := holder.Exec(context.Background(),
				`SELECT 1 FROM ipam_objects WHERE key = $1 FOR UPDATE`, poolKey); err != nil {
				_ = holder.Rollback(context.Background())
				t.Fatalf("hold the pool: %v", err)
			}
			del := func() <-chan error {
				done := make(chan error, 1)
				go func() {
					_, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{})
					done <- err
				}()
				return done
			}
			type outcome struct {
				cidr string
				err  error
			}
			recreate := func() <-chan outcome {
				done := make(chan outcome, 1)
				go func() {
					obj, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
					o := outcome{err: err}
					if c, ok := obj.(*ipam.IPClaim); ok {
						o.cidr = c.Status.AllocatedCIDR
					}
					done <- o
				}()
				return done
			}
			var deleted <-chan error
			var recreated <-chan outcome
			if deleteFirst {
				deleted = del()
				testdb.WaitForLockWaiters(t, db, 1)
				recreated = recreate()
			} else {
				recreated = recreate()
				testdb.WaitForLockWaiters(t, db, 1)
				deleted = del()
			}
			testdb.WaitForLockWaiters(t, db, 2)
			if err := holder.Commit(context.Background()); err != nil {
				t.Fatalf("let go of the pool: %v", err)
			}
			delErr, rec := <-deleted, <-recreated

			for what, err := range map[string]error{"delete": delErr, "recreate": rec.err} {
				if isDeadlock(err) {
					t.Fatalf("%s deadlocked: %v", what, err)
				}
			}
			if delErr != nil {
				t.Fatalf("the retaining delete failed: %v", delErr)
			}
			if deleteFirst {
				if rec.err != nil {
					t.Fatalf("the recreate queued after the delete returned %v, want the retained address", rec.err)
				}
				if rec.cidr != held {
					t.Errorf("recreate got %q, want the retained %q", rec.cidr, held)
				}
				return
			}
			if ipamerrors.ReasonFor(rec.err) != ipamerrors.ReasonClaimExists || !apierrors.IsConflict(rec.err) {
				t.Fatalf("the recreate queued before the delete returned %v, want ClaimExists", rec.err)
			}
			assertStillRetained(t, db, retainClaim("claim-1", ""))
		})
	}
}
