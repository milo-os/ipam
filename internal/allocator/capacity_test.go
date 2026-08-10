package allocator

import (
	"math/big"
	"net"
	"testing"

	"go.miloapis.com/ipam/internal/allocation"
)

func subtractForTest(parent net.IPNet, existing []net.IPNet) []net.IPNet {
	return allocation.SubtractCIDR(parent, existing)
}

// The class model's shared-range case, which is what made summing wrong.
//
// A pool serving a class with uniqueWithin: [network] holds one row per network
// per address, and those rows are *meant* to overlap: eight networks each
// holding 10.71.0.0/32 means one address is gone, not eight. Summing reported a
// /28 as 50% full when 1 of its 16 addresses was taken.
func TestCapacityCountsAnOverlappingAddressOnce(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "10.71.0.0/28")}
	var allocations []net.IPNet
	for range 8 {
		allocations = append(allocations, mustParseCIDR(t, "10.71.0.0/32"))
	}

	v := PoolCapacityFor(parents, allocations)

	if v.Total != "16" {
		t.Errorf("total = %s, want 16", v.Total)
	}
	if v.Allocated != "1" {
		t.Errorf("allocated = %s, want 1 (one address, held by eight networks)", v.Allocated)
	}
	if v.Available != "15" {
		t.Errorf("available = %s, want 15", v.Available)
	}
	// 6.25 exactly, not 6: the truncation that hid sub-1% utilization is gone.
	// The summing implementation reported 50 for this same pool.
	if v.UtilizationPercent != 6.25 {
		t.Errorf("utilizationPercent = %g, want 6.25 (1/16); summing reported 50", v.UtilizationPercent)
	}
}

// The three counts and the percentage must be readings of one measurement.
// They were not: largestFreePrefix said /29 while utilizationPercent said 50%
// in the same status block, and the contradiction is how the bug surfaced.
func TestCapacityAgreesWithLargestFreePrefix(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "10.71.0.0/28")}
	allocations := []net.IPNet{mustParseCIDR(t, "10.71.0.0/32")}

	v := PoolCapacityFor(parents, allocations)
	if v.Available != "15" {
		t.Fatalf("available = %s, want 15", v.Available)
	}
	// 15 free addresses cannot contain an aligned /28 or /29; the largest
	// aligned block that fits is a /29 only if 8 contiguous aligned addresses
	// remain, which they do (10.71.0.8/29).
	if got := largestFreePrefixFor(t, parents, allocations); got != 29 {
		t.Fatalf("largestFreePrefix = /%d, want /29", got)
	}
	if v.UtilizationPercent >= 50 {
		t.Errorf("utilizationPercent = %g, contradicts a /29 still being free", v.UtilizationPercent)
	}
}

// Disjoint allocations are the case where summing and free-derivation agree,
// and they must keep agreeing — this is every well-formed pool.
func TestCapacityUnchangedForDisjointAllocations(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "10.0.0.0/20")}
	allocations := []net.IPNet{
		mustParseCIDR(t, "10.0.0.0/24"),
		mustParseCIDR(t, "10.0.1.0/24"),
		mustParseCIDR(t, "10.0.2.0/24"),
		mustParseCIDR(t, "10.0.3.0/24"),
	}

	v := PoolCapacityFor(parents, allocations)
	if v.Total != "4096" || v.Allocated != "1024" || v.Available != "3072" {
		t.Errorf("got total=%s allocated=%s available=%s, want 4096/1024/3072",
			v.Total, v.Allocated, v.Available)
	}
	if v.UtilizationPercent != 25 {
		t.Errorf("utilizationPercent = %g, want 25", v.UtilizationPercent)
	}
}

// An allocation outside the parents describes space this pool never had, so it
// must not consume any. Summing counted it.
func TestCapacityIgnoresAllocationsOutsideTheParents(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "10.0.0.0/24")}
	allocations := []net.IPNet{mustParseCIDR(t, "192.168.5.0/24")}

	v := PoolCapacityFor(parents, allocations)
	if v.Allocated != "0" || v.Available != "256" || v.UtilizationPercent != 0 {
		t.Errorf("got allocated=%s available=%s utilization=%g, want 0/256/0",
			v.Allocated, v.Available, v.UtilizationPercent)
	}
}

// #30's defect — a nearly-empty wide IPv6 pool reading as full — must stay
// fixed, and the counts are now EXACT rather than saturated.
//
// They used to clamp at MaxInt64, so this test asserted total == MaxInt64: a
// ceiling, not a count, with consumed scaled down against it to preserve the
// ratio. Both figures were fictions that happened to divide correctly. The
// literals below are the real sizes — a /20 of IPv6 holds 2^108 addresses and a
// /48 within it holds 2^80 — written out rather than computed, so a bug in the
// arithmetic under test cannot also produce the expectation.
func TestCapacityDoesNotRenderAWideIPv6PoolAsFull(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "fd00::/20")}
	allocations := []net.IPNet{mustParseCIDR(t, "fd00::/48")}

	const (
		wantTotal     = "324518553658426726783156020576256" // 2^108
		wantAllocated = "1208925819614629174706176"         // 2^80
		wantAvailable = "324518552449500907168526845870080" // 2^108 - 2^80
	)

	v := PoolCapacityFor(parents, allocations)
	if v.Total != wantTotal {
		t.Errorf("total = %s, want %s (2^108, exact)", v.Total, wantTotal)
	}
	if v.Allocated != wantAllocated {
		t.Errorf("allocated = %s, want %s (2^80, exact)", v.Allocated, wantAllocated)
	}
	if v.Available != wantAvailable {
		t.Errorf("available = %s, want %s", v.Available, wantAvailable)
	}
	// The invariant every reader assumes, checked on the values as reported
	// rather than on the ones the test expected.
	total, _ := new(big.Int).SetString(v.Total, 10)
	allocated, _ := new(big.Int).SetString(v.Allocated, 10)
	available, _ := new(big.Int).SetString(v.Available, 10)
	if sum := new(big.Int).Add(allocated, available); sum.Cmp(total) != 0 {
		t.Errorf("allocated+available = %s, want total %s", sum, total)
	}
	// 2^80 / 2^108 is 2^-28 — far below the four decimal places the percentage
	// keeps, so it rounds to 0. That is honest: this pool is empty to any
	// precision a human reads.
	if v.UtilizationPercent != 0 {
		t.Errorf("utilizationPercent = %g, want 0 for a single /48 of a /20", v.UtilizationPercent)
	}
}

// A genuinely full pool still reads as full — the fix must not make exhaustion
// invisible, which is the failure mode that would matter most.
func TestCapacityStillReportsAFullPool(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "10.0.0.0/24")}
	allocations := []net.IPNet{mustParseCIDR(t, "10.0.0.0/24")}

	v := PoolCapacityFor(parents, allocations)
	if v.Available != "0" || v.UtilizationPercent != 100 {
		t.Errorf("got available=%s utilization=%g, want 0/100", v.Available, v.UtilizationPercent)
	}
}

func largestFreePrefixFor(t *testing.T, parents, allocations []net.IPNet) int {
	t.Helper()
	best := 0
	found := false
	for _, p := range parents {
		for _, free := range subtractForTest(p, allocations) {
			ones, _ := free.Mask.Size()
			if !found || ones < best {
				best, found = ones, true
			}
		}
	}
	if !found {
		t.Fatal("no free block")
	}
	return best
}
