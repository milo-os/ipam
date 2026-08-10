package allocation

import (
	"fmt"
	"net"
	"testing"
)

func existingAt(n int) ([]net.IPNet, []net.IPNet) {
	_, parent, _ := net.ParseCIDR("fd00::/20")
	parents := []net.IPNet{*parent}
	var existing []net.IPNet
	for range n {
		got, err := FindFirstAvailableBlock(parents, existing, 48, FirstFit)
		if err != nil {
			panic(err)
		}
		existing = append(existing, *got)
	}
	return parents, existing
}

// Where the per-allocation cost goes, and how each part scales with pool
// occupancy. This is the instrument that characterised #36.
//
// Every pass here is linear in the number of allocations already in the pool
// (16x n produces ~14-16x cost), so performing them once per allocation makes
// the total quadratic in pool occupancy. Measured at n=4000:
//
//	FindFirstAvailableBlock   1.75 ms   3.76 MB   49%
//	UtilizationPercent        0.17 ms   0.38 MB    5%
//
// The result worth acting on is the middle row: **computing the status field
// costs as much as choosing the address.** More than half the Go-side work per
// allocation is bookkeeping, and all three passes recompute the same free
// regions independently — so a single traversal answering all three would
// remove roughly half the remaining cost without changing any behaviour.
func BenchmarkPerAllocationWork(b *testing.B) {
	for _, n := range []int{250, 1000, 4000} {
		parents, existing := existingAt(n)

		b.Run(fmt.Sprintf("FindFirstAvailableBlock/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = FindFirstAvailableBlock(parents, existing, 48, FirstFit)
			}
		})
		b.Run(fmt.Sprintf("UtilizationPercent/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = UtilizationPercent(parents, existing)
			}
		})
	}
}
