package allocation

// Allocate answers in one traversal what FindFirstAvailableBlock,
// UtilizationPercent answers in two. These tests
// exist mainly to hold the two paths to the same answers, since the whole
// value of the merged path is that it changes nothing but the cost.

import (
	"errors"
	"math/big"
	"net"
	"testing"
)

// allocateSeparately is the three-call sequence Allocate replaces, written the
// way a caller would have to write it: allocate, append the new block, then
// measure the pool it produced.
func allocateSeparately(t *testing.T, parents, existing []net.IPNet, prefixLen int, s Strategy, r Reservation) (AllocationResult, error) {
	t.Helper()
	var res AllocationResult

	block, err := FindFirstAvailableBlockWithReservations(parents, existing, prefixLen, s, r)
	after := existing
	if err == nil {
		res.Block = block
		after = append(append([]net.IPNet{}, existing...), *block)
	}
	// Reserved positions are held by the pool, so they count as used whether or
	// not they have been materialised yet.
	if !r.IsZero() {
		reserved, rErr := r.BlocksIn(parents)
		if rErr == nil {
			after = append(append([]net.IPNet{}, after...), reserved...)
		}
	}
	// Measure, not the exported UtilizationPercent. That one SUMS allocation
	// sizes — it double-counts an address held by two allocations, and it
	// returns a truncated integer. It is retained only as the contrast that
	// makes the #41 regression test meaningful, and it must not be the
	// reference a correctness test compares against. The two agreed here until
	// utilization became a float, and only because these fixtures do not
	// overlap and both answers were truncated to the same integer — correct by
	// accident in both directions at once.
	if m, mErr := Measure(parents, after, Reservation{}); mErr == nil {
		res.UtilizationPercent = m.UtilizationPercent
	}
	return res, err
}

func TestAllocate_AgreesWithSeparateCalls(t *testing.T) {
	type tc struct {
		name     string
		parents  []string
		existing []string
		prefix   int
		strategy Strategy
		res      Reservation
	}

	cases := []tc{
		{name: "empty_v4", parents: []string{"10.0.0.0/16"}, prefix: 24, strategy: FirstFit},
		{name: "empty_v6", parents: []string{"fd00::/20"}, prefix: 48, strategy: FirstFit},
		{name: "partly_full", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.0.0/24", "10.0.1.0/24"}, prefix: 24, strategy: FirstFit},
		{name: "fragmented", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.0.0/24", "10.0.2.0/23", "10.0.8.0/21"}, prefix: 24, strategy: FirstFit},
		{name: "bestfit", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.0.0/24", "10.0.2.0/23", "10.0.8.0/21"}, prefix: 24, strategy: BestFit},
		{name: "leastutilized", parents: []string{"10.0.0.0/24", "10.1.0.0/16"}, existing: []string{"10.0.0.0/26"}, prefix: 26, strategy: LeastUtilized},
		{name: "multi_parent", parents: []string{"10.0.0.0/24", "10.0.1.0/24"}, existing: []string{"10.0.0.0/25"}, prefix: 25, strategy: FirstFit},
		{name: "default_strategy", parents: []string{"10.0.0.0/16"}, prefix: 24},
		{name: "whole_parent", parents: []string{"10.0.0.0/24"}, prefix: 24, strategy: FirstFit},
		{name: "last_block", parents: []string{"10.0.0.0/23"}, existing: []string{"10.0.0.0/24"}, prefix: 24, strategy: FirstFit},
		{name: "exhausted", parents: []string{"10.0.0.0/24"}, existing: []string{"10.0.0.0/24"}, prefix: 24, strategy: FirstFit},
		{name: "too_big_for_pool", parents: []string{"10.0.0.0/24"}, prefix: 16, strategy: FirstFit},
		{name: "with_reservation", parents: []string{"10.0.0.0/16"}, prefix: 24, strategy: FirstFit, res: Reservation{UnitPrefixLength: 24, Leading: 2, Trailing: 1}},
		{name: "reservation_v6", parents: []string{"fd20:a1b:2c3d:1::/64"}, prefix: 96, strategy: FirstFit, res: Reservation{UnitPrefixLength: 96, Leading: 1}},
		{name: "reservation_exhausts", parents: []string{"10.0.0.0/30"}, prefix: 32, strategy: FirstFit, res: Reservation{UnitPrefixLength: 32, Leading: 2, Trailing: 2}},
		{name: "v6_wide_pool", parents: []string{"fd00::/20"}, existing: []string{"fd00::/48", "fd00:0:1::/48"}, prefix: 48, strategy: FirstFit},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parents := mustCIDRs(t, c.parents)
			existing := mustCIDRs(t, c.existing)

			want, wantErr := allocateSeparately(t, parents, existing, c.prefix, c.strategy, c.res)
			got, gotErr := Allocate(parents, existing, c.prefix, c.strategy, c.res)

			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("error mismatch: merged=%v separate=%v", gotErr, wantErr)
			}
			if wantErr != nil && !errors.Is(gotErr, wantErr) {
				t.Fatalf("expected %v, got %v", wantErr, gotErr)
			}
			if (got.Block == nil) != (want.Block == nil) {
				t.Fatalf("block presence mismatch: merged=%v separate=%v", got.Block, want.Block)
			}
			if got.Block != nil && cidrStr(*got.Block) != cidrStr(*want.Block) {
				t.Fatalf("block: merged=%s separate=%s", cidrStr(*got.Block), cidrStr(*want.Block))
			}
			if got.UtilizationPercent != want.UtilizationPercent {
				t.Fatalf("utilization: merged=%g%% separate=%g%%", got.UtilizationPercent, want.UtilizationPercent)
			}
		})
	}
}

