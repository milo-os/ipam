package ipclaim

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/registry/ipam/ipallocation"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// withClassLease rewrites the lifecycle fixture's "standard" class with lease.
func withClassLease(t *testing.T, db *pgxpool.Pool, lease *metav1.Duration) {
	t.Helper()
	seedNamedClass(t, db, "standard", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24,
		ReclaimPolicy: ipamv1alpha1.ReclaimRetain, RetentionLease: lease,
	})
}

func getAllocation(t *testing.T, allocREST *ipallocation.ReleasingREST, ctx context.Context, claim *ipam.IPClaim) *ipam.IPAllocation {
	t.Helper()
	obj, err := allocREST.Get(ctx, allocationNameFor(claim.Namespace, claim.Name), &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get IPAllocation of %s: %v", claim.Name, err)
	}
	return obj.(*ipam.IPAllocation)
}

func retainedCondition(t *testing.T, a *ipam.IPAllocation) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(a.Status.Conditions, ipamv1alpha1.AllocationConditionRetained)
	if c == nil {
		t.Fatalf("IPAllocation %s has no Retained condition; conditions = %+v", a.Name, a.Status.Conditions)
	}
	return c
}

// A Retain delete publishes the retention on the IPAllocation in the same
// transaction that fixes the lease: when, until when, and why.
func TestRetainPublishesTheLeaseOnTheAllocation(t *testing.T) {
	r, allocREST, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	withClassLease(t, r.db.(*pgxpool.Pool), &metav1.Duration{Duration: time.Hour})
	ctx := claimCtx(testProject)
	claim := retainClaim("claim-1", "")
	retainAndDelete(t, r, ctx, claim)

	a := getAllocation(t, allocREST, ctx, claim)
	if a.Status.RetainedAt == nil || a.Status.ExpiresAt == nil {
		t.Fatalf("retainedAt = %v, expiresAt = %v; want both set", a.Status.RetainedAt, a.Status.ExpiresAt)
	}
	if d := a.Status.ExpiresAt.Sub(a.Status.RetainedAt.Time); d != time.Hour {
		t.Errorf("expiresAt - retainedAt = %s, want the class's 1h lease", d)
	}
	c := retainedCondition(t, a)
	if c.Status != metav1.ConditionTrue || c.Reason != ipamv1alpha1.RetainedReasonLeaseActive {
		t.Errorf("Retained = %s/%s, want True/LeaseActive", c.Status, c.Reason)
	}
	if !c.LastTransitionTime.Equal(a.Status.RetainedAt) {
		t.Errorf("Retained transitioned at %s, want the retention at %s", c.LastTransitionTime, a.Status.RetainedAt)
	}
}

// With no lease anywhere the allocation says so, and shows no deadline.
func TestRetainWithNoLeaseShowsNoDeadline(t *testing.T) {
	r, allocREST, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	claim := retainClaim("claim-1", "")
	retainAndDelete(t, r, ctx, claim)

	a := getAllocation(t, allocREST, ctx, claim)
	if a.Status.RetainedAt == nil {
		t.Error("retainedAt is unset on a retained allocation")
	}
	if a.Status.ExpiresAt != nil {
		t.Errorf("expiresAt = %s with no lease set, want unset", a.Status.ExpiresAt)
	}
	if c := retainedCondition(t, a); c.Status != metav1.ConditionTrue || c.Reason != ipamv1alpha1.RetainedReasonNoLease {
		t.Errorf("Retained = %s/%s, want True/NoLease", c.Status, c.Reason)
	}
}

// A reclaim ends retention: the timestamps go, and the condition records the
// transition rather than vanishing.
func TestReclaimClearsTheRetentionStatus(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	withClassLease(t, db, &metav1.Duration{Duration: time.Hour})
	ctx := claimCtx(testProject)
	claim := retainClaim("claim-1", "")
	retainAndDelete(t, r, ctx, claim)

	if _, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	a := getAllocation(t, allocREST, ctx, claim)
	if a.Status.RetainedAt != nil || a.Status.ExpiresAt != nil {
		t.Errorf("after reclaim retainedAt = %v, expiresAt = %v; want both unset", a.Status.RetainedAt, a.Status.ExpiresAt)
	}
	if c := retainedCondition(t, a); c.Status != metav1.ConditionFalse || c.Reason != ipamv1alpha1.RetainedReasonReclaimed {
		t.Errorf("Retained = %s/%s, want False/Reclaimed", c.Status, c.Reason)
	}
	var expiresAt *time.Time
	if err := db.QueryRow(context.Background(),
		`SELECT expires_at FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocationKeyFor(claim),
	).Scan(&expiresAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if expiresAt != nil {
		t.Errorf("row expires_at = %s after reclaim, want NULL", expiresAt)
	}
}

// The lease is read from the class the row records, which is the definition a
// reference resolved to, and that definition's project is the pool's project.
// The second is what a row written before class_project was recorded falls
// back on, so the two must agree for every row the create path writes.
func TestARetainedRowsClassIsKeyedOffTheRowAlone(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	withClassLease(t, db, &metav1.Duration{Duration: 90 * time.Minute})
	const consumer = "consumer-project"
	seedReference(t, db, consumer, "standard", false)
	ctx := claimCtx(consumer)

	claim := retainClaim("claim-1", "")
	if _, err := r.Create(ctx, claim.DeepCopy(), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create through the reference: %v", err)
	}
	key := allocationObjectKey(tenant.Identity{Name: consumer}, claim.Namespace, allocationNameFor(claim.Namespace, claim.Name))
	var poolKey string
	var classProject *string
	if err := db.QueryRow(context.Background(),
		`SELECT pool_key, class_project FROM ipam_cidr_allocations WHERE allocation_key = $1`, key,
	).Scan(&poolKey, &classProject); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if classProject == nil || *classProject != testProject {
		t.Fatalf("class_project = %v, want the definition's project %q", classProject, testProject)
	}
	if got := tenant.ProjectFromKey(poolKey); got != *classProject {
		t.Errorf("the pool's project is %q and the recorded class project %q; a row without the latter would read another class", got, *classProject)
	}

	if _, _, err := r.Delete(ctx, claim.Name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	a := getAllocation(t, allocREST, ctx, claim)
	if a.Status.ExpiresAt == nil || a.Status.ExpiresAt.Sub(a.Status.RetainedAt.Time) != 90*time.Minute {
		t.Errorf("expiresAt = %v, want the definition's 90m lease from %v", a.Status.ExpiresAt, a.Status.RetainedAt)
	}
}
