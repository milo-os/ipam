package ipclaim

// A create's writes under the pool lock go out in batches: the allocation row,
// consumption, pool and floor in one; the IPAllocation and the claim in
// another. These pin what a failure in either looks like to the caller, that a
// failed batch gives its connection back, and that the writes still land in
// the order watchers and the search floor rely on.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

func subnetPoolKey(claim *ipam.IPClaim) string {
	return tenant.Identity{Name: testProject}.ResourceKey("ippools", claim.Status.PoolRef.Name)
}

func requireReason(t *testing.T, err error, want ipamerrors.Reason, what string) {
	t.Helper()
	if ipamerrors.ReasonFor(err) != want {
		t.Fatalf("%s returned %v (reason %q), want %s", what, err, ipamerrors.ReasonFor(err), want)
	}
}

// An object already under the IPAllocation's key refuses the claim with
// AllocationRetained, and one under the claim's key with ClaimExists: each
// collision is reported for the object it is on, though both writes share a
// round trip. Either way nothing is allocated and the connection is usable.
func TestAnObjectCollisionInTheWriteBatchIsReportedForItsObject(t *testing.T) {
	r, db := newTenantREST(t, testdb.MaxConns(1))
	ctx := context.Background()
	id := tenant.FromContext(claimCtx(testProject))
	create(t, r, endpointClaim("warm", "net-a", "ams"))

	allocKey := allocationObjectKey(id, "default", allocationNameFor("default", "alloc-taken"))
	if _, err := db.Exec(ctx, `INSERT INTO ipam_objects (key, kind, namespace, name, data) VALUES ($1,'IPAllocation','default','x','{}')`, allocKey); err != nil {
		t.Fatalf("seed IPAllocation object: %v", err)
	}
	_, err := r.Create(claimCtx(testProject), endpointClaim("alloc-taken", "net-a", "ams"), nil, &metav1.CreateOptions{})
	requireReason(t, err, ipamerrors.ReasonAllocationRetained, "a claim whose IPAllocation key is taken")

	claimKey := claimObjectKey(id, "default", "claim-taken")
	if _, err := db.Exec(ctx, `INSERT INTO ipam_objects (key, kind, namespace, name, data) VALUES ($1,'IPClaim','default','claim-taken','{}')`, claimKey); err != nil {
		t.Fatalf("seed claim object: %v", err)
	}
	_, err = r.Create(claimCtx(testProject), endpointClaim("claim-taken", "net-a", "ams"), nil, &metav1.CreateOptions{})
	requireReason(t, err, ipamerrors.ReasonClaimExists, "a claim whose own key is taken")

	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE claim_key LIKE '%taken'`); n != 0 {
		t.Errorf("%d allocation rows survived a refused create", n)
	}
	// One connection, so this runs on the one the failed batches used.
	create(t, r, endpointClaim("after", "net-a", "ams"))
}

// An allocation row already holding the claim's name, or the IPAllocation's
// identity, refuses the claim in the allocation's write batch with the same
// 409 a sequential insert gave.
func TestAnAllocationRowCollisionIsTheSameConflict(t *testing.T) {
	r, db := newTenantREST(t, testdb.MaxConns(1))
	ctx := context.Background()
	id := tenant.FromContext(claimCtx(testProject))
	warm := create(t, r, endpointClaim("warm", "net-a", "ams"))
	poolKey := subnetPoolKey(warm)
	subnet := netip.MustParsePrefix(warm.Status.AllocatedCIDR)
	far := func(n uint64) string { return nthBlock(netip.PrefixFrom(subnet.Addr(), 64), 96, n) }

	// A row bound to the claim's key under another identity: the holder
	// constraint refuses the second row for that claim.
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose)
		 VALUES ($1, $2, $3, 'stray-identity', 'IPv6', 'Claim')`,
		poolKey, far(40000), claimObjectKey(id, "default", "held")); err != nil {
		t.Fatalf("seed held row: %v", err)
	}
	_, err := r.Create(claimCtx(testProject), endpointClaim("held", "net-a", "ams"), nil, &metav1.CreateOptions{})
	requireReason(t, err, ipamerrors.ReasonClaimExists, "a claim whose key holds another row")

	// A row under the IPAllocation's identity that the unlocked claim check
	// does not see, because it is not a claim row.
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose)
		 VALUES ($1, $2, NULL, $3, 'IPv6', 'Reservation')`,
		poolKey, far(40001), allocationObjectKey(id, "default", allocationNameFor("default", "ident"))); err != nil {
		t.Fatalf("seed identity row: %v", err)
	}
	_, err = r.Create(claimCtx(testProject), endpointClaim("ident", "net-a", "ams"), nil, &metav1.CreateOptions{})
	requireReason(t, err, ipamerrors.ReasonAllocationRetained, "a claim whose identity holds another row")

	create(t, r, endpointClaim("after", "net-a", "ams"))
}

// A level that fails after its BEGIN-and-claim batch rolls its transaction
// back and returns its connection: the pool is not left with a checked-out
// connection stuck in an open transaction.
func TestAFailedLevelReturnsItsConnection(t *testing.T) {
	r, db := newTenantREST(t, testdb.MaxConns(1))
	levels := planEndpoint(t, db)

	// An object already under the pool key the first level would write.
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1, 'IPPool', 'squatter', '{}')`, levels[0].PoolKey); err != nil {
		t.Fatalf("seed squatting object: %v", err)
	}
	if _, err := r.Create(claimCtx(testProject), endpointClaim("first", "net-a", "ams"), nil, &metav1.CreateOptions{}); err == nil {
		t.Fatal("a level whose pool key is taken provisioned anyway")
	}
	if n := db.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections still acquired after the failed level", n)
	}

	if _, err := db.Exec(context.Background(), `DELETE FROM ipam_objects WHERE key = $1`, levels[0].PoolKey); err != nil {
		t.Fatalf("remove squatter: %v", err)
	}
	// One connection, so this runs on the one the failed level used.
	create(t, r, endpointClaim("first", "net-a", "ams"))
}

