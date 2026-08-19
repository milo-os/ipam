package allocator

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// gatewayReservation withholds the first /96 of every subnet, which is the
// block a subnet's router address sits in.
var gatewayReservation = &ipamv1alpha1.ReservationSpec{Leading: 1, UnitPrefixLength: 96}

// subnetChain writes the class pair from the tenant IPv6 addressing plan: a
// class that provisions one /64 subnet per network, and the endpoint class that
// draws /96s out of it. It returns the provisioned subnet pool.
func subnetChain(t *testing.T, db *pgxpool.Pool, reservations *ipamv1alpha1.ReservationSpec) string {
	t.Helper()
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", "subnets", ipamv1alpha1.IPClassSpec{
		IPFamily:            ipamv1alpha1.IPv6,
		DefaultPrefixLength: 64,
		PoolPer:             []string{"network"},
		Reservations:        reservations,
	})
	definition(t, tx, "platform", "endpoints", ipamv1alpha1.IPClassSpec{
		IPFamily:            ipamv1alpha1.IPv6,
		ParentClassName:     "subnets",
		DefaultPrefixLength: 96,
	})
	offerPool(t, tx, "platform", "root", "fd20:f000::/48", "subnets", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	read := begin(t, db)
	leaf, err := LoadClass(ctxIn("platform"), read, "endpoints")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_ = read.Rollback(ctx)

	poolKey, err := ResolvePool(ctx, db, leaf, map[string]ipam.ScopeRef{
		"network": claimScopeRef("Network", "net-a"),
	})
	if err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}
	return poolKey
}

// claimBlock allocates one /96 out of the pool, as a claim of the endpoint
// class would.
func claimBlock(t *testing.T, db *pgxpool.Pool, poolKey, claim string) string {
	t.Helper()
	ctx := context.Background()

	tx := begin(t, db)
	cidr, err := NewPostgresPrefixAllocator().AllocatePrefix(ctx, tx, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 96, IPFamily: "IPv6",
		ClaimKey: claim, AllocationKey: claim + "/alloc",
		OwnerProject: "tenant", ClassName: "endpoints",
	})
	if err != nil {
		t.Fatalf("AllocatePrefix: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}
	return cidr
}

// withholdFirst adds a reservation to a pool already written, as one authored
// before anything applied reservations would carry.
func withholdFirst(t *testing.T, db *pgxpool.Pool, poolKey string, unitPrefixLength int) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_objects
		    SET data = convert_to(
		            jsonb_set(ipam_data_to_jsonb(data), '{spec,reservations}',
		                      jsonb_build_object('leading', 1,
		                                         'unitPrefixLength', $2::int))::text,
		            'UTF8')
		  WHERE key = $1`, poolKey, unitPrefixLength); err != nil {
		t.Fatalf("add reservations to pool %q: %v", poolKey, err)
	}
}

func countReservations(t *testing.T, db *pgxpool.Pool, poolKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND purpose = 'Reservation'`, poolKey).Scan(&n); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	return n
}

// The bug this fixes, from the consumer's side: a class withholds the first
// block of every subnet it provisions so the router address is never handed
// out, and the first endpoint to claim receives that block anyway.
func TestAClaimNeverReceivesTheBlockItsClassWithholds(t *testing.T) {
	db := testdb.Pool(t)

	poolKey := subnetChain(t, db, gatewayReservation)

	got := claimBlock(t, db, poolKey, "tenant/ipclaims/endpoint-1")
	if got == "fd20:f000::/96" {
		t.Fatalf("claim received %s, the block holding the subnet's gateway; "+
			"the class withholds the first /96 of every subnet it provisions", got)
	}
	if got != "fd20:f000::1:0:0/96" {
		t.Errorf("claim received %s, want the first /96 after the withheld one", got)
	}
}

// A class with no reservations is unaffected: the first claim takes the first
// block. This is what makes the test above a statement about reservations
// rather than about the search.
func TestWithoutAReservationTheFirstBlockIsHandedOut(t *testing.T) {
	db := testdb.Pool(t)

	poolKey := subnetChain(t, db, nil)

	if got := claimBlock(t, db, poolKey, "tenant/ipclaims/endpoint-1"); got != "fd20:f000::/96" {
		t.Errorf("claim received %s, want the first /96", got)
	}
	if n := countReservations(t, db, poolKey); n != 0 {
		t.Errorf("pool holds %d reservation(s), want none", n)
	}
}

// The pool a class provisions carries the class's reservations, so the pool is
// readable as the thing withholding the space.
func TestAProvisionedPoolCarriesItsClassReservations(t *testing.T) {
	db := testdb.Pool(t)

	poolKey := subnetChain(t, db, gatewayReservation)

	var leading, unit int
	if err := db.QueryRow(context.Background(),
		`SELECT (ipam_data_to_jsonb(data) -> 'spec' -> 'reservations' ->> 'leading')::int,
		        (ipam_data_to_jsonb(data) -> 'spec' -> 'reservations' ->> 'unitPrefixLength')::int
		   FROM ipam_objects WHERE key = $1`, poolKey).Scan(&leading, &unit); err != nil {
		t.Fatalf("read provisioned pool reservations: %v", err)
	}
	if leading != 1 || unit != 96 {
		t.Errorf("pool withholds leading=%d unit=/%d, want leading=1 unit=/96", leading, unit)
	}
}