// Driving a pool to exhaustion exercises every intermediate state, so the two
// paths have to agree at each step rather than only on a hand-picked few.
func TestAllocate_AgreesWithSeparateCalls_FillToExhaustion(t *testing.T) {
	for _, strategy := range []Strategy{FirstFit, BestFit, LeastUtilized} {
		t.Run(string(strategy), func(t *testing.T) {
			parents := mustCIDRs(t, []string{"10.0.0.0/24", "10.0.4.0/24"})
			var existing []net.IPNet

			for i := 0; ; i++ {
				want, wantErr := allocateSeparately(t, parents, existing, 27, strategy, Reservation{})
				got, gotErr := Allocate(parents, existing, 27, strategy, Reservation{})

				if (gotErr == nil) != (wantErr == nil) {
					t.Fatalf("step %d: error mismatch: merged=%v separate=%v", i, gotErr, wantErr)
				}
				if got.UtilizationPercent != want.UtilizationPercent {
					t.Fatalf("step %d: utilization: merged=%g%% separate=%g%%", i, got.UtilizationPercent, want.UtilizationPercent)
				}
				if gotErr != nil {
					if !errors.Is(gotErr, ErrPoolExhausted) {
						t.Fatalf("step %d: expected ErrPoolExhausted, got %v", i, gotErr)
					}
					if i != 16 {
						t.Fatalf("expected 16 allocations before exhaustion, got %d", i)
					}
					if got.UtilizationPercent != 100 {
						t.Fatalf("exhausted pool reports %g%% utilization", got.UtilizationPercent)
					}
					break
				}
				if cidrStr(*got.Block) != cidrStr(*want.Block) {
					t.Fatalf("step %d: block: merged=%s separate=%s", i, cidrStr(*got.Block), cidrStr(*want.Block))
				}
				existing = append(existing, *got.Block)
			}
		})
	}
}

// The figures describe the pool after the block is taken, which is the whole
// point — a caller writes them to status alongside the allocation it just made.
func TestAllocate_ReportsPostAllocationState(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.0.0.0/24"})

	// A /25 out of an empty /24: half used, and the largest free block left is
	// the other /25. Measured before the allocation, both figures would read 0
	// and /24 respectively.
	got, err := Allocate(parents, nil, 25, FirstFit, Reservation{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got.Block) != "10.0.0.0/25" {
		t.Fatalf("expected 10.0.0.0/25, got %s", cidrStr(*got.Block))
	}
	if got.UtilizationPercent != 50 {
		t.Fatalf("expected 50%% utilization after taking half, got %g%%", got.UtilizationPercent)
	}
}

// On exhaustion the figures still describe the pool, because that is what the
// 507 detail path reports and it must not need a second traversal to get it.
func TestAllocate_PopulatesResultOnExhaustion(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.0.0.0/24"})
	existing := mustCIDRs(t, []string{"10.0.0.0/25"})

	// No /25 is free at an aligned position... except the second half, so ask
	// for something that genuinely cannot fit: a /24.
	got, err := Allocate(parents, existing, 24, FirstFit, Reservation{})
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got %v", err)
	}
	if got.Block != nil {
		t.Fatalf("expected no block, got %s", cidrStr(*got.Block))
	}
	if got.UtilizationPercent != 50 {
		t.Fatalf("expected 50%% utilization, got %g%%", got.UtilizationPercent)
	}
}