// A level whose BEGIN-and-claim batch is cancelled while its identity insert
// waits on another provisioner returns its connection, and the next create
// provisions normally.
func TestACancelledLevelBatchReturnsItsConnection(t *testing.T) {
	r, db := newTenantREST(t, testdb.MaxConns(2))
	levels := planEndpoint(t, db)

	// An uncommitted claim on the first level's identity: an insert of the
	// same tuple waits for it to commit or roll back.
	blocker, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.Exec(context.Background(),
		`INSERT INTO ipam_pool_identity (class_name, scope_digest, pool_key) VALUES ($1, $2, $3)`,
		levels[0].Class.Name, levels[0].ScopeDigest, levels[0].PoolKey); err != nil {
		t.Fatalf("hold identity: %v", err)
	}
	// A failure below must not leave the blocker holding its connection, or
	// closing the pool at cleanup waits for it.
	defer func() { _ = blocker.Rollback(context.Background()) }()

	ctx, cancel := context.WithTimeout(claimCtx(testProject), 300*time.Millisecond)
	defer cancel()
	if _, err := r.Create(ctx, endpointClaim("first", "net-a", "ams"), nil, &metav1.CreateOptions{}); err == nil ||
		!strings.Contains(err.Error(), "claim pool identity") {
		t.Fatalf("a level batch waiting past its deadline returned %v, want the identity claim's failure", err)
	}
	if err := blocker.Rollback(context.Background()); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	waitForNoAcquiredConns(t, db)
	create(t, r, endpointClaim("first", "net-a", "ams"))
}

// planEndpoint is the cascade an endpoint claim in net-a/ams resolves.
func planEndpoint(t *testing.T, db *pgxpool.Pool) []allocator.CascadeLevel {
	t.Helper()
	ctx := claimCtx(testProject)
	leaf, err := allocator.LoadClass(ctx, db, "tenant-endpoint")
	if err != nil {
		t.Fatalf("load class: %v", err)
	}
	levels, err := allocator.PlanCascade(ctx, db, leaf, endpointClaim("x", "net-a", "ams").Spec.Scope)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return levels
}

func waitForNoAcquiredConns(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for db.Stat().AcquiredConns() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections still acquired", db.Stat().AcquiredConns())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A create cancelled while it waits for the pool lock, which it now takes in a
// batch with the floor lock, gives its connection back, and the pool keeps
// serving creates.
func TestACancelledLockWaitReturnsItsConnection(t *testing.T) {
	r, db := newTenantREST(t, testdb.MaxConns(2))
	warm := create(t, r, endpointClaim("warm", "net-a", "ams"))

	blocker, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(context.Background(), `SELECT 1 FROM ipam_objects WHERE key = $1 FOR UPDATE`, subnetPoolKey(warm)); err != nil {
		t.Fatalf("lock pool: %v", err)
	}

	ctx, cancel := context.WithTimeout(claimCtx(testProject), 300*time.Millisecond)
	defer cancel()
	if _, err := r.Create(ctx, endpointClaim("waits", "net-a", "ams"), nil, &metav1.CreateOptions{}); err == nil {
		t.Fatal("a create waiting on a held pool lock succeeded before its deadline")
	}
	if err := blocker.Rollback(context.Background()); err != nil {
		t.Fatalf("release blocker: %v", err)
	}

	waitForNoAcquiredConns(t, db)
	create(t, r, endpointClaim("after", "net-a", "ams"))
}

// A dry run that would provision the chain still answers, and leaves nothing
// behind: its levels run as savepoints of its own transaction, not as batches
// that begin transactions of their own.
func TestADryRunFirstClaimProvisionsNothing(t *testing.T) {
	r, db := newTenantREST(t)
	obj, err := r.Create(claimCtx(testProject), endpointClaim("dry", "net-a", "ams"), nil,
		&metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got == "" {
		t.Error("the dry run returned no would-be CIDR")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_pool_identity`); n != 0 {
		t.Errorf("%d identity rows survived a dry run", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`); n != 0 {
		t.Errorf("%d allocation rows survived a dry run", n)
	}
}

// A create's changelog rows are one transaction's, in the order a watcher
// replays them: the pool's capacity, then the IPAllocation, then the claim.
func TestACreatesChangelogRowsCommitTogetherInOrder(t *testing.T) {
	r, db := newTenantREST(t)
	id := tenant.FromContext(claimCtx(testProject))
	warm := create(t, r, endpointClaim("warm", "net-a", "ams"))
	create(t, r, endpointClaim("watched", "net-a", "ams"))

	claimKey := claimObjectKey(id, "default", "watched")
	rows, err := db.Query(context.Background(),
		`SELECT key, event_type FROM ipam_changelog
		  WHERE commit_xid = (SELECT commit_xid FROM ipam_changelog WHERE key = $1 AND event_type = 'ADDED')
		  ORDER BY id`, claimKey)
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var key, event string
		if err := rows.Scan(&key, &event); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, event+" "+key)
	}
	want := []string{
		"MODIFIED " + subnetPoolKey(warm),
		"ADDED " + allocationObjectKey(id, "default", allocationNameFor("default", "watched")),
		"ADDED " + claimKey,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("changelog rows of the create:\n got %v\nwant %v", got, want)
	}
}

