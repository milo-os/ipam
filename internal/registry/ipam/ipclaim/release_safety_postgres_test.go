package ipclaim

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// Patching claimRef off a bound allocation is the first half of handing its
// address out twice: the second half is deleting the now unbound-looking
// object. The update is refused, so the object keeps naming its claim.
func TestClearingClaimRefOnABoundAllocationIsRefused(t *testing.T) {
	r, allocREST, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	allocationName := created.(*ipam.IPClaim).Status.BoundAllocationRef.Name

	obj, err := allocREST.Get(ctx, allocationName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get IPAllocation: %v", err)
	}
	unbound := obj.(*ipam.IPAllocation).DeepCopy()
	unbound.Spec.ClaimRef = nil
	_, _, err = allocREST.Update(ctx, allocationName, rest.DefaultUpdatedObjectInfo(unbound),
		nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("clearing spec.claimRef returned %v, want 422 Invalid", err)
	}

	// Metadata is still the caller's.
	labelled := obj.(*ipam.IPAllocation).DeepCopy()
	labelled.Labels = map[string]string{"team": "edge"}
	if _, _, err := allocREST.Update(ctx, allocationName, rest.DefaultUpdatedObjectInfo(labelled),
		nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{}); err != nil {
		t.Errorf("a label-only update was refused: %v", err)
	}
}

// An object that already reads as unbound over a bound row — written before
// spec was immutable, or by hand — does not get its address released. The row
// is what decides, and the refusal is the same 409 an honest object gets.
func TestReleaseOfABoundRowIsRefusedWhateverTheObjectSays(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claim := created.(*ipam.IPClaim)
	allocationKey := allocationKeyFor(claim)

	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_objects
		    SET data = convert_to((ipam_data_to_jsonb(data) #- '{spec,claimRef}')::text, 'UTF8')
		  WHERE key = $1`, allocationKey); err != nil {
		t.Fatalf("clear claimRef on the stored object: %v", err)
	}
	obj, err := allocREST.Get(ctx, claim.Status.BoundAllocationRef.Name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get IPAllocation: %v", err)
	}
	if obj.(*ipam.IPAllocation).Spec.ClaimRef != nil {
		t.Fatal("setup: the stored object still names its claim")
	}

	_, _, err = allocREST.Delete(ctx, claim.Status.BoundAllocationRef.Name, nil, &metav1.DeleteOptions{})
	if !apierrors.IsConflict(err) {
		t.Fatalf("releasing a bound row returned %v, want 409 Conflict", err)
	}
	if !strings.Contains(err.Error(), "claim-1") {
		t.Errorf("refusal %q does not name the claim holding the row", err)
	}
	row, ok := readAllocationRow(t, db, allocationKey)
	if !ok {
		t.Fatal("the refused release deleted the allocation row")
	}
	if row.claimKey == nil {
		t.Error("the refused release unbound the row")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE key = $1`, allocationKey); n != 1 {
		t.Error("the refused release deleted the IPAllocation object")
	}
}

// The policy is read off the row at release, so an edit to the claim's would
// change only what the claim says. It is refused instead.
func TestReclaimPolicyCannotBeChangedAfterCreate(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	ctx := claimCtx(testProject)

	created, err := r.Create(ctx, retainClaim("claim-1", ipam.ReclaimRetain), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	edited := created.(*ipam.IPClaim).DeepCopy()
	edited.Spec.ReclaimPolicy = ipam.ReclaimDelete
	_, _, err = r.Update(ctx, "claim-1", rest.DefaultUpdatedObjectInfo(edited),
		nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("changing spec.reclaimPolicy returned %v, want 422 Invalid", err)
	}

	// And the release does what the claim was created with.
	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := readAllocationRow(t, db, allocationKeyFor(created.(*ipam.IPClaim))); !ok {
		t.Error("the allocation was freed; the claim was created with Retain")
	}
}

// Releasing a retained allocation and recreating the claim that retained it
// meet on one allocation key in one pool. The release used to delete the row
// before locking the pool while the create locked the pool before inserting the
// row, so the two could each hold what the other waited for and Postgres
// killed one with 40P01, which the caller saw as a 500.
//
// The interleaving is forced rather than hoped for: a third connection holds
// the pool row while both requests start, in a known order, and lets go once
// both are queued. With the recreate queued first, the old order deadlocked on
// every run. With every path taking the pool first, the first in the queue
// simply wins:
//
//   - recreate first: it reclaims the retained row, and the release then finds
//     it bound and is refused with 409;
//   - release first: it frees the address, and the recreate, finding no row
//     left to reclaim, is handed a new allocation.
func TestReleasingARetainedAllocationAndRecreatingItsClaimSerialiseOnThePool(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	allocationName := allocationNameFor("default", "claim-1")
	poolKey := tenant.Identity{Name: testProject}.ResourceKey("ippools", "us-east")

	retain := func() {
		t.Helper()
		if _, err := r.Get(ctx, "claim-1", &metav1.GetOptions{}); apierrors.IsNotFound(err) {
			if _, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
				t.Fatalf("Create: %v", err)
			}
		}
		if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
			t.Fatalf("Delete claim: %v", err)
		}
	}
	release := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			_, _, err := allocREST.Delete(ctx, allocationName, nil, &metav1.DeleteOptions{})
			done <- err
		}()
		return done
	}
	recreate := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
			done <- err
		}()
		return done
	}

	for round := 0; round < 10; round++ {
		recreateFirst := round%2 == 0
		retain()

		holder, err := db.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin holder: %v", err)
		}
		if _, err := holder.Exec(context.Background(),
			`SELECT 1 FROM ipam_objects WHERE key = $1 FOR UPDATE`, poolKey); err != nil {
			_ = holder.Rollback(context.Background())
			t.Fatalf("hold the pool: %v", err)
		}
		var releaseDone, recreateDone <-chan error
		if recreateFirst {
			recreateDone = recreate()
			testdb.WaitForLockWaiters(t, db, 1)
			releaseDone = release()
		} else {
			releaseDone = release()
			testdb.WaitForLockWaiters(t, db, 1)
			recreateDone = recreate()
		}
		testdb.WaitForLockWaiters(t, db, 2)
		if err := holder.Commit(context.Background()); err != nil {
			t.Fatalf("let go of the pool: %v", err)
		}
		releaseErr, claimErr := <-releaseDone, <-recreateDone

		for what, err := range map[string]error{"release": releaseErr, "recreate": claimErr} {
			if isDeadlock(err) {
				t.Fatalf("round %d (recreate first: %v): %s deadlocked: %v", round, recreateFirst, what, err)
			}
		}
		if claimErr != nil {
			t.Fatalf("round %d (recreate first: %v): recreate returned %v, want the claim bound", round, recreateFirst, claimErr)
		}
		if recreateFirst && !apierrors.IsConflict(releaseErr) {
			t.Fatalf("round %d: release queued after the reclaim returned %v, want 409 over the bound row", round, releaseErr)
		}
		if !recreateFirst && releaseErr != nil {
			t.Fatalf("round %d: release queued first failed: %v", round, releaseErr)
		}
	}
}