// A reserved position is a row the pool holds, not a hole the allocator
// remembers. Everything that reads occupancy sees it because it is a row.
func TestAReservedPositionIsHeldAsAnAllocation(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	poolKey := subnetChain(t, db, gatewayReservation)

	var cidr, purpose, claimKey string
	if err := db.QueryRow(ctx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr),
		        purpose, COALESCE(claim_key, '')
		   FROM ipam_cidr_allocations WHERE pool_key = $1`, poolKey).
		Scan(&cidr, &purpose, &claimKey); err != nil {
		t.Fatalf("read reservation row: %v", err)
	}
	if cidr != "fd20:f000::/96" {
		t.Errorf("withheld %s, want the first /96 of the subnet", cidr)
	}
	if purpose != string(ipamv1alpha1.PurposeReservation) {
		t.Errorf("purpose = %q, want Reservation", purpose)
	}
	if claimKey != "" {
		t.Errorf("reservation is bound to claim %q; it belongs to no claim", claimKey)
	}

	// Reserved space is inventory: it counts as allocated, so a reader asking
	// what is left does not count a block nobody may have.
	var allocated string
	if err := db.QueryRow(ctx,
		`SELECT ipam_data_to_jsonb(data) -> 'status' -> 'capacity' ->> 'allocated'
		   FROM ipam_objects WHERE key = $1`, poolKey).Scan(&allocated); err != nil {
		t.Fatalf("read pool capacity: %v", err)
	}
	if allocated != "4294967296" {
		t.Errorf("capacity.allocated = %s, want the 2^32 addresses of the withheld /96", allocated)
	}
}

// Withholding happens once. A pool that already holds its positions is left
// alone, whatever provoked the check.
func TestReservingIsIdempotent(t *testing.T) {
	db := testdb.Pool(t)

	poolKey := subnetChain(t, db, gatewayReservation)

	claimBlock(t, db, poolKey, "tenant/ipclaims/endpoint-1")
	claimBlock(t, db, poolKey, "tenant/ipclaims/endpoint-2")

	if n := countReservations(t, db, poolKey); n != 1 {
		t.Errorf("pool holds %d reservations, want 1", n)
	}
}

// A pool written before anything applied reservations carries the declaration
// and no rows. Its next allocation materialises them, so the fix reaches pools
// that already exist rather than only newly provisioned ones.
func TestAnExistingPoolReservesOnItsNextAllocation(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	poolKey := offerPool(t, tx, "platform", "legacy", "fd30::/64", "endpoints", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}
	withholdFirst(t, db, poolKey, 96)

	if n := countReservations(t, db, poolKey); n != 0 {
		t.Fatalf("pool already holds %d reservation(s); the fixture must start with none", n)
	}

	if got := claimBlock(t, db, poolKey, "tenant/ipclaims/endpoint-1"); got != "fd30::1:0:0/96" {
		t.Errorf("claim received %s, want the first /96 after the withheld one", got)
	}
	if n := countReservations(t, db, poolKey); n != 1 {
		t.Errorf("pool holds %d reservations after allocating, want 1", n)
	}
}

// A reservation the pool cannot express is an operator error. It must not read
// as a pool withholding nothing, and it must not read as a full pool.
func TestAnImpossibleReservationFailsLoudly(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	poolKey := offerPool(t, tx, "platform", "impossible", "fd50::/64", "endpoints", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}
	// A /64 cannot withhold a position wider than itself.
	withholdFirst(t, db, poolKey, 48)

	claimTx := begin(t, db)
	_, err := NewPostgresPrefixAllocator().AllocatePrefix(ctx, claimTx, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 96, IPFamily: "IPv6",
		ClaimKey: "tenant/ipclaims/endpoint-1", OwnerProject: "tenant", ClassName: "endpoints",
	})
	if err == nil {
		t.Fatal("AllocatePrefix succeeded on a pool whose reservation cannot be expressed")
	}
	if errors.Is(err, ErrPoolExhausted) {
		t.Error("AllocatePrefix reported exhaustion; a misconfiguration must not look like a full pool")
	}
	if !errors.Is(err, allocation.ErrInvalidReservation) {
		t.Errorf("AllocatePrefix error = %v, want it to name the invalid reservation", err)
	}
}

// A carve for a child pool must avoid the parent's withheld space too, or the
// child is handed the very block the parent withheld.
func TestACarveAvoidsTheParentsReservedSpace(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", "backbone", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, DefaultPrefixLength: 56, PoolPer: []string{"location"},
	})
	definition(t, tx, "platform", "leaves", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, ParentClassName: "backbone", DefaultPrefixLength: 64,
	})
	rootKey := offerPool(t, tx, "platform", "root", "fd40::/48", "backbone", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}
	withholdFirst(t, db, rootKey, 56)

	read := begin(t, db)
	leaf, err := LoadClass(ctxIn("platform"), read, "leaves")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_ = read.Rollback(ctx)

	poolKey, err := ResolvePool(ctx, db, leaf, map[string]ipam.ScopeRef{
		"location": claimScopeRef("Location", "lon1"),
	})
	if err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}

	var cidr string
	if err := db.QueryRow(ctx,
		`SELECT ipam_data_to_jsonb(data) -> 'status' ->> 'allocatedCIDR'
		   FROM ipam_objects WHERE key = $1`, poolKey).Scan(&cidr); err != nil {
		t.Fatalf("read provisioned pool: %v", err)
	}
	if cidr == "fd40::/56" {
		t.Fatalf("child pool carved %s, the block the root withholds", cidr)
	}
	if cidr != "fd40:0:0:100::/56" {
		t.Errorf("child pool carved %s, want the first /56 after the withheld one", cidr)
	}
}