// Creates and deletes racing on one pool get disjoint blocks, and leave the
// search floor where the next create still finds the lowest free block. The
// floor raise now goes out after the allocation's other writes; a raise that
// overtook a release's lowering would strand the released block below it.
func TestRacingCreatesAndDeletesKeepTheFloorAtTheLowestFreeBlock(t *testing.T) {
	const creates, deletes = 16, 10
	r, db := newTenantREST(t, testdb.MaxConns(creates+deletes+4))
	ctx := claimCtx(testProject)
	warm := create(t, r, endpointClaim("warm", "net-a", "ams"))
	for i := range deletes {
		create(t, r, endpointClaim(fmt.Sprintf("pre-%d", i), "net-a", "ams"))
	}

	var wg sync.WaitGroup
	errs := make(chan error, creates+deletes)
	for i := range creates {
		wg.Go(func() {
			if _, err := r.Create(ctx, endpointClaim(fmt.Sprintf("c-%d", i), "net-a", "ams"), nil, &metav1.CreateOptions{}); err != nil {
				errs <- fmt.Errorf("create c-%d: %w", i, err)
			}
		})
	}
	for i := range deletes {
		wg.Go(func() {
			if _, _, err := r.Delete(ctx, fmt.Sprintf("pre-%d", i), nil, &metav1.DeleteOptions{}); err != nil {
				errs <- fmt.Errorf("delete pre-%d: %w", i, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	poolKey := subnetPoolKey(warm)
	existing := allocatedIn(t, db, poolKey)
	for i := range existing {
		for j := i + 1; j < len(existing); j++ {
			if existing[i].Contains(existing[j].IP) || existing[j].Contains(existing[i].IP) {
				t.Fatalf("blocks %s and %s overlap", existing[i].String(), existing[j].String())
			}
		}
	}

	_, parent, err := net.ParseCIDR(poolCIDR(t, db, poolKey))
	if err != nil {
		t.Fatalf("parse pool CIDR: %v", err)
	}
	want, err := allocation.FindFirstAvailableBlock([]net.IPNet{*parent}, existing, 96, allocation.FirstFit)
	if err != nil {
		t.Fatalf("lowest free block: %v", err)
	}
	probe := create(t, r, endpointClaim("probe", "net-a", "ams"))
	if probe.Status.AllocatedCIDR != want.String() {
		t.Fatalf("the next create got %s, want the lowest free block %s", probe.Status.AllocatedCIDR, want.String())
	}
}

// allocatedIn is every block allocated in poolKey, in any space.
func allocatedIn(t *testing.T, db *pgxpool.Pool, poolKey string) []net.IPNet {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr) FROM ipam_cidr_allocations WHERE pool_key = $1`, poolKey)
	if err != nil {
		t.Fatalf("read allocations: %v", err)
	}
	defer rows.Close()
	var out []net.IPNet
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan allocation: %v", err)
		}
		_, block, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		out = append(out, *block)
	}
	return out
}

func poolCIDR(t *testing.T, db *pgxpool.Pool, poolKey string) string {
	t.Helper()
	var cidr string
	if err := db.QueryRow(context.Background(),
		`SELECT ipam_data_to_jsonb(data) -> 'status' ->> 'allocatedCIDR' FROM ipam_objects WHERE key = $1`, poolKey).Scan(&cidr); err != nil {
		t.Fatalf("read pool %s: %v", poolKey, err)
	}
	return cidr
}