// The same race run free, as a smoke test under real scheduling. The forced
// interleaving above is the regression guard; this one shows nothing else goes
// wrong across many rounds.
func TestReleasingARetainedAllocationRacingItsClaimsRecreateNeverDeadlocks(t *testing.T) {
	const rounds = 200
	r, allocREST, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	allocationName := allocationNameFor("default", "claim-1")

	retain := func() {
		t.Helper()
		if _, err := r.Get(ctx, "claim-1", &metav1.GetOptions{}); apierrors.IsNotFound(err) {
			if _, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
				t.Fatalf("Create: %v", err)
			}
		}
		if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
			t.Fatalf("Delete claim: %v", err)
		}
	}

	var released, reclaimed int
	for i := 0; i < rounds; i++ {
		retain()

		var (
			wg                   sync.WaitGroup
			releaseErr, claimErr error
			start                = make(chan struct{})
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, releaseErr = allocREST.Delete(ctx, allocationName, nil, &metav1.DeleteOptions{})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, claimErr = r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
		}()
		close(start)
		wg.Wait()

		for what, err := range map[string]error{"release": releaseErr, "recreate": claimErr} {
			if isDeadlock(err) {
				t.Fatalf("round %d: %s deadlocked: %v", i, what, err)
			}
			var status apierrors.APIStatus
			if errors.As(err, &status) && status.Status().Code >= 500 {
				t.Fatalf("round %d: %s returned %d: %v", i, what, status.Status().Code, err)
			}
		}
		if claimErr != nil {
			t.Fatalf("round %d: recreate returned %v, want the claim bound either way", i, claimErr)
		}
		switch {
		case releaseErr == nil:
			released++
		case apierrors.IsConflict(releaseErr):
			reclaimed++
		default:
			t.Fatalf("round %d: release returned %v, want success or 409", i, releaseErr)
		}
	}
	t.Logf("%d rounds: the release won %d times, the reclaim won %d times", rounds, released, reclaimed)
}

// isDeadlock reports whether err carries SQLSTATE 40P01, wrapped or flattened
// into a message by the registry.
func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		return true
	}
	return strings.Contains(err.Error(), "40P01") || strings.Contains(err.Error(), "deadlock")
}

// An IPAllocation with no row behind it could take the name a future claim's
// allocation needs, so the endpoint does not offer create: the installer
// advertises and routes the verb only for storage that implements it.
func TestTheAllocationEndpointOffersNoCreate(t *testing.T) {
	_, allocREST, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	if _, ok := any(allocREST).(rest.Creater); ok {
		t.Error("IPAllocation storage implements rest.Creater; allocations must be written only by their claim")
	}
	for name, ok := range map[string]bool{
		"Getter":          implements[rest.Getter](allocREST),
		"Lister":          implements[rest.Lister](allocREST),
		"Watcher":         implements[rest.Watcher](allocREST),
		"Updater":         implements[rest.Updater](allocREST),
		"GracefulDeleter": implements[rest.GracefulDeleter](allocREST),
	} {
		if !ok {
			t.Errorf("IPAllocation storage no longer implements rest.%s", name)
		}
	}
}

func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}
