package allocation

// The comparison that justifies Allocate: one traversal against the three it
// replaces. Companion to BenchmarkPerAllocationWork in cost_bench_test.go,
// which breaks down where the three-call cost goes.
//
// SeparateCalls is what the allocator did per allocation — choose a block,
// then recompute both status figures over the pool including it. Merged is the
// single call answering all three. Both produce identical results; the
// equivalence is pinned by TestAllocate_AgreesWithSeparateCalls.

import (
	"fmt"
	"math/big"
	"net"
	"testing"
)

func BenchmarkAllocateVsSeparateCalls(b *testing.B) {
	for _, n := range []int{250, 1000, 4000} {
		parents, existing := existingAt(n)

		b.Run(fmt.Sprintf("SeparateCalls/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				block, err := FindFirstAvailableBlock(parents, existing, 48, FirstFit)
				if err != nil {
					b.Fatal(err)
				}
				// The second traversal Allocate avoids: measuring the pool
				// after the block is taken.
				after := append(append([]net.IPNet{}, existing...), *block)
				if _, mErr := Measure(parents, after, Reservation{}); mErr != nil {
					b.Fatal(mErr)
				}
			}
		})

		b.Run(fmt.Sprintf("Merged/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Allocate(parents, existing, 48, FirstFit, Reservation{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Row order should not change what allocation costs.
//
// The database returns a pool's allocations in no guaranteed order, and that
// order reaches freeRegions unchanged. Before the sort was decorated, the
// comparator built two big.Ints per comparison, so the work scaled with the
// *comparison count* — and Go's pdqsort makes far fewer comparisons on
// already-ordered input than on shuffled input. Cost therefore depended on
// what order Postgres happened to hand back rows, which is not a property
// anyone can plan capacity against.
//
// Both arms of this benchmark do identical allocation work. Any gap between
// them is order-sensitivity.
func BenchmarkRowOrderSensitivity(b *testing.B) {
	const n = 4000
	parents, existing := existingAt(n)

	sorted := append([]net.IPNet{}, existing...)
	shuffled := append([]net.IPNet{}, existing...)
	// Deterministic shuffle — a 64-bit LCG, so the benchmark is reproducible
	// and the package's test binary pulls in nothing new.
	seed := uint64(0x9E3779B97F4A7C15)
	for i := len(shuffled) - 1; i > 0; i-- {
		seed = seed*6364136223846793005 + 1442695040888963407
		j := int((seed >> 33) % uint64(i+1))
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}

	b.Run("sorted", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = FindFirstAvailableBlock(parents, sorted, 48, FirstFit)
		}
	})
	b.Run("shuffled", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = FindFirstAvailableBlock(parents, shuffled, 48, FirstFit)
		}
	})
}

// The pool-status path: two traversals plus region splitting, against one.
//
// SeparateCalls mirrors what the allocator does per status write —
// computed via SubtractCIDR, which splits every free region into aligned CIDRs
// only to add their sizes back up. Measure answers both from one pass.
func BenchmarkMeasureVsStatusCalls(b *testing.B) {
	for _, n := range []int{250, 1000, 4000} {
		parents, existing := existingAt(n)

		b.Run(fmt.Sprintf("SeparateCalls/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				free := new(big.Int)
				for _, p := range parents {
					for _, c := range SubtractCIDR(p, existing) {
						free.Add(free, addressCount(c))
					}
				}
			}
		})

		b.Run(fmt.Sprintf("Merged/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Measure(parents, existing, Reservation{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
