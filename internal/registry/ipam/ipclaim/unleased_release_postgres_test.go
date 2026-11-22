package ipclaim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/ipallocation"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// fakeReleaseChecker answers every release-unleased check the same way and
// records what it was asked.
type fakeReleaseChecker struct {
	allow bool

	mu    sync.Mutex
	asked []string
}

func releaseChecker(allow bool) *fakeReleaseChecker { return &fakeReleaseChecker{allow: allow} }

func (c *fakeReleaseChecker) CanReleaseUnleased(_ context.Context, namespace, name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, namespace+"/"+name)
	return c.allow, nil
}

func (c *fakeReleaseChecker) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

// allocationStorageWith is a second IPAllocation endpoint over db, answering
// release-unleased checks with checker: a caller other than the admin-like one
// newLifecycleREST wires.
func allocationStorageWith(t *testing.T, db *pgxpool.Pool, checker access.UnleasedReleaseChecker) *ipallocation.ReleasingREST {
	t.Helper()
	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion))
	rest, _, err := ipallocation.NewAllocationStorage(scheme, getter, allocator.NewPostgresPrefixAllocator(), db, checker)
	if err != nil {
		t.Fatalf("allocation storage: %v", err)
	}
	t.Cleanup(rest.Destroy)
	return rest
}

// deadlineOf reads the stored expires_at of claim's allocation row as text:
// "" for NULL, "infinity", or a timestamp.
func deadlineOf(t *testing.T, db *pgxpool.Pool, claim *ipam.IPClaim) string {
	t.Helper()
	var expires *string
	if err := db.QueryRow(context.Background(),
		`SELECT expires_at::text FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocationKeyFor(claim),
	).Scan(&expires); err != nil {
		t.Fatalf("read deadline of %s: %v", claim.Name, err)
	}
	if expires == nil {
		return ""
	}
	return *expires
}

// expectUnleasedForbidden fails unless err is the 403 refusing to release an
// allocation with no lease deadline, and the allocation row and object are
// both still there.
func expectUnleasedForbidden(t *testing.T, db *pgxpool.Pool, err error, claim *ipam.IPClaim) {
	t.Helper()
	if !apierrors.IsForbidden(err) {
		t.Fatalf("release returned %v, want 403 Forbidden", err)
	}
	for _, want := range []string{"no lease deadline", "IPAM admin", "ipallocations." + access.VerbReleaseUnleased} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err.Error(), want)
		}
	}
	assertStillRetained(t, db, claim)
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE key = $1`, allocationKeyFor(claim)); n != 1 {
		t.Errorf("%d IPAllocation objects after the refused release, want 1", n)
	}
}

// A class whose own default is Retain, with no lease and no pool cap, holds
// its address until someone releases it: the VPC and subnet classes. A
// consumer may delete IPAllocations, but not this one; an admin may.
func TestAnUnleasedRetainedAllocationNeedsTheReleaseUnleasedPermission(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	claim := retainClaim("vpc", "")
	retainAndDelete(t, r, ctx, claim)
	if got := deadlineOf(t, db, claim); got != "infinity" {
		t.Fatalf("setup: expires_at = %q, want infinity", got)
	}
	name := allocationNameFor(claim.Namespace, claim.Name)

	consumer := releaseChecker(false)
	_, _, err := allocationStorageWith(t, db, consumer).Delete(ctx, name, nil, &metav1.DeleteOptions{})
	expectUnleasedForbidden(t, db, err, claim)
	if got := consumer.calls(); len(got) != 1 || got[0] != "default/"+name {
		t.Errorf("checker asked about %v, want once about default/%s", got, name)
	}

	// A dry run is refused the same way.
	_, _, err = allocationStorageWith(t, db, releaseChecker(false)).Delete(ctx, name, nil,
		&metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
	expectUnleasedForbidden(t, db, err, claim)

	// No authorizer means nothing vouched for the caller.
	_, _, err = allocationStorageWith(t, db, nil).Delete(ctx, name, nil, &metav1.DeleteOptions{})
	expectUnleasedForbidden(t, db, err, claim)

	if _, _, err := allocationStorageWith(t, db, releaseChecker(true)).Delete(ctx, name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("admin release: %v", err)
	}
	if _, ok := readAllocationRow(t, db, allocationKeyFor(claim)); ok {
		t.Error("the admin's release left the allocation row")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE key = $1`, allocationKeyFor(claim)); n != 0 {
		t.Error("the admin's release left the IPAllocation")
	}
}

// A deadline not yet evaluated is no deadline: nothing says the address will
// ever be released, so the consumer is refused as for 'infinity'.
func TestAnUnevaluatedDeadlineCountsAsNone(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	setPoolCap(t, db, "us-east", &metav1.Duration{Duration: time.Hour})
	ctx := claimCtx(testProject)
	claim := retainClaim("pending", "")
	retainAndDelete(t, r, ctx, claim)
	if _, err := db.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = NULL WHERE allocation_key = $1`,
		allocationKeyFor(claim)); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}

	_, _, err := allocationStorageWith(t, db, releaseChecker(false)).Delete(ctx,
		allocationNameFor(claim.Namespace, claim.Name), nil, &metav1.DeleteOptions{})
	expectUnleasedForbidden(t, db, err, claim)
}

