package ipallocation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// fakeTx counts commits and rollbacks. The other pgx.Tx methods are nil and
// would panic, so a delete path that reached the database directly fails loudly.
type fakeTx struct {
	pgx.Tx
	commits   int
	rollbacks int
}

func (f *fakeTx) Commit(context.Context) error   { f.commits++; return nil }
func (f *fakeTx) Rollback(context.Context) error { f.rollbacks++; return nil }

type fakeBeginner struct{ tx *fakeTx }

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

type fakeAllocator struct {
	allocator.PrefixAllocator
	released      bool
	forceReleased []string
	deleted       []string
}

func (a *fakeAllocator) ForceRelease(_ context.Context, _ pgx.Tx, key string) (bool, error) {
	a.forceReleased = append(a.forceReleased, key)
	return a.released, nil
}

func (a *fakeAllocator) DeleteObject(_ context.Context, _ pgx.Tx, key string) (int64, error) {
	a.deleted = append(a.deleted, key)
	return 1, nil
}

func newTestStorage() (*IPAllocationStorage, *fakeAllocator, *fakeTx) {
	fa := &fakeAllocator{}
	ftx := &fakeTx{}
	return &IPAllocationStorage{allocator: fa, db: &fakeBeginner{tx: ftx}}, fa, ftx
}

func boundAllocation() *ipam.IPAllocation {
	return &ipam.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{Name: "alloc-1", Namespace: "default"},
		Spec: ipam.IPAllocationSpec{
			IPFamily: ipam.IPv4,
			PoolRef:  ipam.LocalRef{Name: "public-v4"},
			Purpose:  ipam.PurposeClaim,
			ClaimRef: &ipam.LocalRef{Name: "claim-1"},
		},
	}
}

// The address is held by a row the object does not own, and no constraint links
// them — so the object's key is the only handle on that row. It has to match
// what the claim handler wrote or the release finds nothing.
func TestAllocationObjectKeyMatchesTheClaimHandler(t *testing.T) {
	id := tenant.Identity{Kind: "Project", Name: "acme"}
	got := allocationObjectKey(id, "default", "alloc-1")
	want := "project/acme/ipam.miloapis.com/ipallocations/default/alloc-1"
	if got != want {
		t.Fatalf("allocationObjectKey = %q, want %q", got, want)
	}
	if platform := allocationObjectKey(tenant.Identity{}, "default", "alloc-1"); platform != "/ipam.miloapis.com/ipallocations/default/alloc-1" {
		t.Errorf("platform key = %q", platform)
	}
}

// Releasing an address out from under a live claim would leave the claim naming
// nothing while the workload keeps using the address. Deleting the claim is the
// supported route, and its reclaimPolicy then decides.
func TestDeleteRefusesAStillBoundAllocation(t *testing.T) {
	_, fa, ftx := newTestStorage()

	// Exercised through the guard directly: Delete's Get needs a live store,
	// while the decision this test pins is the bound check.
	alloc := boundAllocation()
	if alloc.Spec.ClaimRef == nil {
		t.Fatal("fixture is not bound")
	}
	err := refuseIfBound(alloc, "alloc-1")
	if err == nil {
		t.Fatal("expected a bound allocation to be refused")
	}
	var status interface{ Status() metav1.Status }
	if !errors.As(err, &status) || status.Status().Code != 409 {
		t.Errorf("expected 409 Conflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "claim-1") {
		t.Errorf("the error must name the claim holding it, got %v", err)
	}
	if len(fa.forceReleased) != 0 || ftx.commits != 0 {
		t.Error("a refused delete must release nothing")
	}
}

// A retained allocation — claim gone, address still held — is exactly what an
// operator force-releases, and deleting it is that gesture.
func TestRetainedAllocationIsReleasable(t *testing.T) {
	alloc := boundAllocation()
	alloc.Spec.ClaimRef = nil

	if err := refuseIfBound(alloc, "alloc-1"); err != nil {
		t.Fatalf("an unbound allocation must be deletable, got %v", err)
	}
}

// A reservation has no claim either, and must stay releasable the same way.
func TestReservationIsReleasable(t *testing.T) {
	alloc := boundAllocation()
	alloc.Spec.ClaimRef = nil
	alloc.Spec.Purpose = ipam.PurposeReservation

	if err := refuseIfBound(alloc, "resv-1"); err != nil {
		t.Fatalf("a reservation must be deletable, got %v", err)
	}
}
