package allocator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// Tests for PlaceScopeRange: a ScopeRange claim naming its range places the
// class's chain there. The fixture is the network-services-operator shape:
//
//	vpc       /48 per network and project, first /64 withheld, from fd20::/32
//	subnet    /64 per network, location and project, carved from vpc
//	endpoint  /96 Block claims from subnet
//	pubvpc    /48 per network, SHARED, from fd30::/32
//	pubsub    /64 per network, location and project, carved from pubvpc
const placementProject = "projx"

func placementChain(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	tx := begin(t, db)
	definition(t, tx, placementProject, "vpc", ipamv1alpha1.IPClassSpec{
		IPFamily: v6, DefaultPrefixLength: 48, PoolPer: []string{"network", scope.ReservedRoleProject},
		Reservations: &ipamv1alpha1.ReservationSpec{Leading: 1, UnitPrefixLength: 64},
	})
	definition(t, tx, placementProject, "subnet", ipamv1alpha1.IPClassSpec{
		IPFamily: v6, ParentClassName: "vpc", DefaultPrefixLength: 64,
		PoolPer: []string{"network", "location", scope.ReservedRoleProject},
	})
	definition(t, tx, placementProject, "endpoint", ipamv1alpha1.IPClassSpec{
		IPFamily: v6, ParentClassName: "subnet", DefaultPrefixLength: 96,
	})
	definition(t, tx, placementProject, "pubvpc", ipamv1alpha1.IPClassSpec{
		IPFamily: v6, DefaultPrefixLength: 48, PoolPer: []string{"network"},
	})
	definition(t, tx, placementProject, "pubsub", ipamv1alpha1.IPClassSpec{
		IPFamily: v6, ParentClassName: "pubvpc", DefaultPrefixLength: 64,
		PoolPer: []string{"network", "location", scope.ReservedRoleProject},
	})
	offerPool(t, tx, placementProject, "root", "fd20::/32", "vpc", nil)
	offerPool(t, tx, placementProject, "pubroot", "fd30::/32", "pubvpc", nil)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}
}

func placementScope(network, location string) map[string]ipam.ScopeRef {
	s := map[string]ipam.ScopeRef{"network": claimScopeRef("Network", network)}
	if location != "" {
		s["location"] = claimScopeRef("Location", location)
	}
	return s
}

func placementClass(t *testing.T, db *pgxpool.Pool, name string) *ResolvedClass {
	t.Helper()
	tx := begin(t, db)
	defer func() { _ = tx.Rollback(context.Background()) }()
	class, err := LoadClass(ctxIn(placementProject), tx, name)
	if err != nil {
		t.Fatalf("LoadClass %q: %v", name, err)
	}
	return class
}

