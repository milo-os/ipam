package ipclaim

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics/testutil"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// seedNamedClass writes an IPClass into the test project under name.
func seedNamedClass(t *testing.T, db *pgxpool.Pool, name string, spec ipamv1alpha1.IPClassSpec) {
	t.Helper()
	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	data, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal class %s: %v", name, err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPClass',$2,$3)
		 ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data`,
		tenant.Identity{Name: testProject}.ResourceKey("ipclasses", name), name, data); err != nil {
		t.Fatalf("seed class %s: %v", name, err)
	}
}

// seedNamedPool writes a ready IPv4 IPPool offering classNames into the test
// project.
func seedNamedPool(t *testing.T, db *pgxpool.Pool, name, cidr string, classNames ...string) {
	t.Helper()
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ipamv1alpha1.IPPoolSpec{CIDR: cidr, IPFamily: ipamv1alpha1.IPv4, ClassNames: classNames},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: ipamv1alpha1.IPv4,
		},
	}
	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool %s: %v", name, err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", name), name, data); err != nil {
		t.Fatalf("seed pool %s: %v", name, err)
	}
}

// retainAndDelete binds claim, deletes it so its allocation is retained, and
// returns the address it held.
func retainAndDelete(t *testing.T, r *AllocatingREST, ctx context.Context, claim *ipam.IPClaim) string {
	t.Helper()
	created, err := r.Create(ctx, claim.DeepCopy(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create %s: %v", claim.Name, err)
	}
	if _, _, err := r.Delete(ctx, claim.Name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete %s: %v", claim.Name, err)
	}
	row, ok := readAllocationRow(t, r.db.(*pgxpool.Pool), allocationKeyFor(claim))
	if !ok || row.claimKey != nil {
		t.Fatalf("setup: %s did not leave a retained allocation", claim.Name)
	}
	return created.(*ipam.IPClaim).Status.AllocatedCIDR
}

func reclaimCount(t *testing.T, result string) float64 {
	t.Helper()
	v, err := testutil.GetCounterMetricValue(metrics.Reclaims.WithLabelValues(result))
	if err != nil {
		t.Fatalf("read ipam_reclaims_total{result=%q}: %v", result, err)
	}
	return v
}

func poolAllocated(t *testing.T, db *pgxpool.Pool, pool string) string {
	t.Helper()
	var allocated string
	if err := db.QueryRow(context.Background(),
		`SELECT ipam_data_to_jsonb(data) -> 'status' -> 'capacity' ->> 'allocated'
		   FROM ipam_objects WHERE key = $1`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", pool)).Scan(&allocated); err != nil {
		t.Fatalf("read pool %s capacity: %v", pool, err)
	}
	return allocated
}

// assertStillRetained fails unless the allocation behind claim is unbound and
// its IPAllocation names no claim, and no claim object of that name exists: a
// refused reclaim must leave everything as retention left it.
func assertStillRetained(t *testing.T, db *pgxpool.Pool, claim *ipam.IPClaim) {
	t.Helper()
	key := allocationKeyFor(claim)
	row, ok := readAllocationRow(t, db, key)
	if !ok {
		t.Fatal("the retained allocation row is gone")
	}
	if row.claimKey != nil {
		t.Errorf("the retained row is bound to %q", *row.claimKey)
	}
	if row.retainedAt == nil {
		t.Error("retained_at was cleared")
	}
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_objects WHERE key = $1 AND ipam_data_to_jsonb(data) -> 'spec' -> 'claimRef' IS NOT NULL`, key); n != 0 {
		t.Error("the retained IPAllocation names a claim")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE key = $1`,
		claimObjectKey(tenant.Identity{Name: testProject}, claim.Namespace, claim.Name)); n != 0 {
		t.Errorf("a claim object %q was written", claim.Name)
	}
}

// A claim recreated in a pool with no free block left takes back the block it
// retained. Before reclaim the search ran first and answered 507 for an
// address that was the claim's own.
func TestReclaimSucceedsInAFullPool(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	seedNamedClass(t, db, "tiny", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 32, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
	})
	seedNamedPool(t, db, "tiny-pool", "10.9.0.0/31", "tiny")
	tiny := func(name string) *ipam.IPClaim {
		return &ipam.IPClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       ipam.IPClaimSpec{ClassName: "tiny"},
		}
	}

	held := retainAndDelete(t, r, ctx, tiny("tiny-1"))
	if _, err := r.Create(ctx, tiny("tiny-2"), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create tiny-2: %v", err)
	}
	before := poolAllocated(t, db, "tiny-pool")

	if _, err := r.Create(ctx, tiny("tiny-3"), nil, &metav1.CreateOptions{}); !ipamerrors.IsExhausted(err) {
		t.Fatalf("a third name returned %v, want 507: the pool is full", err)
	}

	obj, err := r.Create(ctx, tiny("tiny-1"), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("recreating tiny-1 in the full pool: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
		t.Errorf("tiny-1 got %q back, want its retained %q", got, held)
	}
	if after := poolAllocated(t, db, "tiny-pool"); after != before {
		t.Errorf("pool allocated moved from %s to %s; a reclaim changes no capacity", before, after)
	}
}

// The replacement's reclaim policy is the one the row records from then on:
// a predecessor created under Retain does not make its replacement retain.
func TestReclaimRecordsTheReplacementsReclaimPolicy(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimDelete)
	ctx := claimCtx(testProject)

	held := retainAndDelete(t, r, ctx, retainClaim("claim-1", ipam.ReclaimRetain))

	obj, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	claim := obj.(*ipam.IPClaim)
	if claim.Status.AllocatedCIDR != held {
		t.Fatalf("recreate got %q, want the retained %q", claim.Status.AllocatedCIDR, held)
	}
	var policy string
	if err := db.QueryRow(context.Background(),
		`SELECT reclaim_policy FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocationKeyFor(claim)).Scan(&policy); err != nil {
		t.Fatalf("read reclaim_policy: %v", err)
	}
	if policy != string(ipamv1alpha1.ReclaimDelete) {
		t.Errorf("row reclaim_policy = %q, want Delete from the replacement's class", policy)
	}
	allocObj, err := allocREST.Get(ctx, claim.Status.BoundAllocationRef.Name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get IPAllocation: %v", err)
	}
	if got := allocObj.(*ipam.IPAllocation).Spec.ReclaimPolicy; got != ipam.ReclaimDelete {
		t.Errorf("IPAllocation spec.reclaimPolicy = %q, want Delete", got)
	}

	// And the release does what the row now says.
	if _, _, err := r.Delete(ctx, "claim-1", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := readAllocationRow(t, db, allocationKeyFor(claim)); ok {
		t.Error("the allocation was retained again; the replacement was created under Delete")
	}
}

