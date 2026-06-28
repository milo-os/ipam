package ipclaim

import (
	"context"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
)

// --- fakes -----------------------------------------------------------------

// fakeTx records whether the transaction was committed or rolled back. Only
// Commit/Rollback are exercised by Create; the other pgx.Tx methods are left
// nil and would panic if the code under test unexpectedly touched the DB.
type fakeTx struct {
	pgx.Tx
	commits   int
	rollbacks int
}

func (f *fakeTx) Commit(context.Context) error   { f.commits++; return nil }
func (f *fakeTx) Rollback(context.Context) error { f.rollbacks++; return nil }

type fakeBeginner struct{ tx *fakeTx }

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

// fakeAllocator computes a CIDR without any real persistence and counts the
// mutating calls so the test can assert nothing was written under dry-run.
type fakeAllocator struct {
	cidr      string
	allocateN int
	insertN   int
	releaseN  int
	deleteN   int
	updateN   int
	nextRV    int64

	// captured keys, so tests can assert the tenant prefix is applied.
	gotPoolKey      string
	gotOwnerProject string
	gotInsertKeys   []string
}

func (a *fakeAllocator) AllocatePrefix(_ context.Context, _ pgx.Tx, poolKey string, _ int, _ string, _ string, ownerProject string) (string, error) {
	a.allocateN++
	a.gotPoolKey = poolKey
	a.gotOwnerProject = ownerProject
	return a.cidr, nil
}

func (a *fakeAllocator) InsertObject(_ context.Context, _ pgx.Tx, key, _, _, _ string, _ []byte) (int64, error) {
	a.insertN++
	a.gotInsertKeys = append(a.gotInsertKeys, key)
	a.nextRV++
	return a.nextRV, nil
}

func (a *fakeAllocator) Release(_ context.Context, _ pgx.Tx, _ string) error {
	a.releaseN++
	return nil
}

func (a *fakeAllocator) DeleteObject(_ context.Context, _ pgx.Tx, _ string) (int64, error) {
	a.deleteN++
	return 0, nil
}

func (a *fakeAllocator) UpdateObject(_ context.Context, _ pgx.Tx, _ string, _ []byte) (int64, error) {
	a.updateN++
	a.nextRV++
	return a.nextRV, nil
}

// fakeCodec satisfies runtime.Codec; only Encode is reached (in the persisting
// path) and it writes a fixed placeholder.
type fakeCodec struct{}

func (fakeCodec) Encode(_ runtime.Object, w io.Writer) error {
	_, err := w.Write([]byte("x"))
	return err
}
func (fakeCodec) Identifier() runtime.Identifier { return runtime.Identifier("fake") }
func (fakeCodec) Decode(_ []byte, _ *schema.GroupVersionKind, _ runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	return nil, nil, io.EOF
}

// --- test ------------------------------------------------------------------

func newTestREST() (*AllocatingREST, *fakeAllocator, *fakeTx) {
	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	fa := &fakeAllocator{cidr: "10.0.0.0/24"}
	ftx := &fakeTx{}
	r := &AllocatingREST{
		allocator: fa,
		db:        &fakeBeginner{tx: ftx},
		strategy:  NewStrategy(scheme),
		codec:     fakeCodec{},
	}
	return r, fa, ftx
}

func newClaim() *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"},
		Spec: ipam.IPClaimSpec{
			IPFamily:     ipam.IPv4,
			PrefixLength: 24,
			PoolRef:      &ipam.NamespacedRef{Name: "us-east"},
		},
	}
}

func TestAllocatingREST_Create_DryRun(t *testing.T) {
	cases := []struct {
		name        string
		dryRun      []string
		wantInserts int
		wantCommits int
		wantRolls   int
	}{
		{
			name:        "server dry-run persists nothing",
			dryRun:      []string{metav1.DryRunAll},
			wantInserts: 0, // no IPAllocation row, no claim row
			wantCommits: 0,
			wantRolls:   1, // the allocation tx is rolled back
		},
		{
			name:        "normal create persists and commits",
			dryRun:      nil,
			wantInserts: 2, // IPAllocation + claim
			wantCommits: 1,
			wantRolls:   0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, fa, ftx := newTestREST()
			ctx := genericapirequest.WithNamespace(context.Background(), "default")

			obj, err := r.Create(ctx, newClaim(), nil, &metav1.CreateOptions{DryRun: tc.dryRun})
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}

			claim, ok := obj.(*ipam.IPClaim)
			if !ok {
				t.Fatalf("expected *ipam.IPClaim, got %T", obj)
			}
			// Status must be populated identically on both paths — the whole point
			// of dry-run is to show the would-be CIDR.
			if claim.Status.Phase != ipam.ClaimBound {
				t.Errorf("phase = %q, want Bound", claim.Status.Phase)
			}
			if claim.Status.AllocatedCIDR != "10.0.0.0/24" {
				t.Errorf("allocatedCIDR = %q, want 10.0.0.0/24", claim.Status.AllocatedCIDR)
			}
			if claim.Status.BoundAllocationRef == nil || claim.Status.BoundAllocationRef.Name == "" {
				t.Errorf("boundAllocationRef not set: %+v", claim.Status.BoundAllocationRef)
			}
			// The allocator is always consulted so the CIDR is the real next block.
			if fa.allocateN != 1 {
				t.Errorf("AllocatePrefix calls = %d, want 1", fa.allocateN)
			}
			// Persistence side effects must match the path.
			if fa.insertN != tc.wantInserts {
				t.Errorf("InsertObject calls = %d, want %d", fa.insertN, tc.wantInserts)
			}
			if ftx.commits != tc.wantCommits {
				t.Errorf("tx commits = %d, want %d", ftx.commits, tc.wantCommits)
			}
			if ftx.rollbacks != tc.wantRolls {
				t.Errorf("tx rollbacks = %d, want %d", ftx.rollbacks, tc.wantRolls)
			}
		})
	}
}
