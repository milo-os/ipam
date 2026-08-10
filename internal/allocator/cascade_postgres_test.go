package allocator

// The cascade's concurrency design cannot be tested with a fake transaction.
// Every property that matters is a property of PostgreSQL: that
// `ON CONFLICT ... DO NOTHING` waits on an uncommitted inserter's speculative
// index entry rather than returning immediately, that a second statement under
// READ COMMITTED takes a fresh snapshot and therefore sees the winner's row, and
// that a DEFERRABLE INITIALLY DEFERRED foreign key lets the identity row name a
// pool object written later in the same transaction. A test against a fake would
// only assert that the Go code calls the functions it calls.
//
// So these tests run against a real Postgres in a throwaway container, applying
// the real migrations, and skip when Docker is unavailable. The unit tests in
// cascade_test.go cover the resolution logic; these cover the race.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/component-base/metrics/legacyregistry"

	_ "go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// newMigratedPool returns a migrated, isolated database for this test.
//
// It used to start a Postgres container per test on a freshly-probed port.
// That is why these tests were a second harness with a second skip condition,
// and why eighteen of them under `go test ./...` contended on the daemon and on
// ports — TestTwoTenantsWithOverlappingRootsAreServedFromTheirOwnPool failed
// that way while passing alone. internal/testdb now owns backend selection for
// the whole repo: a container is started once per package when
// IPAM_TEST_POSTGRES_DSN is unset, and each test still gets a private schema,
// so nothing observable here changed.
//
// MaxConns is sized above the largest herd in this file for the reason
// testdb.MaxConns documents: an unset MaxConns queues a herd at the client and
// the test passes without ever creating contention.
func newMigratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testdb.Pool(t, testSchemaName(t), testdb.MaxConns(128))
}

// testSchemaName derives a unique, legal schema identifier from the test name.
func testSchemaName(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("alloc_")
	for _, r := range strings.ToLower(t.Name()) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// seedObject writes an API object straight into ipam_objects, the way an
// operator-authored pool or class arrives.
func seedObject(t *testing.T, pool *pgxpool.Pool, key, kind, name string, obj any) {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal %s %s: %v", kind, name, err)
	}
	_, err = pool.Exec(platformCtx(),
		`INSERT INTO ipam_objects (key, kind, namespace, name, data) VALUES ($1, $2, '', $3, $4)`,
		key, kind, name, data)
	if err != nil {
		t.Fatalf("seed %s %s: %v", kind, name, err)
	}

	// A seeded pool must leave the same trace a pool written through the
	// registry does. Discovery reads ipam_pool_class_offer, which the registry
	// maintains on every spec write; a fixture that skipped it would be a pool
	// that exists, names its classes in JSON, and is invisible to the only
	// query that looks for it. The test would then fail for a reason that has
	// nothing to do with what it is testing.
	if p, ok := obj.(*ipamv1alpha1.IPPool); ok && len(p.Spec.ClassNames) > 0 {
		if _, err := pool.Exec(platformCtx(),
			`SELECT ipam_sync_pool_class_offers($1, $2)`, key, p.Spec.ClassNames); err != nil {
			t.Fatalf("sync class offers for %s: %v", key, err)
		}
	}
}

// seedTenantChain writes the design doc's IPv6 chain — a root pool offering
// tenant-network-ipv6, and the three classes below it.
func seedTenantChain(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	root := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-v6"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       "fd20::/20",
			IPFamily:   ipamv1alpha1.IPv6,
			ClassNames: []string{"tenant-network-ipv6"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: "fd20::/20",
			IPFamily:      ipamv1alpha1.IPv6,
		},
	}
	seedObject(t, pool, platformKey("ippools", "tenant-v6"), "IPPool", "tenant-v6", root)

	classes := []*ipamv1alpha1.IPClass{
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-network-ipv6"},
			Spec: ipamv1alpha1.IPClassSpec{
				IPFamily:             ipamv1alpha1.IPv6,
				PoolPer:              []string{"network"},
				AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 48, Max: 48},
				ReclaimPolicy:        ipamv1alpha1.ReclaimRetain,
			},
		},
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-subnet-ipv6"},
			Spec: ipamv1alpha1.IPClassSpec{
				IPFamily:             ipamv1alpha1.IPv6,
				ParentClassName:      "tenant-network-ipv6",
				PoolPer:              []string{"network", "location"},
				AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 64, Max: 64},
				ReclaimPolicy:        ipamv1alpha1.ReclaimRetain,
			},
		},
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-endpoint-ipv6"},
			Spec: ipamv1alpha1.IPClassSpec{
				IPFamily:             ipamv1alpha1.IPv6,
				ParentClassName:      "tenant-subnet-ipv6",
				UniqueWithin:         []string{"network"},
				AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 96, Max: 96},
			},
		},
	}
	for _, c := range classes {
		seedObject(t, pool, platformKey("ipclasses", c.Name), "IPClass", c.Name, c)
	}
}

func scopeFor(network, location string) map[string]ipam.ScopeRef {
	return map[string]ipam.ScopeRef{
		"network":  {APIGroup: "networking.datumapis.com", Kind: "Network", Name: network},
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: location},
	}
}