// place runs PlaceScopeRange in a transaction of its own, committing only on
// success, the way the registry does.
func place(t *testing.T, db *pgxpool.Pool, class, network, location, cidr string) (string, error) {
	t.Helper()
	ctx := ctxIn(placementProject)
	leaf := placementClass(t, db, class)
	_, want, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("parse %q: %v", cidr, err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	key, err := PlaceScopeRange(ctx, tx, leaf, placementScope(network, location), *want, "claim/"+class+"/"+network+"/"+location)
	if err != nil {
		_ = tx.Rollback(ctx)
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return key, nil
}

func poolRangeOf(t *testing.T, db *pgxpool.Pool, poolKey string) string {
	t.Helper()
	var cidr string
	if err := db.QueryRow(context.Background(),
		`SELECT ipam_data_to_jsonb(data) -> 'status' ->> 'allocatedCIDR' FROM ipam_objects WHERE key = $1`,
		poolKey).Scan(&cidr); err != nil {
		t.Fatalf("read pool %q: %v", poolKey, err)
	}
	return cidr
}

// placementState is everything a placement writes: provisioned pools, carves
// and identity rows. A refusal or a dry-run must leave all three as they were.
type placementState struct{ pools, carves, identities int }

func stateOf(t *testing.T, db *pgxpool.Pool) placementState {
	t.Helper()
	var s placementState
	ctx := context.Background()
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_objects WHERE kind = 'IPPool'
		    AND ipam_data_to_jsonb(data) -> 'metadata' -> 'labels' ? 'ipam.miloapis.com/provisioned-by'`,
	).Scan(&s.pools); err != nil {
		t.Fatalf("count pools: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ipam_cidr_allocations WHERE purpose = 'PoolCarve'`).Scan(&s.carves); err != nil {
		t.Fatalf("count carves: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ipam_pool_identity`).Scan(&s.identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	return s
}

func expectUnchanged(t *testing.T, db *pgxpool.Pool, before placementState, what string) {
	t.Helper()
	if after := stateOf(t, db); after != before {
		t.Errorf("%s changed what is placed: %+v -> %+v", what, before, after)
	}
}

func levelKey(t *testing.T, db *pgxpool.Pool, class string, s map[string]ipam.ScopeRef) string {
	t.Helper()
	ctx := ctxIn(placementProject)
	tx := begin(t, db)
	defer func() { _ = tx.Rollback(context.Background()) }()
	levels, err := PlanScopeRangeCascade(ctx, tx, placementClass(t, db, class), s)
	if err != nil {
		t.Fatalf("plan %q: %v", class, err)
	}
	key, err := lookupPoolIdentity(ctx, tx, class, levels[len(levels)-1].ScopeDigest)
	if err != nil {
		t.Fatalf("look up %q: %v", class, err)
	}
	return key
}

// The fix: every missing level is carved at the block containing the range,
// and a Block claim naming an address inside then binds through the ordinary
// address path.
func TestARangeClaimPlacesEveryLevelAtTheRangeItNames(t *testing.T) {
	db := testdb.Pool(t)
	placementChain(t, db)

	subnetKey, err := place(t, db, "subnet", "net-a", "loc-1", "fd20:0:a:10::/64")
	if err != nil {
		t.Fatalf("place subnet: %v", err)
	}
	if got := poolRangeOf(t, db, subnetKey); got != "fd20:0:a:10::/64" {
		t.Errorf("subnet pool placed at %s, want fd20:0:a:10::/64", got)
	}
	vpcKey := levelKey(t, db, "vpc", placementScope("net-a", ""))
	if got := poolRangeOf(t, db, vpcKey); got != "fd20:0:a::/48" {
		t.Errorf("vpc pool placed at %s, want fd20:0:a::/48", got)
	}

	// A Block claim of the chain resolves to the placed subnet and binds the
	// address it names.
	endpoint := placementClass(t, db, "endpoint")
	_, want, _ := net.ParseCIDR("fd20:0:a:10:0:5::/96")
	poolKey, err := ResolvePoolFor(ctxIn(placementProject), db, endpoint, placementScope("net-a", "loc-1"), want)
	if err != nil {
		t.Fatalf("ResolvePoolFor endpoint: %v", err)
	}
	if poolKey != subnetKey {
		t.Fatalf("endpoint resolved to %q, want the placed subnet %q", poolKey, subnetKey)
	}
	got, err := claimAddress(t, db, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 96, IPFamily: "IPv6", ClassName: "endpoint",
		ClaimKey: "endpoint-5", Address: "fd20:0:a:10:0:5::",
	})
	if err != nil || got != "fd20:0:a:10:0:5::/96" {
		t.Fatalf("addressed endpoint = %s, %v; want fd20:0:a:10:0:5::/96", got, err)
	}

	// And one without an address still allocates inside it.
	auto, err := claimAddress(t, db, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 96, IPFamily: "IPv6", ClassName: "endpoint", ClaimKey: "endpoint-auto",
	})
	if err != nil {
		t.Fatalf("automatic endpoint: %v", err)
	}
	_, autoNet, _ := net.ParseCIDR(auto)
	_, subnet, _ := net.ParseCIDR("fd20:0:a:10::/64")
	if !subnet.Contains(autoNet.IP) || auto == "fd20:0:a:10:0:5::/96" {
		t.Errorf("automatic endpoint got %s, want a free /96 of fd20:0:a:10::/64", auto)
	}
}

func TestAnExistingLevelContainingTheRangeIsReused(t *testing.T) {
	db := testdb.Pool(t)
	placementChain(t, db)

	vpcKey, err := place(t, db, "vpc", "net-a", "", "fd20:0:a::/48")
	if err != nil {
		t.Fatalf("place vpc: %v", err)
	}
	before := stateOf(t, db)
	subnetKey, err := place(t, db, "subnet", "net-a", "loc-2", "fd20:0:a:20::/64")
	if err != nil {
		t.Fatalf("place subnet under the existing vpc: %v", err)
	}
	if got := levelKey(t, db, "vpc", placementScope("net-a", "")); got != vpcKey {
		t.Errorf("vpc level resolved to %q after the subnet, want the existing %q", got, vpcKey)
	}
	after := stateOf(t, db)
	if after.pools != before.pools+1 || after.carves != before.carves+1 {
		t.Errorf("placing one subnet under an existing vpc went %+v -> %+v, want one pool and one carve more", before, after)
	}
	if got := poolRangeOf(t, db, subnetKey); got != "fd20:0:a:20::/64" {
		t.Errorf("subnet pool placed at %s, want fd20:0:a:20::/64", got)
	}

	// Placing the class's own pool where it already is, is a reuse too.
	again, err := place(t, db, "subnet", "net-a", "loc-2", "fd20:0:a:20::/64")
	if err != nil || again != subnetKey {
		t.Errorf("placing the subnet where it is = %q, %v; want %q", again, err, subnetKey)
	}
}

// A level placed by first-free before anyone named a range is where it is.
func TestAnExistingLevelElsewhereIsRefused(t *testing.T) {
	db := testdb.Pool(t)
	placementChain(t, db)

	if _, err := ResolvePool(ctxIn(placementProject), db, placementClass(t, db, "endpoint"), placementScope("net-a", "loc-1")); err != nil {
		t.Fatalf("first-free chain: %v", err)
	}
	vpc := poolRangeOf(t, db, levelKey(t, db, "vpc", placementScope("net-a", "")))
	if vpc != "fd20::/48" {
		t.Fatalf("first-free vpc = %s, want fd20::/48", vpc)
	}

	before := stateOf(t, db)
	_, err := place(t, db, "subnet", "net-a", "loc-2", "fd20:0:a:10::/64")
	if !errors.Is(err, ErrAddressOutsidePool) {
		t.Fatalf("subnet outside the placed vpc: %v, want ErrAddressOutsidePool", err)
	}
	if !strings.Contains(err.Error(), "fd20::/48") {
		t.Errorf("refusal %q does not name the vpc's range fd20::/48", err)
	}
	expectUnchanged(t, db, before, "a subnet outside its vpc")
}

// The class's own pool must be exactly the range, not merely contain it.
func TestTheLeafPoolMustBeExactlyTheRange(t *testing.T) {
	db := testdb.Pool(t)
	placementChain(t, db)

	if _, err := ResolvePool(ctxIn(placementProject), db, placementClass(t, db, "endpoint"), placementScope("net-a", "loc-1")); err != nil {
		t.Fatalf("first-free chain: %v", err)
	}
	subnetKey := levelKey(t, db, "subnet", placementScope("net-a", "loc-1"))
	// The vpc withholds its first /64, so first-free put the subnet second.
	placed := poolRangeOf(t, db, subnetKey)
	if placed != "fd20:0:0:1::/64" {
		t.Fatalf("first-free subnet = %s, want fd20:0:0:1::/64", placed)
	}

	before := stateOf(t, db)
	_, err := place(t, db, "subnet", "net-a", "loc-1", "fd20:0:0:5::/64")
	if !errors.Is(err, ErrAddressOutsidePool) {
		t.Fatalf("subnet elsewhere in its own vpc: %v, want ErrAddressOutsidePool", err)
	}
	if !strings.Contains(err.Error(), placed) {
		t.Errorf("refusal %q does not name the subnet's range %s", err, placed)
	}
	expectUnchanged(t, db, before, "a subnet named away from where it is")

	if got, err := place(t, db, "subnet", "net-a", "loc-1", placed); err != nil || got != subnetKey {
		t.Errorf("naming the subnet where it is = %q, %v; want %q", got, err, subnetKey)
	}
}

func TestAPlacementThatCollidesIsRefusedAndLeavesNothing(t *testing.T) {
	db := testdb.Pool(t)
	placementChain(t, db)

	if _, err := place(t, db, "vpc", "net-a", "", "fd20:0:a::/48"); err != nil {
		t.Fatalf("place vpc: %v", err)
	}
	// Something holds fd20:0:c::/48 in the root as a claim rather than a carve.
	rootKey := offerPoolKey(placementProject, "root")
	if _, err := claimAddress(t, db, PrefixRequest{
		PoolKey: rootKey, PrefixLen: 48, IPFamily: "IPv6", ClassName: "vpc", ClaimKey: "squatter", Address: "fd20:0:c::/48",
	}); err != nil {
		t.Fatalf("seed a claim in the root: %v", err)
	}

	before := stateOf(t, db)
	tests := []struct {
		name, class, network, location, cidr string
		wantErr                              error
		carved                               bool
	}{
		{name: "vpc carved to another network", class: "vpc", network: "net-b", cidr: "fd20:0:a::/48", wantErr: ErrAddressInUse, carved: true},
		{name: "subnet of another network's vpc", class: "subnet", network: "net-c", location: "loc-1", cidr: "fd20:0:a:30::/64", wantErr: ErrAddressInUse, carved: true},
		{name: "vpc held by a claim", class: "vpc", network: "net-d", cidr: "fd20:0:c::/48", wantErr: ErrAddressInUse},
		{name: "subnet the vpc withholds", class: "subnet", network: "net-a", location: "loc-r", cidr: "fd20:0:a::/64", wantErr: ErrAddressReserved},
		{name: "vpc outside every pool offering it", class: "vpc", network: "net-e", cidr: "fd21:0:1::/48", wantErr: ErrAddressOutsidePool},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := place(t, db, tc.class, tc.network, tc.location, tc.cidr)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("place %s at %s: %v, want %v", tc.class, tc.cidr, err, tc.wantErr)
			}
			var inUse *AddressInUseError
			if errors.As(err, &inUse) && inUse.Carved != tc.carved {
				t.Errorf("AddressInUse Carved = %v, want %v (%v)", inUse.Carved, tc.carved, err)
			}
			expectUnchanged(t, db, before, tc.name)
		})
	}
}

