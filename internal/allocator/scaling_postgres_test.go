package allocator

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/testdb"
)

// The point of the bounded search is that the rows read per allocation do not
// grow with the pool's occupancy. This asserts that, so a change that quietly
// restores the full read fails here instead of in production.
//
// It gates on TUPLES, not on time or bytes. A tuple count is the database's own
// answer to "how much did this read", it is a ratio against another measurement
// from the same run, and it does not depend on the machine. Elapsed time does,
// which is why the numbers are logged but nothing asserts on them.
func TestReadsPerAllocationDoNotGrowWithOccupancy(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a pool with 2,000 allocations")
	}
	db := testdb.Pool(t)
	ctx := context.Background()

	const cidr = "fd30::/20"
	poolKey := seedPool(t, db, "scaling", cidr)

	const windowSize = 250
	const windows = 8

	type sample struct {
		occupancy int
		tuples    int64
	}
	var samples []sample

	for w := range windows {
		occupancy := w * windowSize

		// ANALYZE between windows: without it the planner keeps the statistics
		// of an empty table and picks a sequential scan, which measures the
		// planner rather than the search.
		if _, err := db.Exec(ctx, `ANALYZE ipam_cidr_allocations`); err != nil {
			t.Fatalf("analyze at n=%d: %v", occupancy, err)
		}

		before := heapTuplesRead(t, db, "ipam_cidr_allocations")
		for i := range windowSize {
			if got := allocate(t, db, poolKey, "scale-"+itoa(occupancy+i), 48); got == "" {
				t.Fatalf("pool exhausted at n=%d", occupancy+i)
			}
		}
		samples = append(samples, sample{
			occupancy: occupancy,
			tuples:    heapTuplesRead(t, db, "ipam_cidr_allocations") - before,
		})
	}

	t.Logf("%10s %16s", "occupancy", "tuples/alloc")
	for _, s := range samples {
		t.Logf("%10d %16.1f", s.occupancy, float64(s.tuples)/float64(windowSize))
	}

	// The first window is skipped: occupancy moves from 0 to windowSize while it
	// is being measured, so it averages over a range that multiplied underneath
	// it. Every window after that is a fair sample.
	base := float64(samples[1].tuples) / float64(windowSize)
	top := float64(samples[len(samples)-1].tuples) / float64(windowSize)
	if base == 0 {
		t.Fatal("base window read no tuples; the statistics collector is not reporting")
	}

	occupancyRatio := float64(samples[len(samples)-1].occupancy) / float64(samples[1].occupancy)
	readRatio := top / base
	t.Logf("occupancy %.0fx, reads %.1fx", occupancyRatio, readRatio)

	// A full read per allocation would track occupancy: reads would rise by
	// about occupancyRatio. The bound is deliberately loose — this asserts the
	// shape, not a figure, and a bounded search has no reason to approach it.
	const bound = 3.0
	if readRatio > bound {
		t.Errorf("reads per allocation grew %.1fx while occupancy grew %.0fx; "+
			"a bounded search should stay flat (bound %.1fx)", readRatio, occupancyRatio, bound)
	}
}

// heapTuplesRead is the database's own count of tuples returned from the table,
// index scans and sequential scans together. Both terms are needed: the plan
// can flip between them partway through a run, and a figure counting one would
// halve itself at a point that looks exactly like the effect under test.
//
// pg_stat_user_tables is fed by the statistics collector, which flushes on its
// own schedule, so a delta over a short window routinely reads zero — and a
// zero here is indistinguishable from a query that touched no rows.
// pg_stat_force_next_flush makes the read synchronous, and both sides of every
// delta take it.
func heapTuplesRead(t *testing.T, db *pgxpool.Pool, table string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush statistics before reading them: %v", err)
	}
	var seq, idx int64
	if err := db.QueryRow(ctx,
		`SELECT coalesce(seq_tup_read, 0), coalesce(idx_tup_fetch, 0)
		   FROM pg_stat_user_tables
		  WHERE relname = $1 AND schemaname = current_schema()`, table).Scan(&seq, &idx); err != nil {
		t.Fatalf("read pg_stat_user_tables for %s: %v", table, err)
	}
	return seq + idx
}