func loadClassFromDB(t *testing.T, pool *pgxpool.Pool, name string) *ipamv1alpha1.IPClass {
	t.Helper()
	tx, err := pool.Begin(platformCtx())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(platformCtx()) }()
	class, err := LoadClass(platformCtx(), tx, name)
	if err != nil {
		t.Fatalf("load class %s: %v", name, err)
	}
	return class
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(platformCtx(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A single first claim into an unused scope builds the whole chain: the
// network's prefix and that location's subnet, each committed on its own.
func TestCascadeProvisionsTheChain(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()

	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")
	poolKey, err := ResolvePool(ctx, db, leaf, scopeFor("default", "us-central-1"), "acme")
	if err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}

	if n := countRows(t, db, `SELECT count(*) FROM ipam_pool_identity`); n != 2 {
		t.Errorf("pool identities = %d, want 2 (network + subnet)", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE kind = 'IPPool'`); n != 3 {
		t.Errorf("pools = %d, want 3 (root + network + subnet)", n)
	}
	// The leaf draws from the subnet pool, which is the deepest level.
	subnetClass := loadClassFromDB(t, db, "tenant-subnet-ipv6")
	if !strings.Contains(poolKey, sanitizeName(subnetClass.Name)) {
		t.Errorf("resolved pool %q is not the subnet level", poolKey)
	}
	// Each provisioned pool consumed a block from the level above it, recorded
	// as a carve the parent holds rather than as a claim. PoolCarve rather than
	// Reservation: an edge reservation goes away with its pool, a carve means a
	// child pool still exists, and only the delete guard reads the difference.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE purpose = 'PoolCarve'`); n != 2 {
		t.Errorf("pool carve records = %d, want 2", n)
	}
	// A cascade produces no edge reservations of its own unless a class asks for
	// them, so nothing here should be wearing the Reservation label.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE purpose = 'Reservation'`); n != 0 {
		t.Errorf("edge reservations = %d, want 0; a carve must not be recorded as one", n)
	}

	// A second claim into the same scope must find both levels and build
	// nothing.
	poolKey2, err := ResolvePool(ctx, db, leaf, scopeFor("default", "us-central-1"), "acme")
	if err != nil {
		t.Fatalf("second ResolvePool: %v", err)
	}
	if poolKey2 != poolKey {
		t.Errorf("second claim resolved to %q, want the same pool %q", poolKey2, poolKey)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE kind = 'IPPool'`); n != 3 {
		t.Errorf("pools after a second claim = %d, want 3 — nothing new should be built", n)
	}
}

// A second location on an established network builds only that location's
// subnet: the network's prefix already exists and is never renumbered.
func TestCascadeReusesTheNetworkPrefixAcrossLocations(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()
	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")

	first, err := ResolvePool(ctx, db, leaf, scopeFor("default", "us-central-1"), "acme")
	if err != nil {
		t.Fatalf("first ResolvePool: %v", err)
	}
	second, err := ResolvePool(ctx, db, leaf, scopeFor("default", "eu-west-1"), "acme")
	if err != nil {
		t.Fatalf("second ResolvePool: %v", err)
	}

	if first == second {
		t.Error("two locations must get different subnet pools")
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_pool_identity WHERE class_name = 'tenant-network-ipv6'`); n != 1 {
		t.Errorf("network-level pools = %d, want 1 — a second location must not renumber the network", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_pool_identity WHERE class_name = 'tenant-subnet-ipv6'`); n != 2 {
		t.Errorf("subnet-level pools = %d, want 2", n)
	}
	// Both subnets are carved from the one network prefix, so both must sit
	// inside it and not overlap. The exclusion constraint would have rejected an
	// overlap outright, so reaching here already proves non-overlap; this checks
	// containment.
	var networkCIDR string
	if err := db.QueryRow(ctx,
		`SELECT ipam_data_to_jsonb(o.data) -> 'status' ->> 'allocatedCIDR'
		   FROM ipam_objects o JOIN ipam_pool_identity i ON i.pool_key = o.key
		  WHERE i.class_name = 'tenant-network-ipv6'`).Scan(&networkCIDR); err != nil {
		t.Fatalf("read network CIDR: %v", err)
	}
	_, networkNet, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		t.Fatalf("parse network CIDR %q: %v", networkCIDR, err)
	}
	rows, err := db.Query(ctx,
		`SELECT ipam_data_to_jsonb(o.data) -> 'status' ->> 'allocatedCIDR'
		   FROM ipam_objects o JOIN ipam_pool_identity i ON i.pool_key = o.key
		  WHERE i.class_name = 'tenant-subnet-ipv6'`)
	if err != nil {
		t.Fatalf("read subnet CIDRs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cidr string
		if err := rows.Scan(&cidr); err != nil {
			t.Fatalf("scan subnet CIDR: %v", err)
		}
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("parse subnet CIDR %q: %v", cidr, err)
		}
		if !networkNet.Contains(ip) {
			t.Errorf("subnet %s is not inside the network prefix %s", cidr, networkCIDR)
		}
	}
}

// This is the test the design exists for.
//
// It really interleaves: N goroutines call ResolvePool at once for the *same*
// previously-unused scope, released together from a barrier, against a real
// database. Nothing is stubbed and no ordering is imposed — the racing is done
// by the goroutines and arbitrated by PostgreSQL, which is the only thing that
// can arbitrate it.
//
// What it asserts is the whole point of the identity-first ordering:
//
//   - exactly one pool exists at each level, so the losers read the winner's
//     pool rather than creating a second or failing,
//   - every caller returns that same pool,
//   - the root pool gave up exactly one block, so the herd produced one
//     root-lock acquisition rather than N.
//
// A run before the ordering was right fails on the first two: without the
// deferred FK the winner's insert errors, and with `DO NOTHING` instead of
// `DO UPDATE` the losers do not wait and return an unresolvable empty key.
func TestConcurrentFirstClaimsProduceOnePool(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()
	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")

	// 100 rather than a token handful. The two upsert forms are
	// indistinguishable at 24 and diverge by ~5x at 100 — this is the regime the
	// DO NOTHING choice was made for, so it is the regime the test should run in.
	const racers = 100
	lossesBefore := cascadeLosses(t, "tenant-subnet-ipv6")
	var wg sync.WaitGroup
	start := make(chan struct{})
	keys := make([]string, racers)
	errs := make([]error, racers)

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys[i], errs[i] = ResolvePool(ctx, db, leaf, scopeFor("default", "us-central-1"), "acme")
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d failed: %v", i, err)
		}
	}
	for i, key := range keys {
		if key == "" {
			t.Fatalf("racer %d resolved an empty pool key", i)
		}
		if key != keys[0] {
			t.Fatalf("racer %d resolved %q but racer 0 resolved %q — the losers did not read the winner's pool", i, key, keys[0])
		}
	}

	if n := countRows(t, db, `SELECT count(*) FROM ipam_pool_identity WHERE class_name = 'tenant-network-ipv6'`); n != 1 {
		t.Errorf("network-level pools = %d, want exactly 1", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_pool_identity WHERE class_name = 'tenant-subnet-ipv6'`); n != 1 {
		t.Errorf("subnet-level pools = %d, want exactly 1", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE kind = 'IPPool'`); n != 3 {
		t.Errorf("pools = %d, want 3 (root + one network + one subnet)", n)
	}
	// Without this the test could pass by accident: if every goroutine but the
	// first arrived after the winner had committed, they would all take the
	// no-lock fast path and the contention path would never run. Requiring at
	// least one recorded loss is what makes this a race test rather than a
	// sequential one.
	losses := cascadeLosses(t, "tenant-subnet-ipv6") - lossesBefore
	if losses < 1 {
		t.Errorf("no caller lost the identity upsert (losses=%v); the goroutines did not actually contend, so this run proves nothing about the race path", losses)
	}
	// Logged rather than asserted on an exact figure: callers arriving after the
	// winner commits take the no-lock fast path and are neither winners nor
	// losers, so the split moves run to run. A healthy run shows most of the
	// herd losing, which is what says they arrived together.
	t.Logf("herd of %d produced %v identity losses at the subnet level", racers, losses)
	// One block out of the root, not N: this is the property the identity-first
	// ordering buys, and the one a naive implementation loses.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1`, platformKey("ippools", "tenant-v6")); n != 1 {
		t.Errorf("blocks carved from the root pool = %d, want 1", n)
	}
}

// Concurrent claims for *different* scopes must not serialise behind each other
// beyond the one block each takes from the shared root. Distinct networks are
// independent address spaces and their pools are independent rows.
func TestConcurrentDistinctScopesEachGetTheirOwnPool(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()
	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")

	const networks = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	keys := make([]string, networks)
	errs := make([]error, networks)

	for i := range networks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys[i], errs[i] = ResolvePool(ctx, db, leaf,
				scopeFor(fmt.Sprintf("network-%d", i), "us-central-1"), "acme")
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("network %d failed: %v", i, err)
		}
	}
	seen := map[string]bool{}
	for i, key := range keys {
		if seen[key] {
			t.Fatalf("network %d shares pool %q with another network — the scopes were not distinguished", i, key)
		}
		seen[key] = true
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_pool_identity WHERE class_name = 'tenant-network-ipv6'`); n != networks {
		t.Errorf("network-level pools = %d, want %d", n, networks)
	}
	// The exclusion constraint would have rejected any overlap, so a clean run
	// is itself the non-overlap assertion. This checks the count instead: every
	// network took exactly one block from the root.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1`, platformKey("ippools", "tenant-v6")); n != networks {
		t.Errorf("blocks carved from the root = %d, want %d", n, networks)
	}
}

// The identity row is claimed before the pool object exists, which only works
// because the foreign key is deferred to COMMIT. If someone makes it immediate,
// every first claim fails — so assert the constraint's mode directly rather than
// waiting to discover it.
func TestPoolIdentityForeignKeyIsDeferred(t *testing.T) {
	db := newMigratedPool(t)
	var deferrable, deferred bool
	err := db.QueryRow(platformCtx(),
		`SELECT condeferrable, condeferred FROM pg_constraint
		  WHERE conname = 'ipam_pool_identity_pool_fk'`).Scan(&deferrable, &deferred)
	if err != nil {
		t.Fatalf("read constraint: %v", err)
	}
	if !deferrable || !deferred {
		t.Errorf("ipam_pool_identity_pool_fk is deferrable=%v deferred=%v; the cascade requires both true", deferrable, deferred)
	}
}

// cascadeLosses reads the "lost" counter for a class out of the metrics
// registry. The concurrency test uses it to check that it really raced: if no
// caller ever lost the identity upsert, the goroutines serialised and the test
// proved nothing about the contention path.
func cascadeLosses(t *testing.T, className string) float64 {
	t.Helper()
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "ipam_cascade_outcomes_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			var gotClass, gotOutcome string
			for _, label := range m.GetLabel() {
				switch label.GetName() {
				case "class":
					gotClass = label.GetValue()
				case "outcome":
					gotOutcome = label.GetValue()
				}
			}
			if gotClass == className && gotOutcome == "lost" {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// --- reclaim policy ---------------------------------------------------------

// Before this was fixed, reclaim policy was ignored in both directions:
// allocations were written with a hardcoded 'Delete' literal that no caller
// could override, and Release issued an unconditional DELETE. A claim asking for
// Retain lost its address to the next claimant, with nothing in the API to show
// it had happened.
//
// This runs the real SQL for both policies.
func TestReleaseHonoursReclaimPolicy(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	poolKey := platformKey("ippools", "tenant-v6")

	allocate := func(t *testing.T, name, policy string) string {
		t.Helper()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		cidr, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
			PoolKey:       poolKey,
			AllocationKey: "/alloc/" + name,
			ClaimKey:      "/claim/" + name,
			ClassName:     "tenant-network-ipv6",
			ScopeDigest:   scope.AddressSpaceDigest("", nil),
			PrefixLength:  48,
			IPFamily:      "IPv6",
			ReclaimPolicy: policy,
			OwnerProject:  "acme",
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("allocate %s: %v", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
		return cidr
	}

	release := func(t *testing.T, name string) []ReleaseOutcome {
		t.Helper()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		outcomes, err := alloc.Release(ctx, tx, "/claim/"+name)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("release %s: %v", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit release %s: %v", name, err)
		}
		return outcomes
	}

	retainedCIDR := allocate(t, "retained", string(ipamv1alpha1.ReclaimRetain))
	deletedCIDR := allocate(t, "deleted", string(ipamv1alpha1.ReclaimDelete))

	t.Run("Delete releases the address", func(t *testing.T) {
		outcomes := release(t, "deleted")
		if len(outcomes) != 1 || outcomes[0].Retained {
			t.Fatalf("outcomes = %+v, want one non-retained", outcomes)
		}
		if n := countRows(t, db,
			`SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = '/alloc/deleted'`); n != 0 {
			t.Errorf("the row survived a Delete release: %d rows", n)
		}
	})

	t.Run("Retain keeps the address held and unbound", func(t *testing.T) {
		outcomes := release(t, "retained")
		if len(outcomes) != 1 || !outcomes[0].Retained {
			t.Fatalf("outcomes = %+v, want one retained", outcomes)
		}

		var claimKey *string
		var ownerProject string
		if err := db.QueryRow(ctx,
			`SELECT claim_key, owner_project FROM ipam_cidr_allocations WHERE allocation_key = '/alloc/retained'`,
		).Scan(&claimKey, &ownerProject); err != nil {
			t.Fatalf("the retained row was deleted: %v", err)
		}
		if claimKey != nil {
			t.Errorf("claim_key = %q, want NULL — retention unbinds", *claimKey)
		}
		// A retained address that counted against nobody would leave nothing
		// pressuring anyone to hand it back.
		if ownerProject != "acme" {
			t.Errorf("owner_project = %q, want it preserved so the address still has a holder", ownerProject)
		}
	})

	t.Run("a retained address is not handed to the next claimant", func(t *testing.T) {
		next := allocate(t, "next", string(ipamv1alpha1.ReclaimDelete))
		if next == retainedCIDR {
			t.Fatalf("the next claim got the retained address %s — retention did not hold it", retainedCIDR)
		}
		// The released one, by contrast, is genuinely back in circulation: it is
		// the lowest free block, so the first-fit search returns it.
		if next != deletedCIDR {
			t.Errorf("next allocation = %s, want the released block %s back in circulation", next, deletedCIDR)
		}
	})

	t.Run("ForceRelease returns a retained address without a claim", func(t *testing.T) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := alloc.ForceRelease(ctx, tx, "/alloc/retained"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("ForceRelease: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if n := countRows(t, db,
			`SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key = '/alloc/retained'`); n != 0 {
			t.Errorf("ForceRelease left %d rows", n)
		}
	})
}

// Two networks may hold the same address out of one shared range — that is what
// uniqueWithin: [network] states, and it is the whole IPv4 tenant story. The
// exclusion constraint must permit it, and the search must not see the other
// network's allocation.
func TestAllocationsInDifferentScopesMayShareAnAddress(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	shared := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-v4-us-central-1"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       "10.128.0.0/20",
			IPFamily:   ipamv1alpha1.IPv4,
			ClassNames: []string{"tenant-endpoint-ipv4"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "10.128.0.0/20", IPFamily: ipamv1alpha1.IPv4,
		},
	}
	poolKey := platformKey("ippools", "tenant-v4-us-central-1")
	seedObject(t, db, poolKey, "IPPool", shared.Name, shared)

	allocateIn := func(t *testing.T, network, name string) string {
		t.Helper()
		digest := scope.AddressSpaceDigest("", map[string]ipam.ScopeRef{
			"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: network},
		})
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		cidr, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
			PoolKey:       poolKey,
			AllocationKey: "/alloc/" + name,
			ClaimKey:      "/claim/" + name,
			ClassName:     "tenant-endpoint-ipv4",
			ScopeDigest:   digest,
			PrefixLength:  32,
			IPFamily:      "IPv4",
			ReclaimPolicy: string(ipamv1alpha1.ReclaimDelete),
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("allocate for %s: %v", network, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit for %s: %v", network, err)
		}
		return cidr
	}

	a := allocateIn(t, "network-a", "a")
	b := allocateIn(t, "network-b", "b")
	if a != b {
		t.Errorf("two networks drawing from a shared range got %s and %s; both should get the first free address, because neither sees the other", a, b)
	}

	// Within one network, the second claim must get a different address.
	c := allocateIn(t, "network-a", "a2")
	if c == a {
		t.Errorf("two claims in one network both got %s", a)
	}
}

// A gateway reservation on a cascade-provisioned subnet is the one thing in the
// worked example that could not be expressed until IPClassSpec.Reservations
// existed: the subnet pools are built by the allocator, so nobody can hand-write
// a reservation on them.
//
// The property that matters is not that a row exists — it is that the reserved
// block is never handed out.
func TestCascadeAppliesClassReservations(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()

	// The subnet class provisions the /64s endpoints carve from, so it is where
	// "the first /96 of every subnet holds the gateway" is stated.
	subnet := loadClassFromDB(t, db, "tenant-subnet-ipv6")
	subnet.Spec.Reservations = &ipamv1alpha1.ReservationSpec{Leading: 1, UnitPrefixLength: 96}
	reseedClass(t, db, subnet)

	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")
	poolKey, err := ResolvePool(ctx, db, leaf, scopeFor("default", "us-central-1"), "acme")
	if err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}

	// The reservation is recorded against the subnet pool, held by the pool
	// itself with no claim.
	var reservedCIDR string
	var claimKey *string
	if err := db.QueryRow(ctx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr), claim_key
		   FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND purpose = 'Reservation'`, poolKey,
	).Scan(&reservedCIDR, &claimKey); err != nil {
		t.Fatalf("the subnet pool carries no reservation: %v", err)
	}
	if claimKey != nil {
		t.Errorf("a reservation must have no claim, got %q", *claimKey)
	}

	// It is also visible on the pool, so `kubectl get ippool -o yaml` explains
	// why the first block is unavailable without anyone finding the class.
	var leading int
	if err := db.QueryRow(ctx,
		`SELECT (ipam_data_to_jsonb(data) -> 'spec' -> 'reservations' ->> 'leading')::int
		   FROM ipam_objects WHERE key = $1`, poolKey).Scan(&leading); err != nil {
		t.Fatalf("read pool reservations: %v", err)
	}
	if leading != 1 {
		t.Errorf("pool spec.reservations.leading = %d, want 1", leading)
	}

	// The proof: claim repeatedly and never receive the reserved block.
	alloc := NewPostgresPrefixAllocator()
	digest := scope.AddressSpaceDigest("", map[string]ipam.ScopeRef{"network": scopeFor("default", "us-central-1")["network"]})
	for i := range 4 {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		cidr, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
			PoolKey:       poolKey,
			AllocationKey: fmt.Sprintf("/alloc/endpoint-%d", i),
			ClaimKey:      fmt.Sprintf("/claim/endpoint-%d", i),
			ClassName:     "tenant-endpoint-ipv6",
			ScopeDigest:   digest,
			PrefixLength:  96,
			IPFamily:      "IPv6",
			ReclaimPolicy: string(ipamv1alpha1.ReclaimDelete),
			OwnerProject:  "acme",
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("allocate endpoint %d: %v", i, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit endpoint %d: %v", i, err)
		}
		if cidr == reservedCIDR {
			t.Fatalf("endpoint %d was handed the reserved block %s", i, reservedCIDR)
		}
	}

	// A reservation is excluded from *every* address space carved from the pool,
	// not just the one whose claim triggered it — one reservation per pool, not
	// one per network. A second network drawing from the same subnet pool must
	// also be refused the gateway block.
	otherDigest := scope.AddressSpaceDigest("", map[string]ipam.ScopeRef{
		"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "other"},
	})
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cidr, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
		PoolKey:       poolKey,
		AllocationKey: "/alloc/other-network",
		ClaimKey:      "/claim/other-network",
		ClassName:     "tenant-endpoint-ipv6",
		ScopeDigest:   otherDigest,
		PrefixLength:  96,
		IPFamily:      "IPv6",
		ReclaimPolicy: string(ipamv1alpha1.ReclaimDelete),
		OwnerProject:  "acme",
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("allocate in a second scope: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if cidr == reservedCIDR {
		t.Fatalf("a second address space was handed the reserved block %s; a reservation belongs to the pool, not to one space", reservedCIDR)
	}
}

// reseedClass overwrites a stored IPClass, for tests that need to change class
// policy after the catalog is seeded.
func reseedClass(t *testing.T, pool *pgxpool.Pool, class *ipamv1alpha1.IPClass) {
	t.Helper()
	data, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal class: %v", err)
	}
	if _, err := pool.Exec(platformCtx(),
		`UPDATE ipam_objects SET data = $2 WHERE key = $1`,
		platformKey("ipclasses", class.Name), data); err != nil {
		t.Fatalf("reseed class %s: %v", class.Name, err)
	}
}

// --- regressions found by e2e against a live cluster -------------------------

// Deleting a cascade-provisioned pool used to leave its carve against the parent
// behind. Because pool names are a pure function of the scope, the next claim
// into that scope recomputed the same allocation key, hit the unique index, and
// wedged the scope permanently — while the parent became undeletable, its guard
// reporting an allocation the operator had no API to release.
//
// Two things were wrong and both are fixed: the provisioned pool now records a
// parentPoolRef, and the delete releases the carve whether or not it does.
func TestDeletingAProvisionedPoolReleasesItsCarve(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()
	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")

	first, err := ResolvePool(ctx, db, leaf, scopeFor("vpc-x", "us-central-1"), "acme")
	if err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}

	// The provisioned pool must record what it was carved from — an operator
	// reading it should see the lineage, and the delete path keys off it.
	var parentRef string
	if err := db.QueryRow(ctx,
		`SELECT coalesce(ipam_data_to_jsonb(data) -> 'spec' -> 'parentPoolRef' ->> 'name', '')
		   FROM ipam_objects WHERE key = $1`, first).Scan(&parentRef); err != nil {
		t.Fatalf("read parentPoolRef: %v", err)
	}
	if parentRef == "" {
		t.Error("a cascade-provisioned pool must record its parentPoolRef")
	}

	// Delete both provisioned levels, deepest first, the way an operator
	// reclaiming a network's space would.
	subnetKey, networkKey := first, ""
	if err := db.QueryRow(ctx,
		`SELECT pool_key FROM ipam_pool_identity WHERE class_name = 'tenant-network-ipv6'`).Scan(&networkKey); err != nil {
		t.Fatalf("find network pool: %v", err)
	}
	for _, key := range []string{subnetKey, networkKey} {
		deletePoolLikeRegistry(t, db, key)
	}

	// No carve may survive anywhere.
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`); n != 0 {
		t.Fatalf("%d allocation row(s) survived the pool deletions; the carve leaked", n)
	}

	// And the scope must be reusable — this is the wedge.
	second, err := ResolvePool(ctx, db, leaf, scopeFor("vpc-x", "us-central-1"), "acme")
	if err != nil {
		t.Fatalf("re-claiming into a scope whose pools were deleted must work, got: %v", err)
	}
	if second != first {
		t.Errorf("the rebuilt pool key = %q, want the same deterministic key %q", second, first)
	}
}

// A pool's own edge reservations must not block its deletion. They are the
// pool's bookkeeping and go away with it; counting them made a pool with any
// reservation permanently undeletable, with a message telling the operator to
// release claims that did not exist.
func TestPoolReservationsDoNotBlockDeletion(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()

	poolKey := platformKey("ippools", "public-v4")
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "public-v4"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:     "10.210.0.0/20",
			IPFamily: ipamv1alpha1.IPv4,
			Reservations: &ipamv1alpha1.ReservationSpec{
				Leading: 2, Trailing: 2, UnitPrefixLength: 32,
			},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "10.210.0.0/20", IPFamily: ipamv1alpha1.IPv4,
		},
	}
	seedObject(t, db, poolKey, "IPPool", "public-v4", pool)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parents := []net.IPNet{mustCIDR(t, "10.210.0.0/20")}
	if _, err := ProvisionReservations(ctx, tx, poolKey, "IPv4", "", parents,
		Reservation{Leading: 2, Trailing: 2, UnitPrefixLength: 32}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ProvisionReservations: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The four blocks the addressing plan expects, at both edges.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose = 'Reservation'`, poolKey); n != 4 {
		t.Fatalf("reservation rows = %d, want 4", n)
	}

	// The delete guard must not see them as somebody else's claim on the pool.
	// Mirrors the guard's predicate: everything that is not an edge reservation
	// blocks, which is claims and the carves backing child pools.
	blocking := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND purpose <> 'Reservation'`, poolKey)
	if blocking != 0 {
		t.Errorf("a pool's own reservations must not block its deletion, %d counted as blocking", blocking)
	}

	// And they must go with the pool rather than outliving it.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := ReleasePoolReservations(ctx, tx, poolKey); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ReleasePoolReservations: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1`, poolKey); n != 0 {
		t.Errorf("%d reservation row(s) outlived the pool", n)
	}
}

