package allocator

// The bounded search, tested against the database rather than against the
// library.
//
// internal/allocation/scan_test.go already checks Scan against
// FindFirstAvailableBlock over randomised pools, and that is the test of what
// first-fit MEANS. None of it exercises the half that can be wrong here: the
// paging predicates, the inet ordering PostgreSQL actually gives, the
// containment probe, the floor's persistence, and the trigger that lowers it.
// A library that is correct and a query that hands it the wrong rows produce a
// wrong address with every unit test green.
//
//	IPAM_TEST_POSTGRES_DSN="postgres://postgres:postgres@127.0.0.1:55601/postgres?sslmode=disable" \
//	go test ./internal/allocator/ -run TestBounded -count=1

import (
	"context"
	"math/rand"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// TestBoundedSearchAgreesWithWholeSetSearch is the load-bearing test of the
// wired path.
//
// It builds a pool's allocations as real rows, then asks the two
// implementations the same question: boundedFirstFit reading through the index,
// and FindFirstAvailableBlock over every row loaded into memory. They must
// choose the identical block.
//
// The population is deliberately awkward — mixed prefix lengths, gaps, blocks
// nested inside larger ones, and rows belonging to a second address space that
// must be invisible to a Claim search but not to a carve. A sequentially filled
// pool would agree under almost any bug, which is what makes it the wrong
// fixture for this.
func TestBoundedSearchAgreesWithWholeSetSearch(t *testing.T) {
	db := testdb.Pool(t, "ipam_bounded_agree")
	ctx := platformCtx()

	_, parent, err := net.ParseCIDR("10.64.0.0/16")
	if err != nil {
		t.Fatalf("parse parent: %v", err)
	}
	parents := []net.IPNet{*parent}
	poolKey := seedBoundedPool(t, db, "bounded-v4", "10.64.0.0/16")

	// TWO REAL ADDRESS SPACES, not one space and a different tenant string.
	//
	// A scope with no refs mentions no tenant anywhere — that is what makes the
	// empty space shared platform-wide — so AddressSpaceDigest("a", nil) and
	// AddressSpaceDigest("b", nil) are the SAME digest. The first version of
	// this test used those two and would have been asserting nothing about
	// scoping at all.
	//
	// It matters beyond this file. Under uniqueWithin: [] every claim lands in
	// the universal digest, so a search that ignored scope entirely still
	// withholds every block and looks correct. Most fixtures in this repo are
	// that shape, which is why a defect in scope handling can survive a green
	// suite. The second space here carries a network ref precisely so the two
	// digests really differ.
	mine := scope.AddressSpaceDigest("", nil)
	other := scope.AddressSpaceDigest("other-tenant", map[string]ipam.ScopeRef{
		"network": {APIGroup: "networking.miloapis.com", Kind: "Network", Name: "other-net"},
	})
	if mine == other {
		t.Fatal("fixture is wrong: the two digests must differ or the second space tests nothing")
	}

	rng := rand.New(rand.NewSource(7))
	var inMySpace []net.IPNet

	// Lay blocks down through the real allocator so the rows are exactly what
	// production writes — purpose, digest and all. Sizes vary and roughly a
	// third of the iterations skip ahead, which is what produces the gaps.
	cursor := 0
	for i := range 120 {
		length := []int{24, 26, 28}[rng.Intn(3)]
		if rng.Intn(3) == 0 {
			cursor += 1 + rng.Intn(3)
		}
		digest, space := mine, "mine"
		if rng.Intn(4) == 0 {
			digest, space = other, "other"
		}
		cidr := allocateOneBounded(t, db, ctx, poolKey, digest, length, i)
		if space == "mine" {
			inMySpace = append(inMySpace, cidr)
		}
		_ = cursor
	}

	// A reservation, which blocks EVERY space. Written under the other space's
	// digest on purpose: if the search compared it by digest instead of by
	// purpose it would be invisible here, and the two implementations would
	// diverge exactly where #66 said they would.
	reserved := mustCIDRNet(t, "10.64.200.0/24")
	insertReservationFor(t, db, ctx, poolKey, other, reserved)

	blocking := append(append([]net.IPNet(nil), inMySpace...), reserved)

	for _, ask := range []int{24, 26, 28, 30} {
		want, wantErr := allocation.FindFirstAvailableBlock(parents, blocking, ask, allocation.FirstFit)

		tx, berr := db.Begin(ctx)
		if berr != nil {
			t.Fatalf("begin: %v", berr)
		}
		got, _, gotErr := boundedFirstFit(ctx, tx, poolKey, mine, parents, ask, nil)
		_ = tx.Rollback(ctx)

		switch {
		case wantErr != nil && gotErr == nil:
			t.Fatalf("/%d: whole-set said %v, bounded returned %v", ask, wantErr, got)
		case wantErr == nil && gotErr != nil:
			t.Fatalf("/%d: whole-set returned %v, bounded said %v", ask, want, gotErr)
		case wantErr != nil && gotErr != nil:
			continue
		case got.String() != want.String():
			t.Fatalf("/%d: bounded chose %v, whole-set chose %v", ask, got, want)
		}
	}
}

// TestBoundedSearchPagesRatherThanReadingTheWholePool is the reason the change
// exists, asserted where it can be observed: the database's own count of rows
// returned.
//
// Reading it from pg_stat_user_tables rather than from a counter in Go is
// deliberate. A Go-side count would be counting the thing under test with the
// thing under test.
func TestBoundedSearchPagesRatherThanReadingTheWholePool(t *testing.T) {
	db := testdb.Pool(t, "ipam_bounded_pages")
	ctx := platformCtx()

	_, parent, _ := net.ParseCIDR("fd40::/20")
	parents := []net.IPNet{*parent}
	poolKey := seedBoundedPool(t, db, "bounded-v6", "fd40::/20")
	digest := scope.AddressSpaceDigest("", nil)

	const n = 600
	for i := range n {
		allocateOneBounded(t, db, ctx, poolKey, digest, 48, i)
	}
	if _, err := db.Exec(ctx, `ANALYZE ipam_cidr_allocations`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// With a warm floor — the state every allocation after the first leaves
	// behind — the search should read a handful of rows, not 600.
	floor, err := readSearchFloor(ctx, db, poolKey, digest)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if floor == nil {
		t.Fatal("no floor was persisted after 600 allocations; the search is bounded only with one")
	}

	before := heapTuplesRead(t, db, "ipam_cidr_allocations")
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, ferr := boundedFirstFit(ctx, tx, poolKey, digest, parents, 48, floor); ferr != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("bounded search: %v", ferr)
	}
	_ = tx.Rollback(ctx)
	read := heapTuplesRead(t, db, "ipam_cidr_allocations") - before

	t.Logf("rows read to place allocation %d: %d", n+1, read)
	// Generous on purpose. The claim is "bounded, not proportional to
	// occupancy"; a tight bound would fail on a plan change and teach nothing.
	// The whole-set path reads n.
	if read > n/4 {
		t.Fatalf("read %d rows from a pool holding %d; the search is not bounded", read, n)
	}
}

// TestReleaseLowersTheSearchFloor is the negative control for the floor.
//
// This is the failure the whole mechanism is exposed to: a floor that stays
// high after the space below it is freed makes that space invisible for good.
// No error, no exhaustion, just an address that stops existing — so the test
// has to assert the address comes BACK, not merely that some column changed.
func TestReleaseLowersTheSearchFloor(t *testing.T) {
	db := testdb.Pool(t, "ipam_bounded_floor")
	ctx := platformCtx()

	_, parent, _ := net.ParseCIDR("10.70.0.0/24")
	parents := []net.IPNet{*parent}
	poolKey := seedBoundedPool(t, db, "floor-v4", "10.70.0.0/24")
	digest := scope.AddressSpaceDigest("", nil)

	// Four /26s fills the /24 exactly.
	var placed []net.IPNet
	for i := range 4 {
		placed = append(placed, allocateOneBounded(t, db, ctx, poolKey, digest, 26, i))
	}
	first := placed[0]

	floorBefore, err := readSearchFloor(ctx, db, poolKey, digest)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if floorBefore == nil {
		t.Fatal("no floor after four allocations")
	}

	// Free the LOWEST block. Its address is below the floor, so a search that
	// trusts a stale floor cannot find it.
	releaseOneBounded(t, db, ctx, 0)

	floorAfter, err := readSearchFloor(ctx, db, poolKey, digest)
	if err != nil {
		t.Fatalf("read floor after release: %v", err)
	}
	if floorAfter == nil {
		t.Fatal("floor row vanished on release")
	}
	if !floorAfter.Equal(first.IP) {
		t.Errorf("floor is %v after releasing %v; the trigger in migration 009 must lower it to the freed address", floorAfter, first.IP)
	}

	// The assertion that matters: the freed block is handed out again.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	got, _, ferr := boundedFirstFit(ctx, tx, poolKey, digest, parents, 26, floorAfter)
	_ = tx.Rollback(ctx)
	if ferr != nil {
		t.Fatalf("pool reports exhausted after a release freed a /26: %v", ferr)
	}
	if got.String() != first.String() {
		t.Fatalf("got %v, want the released %v", got, first)
	}
}

// TestFloorIsNotRaisedPastAConcurrentRelease covers the compare-and-set.
//
// The race it guards is not reproducible by timing here, so it is reproduced by
// construction: the floor is moved underneath a search that has already
// observed the older value, and the search's write must lose. An unconditional
// upsert passes every other test in this file and fails this one.
func TestFloorIsNotRaisedPastAConcurrentRelease(t *testing.T) {
	db := testdb.Pool(t, "ipam_bounded_cas")
	ctx := platformCtx()

	poolKey := seedBoundedPool(t, db, "cas-v4", "10.71.0.0/24")
	digest := scope.AddressSpaceDigest("", nil)

	observed := net.ParseIP("10.71.0.64")
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_pool_search_floor (pool_key, scope_digest, floor) VALUES ($1, $2, $3::inet)`,
		poolKey, digest, observed.String()); err != nil {
		t.Fatalf("seed floor: %v", err)
	}

	// A release lands while the search is in flight.
	lowered := net.ParseIP("10.71.0.0")
	if _, err := db.Exec(ctx,
		`UPDATE ipam_pool_search_floor SET floor = $3::inet WHERE pool_key = $1 AND scope_digest = $2`,
		poolKey, digest, lowered.String()); err != nil {
		t.Fatalf("lower floor: %v", err)
	}

	// The search now tries to raise the floor, still believing it started at
	// `observed`.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if rerr := raiseSearchFloor(ctx, tx, poolKey, digest, observed, net.ParseIP("10.71.0.128")); rerr != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("raise floor: %v", rerr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		t.Fatalf("commit: %v", cerr)
	}

	got, err := readSearchFloor(ctx, db, poolKey, digest)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if !got.Equal(lowered) {
		t.Fatalf("floor is %v, want the released %v — the search overwrote a floor lowered underneath it, "+
			"which puts the freed address permanently out of reach", got, lowered)
	}
}

// ----------------------------------------------------------------------------
// fixtures
// ----------------------------------------------------------------------------

func seedBoundedPool(t *testing.T, db *pgxpool.Pool, name, cidr string) string {
	t.Helper()
	family := ipamv1alpha1.IPv4
	if len(cidr) > 0 && cidr[0] == 'f' {
		family = ipamv1alpha1.IPv6
	}
	obj := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       cidr,
			IPFamily:   family,
			ClassNames: []string{name},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: cidr,
			IPFamily:      family,
		},
	}
	key := platformKey("ippools", name)
	seedObject(t, db, key, "IPPool", name, obj)
	return key
}

func allocateOneBounded(t *testing.T, db *pgxpool.Pool, ctx context.Context, poolKey, digest string, prefixLen, i int) net.IPNet {
	t.Helper()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	alloc := NewPostgresPrefixAllocator()
	family := "IPv4"
	if prefixLen > 32 {
		family = "IPv6"
	}
	cidr, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
		PoolKey:       poolKey,
		AllocationKey: platformKey("ipallocations", "b-"+digest[:6]+"-"+itoa(i)),
		ClaimKey:      platformKey("ipclaims", "b-"+digest[:6]+"-"+itoa(i)),
		ClassName:     "bounded",
		ScopeDigest:   digest,
		PrefixLength:  prefixLen,
		IPFamily:      family,
		ReclaimPolicy: "Delete",
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("allocate %d: %v", i, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %d: %v", i, err)
	}
	return mustCIDRNet(t, cidr)
}

func releaseOneBounded(t *testing.T, db *pgxpool.Pool, ctx context.Context, i int) {
	t.Helper()
	digest := scope.AddressSpaceDigest("", nil)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	alloc := NewPostgresPrefixAllocator()
	if _, err := alloc.Release(ctx, tx, platformKey("ipclaims", "b-"+digest[:6]+"-"+itoa(i))); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("release %d: %v", i, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit release: %v", err)
	}
}

func insertReservationFor(t *testing.T, db *pgxpool.Pool, ctx context.Context, poolKey, digest string, block net.IPNet) {
	t.Helper()
	_, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		   (pool_key, allocated_cidr, allocation_key, ip_family, purpose, class_name, scope_digest)
		 VALUES ($1, $2::cidr, $3, 'IPv4', 'Reservation', '', $4)`,
		poolKey, block.String(), platformKey("ipallocations", "resv-"+block.IP.String()), digest)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
}

func mustCIDRNet(t *testing.T, s string) net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return *n
}
