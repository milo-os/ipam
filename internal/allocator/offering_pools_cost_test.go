package allocator

// What a LIST of the class catalog costs (#98).
//
// loadtest measured cluster-wide IPClass LIST at p50 132ms / p95 237ms against
// a 200ms gate, roughly 10x an earlier run the same day, while every other read
// stayed comfortable. #57 is the candidate: it made status.offeringPools real
// and computed it ON READ, so a LIST now does per-class work a LIST never did.
//
// # Why this counts queries instead of timing anything
//
// A fitting mechanism is not a cause, and wall clock cannot settle it here.
// verification-conventions rule 9 is explicit: on a database-backed path the
// clock cannot resolve anything below roughly 10%, and a timing comparison
// needs both arms in one sitting on a fresh database to mean anything at all.
//
// Query count is deterministic, reproduces exactly, and answers the question
// the latency raises — does the work scale with the number of classes? If it
// does, the mechanism is confirmed without a stopwatch. If it does not, the
// cause is elsewhere and the latency belongs to something else entirely.
//
// pgx.Tx is an interface, so counting is a wrapper rather than an
// instrumented pool: no change to the fixture, and it counts exactly the round
// trips this code path issues.

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// countingTx counts the round trips a code path issues, by wrapping the
// transaction it was already given.
type countingTx struct {
	pgx.Tx
	queries int
}

func (c *countingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.queries++
	return c.Tx.Query(ctx, sql, args...)
}

func (c *countingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.queries++
	return c.Tx.QueryRow(ctx, sql, args...)
}

// seedCatalog writes `chains` independent three-deep chains, each backed by its
// own root pool — the shape the real catalog has, rather than a flat list that
// would understate the ancestry walk.
func seedCatalog(t *testing.T, db *pgxpool.Pool, chains int) []string {
	t.Helper()
	var names []string

	for i := 0; i < chains; i++ {
		root := fmt.Sprintf("cost-net-v6-%d", i)
		mid := fmt.Sprintf("cost-subnet-v6-%d", i)
		leaf := fmt.Sprintf("cost-endpoint-v6-%d", i)

		poolName := fmt.Sprintf("cost-pool-v6-%d", i)
		seedObject(t, db, platformKey("ippools", poolName), "IPPool", poolName,
			&ipamv1alpha1.IPPool{
				TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
				ObjectMeta: metav1.ObjectMeta{Name: poolName},
				Spec: ipamv1alpha1.IPPoolSpec{
					CIDR:       fmt.Sprintf("fd%02x::/20", 0x40+i),
					IPFamily:   ipamv1alpha1.IPv6,
					ClassNames: []string{root},
				},
				Status: ipamv1alpha1.IPPoolStatus{
					Phase:         ipamv1alpha1.PoolReady,
					AllocatedCIDR: fmt.Sprintf("fd%02x::/20", 0x40+i),
					IPFamily:      ipamv1alpha1.IPv6,
				},
			})

		for _, c := range []struct{ name, parent string }{
			{root, ""}, {mid, root}, {leaf, mid},
		} {
			class := &ipamv1alpha1.IPClass{
				TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
				ObjectMeta: metav1.ObjectMeta{Name: c.name},
				Spec: ipamv1alpha1.IPClassSpec{
					IPFamily:             ipamv1alpha1.IPv6,
					ParentClassName:      c.parent,
					AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 96, Max: 96},
				},
			}
			seedObject(t, db, platformKey("ipclasses", c.name), "IPClass", c.name, class)
			names = append(names, c.name)
		}
	}
	return names
}

func countQueriesFor(t *testing.T, db *pgxpool.Pool, names []string) int {
	t.Helper()
	ctx := platformCtx()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	counter := &countingTx{Tx: tx}
	got, err := OfferingPoolCounts(ctx, counter, names)
	if err != nil {
		t.Fatalf("count offering pools: %v", err)
	}
	if len(got) != len(names) {
		t.Fatalf("counts returned = %d, want %d; the fixture is not being resolved and the "+
			"query count would be measuring a no-op", len(got), len(names))
	}
	return counter.queries
}

// TestOfferingPoolCountsDoesNotScaleWithCatalogSize is the discriminator #98
// asks for, and the regression guard once it passes.
//
// A LIST resolves every class on the page. If the work per LIST grows with the
// number of classes, a catalog that doubles doubles the cost of every catalog
// read — and unlike a slow query this gets worse as the product is adopted,
// which is the shape worth refusing outright rather than tuning.
//
// The bound is deliberately generous. This is not asserting a constant; it is
// asserting that tripling the catalog does not triple the work, which is the
// difference between a batched implementation and a per-item one. A tighter
// bound would fail on an implementation detail rather than on the property.
func TestOfferingPoolCountsDoesNotScaleWithCatalogSize(t *testing.T) {
	db := newMigratedPool(t)
	all := seedCatalog(t, db, 6) // 6 chains x 3 classes = 18 classes, 6 roots

	small := countQueriesFor(t, db, all[:3]) // one chain
	large := countQueriesFor(t, db, all)     // six chains, 6x the classes

	t.Logf("queries: %d classes -> %d, %d classes -> %d", 3, small, len(all), large)

	// Positive control: the work must be non-trivial, or "it did not grow" is
	// equally true of a function that does nothing.
	if small == 0 {
		t.Fatal("no queries issued for the small catalog; the counter is not wired to the " +
			"path under test")
	}

	// Six times the classes must not cost anything like six times the queries.
	// Allowing 2x absorbs a batched implementation that still does a small
	// fixed amount of per-page work.
	if large > small*2 {
		t.Errorf("query count scales with catalog size: %d classes cost %d queries, %d cost %d "+
			"(%.1fx the classes, %.1fx the queries).\n"+
			"A LIST resolves every class on the page, so per-class work makes every catalog "+
			"read more expensive as the catalog grows. Batch the lookup across the page: one "+
			"query for all the classes, one for all their ancestors, one for all the offers.",
			len(all), large, 3, small,
			float64(len(all))/3.0, float64(large)/float64(small))
	}
}