// deletePoolLikeRegistry performs the delete the IPPool registry performs, so
// the regression above exercises the real teardown rather than a raw DELETE.
func deletePoolLikeRegistry(t *testing.T, db *pgxpool.Pool, poolKey string) {
	t.Helper()
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := alloc.ForceRelease(ctx, tx, poolKey); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ForceRelease %s: %v", poolKey, err)
	}
	if err := ReleasePoolReservations(ctx, tx, poolKey); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ReleasePoolReservations %s: %v", poolKey, err)
	}
	if _, err := deleteObject(ctx, tx, poolKey); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("deleteObject %s: %v", poolKey, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit delete of %s: %v", poolKey, err)
	}
}

func mustCIDR(t *testing.T, s string) net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return *n
}

// The eligibility half of a pool's scope, proven end to end.
//
// `public-unicast-ipv4` names no scope role at all — `uniqueWithin: []`, no
// `poolPer` — while its pools carry `scope.location`. The claim reaches the
// right location's pool because the *claim* carries a location, not because the
// class mentions one. A validation rule that required the class to name the role
// rejected this exact configuration, which is two of the design's five worked
// example classes and every fabric class.
func TestALocatedPoolServesAClassThatNamesNoLocation(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()

	locatedPool := func(name, cidr, location string) {
		pool := &ipamv1alpha1.IPPool{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: ipamv1alpha1.IPPoolSpec{
				CIDR:       cidr,
				IPFamily:   ipamv1alpha1.IPv4,
				ClassNames: []string{"public-unicast-ipv4"},
				Scope: map[string]ipamv1alpha1.ScopeRef{
					"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: location},
				},
			},
			Status: ipamv1alpha1.IPPoolStatus{
				Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: ipamv1alpha1.IPv4,
			},
		}
		seedObject(t, db, platformKey("ippools", name), "IPPool", name, pool)
	}
	locatedPool("public-v4-us-central-1", "198.51.100.0/24", "us-central-1")
	locatedPool("public-v4-eu-west-1", "203.0.113.0/24", "eu-west-1")

	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "public-unicast-ipv4"},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily:             ipamv1alpha1.IPv4,
			AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 32, Max: 32},
			ReclaimPolicy:        ipamv1alpha1.ReclaimRetain,
		},
	}
	seedObject(t, db, platformKey("ipclasses", "public-unicast-ipv4"), "IPClass", class.Name, class)

	// A claim from each location must reach that location's pool and no other.
	for location, wantPool := range map[string]string{
		"us-central-1": platformKey("ippools", "public-v4-us-central-1"),
		"eu-west-1":    platformKey("ippools", "public-v4-eu-west-1"),
	} {
		got, err := ResolvePool(ctx, db, class, scopeFor("default", location), "acme")
		if err != nil {
			t.Fatalf("a claim from %s could not reach a public pool: %v", location, err)
		}
		if got != wantPool {
			t.Errorf("a claim from %s resolved to %q, want %q", location, got, wantPool)
		}
	}

	// A claim carrying no location must not be handed located space by default.
	// The asymmetry is deliberate: a pool that declares where it is serves only
	// callers that can say where they are.
	if _, err := ResolvePool(ctx, db, class,
		map[string]ipam.ScopeRef{"network": scopeFor("default", "us-central-1")["network"]}, "acme"); err == nil {
		t.Error("a claim naming no location must not reach a located pool")
	}
}

