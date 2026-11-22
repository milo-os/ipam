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

	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

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

// Releasing a retained allocation and recreating the claim that retained it
// meet on one allocation key in one pool. The release used to delete the row
// before locking the pool while the create locked the pool before inserting the
// row, so the two could each hold what the other waited for and Postgres
// killed one with 40P01, which the caller saw as a 500.
//
// With every path taking the pool first, one of them waits. Each round ends in
// one of two outcomes and nothing else: the release wins and the recreate is
// handed a fresh allocation, or the recreate wins the lock, finds the retained
// row and is refused with 409 AllocationRetained, and the release then frees it.
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

	var recreated, refused int
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
		if releaseErr != nil {
			t.Fatalf("round %d: release of the retained allocation failed: %v", i, releaseErr)
		}
		switch {
		case claimErr == nil:
			recreated++
		case apierrors.IsConflict(claimErr):
			refused++
		default:
			t.Fatalf("round %d: recreate returned %v, want success or 409", i, claimErr)
		}
	}
	t.Logf("%d rounds: recreate won after the release %d times, was refused %d times", rounds, recreated, refused)
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