// A shared level is never placed by one consumer, but one that exists and
// holds the range is used.
func TestASharedLevelIsPlacedOnlyWhenItExists(t *testing.T) {
	db := testdb.Pool(t)
	placementChain(t, db)

	before := stateOf(t, db)
	if _, err := place(t, db, "pubvpc", "net-a", "", "fd30:0:1::/48"); !errors.Is(err, ErrSharedPoolUnplaced) {
		t.Fatalf("placing a missing shared pool: %v, want ErrSharedPoolUnplaced", err)
	}
	if _, err := place(t, db, "pubsub", "net-a", "loc-1", "fd30:0:1:7::/64"); !errors.Is(err, ErrSharedPoolUnplaced) {
		t.Fatalf("placing under a missing shared pool: %v, want ErrSharedPoolUnplaced", err)
	}
	expectUnchanged(t, db, before, "a refused shared placement")

	// The allocator places the shared pool, first-free.
	if _, err := ResolveScopeRange(ctxIn(placementProject), db, placementClass(t, db, "pubvpc"), placementScope("net-a", "")); err != nil {
		t.Fatalf("first-free shared pool: %v", err)
	}
	if got := poolRangeOf(t, db, levelKey(t, db, "pubvpc", placementScope("net-a", ""))); got != "fd30::/48" {
		t.Fatalf("shared pool = %s, want fd30::/48", got)
	}
	key, err := place(t, db, "pubsub", "net-a", "loc-1", "fd30:0:0:7::/64")
	if err != nil {
		t.Fatalf("placing inside an existing shared pool: %v", err)
	}
	if got := poolRangeOf(t, db, key); got != "fd30:0:0:7::/64" {
		t.Errorf("per-project subnet under a shared pool = %s, want fd30:0:0:7::/64", got)
	}
	if _, err := place(t, db, "pubsub", "net-a", "loc-2", "fd30:0:9:7::/64"); !errors.Is(err, ErrAddressOutsidePool) {
		t.Errorf("placing outside the existing shared pool: %v, want ErrAddressOutsidePool", err)
	}
}