// The three purpose values, and which reader acts on which distinction.
//
// A pool holding both an edge reservation and a child pool's carve is the case
// that motivated the third value: the search must treat them alike, and the
// delete guard must not. Before PoolCarve existed they were told apart by an
// allocation-key naming convention, and the pool was undeletable regardless
// because its own reservations were counted as claims against it.
func TestPoolCarveIsDistinctFromAnEdgeReservation(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()

	// Give the network class an edge reservation, so the pool it provisions
	// carries one — and then let a claim cascade so that same pool also gets a
	// carve taken out of it by the subnet below.
	network := loadClassFromDB(t, db, "tenant-network-ipv6")
	network.Spec.Reservations = &ipamv1alpha1.ReservationSpec{Leading: 1, UnitPrefixLength: 64}
	reseedClass(t, db, network)

	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")
	if _, err := ResolvePool(ctx, db, leaf, scopeFor("default", "us-central-1"), "acme"); err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}

	var networkPool string
	if err := db.QueryRow(ctx,
		`SELECT pool_key FROM ipam_pool_identity WHERE class_name = 'tenant-network-ipv6'`).Scan(&networkPool); err != nil {
		t.Fatalf("find network pool: %v", err)
	}

	// One of each, in the same pool, distinguishable.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose = 'Reservation'`, networkPool); n != 1 {
		t.Errorf("edge reservations in the network pool = %d, want 1", n)
	}
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose = 'PoolCarve'`, networkPool); n != 1 {
		t.Errorf("carves in the network pool = %d, want 1", n)
	}

	// What the delete guard sees: the carve blocks, the reservation does not.
	blocking := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose <> 'Reservation'`, networkPool)
	if blocking != 1 {
		t.Errorf("blocking allocations = %d, want 1 — the carve must block the delete and the reservation must not", blocking)
	}

	// What the allocator's search sees: both, identically. Neither belongs to an
	// address space, so both are excluded from every scope carved from the pool.
	scopeIndependent := countRows(t, db,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose <> 'Claim'`, networkPool)
	if scopeIndependent != 2 {
		t.Errorf("scope-independent rows = %d, want 2 — the search must not distinguish a carve from a reservation", scopeIndependent)
	}

	// And the carve's identity is the child pool's own object key, which is what
	// makes migration 003's backfill discriminator exact rather than a heuristic.
	var carveKey string
	if err := db.QueryRow(ctx,
		`SELECT allocation_key FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose = 'PoolCarve'`,
		networkPool).Scan(&carveKey); err != nil {
		t.Fatalf("read carve key: %v", err)
	}
	var kind string
	if err := db.QueryRow(ctx, `SELECT kind FROM ipam_objects WHERE key = $1`, carveKey).Scan(&kind); err != nil {
		t.Fatalf("a carve's allocation_key must name a real object: %v", err)
	}
	if kind != "IPPool" {
		t.Errorf("the object a carve names is a %s, want IPPool", kind)
	}
}

// Migration 003's backfill re-derives PoolCarve by finding the child pool object
// a carve names, so it depends on that object existing. The premise is sound —
// a carve *is* a child pool's block — but only as long as no API path can delete
// the pool object while leaving the carve behind.
//
// Nothing in the schema enforces it: the foreign key on
// ipam_cidr_allocations.pool_key protects a pool against losing allocations held
// *in* it, while a carve's allocation_key names the child pool and carries no
// constraint at all. So this asserts the premise directly rather than assuming
// it, and it is the assertion that would catch a future delete path that skips
// the registry's teardown — as IPPool's DeleteCollection did until it was
// overridden.
func TestNoCarveOutlivesTheChildPoolItNames(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()
	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")

	if _, err := ResolvePool(ctx, db, leaf, scopeFor("default", "us-central-1"), "acme"); err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}

	assertNoOrphanedCarves := func(when string) {
		t.Helper()
		var orphans int
		if err := db.QueryRow(ctx,
			`SELECT count(*) FROM ipam_cidr_allocations a
			  WHERE a.purpose = 'PoolCarve'
			    AND NOT EXISTS (
			          SELECT 1 FROM ipam_objects o
			           WHERE o.key = a.allocation_key AND o.kind = 'IPPool')`,
		).Scan(&orphans); err != nil {
			t.Fatalf("count orphaned carves %s: %v", when, err)
		}
		if orphans != 0 {
			t.Errorf("%d carve(s) name a pool that does not exist %s; migration 003's backfill would fold them to Reservation and the scope would wedge", orphans, when)
		}
	}

	assertNoOrphanedCarves("after provisioning")

	// Tear the chain down the way the registry does, deepest first.
	var pools []string
	rows, err := db.Query(ctx, `SELECT pool_key FROM ipam_pool_identity ORDER BY class_name DESC`)
	if err != nil {
		t.Fatalf("list provisioned pools: %v", err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			t.Fatalf("scan pool key: %v", err)
		}
		pools = append(pools, key)
	}
	rows.Close()

	for _, key := range pools {
		deletePoolLikeRegistry(t, db, key)
		assertNoOrphanedCarves("after deleting " + key)
	}
}

// Retention must not be a one-way door.
//
// The design requires a retained address to come back: it "can be force-released
// by an operator with an audit record", and on a finite public range that
// capacity has to return or the range bleeds. This walks the whole round trip
// against a real database — allocate, retain through the claim's deletion, force
// release, and confirm the address is genuinely re-issuable rather than merely
// absent from a table.
func TestRetainedAddressCanReturnToCirculation(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()

	poolKey := platformKey("ippools", "public-v4")
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "public-v4"},
		Spec:       ipamv1alpha1.IPPoolSpec{CIDR: "198.51.100.0/30", IPFamily: ipamv1alpha1.IPv4},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "198.51.100.0/30", IPFamily: ipamv1alpha1.IPv4,
		},
	}
	seedObject(t, db, poolKey, "IPPool", "public-v4", pool)

	allocate := func(t *testing.T, name, policy string) string {
		t.Helper()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		cidr, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
			PoolKey:       poolKey,
			AllocationKey: "/alloc/" + name,
			ClaimKey:      "/claim/" + name,
			ClassName:     "public-unicast-ipv4",
			ScopeDigest:   scope.AddressSpaceDigest("", nil),
			PrefixLength:  32,
			IPFamily:      "IPv4",
			ReclaimPolicy: policy,
			OwnerProject:  "acme",
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("allocate %s: %v", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
		return cidr
	}

	// A /30 holds four addresses, so exhaustion is reachable and "the address
	// came back" is provable rather than merely plausible.
	first := allocate(t, "retained", string(ipamv1alpha1.ReclaimRetain))
	for i := range 3 {
		allocate(t, fmt.Sprintf("filler-%d", i), string(ipamv1alpha1.ReclaimDelete))
	}

	// The claim goes; the address is retained.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	outcomes, err := alloc.Release(ctx, tx, "/claim/retained")
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Release: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Retained {
		t.Fatalf("outcomes = %+v, want one retained", outcomes)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit release: %v", err)
	}

	// The pool is full and the retained address is genuinely still held: a new
	// claim must fail rather than quietly reusing it.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = alloc.AllocatePrefix(ctx, tx, AllocateRequest{
		PoolKey: poolKey, AllocationKey: "/alloc/next", ClaimKey: "/claim/next",
		ScopeDigest: scope.AddressSpaceDigest("", nil), PrefixLength: 32, IPFamily: "IPv4",
		ReclaimPolicy: string(ipamv1alpha1.ReclaimDelete),
	})
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected exhaustion while the address is retained, got %v", err)
	}

	// The operator force-release — what deleting the unbound IPAllocation does.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	released, err := alloc.ForceRelease(ctx, tx, "/alloc/retained")
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ForceRelease: %v", err)
	}
	if !released {
		_ = tx.Rollback(ctx)
		t.Fatal("ForceRelease reported no row; a retained allocation must have one")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit force release: %v", err)
	}

	// And the capacity is really back — same address, issued to a new claim.
	reissued := allocate(t, "next", string(ipamv1alpha1.ReclaimDelete))
	if reissued != first {
		t.Errorf("re-issued %s, want the force-released address %s back in circulation", reissued, first)
	}
}

// Releasing a key no row matches must be reported rather than swallowed. It is
// legitimate (a root pool has no carve; a repeat delete is idempotent) but it is
// also what a drifted allocation key looks like, and that failure is silent and
// permanent: the caller reports success and the address stays held forever.
func TestForceReleaseReportsWhenThereWasNothingToRelease(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	released, err := NewPostgresPrefixAllocator().ForceRelease(ctx, tx, "/alloc/never-existed")
	if err != nil {
		t.Fatalf("releasing an absent key must not error: %v", err)
	}
	if released {
		t.Error("ForceRelease reported releasing a row that never existed")
	}
}

// Per-pool capacity gauges carry pool_key as a label, so every pool that reaches
// them becomes a permanent time series. Operator-authored pools are a bounded
// set; cascade-provisioned ones are one per (class, scope) and are not.
//
// This pins the boundary, because the failure it prevents is invisible: nothing
// breaks when a series is exported, it just accumulates, and the cost shows up
// months later as a metrics endpoint that will not scrape.
func TestProvisionedPoolsAreExcludedFromPerPoolGauges(t *testing.T) {
	db := newMigratedPool(t)
	seedTenantChain(t, db)
	ctx := platformCtx()

	before := provisionedSkips(t)

	// A cascade: the root is operator-authored, the two levels below are not.
	leaf := loadClassFromDB(t, db, "tenant-endpoint-ipv6")
	if _, err := ResolvePool(ctx, db, leaf, scopeFor("default", "us-central-1"), "acme"); err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}

	// Every pool the cascade built, read back from the identity table — the
	// authoritative record of which pools were provisioned rather than authored.
	//
	// This used to test `strings.Contains(poolKey, "project/")`, on the premise
	// that a project-prefixed key meant a cascade-provisioned pool and an
	// unprefixed one meant an operator-authored pool. That premise is gone: the
	// platform is a project, so every key is prefixed and the check matched
	// everything. It never matched what the code does either — the exclusion is
	// `pool.Spec.ClassRef != nil` (see PublishPoolCapacity), which is a property
	// of the object, not of where it is stored.
	provisioned := map[string]bool{}
	rows, err := db.Query(ctx, `SELECT pool_key FROM ipam_pool_identity`)
	if err != nil {
		t.Fatalf("read pool identities: %v", err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan pool identity: %v", err)
		}
		provisioned[key] = true
	}
	rows.Close()
	if len(provisioned) == 0 {
		t.Fatal("the cascade provisioned no pools; this run cannot show that provisioned pools are excluded")
	}

	series := gaugeSeriesFor(t, "ipam_pool_utilization_ratio")
	for _, poolKey := range series {
		if provisioned[poolKey] {
			t.Errorf("a cascade-provisioned pool was exported as a gauge series: %q", poolKey)
		}
	}
	// The operator-authored root must still be exported — the exclusion is
	// targeted, not a blanket removal, and the exhaustion alerts depend on it.
	var sawRoot bool
	for _, poolKey := range series {
		if poolKey == platformKey("ippools", "tenant-v6") {
			sawRoot = true
		}
	}
	if !sawRoot {
		t.Error("the operator-authored root pool must still export a gauge; the exhaustion alerts key on it")
	}

	// And the omission is counted rather than silent, so a missing series is
	// distinguishable from a broken metric.
	if provisionedSkips(t) <= before {
		t.Error("skipped provisioned-pool capacity updates were not counted")
	}
}

// gaugeSeriesFor returns the pool_key label of every series of a gauge.
func gaugeSeriesFor(t *testing.T, name string) []string {
	t.Helper()
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var out []string
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "pool_key" {
					out = append(out, label.GetValue())
				}
			}
		}
	}
	return out
}

func provisionedSkips(t *testing.T) float64 {
	t.Helper()
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() == "ipam_provisioned_pool_capacity_skipped_total" {
			for _, m := range family.GetMetric() {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestMain removes the package's throwaway Postgres container. Without it the
// container outlives the test binary, because it is shared by every test and so
// no single test can own its teardown.
func TestMain(m *testing.M) { testdb.TestMain(m) }