// Each term the retained allocation was handed out under must agree, or the
// recreate is refused by name and nothing is bound.
func TestReclaimRefusesAMismatch(t *testing.T) {
	cases := []struct {
		name  string
		field string
		// setup runs after the predecessor is retained, and may change the
		// world the recreate resolves in.
		setup func(t *testing.T, db *pgxpool.Pool)
		claim func() *ipam.IPClaim
	}{
		{
			name:  "class",
			field: "spec.className",
			claim: func() *ipam.IPClaim {
				c := retainClaim("claim-1", "")
				c.Spec.ClassName = "other"
				return c
			},
		},
		{
			name:  "family",
			field: "spec.ipFamily",
			setup: func(t *testing.T, db *pgxpool.Pool) {
				// The class is redefined for the other family under the same
				// name, as deleting and recreating it would.
				seedNamedClass(t, db, "standard", ipamv1alpha1.IPClassSpec{
					IPFamily: ipamv1alpha1.IPv6, DefaultPrefixLength: 64, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
				})
			},
			claim: func() *ipam.IPClaim {
				c := retainClaim("claim-1", "")
				c.Spec.IPFamily = ""
				return c
			},
		},
		{
			name:  "prefix length",
			field: "spec.prefixLength",
			claim: func() *ipam.IPClaim {
				c := retainClaim("claim-1", "")
				c.Spec.PrefixLength = int32p(25)
				return c
			},
		},
		{
			name:  "a length the class no longer allows",
			field: "spec.prefixLength",
			setup: func(t *testing.T, db *pgxpool.Pool) {
				seedNamedClass(t, db, "standard", ipamv1alpha1.IPClassSpec{
					IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 26, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
					AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 26, Max: 28},
				})
			},
			claim: func() *ipam.IPClaim { return retainClaim("claim-1", "") },
		},
		{
			name:  "requested address",
			field: "spec.address",
			claim: func() *ipam.IPClaim {
				c := retainClaim("claim-1", "")
				c.Spec.Address = "10.0.200.0"
				return c
			},
		},
		{
			name:  "scope",
			field: "spec.scope",
			claim: func() *ipam.IPClaim {
				c := retainClaim("claim-1", "")
				c.Spec.ClassName = "scoped"
				c.Spec.Scope = networkScope("net-b")
				return c
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
			ctx := claimCtx(testProject)
			seedNamedClass(t, db, "other", ipamv1alpha1.IPClassSpec{
				IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
			})
			seedNamedClass(t, db, "scoped", ipamv1alpha1.IPClassSpec{
				IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
				UniqueWithin: []string{"network"},
			})
			seedNamedPool(t, db, "other-pool", "10.1.0.0/16", "other")
			seedNamedPool(t, db, "scoped-pool", "10.2.0.0/16", "scoped")

			predecessor := retainClaim("claim-1", "")
			if tc.field == "spec.scope" {
				predecessor.Spec.ClassName = "scoped"
				predecessor.Spec.Scope = networkScope("net-a")
			}
			retainAndDelete(t, r, ctx, predecessor)
			if tc.setup != nil {
				tc.setup(t, db)
			}
			mismatches := reclaimCount(t, "mismatch")

			_, err := r.Create(ctx, tc.claim(), nil, &metav1.CreateOptions{})
			if ipamerrors.ReasonFor(err) != ipamerrors.ReasonRetainedAllocationMismatch {
				t.Fatalf("recreate returned %v (reason %q), want RetainedAllocationMismatch", err, ipamerrors.ReasonFor(err))
			}
			if !apierrors.IsConflict(err) {
				t.Errorf("mismatch is not a 409: %v", err)
			}
			allocationName, field, _ := ipamerrors.RetainedAllocationMismatch(err)
			if field != tc.field {
				t.Errorf("mismatch names field %q, want %q", field, tc.field)
			}
			if want := allocationNameFor("default", "claim-1"); allocationName != want {
				t.Errorf("mismatch names allocation %q, want %q", allocationName, want)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("message %q does not name the field", err)
			}
			if got := reclaimCount(t, "mismatch"); got != mismatches+1 {
				t.Errorf("ipam_reclaims_total{result=mismatch} moved by %v, want 1", got-mismatches)
			}
			assertStillRetained(t, db, predecessor)
		})
	}
}

