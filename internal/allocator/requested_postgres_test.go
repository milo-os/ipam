package allocator

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// allocationKeyIn is an IPAllocation storage key as the registry writes it.
func allocationKeyIn(project, namespace, name string) string {
	return tenant.Identity{Name: project}.ApplyPrefix("/ipam.miloapis.com/ipallocations/" + namespace + "/" + name)
}

// claimAddress asks for a named block and returns what AllocatePrefix
// answered, committing on success.
func claimAddress(t *testing.T, db *pgxpool.Pool, req PrefixRequest) (string, error) {
	t.Helper()
	ctx := context.Background()
	if req.IPFamily == "" {
		req.IPFamily = "IPv4"
	}
	if req.ClassName == "" {
		req.ClassName = "standard"
	}
	if req.OwnerProject == "" {
		req.OwnerProject = testProject
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cidr, err := NewPostgresPrefixAllocator().AllocatePrefix(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return cidr, nil
}

func countAllocations(t *testing.T, db *pgxpool.Pool, poolKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ipam_cidr_allocations WHERE pool_key = $1 AND purpose = 'Claim'`,
		poolKey).Scan(&n); err != nil {
		t.Fatalf("count allocations: %v", err)
	}
	return n
}

// The bug in #183: the named address was accepted and then ignored, and the
// claim was bound to the next free block instead.
func TestARequestedAddressIsBoundExactly(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "requested-exact", "10.90.0.0/24")

	got, err := claimAddress(t, db, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 32, ClaimKey: "c-bare", Address: "10.90.0.10",
	})
	if err != nil {
		t.Fatalf("bare address: %v", err)
	}
	if got != "10.90.0.10/32" {
		t.Errorf("bare address bound %s, want 10.90.0.10/32", got)
	}

	got, err = claimAddress(t, db, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 28, ClaimKey: "c-cidr", Address: "10.90.0.64/28",
	})
	if err != nil {
		t.Fatalf("CIDR address: %v", err)
	}
	if got != "10.90.0.64/28" {
		t.Errorf("CIDR address bound %s, want 10.90.0.64/28", got)
	}

	// An automatic claim searches around what was adopted.
	auto, err := claimAddress(t, db, PrefixRequest{PoolKey: poolKey, PrefixLen: 28, ClaimKey: "c-auto"})
	if err != nil {
		t.Fatalf("automatic claim: %v", err)
	}
	_, autoNet, _ := net.ParseCIDR(auto)
	for _, held := range []string{"10.90.0.10/32", "10.90.0.64/28"} {
		_, h, _ := net.ParseCIDR(held)
		if allocation.CIDRsOverlap(*autoNet, *h) {
			t.Errorf("automatic claim got %s, which overlaps adopted %s", auto, held)
		}
	}
}

// The address the FirstFit search would hand out next, once adopted, is skipped
// by the search rather than handed out twice.
func TestTheSearchSkipsAnAdoptedNextAddress(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "requested-next", "10.91.0.0/29")

	first, err := claimAddress(t, db, PrefixRequest{PoolKey: poolKey, PrefixLen: 32, ClaimKey: "c1"})
	if err != nil || first != "10.91.0.0/32" {
		t.Fatalf("first automatic claim = %s, %v; want 10.91.0.0/32", first, err)
	}
	if _, err := claimAddress(t, db, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 32, ClaimKey: "adopt", Address: "10.91.0.1",
	}); err != nil {
		t.Fatalf("adopt the next address: %v", err)
	}
	next, err := claimAddress(t, db, PrefixRequest{PoolKey: poolKey, PrefixLen: 32, ClaimKey: "c2"})
	if err != nil {
		t.Fatalf("second automatic claim: %v", err)
	}
	if next == "10.91.0.1/32" {
		t.Fatalf("the search handed out the adopted address %s again", next)
	}
	if next != "10.91.0.2/32" {
		t.Errorf("second automatic claim = %s, want 10.91.0.2/32", next)
	}
}

func TestARequestedAddressIsRefusedNotMoved(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "requested-refused", "10.92.0.0/24")

	holderKey := allocationKeyIn(testProject, "dns", "alloc-holder")
	if _, err := claimAddress(t, db, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 32, ClaimKey: "holder", AllocationKey: holderKey, Address: "10.92.0.7",
	}); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	withhold(t, db, poolKey, "carve-key", "10.92.0.128/25", "PoolCarve")

	tests := []struct {
		name    string
		req     PrefixRequest
		wantErr error
		check   func(t *testing.T, err error)
	}{
		{
			name:    "unaligned is never rounded",
			req:     PrefixRequest{PrefixLen: 28, Address: "10.92.0.5"},
			wantErr: allocation.ErrUnalignedAddress,
		},
		{
			name:    "outside the pool",
			req:     PrefixRequest{PrefixLen: 32, Address: "10.93.0.1"},
			wantErr: ErrAddressOutsidePool,
		},
		{
			name:    "straddling the pool's edge",
			req:     PrefixRequest{PrefixLen: 23, Address: "10.92.0.0"},
			wantErr: ErrAddressOutsidePool,
		},
		{
			name:    "held by a claim in this project",
			req:     PrefixRequest{PrefixLen: 32, Address: "10.92.0.7", AllocationKey: allocationKeyIn(testProject, "dns", "alloc-new")},
			wantErr: ErrAddressInUse,
			check: func(t *testing.T, err error) {
				var inUse *AddressInUseError
				if !errors.As(err, &inUse) {
					t.Fatalf("error %v is not an AddressInUseError", err)
				}
				if inUse.AllocationNamespace != "dns" || inUse.AllocationName != "alloc-holder" || inUse.Retained || inUse.Carved {
					t.Errorf("holder = %+v, want the live allocation dns/alloc-holder", inUse)
				}
			},
		},
		{
			name:    "a block containing a held address",
			req:     PrefixRequest{PrefixLen: 29, Address: "10.92.0.0/29", AllocationKey: allocationKeyIn(testProject, "dns", "alloc-new")},
			wantErr: ErrAddressInUse,
		},
		{
			name:    "held by a claim in another project names no holder",
			req:     PrefixRequest{PrefixLen: 32, Address: "10.92.0.7", AllocationKey: allocationKeyIn("other", "dns", "alloc-new"), OwnerProject: "other"},
			wantErr: ErrAddressInUse,
			check: func(t *testing.T, err error) {
				var inUse *AddressInUseError
				if !errors.As(err, &inUse) || inUse.AllocationName != "" || inUse.AllocationNamespace != "" {
					t.Errorf("holder = %+v, want another project's holder left unnamed", inUse)
				}
			},
		},
		{
			name:    "carved to a child pool",
			req:     PrefixRequest{PrefixLen: 32, Address: "10.92.0.200"},
			wantErr: ErrAddressInUse,
			check: func(t *testing.T, err error) {
				var inUse *AddressInUseError
				if !errors.As(err, &inUse) || !inUse.Carved {
					t.Errorf("holder = %+v, want a carve", inUse)
				}
				if !strings.Contains(err.Error(), "child pool") {
					t.Errorf("message %q does not say the address is carved to a child pool", err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := countAllocations(t, db, poolKey)
			tc.req.PoolKey = poolKey
			tc.req.ClaimKey = "c-" + strings.ReplaceAll(tc.name, " ", "-")
			got, err := claimAddress(t, db, tc.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("AllocatePrefix = %q, %v; want %v", got, err, tc.wantErr)
			}
			if tc.check != nil {
				tc.check(t, err)
			}
			if after := countAllocations(t, db, poolKey); after != before {
				t.Errorf("a refused claim left %d allocation rows behind", after-before)
			}
		})
	}
}

// A retained address is held by nobody's claim, and the refusal has to say so:
// deleting the IPAllocation is the only thing that frees it.
func TestARetainedAddressIsReportedAsRetained(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "requested-retained", "10.94.0.0/24")

	holderKey := allocationKeyIn(testProject, "dns", "alloc-kept")
	if _, err := claimAddress(t, db, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 32, ClaimKey: "kept", AllocationKey: holderKey,
		Address: "10.94.0.9", ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
	}); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	releaseReturningRetained(t, db, "kept")

	_, err := claimAddress(t, db, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 32, ClaimKey: "again", Address: "10.94.0.9",
		AllocationKey: allocationKeyIn(testProject, "dns", "alloc-again"),
	})
	var inUse *AddressInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("AllocatePrefix = %v, want AddressInUseError", err)
	}
	if !inUse.Retained || inUse.AllocationName != "alloc-kept" {
		t.Errorf("holder = %+v, want the retained allocation alloc-kept", inUse)
	}
	if !strings.Contains(err.Error(), "delete that IPAllocation") {
		t.Errorf("message %q does not say how to free a retained address", err)
	}
}

// A withheld position is the pool's, not a claim's, and is reported as such —
// including on the first allocation, before anything has materialised it.
func TestAReservedAddressIsReportedAsReserved(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "requested-reserved", "10.95.0.0/28")
	withholdFirst(t, db, poolKey, 32)

	_, err := claimAddress(t, db, PrefixRequest{PoolKey: poolKey, PrefixLen: 32, ClaimKey: "router", Address: "10.95.0.0"})
	if !errors.Is(err, ErrAddressReserved) {
		t.Fatalf("AllocatePrefix = %v, want ErrAddressReserved", err)
	}
	if errors.Is(err, ErrAddressInUse) {
		t.Error("a reserved address was also reported as in use")
	}
	if got, err := claimAddress(t, db, PrefixRequest{PoolKey: poolKey, PrefixLen: 32, ClaimKey: "host", Address: "10.95.0.1"}); err != nil || got != "10.95.0.1/32" {
		t.Errorf("the address after the reservation = %s, %v; want 10.95.0.1/32", got, err)
	}
}

// Uniqueness is per address space: the same address binds in a second network
// and is refused in the first.
func TestARequestedAddressIsUniqueWithinItsSpace(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "requested-spaces", "fd83:183::/64")

	req := func(claim, network string) PrefixRequest {
		return PrefixRequest{
			PoolKey: poolKey, PrefixLen: 128, IPFamily: "IPv6", ClaimKey: claim,
			ScopeDigest: spaceOf(network), Address: "fd83:183::53",
		}
	}
	if got, err := claimAddress(t, db, req("a", "net-a")); err != nil || got != "fd83:183::53/128" {
		t.Fatalf("net-a = %s, %v; want fd83:183::53/128", got, err)
	}
	if got, err := claimAddress(t, db, req("b", "net-b")); err != nil || got != "fd83:183::53/128" {
		t.Fatalf("net-b = %s, %v; want fd83:183::53/128", got, err)
	}
	if _, err := claimAddress(t, db, req("a2", "net-a")); !errors.Is(err, ErrAddressInUse) {
		t.Fatalf("second claim in net-a = %v, want ErrAddressInUse", err)
	}
}

// Releasing the holder frees the address for the next claim that names it.
func TestReleasingTheHolderFreesTheAddress(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "requested-release", "10.96.0.0/24")

	if _, err := claimAddress(t, db, PrefixRequest{PoolKey: poolKey, PrefixLen: 32, ClaimKey: "first", Address: "10.96.0.20"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	releaseReturningRetained(t, db, "first")
	if got, err := claimAddress(t, db, PrefixRequest{PoolKey: poolKey, PrefixLen: 32, ClaimKey: "second", Address: "10.96.0.20"}); err != nil || got != "10.96.0.20/32" {
		t.Fatalf("second = %s, %v; want 10.96.0.20/32", got, err)
	}
}

// N claims racing for one address: exactly one wins, every other is refused as
// in use, and only one row is written. The pool row lock is what serialises
// them; the exclusion constraint is the backstop.
func TestConcurrentClaimsForOneAddressHaveOneWinner(t *testing.T) {
	const herd = 16
	db := testdb.Pool(t, testdb.MaxConns(herd+4))
	poolKey := seedPool(t, db, "requested-race", "10.97.0.0/24")

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]error, herd)
		cidrs   = make([]string, herd)
	)
	for i := 0; i < herd; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx := context.Background()
			key := allocationKeyIn(testProject, "race", "alloc-"+string(rune('a'+i)))
			tx, err := db.Begin(ctx)
			if err != nil {
				results[i] = err
				return
			}
			cidr, err := NewPostgresPrefixAllocator().AllocatePrefix(ctx, tx, PrefixRequest{
				PoolKey: poolKey, PrefixLen: 32, IPFamily: "IPv4",
				ClaimKey: key, AllocationKey: key, OwnerProject: testProject,
				ClassName: "standard", Address: "10.97.0.42",
			})
			if err != nil {
				_ = tx.Rollback(ctx)
				results[i] = err
				return
			}
			results[i] = tx.Commit(ctx)
			cidrs[i] = cidr
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
			if cidrs[i] != "10.97.0.42/32" {
				t.Errorf("winner %d bound %s, want 10.97.0.42/32", i, cidrs[i])
			}
		case errors.Is(err, ErrAddressInUse):
		default:
			t.Errorf("claim %d failed with %v, want success or ErrAddressInUse", i, err)
		}
	}
	if winners != 1 {
		t.Errorf("%d claims won the address, want exactly 1", winners)
	}
	if n := countAllocations(t, db, poolKey); n != 1 {
		t.Errorf("%d allocation rows hold the address, want 1", n)
	}
}

// Two pools offer one class; the claim lands in the one holding its address,
// whichever the strategy would otherwise prefer. An address in neither names
// both.
func TestDiscoveryPicksThePoolHoldingTheAddress(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", "dns", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 32})
	first := offerPool(t, tx, "platform", "dns-a", "10.98.0.0/24", "dns", nil)
	second := offerPool(t, tx, "platform", "dns-b", "10.99.0.0/24", "dns", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	read := begin(t, db)
	defer func() { _ = read.Rollback(ctx) }()
	class, err := LoadClass(ctxIn("platform"), read, "dns")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}

	block := func(s string) *net.IPNet {
		b, err := allocation.RequestedBlock(s, 32)
		if err != nil {
			t.Fatalf("RequestedBlock(%q): %v", s, err)
		}
		return &b
	}
	if got, err := DiscoverPoolFor(ctx, read, class, nil, nil); err != nil || got != first {
		t.Errorf("no address: %q, %v; want the first pool %q", got, err, first)
	}
	if got, err := DiscoverPoolFor(ctx, read, class, nil, block("10.99.0.7")); err != nil || got != second {
		t.Errorf("10.99.0.7: %q, %v; want %q", got, err, second)
	}
	if got, err := DiscoverPoolFor(ctx, read, class, nil, block("10.98.0.7")); err != nil || got != first {
		t.Errorf("10.98.0.7: %q, %v; want %q", got, err, first)
	}
	_, err = DiscoverPoolFor(ctx, read, class, nil, block("10.100.0.7"))
	if !errors.Is(err, ErrAddressOutsidePool) {
		t.Fatalf("10.100.0.7: %v, want ErrAddressOutsidePool", err)
	}
	for _, name := range []string{"dns-a", "dns-b"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("message %q does not name pool %s it checked", err, name)
		}
	}
}

// A chained class does not steer the cascade around the address: the leaf pool
// is provisioned as for any claim, and an address outside it is refused.
func TestAChainedClassRefusesAnAddressOutsideItsLeafPool(t *testing.T) {
	db := testdb.Pool(t)
	leafPool := subnetChain(t, db, nil)

	tx := begin(t, db)
	pool, err := lockAndDecodeIPPool(context.Background(), tx, leafPool)
	_ = tx.Rollback(context.Background())
	if err != nil {
		t.Fatalf("read leaf pool: %v", err)
	}
	parents, err := parsePoolCIDR(pool)
	if err != nil {
		t.Fatalf("parse leaf pool: %v", err)
	}
	inside := parents[0].IP.String() // the subnet's first /96
	if got, err := claimAddress(t, db, PrefixRequest{
		PoolKey: leafPool, PrefixLen: 96, IPFamily: "IPv6", ClaimKey: "in", Address: inside,
	}); err != nil {
		t.Fatalf("address inside the leaf pool: %s, %v", got, err)
	}
	if _, err := claimAddress(t, db, PrefixRequest{
		PoolKey: leafPool, PrefixLen: 96, IPFamily: "IPv6", ClaimKey: "out", Address: "fd20:f000:ffff::",
	}); !errors.Is(err, ErrAddressOutsidePool) {
		t.Fatalf("address outside the leaf pool: %v, want ErrAddressOutsidePool", err)
	}
}
