package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// testProject is the project fixtures are seeded into. A real name, not "":
// every object lives under a "project/<name>/" prefix, so a fixture at an
// unprefixed key would be one no deployment writes.
const testProject = "platform"

func seedPool(t *testing.T, db *pgxpool.Pool, name, cidr string) string {
	t.Helper()
	family := ipamv1alpha1.IPv4
	if cidr[0] == 'f' {
		family = ipamv1alpha1.IPv6
	}
	obj := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ipamv1alpha1.IPPoolSpec{CIDR: cidr, IPFamily: family},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: family,
		},
	}
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	key := tenant.Identity{Name: testProject}.ResourceKey("ippools", name)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		key, name, data); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return key
}

// measureNow recomputes consumption from the full allocation set, the way the
// write path did before it maintained a total.
func measureNow(t *testing.T, db *pgxpool.Pool, poolKey, cidr string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, err := loadExistingAllocations(ctx, tx, poolKey)
	if err != nil {
		t.Fatalf("load allocations: %v", err)
	}
	parents := []net.IPNet{mustNet(t, cidr)}
	m, err := allocation.Measure(parents, existing, allocation.Reservation{})
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	return m.Consumed.String()
}

func storedConsumption(t *testing.T, db *pgxpool.Pool, poolKey string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	got, ok, err := readConsumed(ctx, tx, poolKey)
	if err != nil {
		t.Fatalf("read consumption: %v", err)
	}
	if !ok {
		return "<none>"
	}
	return got.String()
}

func allocate(t *testing.T, db *pgxpool.Pool, poolKey, claim string, prefixLen int) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cidr, err := NewPostgresPrefixAllocator().AllocatePrefix(ctx, tx, PrefixRequest{
		PoolKey: poolKey, PrefixLen: prefixLen, IPFamily: "IPv4",
		ClaimKey: claim, AllocationKey: claim, OwnerProject: testProject,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		if errors.Is(err, ErrPoolExhausted) {
			return ""
		}
		t.Fatalf("allocate %s: %v", claim, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return cidr
}

func release(t *testing.T, db *pgxpool.Pool, claim string) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := NewPostgresPrefixAllocator().Release(ctx, tx, claim); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("release %s: %v", claim, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit release: %v", err)
	}
}

// The maintained total must equal a fresh measurement after every write. Drift
// is the characteristic failure of a running total, and it surfaces later as
// utilization diverging from reality with nothing left to compare against.
func TestMaintainedTotalTracksAFreshMeasurement(t *testing.T) {
	db := testdb.Pool(t)
	const cidr = "10.40.0.0/16"
	poolKey := seedPool(t, db, "consumption-v4", cidr)

	rng := rand.New(rand.NewSource(20260811))
	held := map[string]bool{}

	for step := range 60 {
		claim := fmt.Sprintf("claim-%d", step)
		if len(held) > 0 && rng.Intn(3) == 0 {
			for c := range held {
				release(t, db, c)
				delete(held, c)
				break
			}
		} else {
			if got := allocate(t, db, poolKey, claim, 24+rng.Intn(4)); got != "" {
				held[claim] = true
			}
		}

		want := measureNow(t, db, poolKey, cidr)
		if got := storedConsumption(t, db, poolKey); got != want {
			t.Fatalf("step %d: maintained total = %s, fresh measurement = %s", step, got, want)
		}
	}
}

// A pool whose first write is a release still ends with a correct total, rather
// than subtracting from a total that was never established.
func TestAPoolWithNoTotalYetSeedsFromWhatItHolds(t *testing.T) {
	db := testdb.Pool(t)
	const cidr = "10.41.0.0/16"
	poolKey := seedPool(t, db, "consumption-seed", cidr)

	// Two allocations written straight to the table, so no total is recorded.
	ctx := context.Background()
	for i, c := range []string{"10.41.0.0/24", "10.41.1.0/24"} {
		if _, err := db.Exec(ctx,
			`INSERT INTO ipam_cidr_allocations
			   (pool_key, allocated_cidr, claim_key, allocation_key, ip_family)
			 VALUES ($1,$2,$3,$3,'IPv4')`, poolKey, c, fmt.Sprintf("pre-%d", i)); err != nil {
			t.Fatalf("seed allocation: %v", err)
		}
	}
	if got := storedConsumption(t, db, poolKey); got != "<none>" {
		t.Fatalf("expected no total yet, got %s", got)
	}

	release(t, db, "pre-0")

	want := measureNow(t, db, poolKey, cidr)
	if got := storedConsumption(t, db, poolKey); got != want {
		t.Fatalf("after seeding release: total = %s, fresh measurement = %s", got, want)
	}
}

// Two allocations may hold the same address in different address spaces. The
// total counts that address once, where summing allocation sizes counts it
// twice.
func TestOverlappingAllocationsCountAnAddressOnce(t *testing.T) {
	db := testdb.Pool(t)
	const cidr = "10.42.0.0/16"
	poolKey := seedPool(t, db, "consumption-overlap", cidr)
	ctx := context.Background()

	for i, digest := range []string{"space-a", "space-b"} {
		if _, err := db.Exec(ctx,
			`INSERT INTO ipam_cidr_allocations
			   (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, scope_digest)
			 VALUES ($1,'10.42.0.0/24',$2,$2,'IPv4',$3)`,
			poolKey, fmt.Sprintf("shared-%d", i), digest); err != nil {
			t.Fatalf("seed overlapping allocation: %v", err)
		}
	}

	// Any write establishes the total; a release of one of the two is enough.
	release(t, db, "shared-0")

	want := measureNow(t, db, poolKey, cidr)
	got := storedConsumption(t, db, poolKey)
	if got != want {
		t.Fatalf("total = %s, fresh measurement = %s", got, want)
	}
	if got != "256" {
		t.Fatalf("one /24 still held by one holder should be 256 addresses, got %s", got)
	}
}

func mustNet(t *testing.T, s string) net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return *n
}

// Adding a block that overlaps one already held must count only the addresses
// that were not consumed already.
//
// AllocatePrefix never produces this on its own — it hands out disjoint blocks,
// and the EXCLUDE constraint forbids overlap within one address space. It
// arises across address spaces, where two holders of one address is the point.
// The delta is exercised directly because no path through AllocatePrefix
// reaches it.
func TestAddingAnOverlappingBlockCountsOnlyWhatIsNew(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	const cidr = "10.43.0.0/16"
	poolKey := seedPool(t, db, "consumption-add-overlap", cidr)
	parents := []net.IPNet{mustNet(t, cidr)}

	// A /24 already held in one address space.
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		   (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, scope_digest)
		 VALUES ($1,'10.43.0.0/24','held','held','IPv4','space-a')`, poolKey); err != nil {
		t.Fatalf("seed allocation: %v", err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// A /23 covering it, in another space: 512 addresses, 256 already consumed.
	got, err := consumptionAfterAllocate(ctx, tx, poolKey, parents, mustNet(t, "10.43.0.0/23"))
	if err != nil {
		t.Fatalf("consumptionAfterAllocate: %v", err)
	}
	if got.String() != "512" {
		t.Errorf("consumed = %s, want 512: the /23 adds 256 addresses to the 256 already held", got)
	}
}