// The identity row is the serialisation point. The loser reads the winner's
// pool and is refused against it: never a second pool, never a stray carve.
func TestRacingPlacementsForOneScopePlaceOne(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(16))
	placementChain(t, db)

	const rounds = 8
	for r := 0; r < rounds; r++ {
		network := fmt.Sprintf("race-%d", r)
		ranges := []string{fmt.Sprintf("fd20:0:%x::/48", 0x100+2*r), fmt.Sprintf("fd20:0:%x::/48", 0x101+2*r)}
		errs := make([]error, len(ranges))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range ranges {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = place(t, db, "vpc", network, "", ranges[i])
			}(i)
		}
		close(start)
		wg.Wait()

		won := -1
		for i, err := range errs {
			switch {
			case err == nil:
				if won >= 0 {
					t.Fatalf("round %d: both %s and %s were placed", r, ranges[won], ranges[i])
				}
				won = i
			case !errors.Is(err, ErrAddressOutsidePool):
				t.Fatalf("round %d: loser for %s got %v, want ErrAddressOutsidePool", r, ranges[i], err)
			}
		}
		if won < 0 {
			t.Fatalf("round %d: neither placement succeeded: %v", r, errs)
		}
		if got := poolRangeOf(t, db, levelKey(t, db, "vpc", placementScope(network, ""))); got != ranges[won] {
			t.Errorf("round %d: pool at %s, want the winner's %s", r, got, ranges[won])
		}
	}
	if s := stateOf(t, db); s.pools != rounds || s.carves != rounds || s.identities != rounds {
		t.Errorf("after %d races: %+v, want %d pools, carves and identities", rounds, s, rounds)
	}
}