// The requested address is a term like any other: naming the retained address
// reclaims it, in either form.
func TestReclaimAcceptsTheRetainedAddressNamedAgain(t *testing.T) {
	for _, form := range []string{"bare", "cidr"} {
		t.Run(form, func(t *testing.T) {
			r, _, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
			ctx := claimCtx(testProject)
			held := retainAndDelete(t, r, ctx, retainClaim("claim-1", ""))

			c := retainClaim("claim-1", "")
			c.Spec.Address = held
			if form == "bare" {
				c.Spec.Address = strings.TrimSuffix(held, "/24")
			}
			obj, err := r.Create(ctx, c, nil, &metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("recreate naming %s: %v", c.Spec.Address, err)
			}
			if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
				t.Errorf("got %q, want %q", got, held)
			}
		})
	}
}

// A claim that states no length takes the retained size even where the class
// default is another one, as long as the class allows it. A bare address is
// read at that size.
func TestReclaimAdoptsTheRetainedPrefixLength(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	seedNamedClass(t, db, "standard", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24, ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
		AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 24, Max: 28},
	})

	predecessor := retainClaim("claim-1", "")
	predecessor.Spec.PrefixLength = int32p(28)
	held := retainAndDelete(t, r, ctx, predecessor)

	c := retainClaim("claim-1", "")
	c.Spec.Address = strings.TrimSuffix(held, "/28")
	obj, err := r.Create(ctx, c, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("recreate without a length: %v", err)
	}
	claim := obj.(*ipam.IPClaim)
	if claim.Status.AllocatedCIDR != held {
		t.Errorf("got %q, want the retained %q", claim.Status.AllocatedCIDR, held)
	}
	if p := claim.Spec.PrefixLength; p == nil || *p != 28 {
		t.Errorf("spec.prefixLength = %v, want the adopted 28", p)
	}
}

// A row still bound to a claim is that claim's, and a second create of its
// name gets the refusal a duplicate always got.
func TestReclaimOfABoundRowIsClaimExists(t *testing.T) {
	r, _, _ := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)

	if _, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
	if ipamerrors.ReasonFor(err) != ipamerrors.ReasonClaimExists {
		t.Fatalf("second create returned %v (reason %q), want ClaimExists", err, ipamerrors.ReasonFor(err))
	}
}

