package ipclaim

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// setPoolCap sets pool's maxRetentionLease in the test project, or clears it
// when lease is nil. It writes the stored object directly, as an operator's
// edit would leave it, without the update hook that tightens existing leases.
func setPoolCap(t *testing.T, db *pgxpool.Pool, pool string, lease *metav1.Duration) {
	t.Helper()
	ctx := context.Background()
	key := tenant.Identity{Name: testProject}.ResourceKey("ippools", pool)
	var data []byte
	if err := db.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, key).Scan(&data); err != nil {
		t.Fatalf("read pool %s: %v", pool, err)
	}
	var p ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("decode pool %s: %v", pool, err)
	}
	p.Spec.MaxRetentionLease = lease
	data, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("encode pool %s: %v", pool, err)
	}
	if _, err := db.Exec(ctx, `UPDATE ipam_objects SET data = $2 WHERE key = $1`, key, data); err != nil {
		t.Fatalf("write pool %s: %v", pool, err)
	}
}

// expectLeaseRequired fails unless err is the 422 refusal of an unleased
// Retain override naming pool.
func expectLeaseRequired(t *testing.T, err error, pool string) {
	t.Helper()
	if err == nil {
		t.Fatal("create succeeded; want 422 RetentionLeaseRequired")
	}
	if got := ipamerrors.ReasonFor(err); got != ipamerrors.ReasonRetentionLeaseRequired {
		t.Fatalf("create returned %v (reason %q), want RetentionLeaseRequired", err, got)
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("refusal is not a 422 Invalid: %v", err)
	}
	for _, want := range []string{`class "standard"`, `pool "` + pool + `"`, "retentionLease", "maxRetentionLease"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %s", err.Error(), want)
		}
	}
}