// A retained allocation with a finite deadline is the consumer's to give back,
// running or overdue: releasing it only brings the deadline forward. The
// release-unleased permission is never asked about.
func TestALeasedRetainedAllocationIsAConsumersToRelease(t *testing.T) {
	for _, overdue := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "overdue"}[overdue], func(t *testing.T) {
			r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
			setPoolCap(t, db, "us-east", &metav1.Duration{Duration: time.Hour})
			ctx := claimCtx(testProject)
			claim := retainClaim("leased", "")
			retainAndDelete(t, r, ctx, claim)
			if got := deadlineOf(t, db, claim); got == "" || got == "infinity" {
				t.Fatalf("setup: expires_at = %q, want a finite deadline", got)
			}
			if overdue {
				if _, err := db.Exec(ctx,
					`UPDATE ipam_cidr_allocations SET expires_at = now() - interval '1 minute' WHERE allocation_key = $1`,
					allocationKeyFor(claim)); err != nil {
					t.Fatalf("make overdue: %v", err)
				}
			}

			consumer := releaseChecker(false)
			if _, _, err := allocationStorageWith(t, db, consumer).Delete(ctx,
				allocationNameFor(claim.Namespace, claim.Name), nil, &metav1.DeleteOptions{}); err != nil {
				t.Fatalf("consumer release of a leased allocation: %v", err)
			}
			if _, ok := readAllocationRow(t, db, allocationKeyFor(claim)); ok {
				t.Error("the release left the allocation row")
			}
			if got := consumer.calls(); len(got) != 0 {
				t.Errorf("release-unleased was asked about %v for a leased allocation", got)
			}
		})
	}
}

// The deadline that decides is the one under the locks the release deletes
// under, not the one an earlier read saw. Here the row has a lease when the
// release starts and loses it, in a transaction holding the pool, before the
// release gets the lock: the release is refused, and the row stays.
func TestTheReleaseJudgesTheDeadlineUnderThePoolLock(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	setPoolCap(t, db, "us-east", &metav1.Duration{Duration: time.Hour})
	ctx := claimCtx(testProject)
	claim := retainClaim("raced", "")
	retainAndDelete(t, r, ctx, claim)
	if got := deadlineOf(t, db, claim); got == "" || got == "infinity" {
		t.Fatalf("setup: expires_at = %q, want a finite deadline", got)
	}

	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM ipam_objects WHERE key = $1 FOR UPDATE`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", "us-east")); err != nil {
		t.Fatalf("lock pool: %v", err)
	}
	if _, err := holder.Exec(ctx, `UPDATE ipam_cidr_allocations SET expires_at = 'infinity' WHERE allocation_key = $1`,
		allocationKeyFor(claim)); err != nil {
		t.Fatalf("drop deadline: %v", err)
	}

	consumer := releaseChecker(false)
	rest := allocationStorageWith(t, db, consumer)
	done := make(chan error, 1)
	go func() {
		_, _, err := rest.Delete(ctx, allocationNameFor(claim.Namespace, claim.Name), nil, &metav1.DeleteOptions{})
		done <- err
	}()
	testdb.WaitForLockWaiters(t, db, 1)
	if got := consumer.calls(); len(got) != 0 {
		t.Fatalf("release-unleased was asked about %v before the release held the lock; the unlocked read saw a deadline", got)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	expectUnleasedForbidden(t, db, <-done, claim)
	if got := consumer.calls(); len(got) != 1 {
		t.Errorf("release-unleased asked %d times, want once, under the lock", len(got))
	}
}
