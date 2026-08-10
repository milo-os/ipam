package allocator

// The instrument for #39: how per-allocation cost grows with the number of
// allocations already in the pool.
//
// #36 characterised the Go-side shape with a pure benchmark over a synthetic
// set. This one measures the real path — a pgx transaction, the pool row lock,
// the row load, the search, the insert, and the capacity recompute — because
// that is where the exponent actually costs somebody a request, and because a
// Go-only benchmark cannot see the row transfer or the plan.
//
// # Read this before quoting a number out of it
//
// It reports a RATIO between occupancy levels measured in one process against
// one freshly migrated schema, never an absolute (verification-conventions rule
// 9). Wall-clock seconds from this file are not comparable to seconds from any
// other session, and the ratio is the only figure that survives.
//
// Three metrics per window, deliberately not one:
//
//   - elapsed per allocation — what a caller feels, and the noisiest.
//   - bytes allocated per allocation — deterministic to a fraction of a percent
//     and comparable across sessions. When it and elapsed disagree, believe it.
//   - heap tuples read per allocation, from pg_stat_user_tables — the database's
//     own account of how much of the table each call touched. This is the one
//     that answers "is the search bounded" directly, rather than by inference
//     from a stopwatch.
//
// Opt-in: it allocates thousands of blocks and takes minutes.
//
//	IPAM_ALLOC_SCALING=1 \
//	IPAM_TEST_POSTGRES_DSN="postgres://postgres:postgres@127.0.0.1:55601/postgres?sslmode=disable" \
//	go test ./internal/allocator/ -run TestAllocationCostScaling -count=1 -v

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// scalingWindow is one measurement: the cost of `count` allocations taken when
// the pool already held `occupancy` of them.
type scalingWindow struct {
	occupancy int
	count     int
	elapsed   time.Duration
	bytes     uint64
	tuples    int64
}

func (w scalingWindow) perAllocMillis() float64 {
	return float64(w.elapsed.Microseconds()) / float64(w.count) / 1000
}
func (w scalingWindow) perAllocKiB() float64 {
	return float64(w.bytes) / float64(w.count) / 1024
}
func (w scalingWindow) perAllocTuples() float64 {
	return float64(w.tuples) / float64(w.count)
}

func TestAllocationCostScaling(t *testing.T) {
	if os.Getenv("IPAM_ALLOC_SCALING") == "" {
		t.Skip("IPAM_ALLOC_SCALING not set; this is the #39 measurement instrument, not a gate")
	}
	db := testdb.Pool(t, "ipam_alloc_scaling")
	ctx := platformCtx()

	poolKey := seedScalingPool(t, db)
	alloc := NewPostgresPrefixAllocator()
	digest := scope.AddressSpaceDigest("", nil)

	allocateOne := func(n int) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin at n=%d: %v", n, err)
		}
		_, err = alloc.AllocatePrefix(ctx, tx, AllocateRequest{
			PoolKey:       poolKey,
			AllocationKey: platformKey("ipallocations", "scale-"+itoa(n)),
			ClaimKey:      platformKey("ipclaims", "scale-"+itoa(n)),
			ClassName:     "scale-v6",
			ScopeDigest:   digest,
			PrefixLength:  48,
			IPFamily:      "IPv6",
			ReclaimPolicy: "Delete",
			OwnerProject:  "scale",
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("allocate %d: %v", n, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %d: %v", n, err)
		}
	}

	const windowSize = 100
	checkpoints := []int{0, 500, 1000, 2000, 4000}

	var windows []scalingWindow
	n := 0
	for _, target := range checkpoints {
		for n < target {
			allocateOne(n)
			n++
		}
		// The window itself, measured. ANALYZE first so the plan is settled
		// before the clock starts: autoanalyze otherwise fires partway through a
		// window and flips the plan mid-measurement, which reads as noise and is
		// not (see the note on loadPoolAllocations).
		if _, err := db.Exec(ctx, `ANALYZE ipam_cidr_allocations`); err != nil {
			t.Fatalf("analyze at n=%d: %v", n, err)
		}
		tuplesBefore := heapTuplesRead(t, db, "ipam_cidr_allocations")
		var msBefore, msAfter runtime.MemStats
		runtime.ReadMemStats(&msBefore)

		start := time.Now()
		for range windowSize {
			allocateOne(n)
			n++
		}
		elapsed := time.Since(start)

		runtime.ReadMemStats(&msAfter)
		windows = append(windows, scalingWindow{
			occupancy: target,
			count:     windowSize,
			elapsed:   elapsed,
			bytes:     msAfter.TotalAlloc - msBefore.TotalAlloc,
			tuples:    heapTuplesRead(t, db, "ipam_cidr_allocations") - tuplesBefore,
		})
	}

	t.Log("per-allocation cost against pool occupancy (ratios only; seconds do not travel)")
	t.Logf("%10s %12s %12s %14s", "occupancy", "ms/alloc", "KiB/alloc", "tuples/alloc")
	for _, w := range windows {
		t.Logf("%10d %12.3f %12.1f %14.1f",
			w.occupancy, w.perAllocMillis(), w.perAllocKiB(), w.perAllocTuples())
	}

	// The ratio deliberately skips the n=0 window. Occupancy moves by
	// windowSize DURING every window, which at n=4000 is 2.5% and at n=0 is the
	// whole range — so the first window measures an average over an occupancy
	// that multiplied while it was being measured, and dividing by it flatters
	// the result. Every window from the second on is a fair sample.
	base, top := windows[1], windows[len(windows)-1]
	t.Logf("n=%d over n=%d (occupancy %.0fx):  elapsed %.1fx   bytes %.1fx   tuples %.1fx",
		top.occupancy, base.occupancy,
		float64(top.occupancy)/float64(base.occupancy),
		top.perAllocMillis()/base.perAllocMillis(),
		top.perAllocKiB()/base.perAllocKiB(),
		top.perAllocTuples()/base.perAllocTuples())

	// No threshold. This file measures; it does not gate. A gate here would
	// have to name an absolute, and an absolute measured on one machine in one
	// hour is the thing rule 9 exists to stop being written down.
}