// Dry-run runs in one transaction it rolls back, so nothing is placed: with
// an address, and without one, where each level runs in a savepoint.
func TestADryRunPlacesNothing(t *testing.T) {
	db := testdb.Pool(t)
	placementChain(t, db)
	ctx := ctxIn(placementProject)
	before := stateOf(t, db)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, want, _ := net.ParseCIDR("fd20:0:f:10::/64")
	key, err := PlaceScopeRange(ctx, tx, placementClass(t, db, "subnet"), placementScope("net-d", "loc-1"), *want, "claim/dry")
	if err != nil {
		t.Fatalf("dry-run placement: %v", err)
	}
	cidr, err := BindScopeRange(ctx, tx, key, "claim/dry")
	if err != nil || cidr != "fd20:0:f:10::/64" {
		t.Fatalf("dry-run bind = %s, %v; want fd20:0:f:10::/64", cidr, err)
	}
	_ = tx.Rollback(ctx)
	expectUnchanged(t, db, before, "an addressed dry-run")

	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := ResolveScopeRange(ctx, tx, placementClass(t, db, "subnet"), placementScope("net-e", "loc-1")); err != nil {
		t.Fatalf("dry-run first-free range: %v", err)
	}
	if _, err := ResolvePool(ctx, tx, placementClass(t, db, "endpoint"), placementScope("net-f", "loc-1")); err != nil {
		t.Fatalf("dry-run first-free block chain: %v", err)
	}
	_ = tx.Rollback(ctx)
	expectUnchanged(t, db, before, "an unaddressed dry-run")
}

func offerPoolKey(project, name string) string {
	return tenant.Identity{Name: project}.ResourceKey("ippools", name)
}