// A dry-run reclaim reports the address it would take back and binds nothing.
func TestADryRunReclaimBindsNothing(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	claim := retainClaim("claim-1", "")
	held := retainAndDelete(t, r, ctx, claim)
	reclaimed := reclaimCount(t, "reclaimed")

	obj, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatalf("dry-run recreate: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != held {
		t.Errorf("dry run reports %q, want the retained %q", got, held)
	}
	assertStillRetained(t, db, claim)
	if got := reclaimCount(t, "reclaimed"); got != reclaimed {
		t.Errorf("a dry run moved ipam_reclaims_total{result=reclaimed} by %v", got-reclaimed)
	}
}

// Two creates of one name over one retained row, queued on the pool in a
// known order: the first takes the address back, the second is told the name
// is held, and neither deadlocks.
func TestTwoRecreatesOfOneNameOverARetainedRowHaveOneWinner(t *testing.T) {
	r, _, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	poolKey := tenant.Identity{Name: testProject}.ResourceKey("ippools", "us-east")
	held := retainAndDelete(t, r, ctx, retainClaim("claim-1", ""))

	holder, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := holder.Exec(context.Background(),
		`SELECT 1 FROM ipam_objects WHERE key = $1 FOR UPDATE`, poolKey); err != nil {
		_ = holder.Rollback(context.Background())
		t.Fatalf("hold the pool: %v", err)
	}
	type outcome struct {
		obj *ipam.IPClaim
		err error
	}
	recreate := func() <-chan outcome {
		done := make(chan outcome, 1)
		go func() {
			obj, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{})
			c, _ := obj.(*ipam.IPClaim)
			done <- outcome{c, err}
		}()
		return done
	}
	first := recreate()
	testdb.WaitForLockWaiters(t, db, 1)
	second := recreate()
	testdb.WaitForLockWaiters(t, db, 2)
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatalf("let go of the pool: %v", err)
	}
	a, b := <-first, <-second

	for what, o := range map[string]outcome{"first": a, "second": b} {
		if isDeadlock(o.err) {
			t.Fatalf("%s recreate deadlocked: %v", what, o.err)
		}
	}
	if a.err != nil {
		t.Fatalf("the recreate queued first returned %v, want the retained address", a.err)
	}
	if a.obj.Status.AllocatedCIDR != held {
		t.Errorf("winner got %q, want the retained %q", a.obj.Status.AllocatedCIDR, held)
	}
	if ipamerrors.ReasonFor(b.err) != ipamerrors.ReasonClaimExists {
		t.Errorf("the recreate queued second returned %v, want ClaimExists", b.err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE purpose = 'Claim'`); n != 1 {
		t.Errorf("%d claim allocations, want 1: the loser must not allocate a second address", n)
	}
}

// The bookkeeping a reclaim leaves: the row is bound and no longer retained,
// and the IPAllocation is the same object, pointing at the new claim.
func TestReclaimRebindsTheRetainedAllocationObject(t *testing.T) {
	r, allocREST, db := newLifecycleREST(t, ipamv1alpha1.ReclaimRetain)
	ctx := claimCtx(testProject)
	claim := retainClaim("claim-1", "")
	retainAndDelete(t, r, ctx, claim)

	allocationName := allocationNameFor("default", "claim-1")
	before, err := allocREST.Get(ctx, allocationName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get retained IPAllocation: %v", err)
	}

	if _, err := r.Create(ctx, retainClaim("claim-1", ""), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	row, ok := readAllocationRow(t, db, allocationKeyFor(claim))
	if !ok || row.claimKey == nil {
		t.Fatal("the reclaimed row is missing or unbound")
	}
	if want := claimObjectKey(tenant.Identity{Name: testProject}, "default", "claim-1"); *row.claimKey != want {
		t.Errorf("row claim_key = %q, want %q", *row.claimKey, want)
	}
	if row.retainedAt != nil {
		t.Errorf("retained_at = %s on a bound row, want NULL", *row.retainedAt)
	}

	after, err := allocREST.Get(ctx, allocationName, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get reclaimed IPAllocation: %v", err)
	}
	a := after.(*ipam.IPAllocation)
	if a.UID != before.(*ipam.IPAllocation).UID {
		t.Error("the IPAllocation was replaced rather than rebound")
	}
	if a.Spec.ClaimRef == nil || a.Spec.ClaimRef.Name != "claim-1" {
		t.Errorf("spec.claimRef = %v, want claim-1", a.Spec.ClaimRef)
	}
}