// A fully-allocated pool has no free block at all, which is the zero sentinel
// rather than a prefix length of 0.
func TestAllocate_FullPoolReportsZeroLargestFree(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.0.0.0/24"})
	existing := mustCIDRs(t, []string{"10.0.0.0/24"})

	got, err := Allocate(parents, existing, 26, FirstFit, Reservation{})
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got %v", err)
	}
	if got.UtilizationPercent != 100 {
		t.Fatalf("expected 100%%, got %g%%", got.UtilizationPercent)
	}
}

func TestAllocate_ErrorPaths(t *testing.T) {
	t.Run("no_parents", func(t *testing.T) {
		if _, err := Allocate(nil, nil, 24, FirstFit, Reservation{}); !errors.Is(err, ErrNoParent) {
			t.Fatalf("expected ErrNoParent, got %v", err)
		}
	})

	// A malformed reservation is operator misconfiguration and must stay
	// distinguishable from an exhausted pool.
	t.Run("invalid_reservation", func(t *testing.T) {
		parents := mustCIDRs(t, []string{"10.0.0.0/24"})
		_, err := Allocate(parents, nil, 26, FirstFit, Reservation{UnitPrefixLength: 16, Leading: 1})
		if !errors.Is(err, ErrInvalidReservation) {
			t.Fatalf("expected ErrInvalidReservation, got %v", err)
		}
	})
	t.Run("reservation_too_large", func(t *testing.T) {
		parents := mustCIDRs(t, []string{"10.0.0.0/24"})
		_, err := Allocate(parents, nil, 26, FirstFit, Reservation{UnitPrefixLength: 26, Leading: 3, Trailing: 3})
		if !errors.Is(err, ErrReservationTooLarge) {
			t.Fatalf("expected ErrReservationTooLarge, got %v", err)
		}
	})
}