// assertNothingAllocated fails if the refused claim left a claim or an
// allocation row behind. Call it while us-east holds nothing else.
func assertNothingAllocated(t *testing.T, db *pgxpool.Pool, claim *ipam.IPClaim) {
	t.Helper()
	if _, ok := readAllocationRow(t, db, allocationKeyFor(claim)); ok {
		t.Error("a refused claim left an allocation row")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE key = $1`,
		claimObjectKey(tenant.Identity{Name: testProject}, claim.Namespace, claim.Name)); n != 0 {
		t.Error("a refused claim was written")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", "us-east")); n != 0 {
		t.Errorf("us-east holds %d allocation rows after a refused claim, want none", n)
	}
}

// A claim may not retain past a Delete class's default with no lease to bound
// it: the class sets none and the pool caps nothing, so the address would be
// held for good. The refusal is a 422, counted as the caller's, and nothing is
// allocated.
func TestARetainOverrideWithNoLeaseIsRefused(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	ctx := claimCtx(testProject)
	before := failures(t, reasonInvalid)

	claim := retainClaim("claim-1", ipam.ReclaimRetain)
	_, err := r.Create(ctx, claim.DeepCopy(), nil, &metav1.CreateOptions{})
	expectLeaseRequired(t, err, "us-east")
	assertNothingAllocated(t, db, claim)
	if got := failures(t, reasonInvalid); got != before+1 {
		t.Errorf("ipam_allocation_failures_total{reason=invalid} moved by %v, want 1", got-before)
	}

	// The class's own default still binds.
	if _, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
		t.Errorf("the same claim without the override: %v", err)
	}
}

// A lease on the class is enough, whatever the pool says.
func TestARetainOverrideUnderAClassLeaseIsAccepted(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	withLeasedDeleteClass(t, db)
	ctx := claimCtx(testProject)

	if _, err := r.Create(ctx, retainClaim("claim-1", ipam.ReclaimRetain), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create under a class lease: %v", err)
	}
}

// A cap on the pool the claim lands in is enough with no class lease, and a
// cap on another pool of the class is not: the guard judges where the claim
// is placed.
func TestARetainOverrideIsJudgedByThePoolItLandsIn(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	seedNamedPool(t, db, "us-west", "10.1.0.0/16", "standard")
	setPoolCap(t, db, "us-east", &metav1.Duration{Duration: time.Hour})
	ctx := claimCtx(testProject)

	capped := retainClaim("in-east", ipam.ReclaimRetain)
	capped.Spec.Address = "10.0.5.0"
	if _, err := r.Create(ctx, capped.DeepCopy(), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create in the capped pool: %v", err)
	}

	uncapped := retainClaim("in-west", ipam.ReclaimRetain)
	uncapped.Spec.Address = "10.1.5.0"
	_, err := r.Create(ctx, uncapped.DeepCopy(), nil, &metav1.CreateOptions{})
	expectLeaseRequired(t, err, "us-west")

	// The cap accepted is the one the address is retained under.
	if _, _, err := r.Delete(ctx, capped.Name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if a := getAllocation(t, allocREST, ctx, capped); a.Status.ExpiresAt == nil || !isLease(a, time.Hour) {
		t.Errorf("expiresAt = %v, want the pool's 1h cap from %v", a.Status.ExpiresAt, a.Status.RetainedAt)
	}
}

// A class whose own default is Retain retains by its owner's decision. Neither
// a claim agreeing with it nor one leaving the policy to it is refused, lease
// or none.
func TestAClassDefaultRetainNeedsNoLease(t *testing.T) {
	r, _, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	for name, policy := range map[string]ipam.ReclaimPolicy{"unstated": "", "stated": ipam.ReclaimRetain} {
		if _, err := r.Create(ctx, retainClaim(name, policy), nil, &metav1.CreateOptions{}); err != nil {
			t.Errorf("Create with reclaimPolicy %q under a Retain class: %v", policy, err)
		}
	}
}

// A reclaiming claim takes its own policy onto the row, so its Retain override
// is held to the same rule, against the pool the retained row is in. The
// address stays retained, and the claim without the override takes it back.
func TestAReclaimIsHeldToTheSameRule(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	setPoolCap(t, db, "us-east", &metav1.Duration{Duration: time.Hour})
	ctx := claimCtx(testProject)

	claim := retainClaim("claim-1", ipam.ReclaimRetain)
	held := retainAndDelete(t, r, ctx, claim)

	// Under the cap, the override reclaims.
	obj, err := r.Create(ctx, claim.DeepCopy(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("reclaim under the cap: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
		t.Fatalf("reclaim got %q, want the retained %q", got, held)
	}
	if _, _, err := r.Delete(ctx, claim.Name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// With the cap gone, the same override would retain with no lease.
	setPoolCap(t, db, "us-east", nil)
	reclaimed := reclaimCount(t, "reclaimed")
	mismatched := reclaimCount(t, "mismatch")
	_, err = r.Create(ctx, claim.DeepCopy(), nil, &metav1.CreateOptions{})
	expectLeaseRequired(t, err, "us-east")
	assertStillRetained(t, db, claim)
	if reclaimCount(t, "reclaimed") != reclaimed || reclaimCount(t, "mismatch") != mismatched {
		t.Error("a refused reclaim moved ipam_reclaims_total; it is neither a reclaim nor a mismatch")
	}

	obj, err = r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("reclaim without the override: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
		t.Errorf("reclaim without the override got %q, want the retained %q", got, held)
	}
}

// A dry run is refused exactly as the create would be, fresh or reclaiming,
// and accepted where the create would be.
func TestADryRunIsHeldToTheSameRule(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	ctx := claimCtx(testProject)
	dry := &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}

	fresh := retainClaim("fresh", ipam.ReclaimRetain)
	_, err := r.Create(ctx, fresh.DeepCopy(), nil, dry)
	expectLeaseRequired(t, err, "us-east")
	assertNothingAllocated(t, db, fresh)

	setPoolCap(t, db, "us-east", &metav1.Duration{Duration: time.Hour})
	if _, err := r.Create(ctx, fresh.DeepCopy(), nil, dry); err != nil {
		t.Errorf("dry run under the cap: %v", err)
	}
	assertNothingAllocated(t, db, fresh)

	claim := retainClaim("claim-1", ipam.ReclaimRetain)
	retainAndDelete(t, r, ctx, claim)
	setPoolCap(t, db, "us-east", nil)
	_, err = r.Create(ctx, claim.DeepCopy(), nil, dry)
	expectLeaseRequired(t, err, "us-east")
	assertStillRetained(t, db, claim)
}

// poolAndCarveCount is how many IPPools exist and how many PoolCarve rows back
// them: what a refused claim must not have added to.
func poolAndCarveCount(t *testing.T, db *pgxpool.Pool) (pools, carves int) {
	t.Helper()
	return countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE kind = 'IPPool'`),
		countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE purpose = 'PoolCarve'`)
}

// A chained class provisions a pool per scope, and a provisioned pool carries
// no cap. A Retain override under such a class with no lease is therefore
// refused before resolution, leaving no pool or carve behind; the message
// points at the class's lease, since a cap would cover one scope. Once the
// leaf exists the refusal is made under its lock and names it, and once an
// operator caps it the override is accepted.
func TestAChainedClassOverrideWithNoLeaseProvisionsNothing(t *testing.T) {
	r, db := newTenantREST(t)
	ctx := claimCtx(testProject)
	pools, carves := poolAndCarveCount(t, db)

	refused := endpointClaim("ep-1", "net-a", "ams")
	refused.Spec.ReclaimPolicy = ipam.ReclaimRetain
	_, err := r.Create(ctx, refused.DeepCopy(), nil, &metav1.CreateOptions{})
	if ipamerrors.ReasonFor(err) != ipamerrors.ReasonRetentionLeaseRequired || !apierrors.IsInvalid(err) {
		t.Fatalf("create returned %v, want 422 RetentionLeaseRequired", err)
	}
	if !strings.Contains(err.Error(), "Set retentionLease on the class") || strings.Contains(err.Error(), "maxRetentionLease on the pool") {
		t.Errorf("message %q should point at the class's lease and not at a pool cap", err.Error())
	}
	if p, c := poolAndCarveCount(t, db); p != pools || c != carves {
		t.Errorf("a refused claim left %d pools and %d carves, want %d and %d", p, c, pools, carves)
	}

	// The scope's chain is provisioned by a claim that leaves the policy to
	// the class.
	leaf := create(t, r, endpointClaim("ep-plain", "net-a", "ams")).Status.PoolRef.Name
	_, err = r.Create(ctx, refused.DeepCopy(), nil, &metav1.CreateOptions{})
	if ipamerrors.ReasonFor(err) != ipamerrors.ReasonRetentionLeaseRequired || !strings.Contains(err.Error(), `pool "`+leaf+`"`) {
		t.Fatalf("override into the existing leaf returned %v, want RetentionLeaseRequired naming %s", err, leaf)
	}

	setPoolCap(t, db, leaf, &metav1.Duration{Duration: time.Hour})
	if _, err := r.Create(ctx, refused.DeepCopy(), nil, &metav1.CreateOptions{}); err != nil {
		t.Errorf("override into the capped leaf: %v", err)
	}
}

// A lease stored before validation refused one, 0s or negative, is no lease,
// as it is when the retention is evaluated. An override under it is refused.
func TestARetainOverrideUnderANonPositiveStoredLeaseIsRefused(t *testing.T) {
	for name, lease := range map[string]time.Duration{"zero": 0, "negative": -time.Hour} {
		t.Run(name, func(t *testing.T) {
			r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
			seedNamedClass(t, db, "standard", ipamv1alpha1.IPClassSpec{
				IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24,
				ReclaimPolicy: ipamv1alpha1.ReclaimDelete, RetentionLease: &metav1.Duration{Duration: lease},
			})
			claim := retainClaim("claim-1", ipam.ReclaimRetain)
			_, err := r.Create(claimCtx(testProject), claim.DeepCopy(), nil, &metav1.CreateOptions{})
			expectLeaseRequired(t, err, "us-east")
			assertNothingAllocated(t, db, claim)
		})
	}
}

// A generated name is refused like any other: the guard is about the policy,
// not the name, and a refusal is not a name collision to draw again.
func TestAGeneratedNameWithAnUnleasedOverrideIsRefused(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	ctx := claimCtx(testProject)
	claim := retainClaim("", ipam.ReclaimRetain)
	claim.GenerateName = "web-"

	_, err := r.Create(ctx, claim.DeepCopy(), nil, &metav1.CreateOptions{})
	expectLeaseRequired(t, err, "us-east")
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE purpose = 'Claim'`); n != 0 {
		t.Errorf("a refused generated-name claim left %d allocation rows", n)
	}

	withLeasedDeleteClass(t, db)
	obj, err := r.Create(ctx, claim.DeepCopy(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("generated-name override under a class lease: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Name; !strings.HasPrefix(got, "web-") {
		t.Errorf("claim name = %q, want one generated from web-", got)
	}
}
