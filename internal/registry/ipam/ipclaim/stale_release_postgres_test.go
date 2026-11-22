package ipclaim

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/ipallocation"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// steppedAllocator runs a hook either side of ReleaseRetainedAllocation, so a
// test can put other requests between a release's read of the object and its
// transaction.
type steppedAllocator struct {
	allocator.PrefixAllocator
	before, after func()
}

func (s *steppedAllocator) ReleaseRetainedAllocation(ctx context.Context, tx pgx.Tx, key string, guard allocator.UnleasedReleaseGuard) error {
	if s.before != nil {
		s.before()
	}
	err := s.PrefixAllocator.ReleaseRetainedAllocation(ctx, tx, key, guard)
	if s.after != nil {
		s.after()
	}
	return err
}

// A release that read the retained IPAllocation, and then found no row because
// another release got there first, must not delete what is under that name by
// the time it deletes: a claim recreated in between has written a new, bound
// IPAllocation there. The release is refused as stale and the new object stays.
func TestAStaleReleaseLeavesARecreatedClaimsAllocationAlone(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion))
	stepped := &steppedAllocator{PrefixAllocator: allocator.NewPostgresPrefixAllocator()}
	staleREST, _, err := ipallocation.NewAllocationStorage(scheme, getter, stepped, db, releaseChecker(true))
	if err != nil {
		t.Fatalf("allocation storage: %v", err)
	}
	t.Cleanup(staleREST.Destroy)

	created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	allocationName := created.(*ipam.IPClaim).Status.BoundAllocationRef.Name
	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete claim: %v", err)
	}

	// The stale release has read the object. Another release frees the row
	// first, and once the stale one has found nothing to free, the claim is
	// recreated.
	stepped.before = func() {
		if _, _, err := allocREST.Delete(ctx, allocationName, nil, &metav1.DeleteOptions{}); err != nil {
			t.Errorf("competing release: %v", err)
		}
	}
	stepped.after = func() {
		if _, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
			t.Errorf("recreate: %v", err)
		}
	}
	_, _, err = staleREST.Delete(ctx, allocationName, nil, &metav1.DeleteOptions{})
	if !apierrors.IsConflict(err) {
		t.Errorf("stale release returned %v, want 409 Conflict", err)
	}

	obj, err := allocREST.Get(ctx, allocationName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the recreated claim's IPAllocation is gone: %v", err)
	}
	if ref := obj.(*ipam.IPAllocation).Spec.ClaimRef; ref == nil || ref.Name != "claim-1" {
		t.Errorf("IPAllocation claimRef = %v, want claim-1", ref)
	}
	if row, ok := readAllocationRow(t, db, allocationKeyFor(created.(*ipam.IPClaim))); !ok || row.claimKey == nil {
		t.Error("the recreated claim's allocation row is missing or unbound")
	}
}

// A dry-run release reads the row as a real one does, so it cannot report
// success for an allocation the real release would refuse.
func TestADryRunReleaseOfABoundRowIsRefused(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claim := created.(*ipam.IPClaim)
	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_objects
		    SET data = convert_to((ipam_data_to_jsonb(data) #- '{spec,claimRef}')::text, 'UTF8')
		  WHERE key = $1`, allocationKeyFor(claim)); err != nil {
		t.Fatalf("clear claimRef on the stored object: %v", err)
	}

	_, _, err = allocREST.Delete(ctx, claim.Status.BoundAllocationRef.Name, nil,
		&metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
	if !apierrors.IsConflict(err) {
		t.Fatalf("dry-run release of a bound row returned %v, want 409 Conflict", err)
	}

	// And a dry run of a releasable one frees nothing.
	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete claim: %v", err)
	}
	if _, _, err := allocREST.Delete(ctx, claim.Status.BoundAllocationRef.Name, nil,
		&metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		t.Fatalf("dry-run release of a retained allocation: %v", err)
	}
	if _, ok := readAllocationRow(t, db, allocationKeyFor(claim)); !ok {
		t.Error("a dry-run release freed the allocation row")
	}
	if _, err := allocREST.Get(ctx, claim.Status.BoundAllocationRef.Name, &metav1.GetOptions{}); err != nil {
		t.Errorf("a dry-run release deleted the IPAllocation: %v", err)
	}
}
