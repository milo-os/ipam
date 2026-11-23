package ipclaim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/authorization/authorizer"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/ipallocation"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
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

// claimStorageWith is a second IPClaim endpoint over db, answering
// release-unleased checks with checker.
func claimStorageWith(t *testing.T, db *pgxpool.Pool, checker access.UnleasedReleaseChecker) *AllocatingREST {
	t.Helper()
	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion)
	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(codec)
	rest, _, err := NewAllocatingStorage(scheme, getter, allocator.NewPostgresPrefixAllocator(), db, codec, nil, nil, checker)
	if err != nil {
		t.Fatalf("claim storage: %v", err)
	}
	t.Cleanup(rest.Destroy)
	return rest
}

// expectUnleasedReclaimForbidden fails unless err is the 403 refusing to
// reclaim an unleased address under Delete, and the address is still
// retained with nothing else written.
func expectUnleasedReclaimForbidden(t *testing.T, db *pgxpool.Pool, err error, claim *ipam.IPClaim, rowsBefore int) {
	t.Helper()
	if !apierrors.IsForbidden(err) {
		t.Fatalf("reclaim returned %v, want 403 Forbidden", err)
	}
	for _, want := range []string{"no lease deadline", "reclaimPolicy Delete", "reclaimPolicy: Retain", "IPAM admin",
		"ipallocations." + access.VerbReleaseUnleased} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err.Error(), want)
		}
	}
	assertStillRetained(t, db, claim)
	if got := deadlineOf(t, db, claim); got != "infinity" {
		t.Errorf("expires_at = %q after the refused reclaim, want it left at infinity", got)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`); n != rowsBefore {
		t.Errorf("%d allocation rows after the refused reclaim, want the %d before it", n, rowsBefore)
	}
}

// Reclaiming an unleased address under Delete and then deleting the claim
// would free it, so the reclaim needs what releasing it does. Under Retain it
// stays held and needs nothing; an admin may reclaim it under Delete.
func TestAReclaimUnderDeleteOfAnUnleasedAddressNeedsTheReleaseUnleasedPermission(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	claim := retainClaim("vpc", "")
	held := retainAndDelete(t, r, ctx, claim)
	if got := deadlineOf(t, db, claim); got != "infinity" {
		t.Fatalf("setup: expires_at = %q, want infinity", got)
	}
	rowsBefore := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`)
	deleting := retainClaim("vpc", ipam.ReclaimDelete)

	consumer := releaseChecker(false)
	_, err := claimStorageWith(t, db, consumer).Create(ctx, deleting.DeepCopy(), nil, &metav1.CreateOptions{})
	expectUnleasedReclaimForbidden(t, db, err, claim, rowsBefore)
	name := allocationNameFor(claim.Namespace, claim.Name)
	if got := consumer.calls(); len(got) != 1 || got[0] != "default/"+name {
		t.Errorf("checker asked about %v, want once about default/%s", got, name)
	}

	_, err = claimStorageWith(t, db, releaseChecker(false)).Create(ctx, deleting.DeepCopy(), nil,
		&metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	expectUnleasedReclaimForbidden(t, db, err, claim, rowsBefore)

	_, err = claimStorageWith(t, db, nil).Create(ctx, deleting.DeepCopy(), nil, &metav1.CreateOptions{})
	expectUnleasedReclaimForbidden(t, db, err, claim, rowsBefore)

	// Under the class's Retain, stated or not, the address stays held.
	for _, policy := range []ipam.ReclaimPolicy{"", ipam.ReclaimRetain} {
		consumer := releaseChecker(false)
		rest := claimStorageWith(t, db, consumer)
		obj, err := rest.Create(ctx, retainClaim("vpc", policy), nil, &metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("consumer reclaim under reclaimPolicy %q: %v", policy, err)
		}
		if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
			t.Errorf("reclaim under %q got %q, want the retained %q", policy, got, held)
		}
		if got := consumer.calls(); len(got) != 0 {
			t.Errorf("release-unleased was asked about %v for a reclaim under %q", got, policy)
		}
		if _, _, err := rest.Delete(ctx, "vpc", nil, &metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete the reclaiming claim: %v", err)
		}
		assertStillRetained(t, db, claim)
	}

	admin := releaseChecker(true)
	obj, err := claimStorageWith(t, db, admin).Create(ctx, deleting.DeepCopy(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("admin reclaim under Delete: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
		t.Errorf("admin reclaim got %q, want the retained %q", got, held)
	}
	if got := admin.calls(); len(got) != 1 {
		t.Errorf("release-unleased asked %d times for the admin's reclaim, want once", len(got))
	}
}

// With a lease deadline the address would be freed by its deadline anyway, so
// a reclaim under Delete is the consumer's and asks nothing.
func TestAReclaimUnderDeleteOfALeasedAddressNeedsNoPermission(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	setPoolCap(t, db, "us-east", &metav1.Duration{Duration: time.Hour})
	ctx := claimCtx(testProject)
	claim := retainClaim("leased", "")
	held := retainAndDelete(t, r, ctx, claim)

	consumer := releaseChecker(false)
	obj, err := claimStorageWith(t, db, consumer).Create(ctx, retainClaim("leased", ipam.ReclaimDelete), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("consumer reclaim of a leased address under Delete: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
		t.Errorf("reclaim got %q, want the retained %q", got, held)
	}
	if got := consumer.calls(); len(got) != 0 {
		t.Errorf("release-unleased was asked about %v for a leased address", got)
	}
}

// As for the release, the row as locked decides: a deadline that is gone by
// the time the reclaim holds the pool refuses it, though the read before the
// lock saw one.
func TestTheReclaimJudgesTheDeadlineUnderThePoolLock(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	setPoolCap(t, db, "us-east", &metav1.Duration{Duration: time.Hour})
	ctx := claimCtx(testProject)
	claim := retainClaim("raced", "")
	retainAndDelete(t, r, ctx, claim)
	rowsBefore := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`)

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
	rest := claimStorageWith(t, db, consumer)
	done := make(chan error, 1)
	go func() {
		_, err := rest.Create(ctx, retainClaim("raced", ipam.ReclaimDelete), nil, &metav1.CreateOptions{})
		done <- err
	}()
	testdb.WaitForLockWaiters(t, db, 1)
	if got := consumer.calls(); len(got) != 0 {
		t.Fatalf("release-unleased was asked about %v before the reclaim held the lock", got)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	expectUnleasedReclaimForbidden(t, db, <-done, claim, rowsBefore)
	if got := consumer.calls(); len(got) != 1 {
		t.Errorf("release-unleased asked %d times, want once, under the lock", len(got))
	}
}

// rbacAuthorizer allows exactly the verbs it is given on ipallocations, as
// Kubernetes RBAC would for an identity bound to such a role.
type rbacAuthorizer map[string]bool

func (a rbacAuthorizer) Authorize(_ context.Context, attrs authorizer.Attributes) (authorizer.Decision, string, error) {
	if attrs.GetResource() == "ipallocations" && a[attrs.GetVerb()] {
		return authorizer.DecisionAllow, "", nil
	}
	return authorizer.DecisionDeny, "", nil
}

func (a rbacAuthorizer) ConditionsAwareAuthorize(ctx context.Context, attrs authorizer.Attributes) authorizer.ConditionsAwareDecision {
	return authorizer.ConditionsAwareDecisionFromParts(a.Authorize(ctx, attrs))
}

func (a rbacAuthorizer) EvaluateConditions(context.Context, authorizer.ConditionsAwareDecision, authorizer.ConditionsData) (authorizer.Decision, string, error) {
	return authorizer.DecisionDeny, "", authorizer.ErrorConditionEvaluationNotSupported
}

// Deleting a namespace frees what its allocations hold, unleased or not: the
// namespace controller holds deletecollection on every resource, and that
// admits the release, one allocation or the collection. A consumer, holding
// plain delete, is still refused.
func TestTheNamespaceControllerReleasesUnleasedAllocations(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	one, two := retainClaim("vpc-1", ""), retainClaim("vpc-2", "")
	retainAndDelete(t, r, ctx, one)
	retainAndDelete(t, r, ctx, two)
	for _, c := range []*ipam.IPClaim{one, two} {
		if got := deadlineOf(t, db, c); got != "infinity" {
			t.Fatalf("setup: %s expires_at = %q, want infinity", c.Name, got)
		}
	}

	consumer := allocationStorageWith(t, db, access.NewUnleasedReleaseChecker(rbacAuthorizer{"delete": true}))
	_, _, err := consumer.Delete(ctx, allocationNameFor(one.Namespace, one.Name), nil, &metav1.DeleteOptions{})
	expectUnleasedForbidden(t, db, err, one)

	controller := allocationStorageWith(t, db,
		access.NewUnleasedReleaseChecker(rbacAuthorizer{"delete": true, access.VerbDeleteCollection: true}))
	if _, _, err := controller.Delete(ctx, allocationNameFor(one.Namespace, one.Name), nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("release by a deletecollection holder: %v", err)
	}
	if _, ok := readAllocationRow(t, db, allocationKeyFor(one)); ok {
		t.Error("the release left vpc-1's row")
	}

	// The namespace deleter's own call, which ReleasingREST serves item by item.
	if _, err := controller.DeleteCollection(ctx, nil, &metav1.DeleteOptions{}, &metainternalversion.ListOptions{}); err != nil {
		t.Fatalf("DeleteCollection by a deletecollection holder: %v", err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE purpose = 'Claim'`); n != 0 {
		t.Errorf("%d claim rows left after the collection was deleted, want 0", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE kind = 'IPAllocation'`); n != 0 {
		t.Errorf("%d IPAllocations left after the collection was deleted, want 0", n)
	}
}

// A consumer refused by the read before the transaction is refused without
// waiting for the pool: a release in progress elsewhere holds it, and the
// refusal still comes back at once. A reclaim does wait, as any claim does:
// whether the claim agrees with the row is judged under the lock, and a
// mismatch is refused before the permission.
func TestAnUnleasedReleaseIsRefusedWithoutWaitingForThePool(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	claim := retainClaim("vpc", "")
	retainAndDelete(t, r, ctx, claim)

	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM ipam_objects WHERE key = $1 FOR UPDATE`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", "us-east")); err != nil {
		t.Fatalf("lock pool: %v", err)
	}

	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _, err = allocationStorageWith(t, db, releaseChecker(false)).Delete(deadline,
		allocationNameFor(claim.Namespace, claim.Name), nil, &metav1.DeleteOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("release while the pool is held returned %v, want an immediate 403", err)
	}
}

// A recreated claim that could not take the address back under any policy is
// told why with the mismatch 409, not sent to an admin with a 403: an admin's
// permission would not make it agree. The authorizer is still asked before
// the transaction, and the address stays retained.
func TestAMismatchedReclaimUnderDeleteOfAnUnleasedAddressIsAConflict(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	claim := retainClaim("vpc", "")
	retainAndDelete(t, r, ctx, claim)
	rowsBefore := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`)

	mismatched := retainClaim("vpc", ipam.ReclaimDelete)
	length := int32(26)
	mismatched.Spec.PrefixLength = &length
	consumer := releaseChecker(false)
	_, err := claimStorageWith(t, db, consumer).Create(ctx, mismatched, nil, &metav1.CreateOptions{})
	if ipamerrors.ReasonFor(err) != ipamerrors.ReasonRetainedAllocationMismatch {
		t.Fatalf("mismatched reclaim under Delete returned %v, want 409 %s", err, ipamerrors.ReasonRetainedAllocationMismatch)
	}
	if !strings.Contains(err.Error(), "asks for a /26") {
		t.Errorf("message %q does not name the prefix length asked for", err.Error())
	}
	if got := consumer.calls(); len(got) != 1 {
		t.Errorf("release-unleased asked %d times, want once, before the transaction", len(got))
	}
	assertStillRetained(t, db, claim)
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`); n != rowsBefore {
		t.Errorf("%d allocation rows after the refused reclaim, want the %d before it", n, rowsBefore)
	}
}