// heapTuplesRead is the database's own count of tuples returned from the table,
// index scans and sequential scans together.
//
// Both terms are needed and the reason is the plan instability documented on
// loadPoolAllocations: the same query flips between a seq scan and an index
// scan partway through a run, so a figure counting only one of them halves
// itself at a point that moves between runs and looks exactly like the effect
// under test.
// pg_stat_user_tables is fed by the statistics collector, which flushes on its
// own schedule — so a delta taken across a window of a few hundred milliseconds
// routinely reads ZERO, and a zero here is indistinguishable from a query that
// touched no rows. The first run of this file reported 0.0 tuples/alloc at
// three of five checkpoints and four-figure values at the other two, which is
// verification-conventions rule 11 in miniature: the instrument was returning
// nothing and it looked like a finding. pg_stat_force_next_flush (PostgreSQL
// 15+) makes the read synchronous, and both sides of every delta take it.
func heapTuplesRead(t *testing.T, db *pgxpool.Pool, table string) int64 {
	t.Helper()
	if _, err := db.Exec(context.Background(), `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush statistics before reading them: %v", err)
	}
	var seq, idx int64
	err := db.QueryRow(context.Background(),
		`SELECT coalesce(seq_tup_read, 0), coalesce(idx_tup_fetch, 0)
		   FROM pg_stat_user_tables
		  WHERE relname = $1
		    AND schemaname = current_schema()`, table).Scan(&seq, &idx)
	if err != nil {
		t.Fatalf("read pg_stat_user_tables for %s: %v", table, err)
	}
	return seq + idx
}

// seedScalingPool writes an IPv6 root pool wide enough that 4,100 /48s fit
// without the pool's own fullness becoming the variable under test.
//
// fd30::/20 holds 2^28 /48s. Filling it to 4,100 is 0.0015% — so every window
// measures the cost of the SEARCH over what is already allocated, not the cost
// of a search running out of room.
func seedScalingPool(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	const name = "scale-v6"
	obj := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       "fd30::/20",
			IPFamily:   ipamv1alpha1.IPv6,
			ClassNames: []string{"scale-v6"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: "fd30::/20",
			IPFamily:      ipamv1alpha1.IPv6,
		},
	}
	key := platformKey("ippools", name)
	seedObject(t, db, key, "IPPool", name, obj)
	return key
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