// Utilization is derived from free space rather than by summing the existing
// entries, so a pool carrying a duplicate or overlapping row reports the truth
// instead of over-counting. UtilizationPercent, which sums, does not.
func TestAllocate_UtilizationIgnoresDuplicateRows(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.0.0.0/24"})
	existing := mustCIDRs(t, []string{"10.0.0.0/25", "10.0.0.0/25", "10.0.0.0/26"})

	got, err := Allocate(parents, existing, 25, FirstFit, Reservation{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.UtilizationPercent != 100 {
		t.Fatalf("expected 100%% (both halves now held), got %g%%", got.UtilizationPercent)
	}
	// The summing implementation counts the duplicate and the nested block on
	// top of the real occupancy.
	if sum := UtilizationPercent(parents, append(existing, *got.Block)); sum <= 100 {
		t.Logf("summing implementation reports %d%%", sum)
	}
}

// ----------------------------------------------------------------------------
// Measure
// ----------------------------------------------------------------------------

// The case from #41: a /28 where eight networks each legitimately hold the same
// address. One address is consumed, not eight. The summing implementation
// reports eight times the real occupancy; this is the whole reason Measure
// exists.
func TestMeasure_CountsSharedAddressOnce(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.71.0.0/28"})
	var existing []net.IPNet
	for i := 0; i < 8; i++ {
		existing = append(existing, mustCIDR(t, "10.71.0.0/32"))
	}

	got, err := Measure(parents, existing, Reservation{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Consumed.Int64() != 1 {
		t.Fatalf("expected 1 address consumed, got %s", got.Consumed)
	}
	if got.Free.Int64() != 15 {
		t.Fatalf("expected 15 free, got %s", got.Free)
	}
	if got.Total.Int64() != 16 {
		t.Fatalf("expected 16 total, got %s", got.Total)
	}
	// 1/16 = 6.25% exactly. It used to read 6: integer division truncated it,
	// which is the defect that made every pool sized for growth report 0%.
	if got.UtilizationPercent != 6.25 {
		t.Fatalf("expected 6.25%%, got %g%%", got.UtilizationPercent)
	}
	// The summing implementation is what over-reported.
	if sum := UtilizationPercent(parents, existing); sum != 50 {
		t.Fatalf("expected the summing implementation to report 50%%, got %d%%", sum)
	}
}

func TestMeasure_Table(t *testing.T) {
	type tc struct {
		name         string
		parents      []string
		existing     []string
		res          Reservation
		wantTotal    int64
		wantConsumed int64
		wantPct      float64
		wantLargest  int
	}

	cases := []tc{
		{name: "empty", parents: []string{"10.0.0.0/24"}, wantTotal: 256, wantConsumed: 0, wantPct: 0, wantLargest: 24},
		{name: "half", parents: []string{"10.0.0.0/24"}, existing: []string{"10.0.0.0/25"}, wantTotal: 256, wantConsumed: 128, wantPct: 50, wantLargest: 25},
		{name: "full", parents: []string{"10.0.0.0/24"}, existing: []string{"10.0.0.0/24"}, wantTotal: 256, wantConsumed: 256, wantPct: 100, wantLargest: 0},
		{
			name: "nested_rows_count_once", parents: []string{"10.0.0.0/24"},
			existing:  []string{"10.0.0.0/25", "10.0.0.0/26", "10.0.0.64/26"},
			wantTotal: 256, wantConsumed: 128, wantPct: 50, wantLargest: 25,
		},
		{
			name: "row_outside_parent_ignored", parents: []string{"10.0.0.0/24"},
			existing:  []string{"192.168.0.0/24"},
			wantTotal: 256, wantConsumed: 0, wantPct: 0, wantLargest: 24,
		},
		{
			name: "multi_parent", parents: []string{"10.0.0.0/24", "10.0.1.0/24"},
			existing:  []string{"10.0.0.0/25"},
			wantTotal: 512, wantConsumed: 128, wantPct: 25, wantLargest: 24,
		},
		{
			name: "reservation_counts_as_consumed", parents: []string{"10.0.0.0/24"},
			res:       Reservation{UnitPrefixLength: 26, Leading: 1},
			wantTotal: 256, wantConsumed: 64, wantPct: 25, wantLargest: 25,
		},
		{
			name: "reservation_idempotent_with_materialised_rows", parents: []string{"10.0.0.0/24"},
			existing:  []string{"10.0.0.0/26"},
			res:       Reservation{UnitPrefixLength: 26, Leading: 1},
			wantTotal: 256, wantConsumed: 64, wantPct: 25, wantLargest: 25,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Measure(mustCIDRs(t, c.parents), mustCIDRs(t, c.existing), c.res)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Total.Int64() != c.wantTotal {
				t.Fatalf("total: expected %d, got %s", c.wantTotal, got.Total)
			}
			if got.Consumed.Int64() != c.wantConsumed {
				t.Fatalf("consumed: expected %d, got %s", c.wantConsumed, got.Consumed)
			}
			if free := c.wantTotal - c.wantConsumed; got.Free.Int64() != free {
				t.Fatalf("free: expected %d, got %s", free, got.Free)
			}
			if got.UtilizationPercent != c.wantPct {
				t.Fatalf("pct: expected %g, got %g", c.wantPct, got.UtilizationPercent)
			}
		})
	}
}

// Consumed + Free == Total must hold exactly, including on IPv6 spaces far
// wider than an int64 — the case where a saturating count loses the ratio.
func TestMeasure_ExactOnWideIPv6(t *testing.T) {
	parents := mustCIDRs(t, []string{"fd00::/20"})
	existing := mustCIDRs(t, []string{"fd00::/48", "fd00:0:1::/48"})

	got, err := Measure(parents, existing, Reservation{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	sum := new(big.Int).Add(got.Consumed, got.Free)
	if sum.Cmp(got.Total) != 0 {
		t.Fatalf("consumed+free=%s != total=%s", sum, got.Total)
	}
	// 2 x /48 out of a /20 is a vanishing fraction — it must not round to
	// anything but 0, and must not report as full.
	if got.UtilizationPercent != 0 {
		t.Fatalf("expected 0%%, got %g%%", got.UtilizationPercent)
	}
	// Total is 2^108, far past int64. Exactness is the point.
	if got.Total.BitLen() != 109 {
		t.Fatalf("expected a 2^108 address space, got a %d-bit count", got.Total.BitLen()-1)
	}
}

// Measure and Allocate must report the same pool identically — Allocate is
// defined as Measure over the pool including the block it just took.
func TestMeasure_AgreesWithAllocate(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.0.0.0/22"})
	var existing []net.IPNet

	for i := 0; i < 8; i++ {
		got, err := Allocate(parents, existing, 25, FirstFit, Reservation{})
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		existing = append(existing, *got.Block)

		m, err := Measure(parents, existing, Reservation{})
		if err != nil {
			t.Fatalf("step %d: measure: %v", i, err)
		}
		if m.UtilizationPercent != got.UtilizationPercent {
			t.Fatalf("step %d: utilization: allocate=%g%% measure=%g%%", i, got.UtilizationPercent, m.UtilizationPercent)
		}
	}
}

func TestMeasure_PropagatesReservationErrors(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.0.0.0/24"})
	if _, err := Measure(parents, nil, Reservation{UnitPrefixLength: 16, Leading: 1}); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("expected ErrInvalidReservation, got %v", err)
	}
}
