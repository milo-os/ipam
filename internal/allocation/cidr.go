// Package allocation provides pure-Go CIDR and ASN allocation primitives.
//
// INVARIANT: this package has zero non-stdlib imports. It depends only on
// "net", "math/big", "sort", "errors", and "fmt". Other allocation services
// (VLAN, port, etc.) may import it directly.
package allocation

import (
	"errors"
	"math/big"
	"net"
	"sort"
)

// Strategy selects how a free sub-block is chosen when multiple are available.
type Strategy string

const (
	// FirstFit returns the first free aligned block found while scanning the
	// parent ranges in order.
	FirstFit Strategy = "FirstFit"
	// BestFit returns the candidate whose surrounding free region is smallest,
	// minimising fragmentation.
	BestFit Strategy = "BestFit"
	// LeastUtilized returns a candidate from the parent range with the lowest
	// allocation density, spreading load across parents.
	LeastUtilized Strategy = "LeastUtilized"
)

var (
	// ErrPoolExhausted indicates that no free block of the requested size
	// exists across the configured parent ranges.
	ErrPoolExhausted = errors.New("ipam: pool exhausted")
	// ErrInvalidPrefixLen indicates that the requested prefix length is
	// outside the parent range or otherwise invalid.
	ErrInvalidPrefixLen = errors.New("ipam: invalid prefix length")
	// ErrNoParent indicates that the pool has no parent ranges configured.
	ErrNoParent = errors.New("ipam: no parent ranges configured")
	// ErrNotInPool indicates a Release request for a CIDR that is not tracked.
	ErrNotInPool = errors.New("ipam: cidr not present in pool")
)

// CIDRPool is a snapshot of parent ranges and existing allocations. All
// methods are pure functions — no I/O, no shared state.
type CIDRPool struct {
	Ranges   []net.IPNet
	Existing []net.IPNet
	Strategy Strategy
	// Reservation withholds positions at the edges of every range. The zero
	// value withholds nothing.
	Reservation Reservation
}

// Allocate returns the next free aligned sub-block of prefixLen bits using
// the pool's strategy, skipping any reserved edge positions.
func (p *CIDRPool) Allocate(prefixLen int) (*net.IPNet, error) {
	return FindFirstAvailableBlockWithReservations(p.Ranges, p.Existing, prefixLen, p.Strategy, p.Reservation)
}

// Release returns a copy of the existing allocations with cidr removed.
// The pool itself is not mutated.
func (p *CIDRPool) Release(cidr net.IPNet) ([]net.IPNet, error) {
	out := make([]net.IPNet, 0, len(p.Existing))
	found := false
	for _, e := range p.Existing {
		if cidrEquals(e, cidr) {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return p.Existing, ErrNotInPool
	}
	return out, nil
}

// FragmentationPct reports the proportion of free address space that is split
// across multiple non-contiguous regions. 0.0 means a single contiguous free
// region (or fully allocated); higher values mean more fragmentation.
func (p *CIDRPool) FragmentationPct() float64 {
	var totalFree, largestFree big.Int
	for _, parent := range p.Ranges {
		within := filterWithin(parent, p.Existing)
		regions := freeRegions(parent, within)
		for _, region := range regions {
			size := new(big.Int).Sub(new(big.Int).Add(region.end, big.NewInt(1)), region.start)
			totalFree.Add(&totalFree, size)
			if size.Cmp(&largestFree) > 0 {
				largestFree.Set(size)
			}
		}
	}
	if totalFree.Sign() == 0 {
		return 0.0
	}
	totalF, _ := new(big.Float).SetInt(&totalFree).Float64()
	largestF, _ := new(big.Float).SetInt(&largestFree).Float64()
	if totalF == 0 {
		return 0.0
	}
	return 1.0 - (largestF / totalF)
}

// CIDRsOverlap reports whether two CIDRs share any address.
func CIDRsOverlap(a, b net.IPNet) bool {
	if !sameFamily(a.IP, b.IP) {
		return false
	}
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// SubtractCIDR returns the maximal free regions inside parent after removing
// any CIDRs in existing that fall within parent. Each returned IPNet is an
// aligned sub-CIDR; multi-CIDR holes may yield multiple results.
func SubtractCIDR(parent net.IPNet, existing []net.IPNet) []net.IPNet {
	_, bits := parent.Mask.Size()
	within := filterWithin(parent, existing)
	regions := freeRegions(parent, within)
	var out []net.IPNet
	for _, r := range regions {
		out = append(out, splitRegionIntoAlignedCIDRs(r.start, r.end, bits)...)
	}
	return out
}

// CountAddresses returns the number of addresses in cidr. Capped at MaxInt64
// for very large IPv6 prefixes to avoid overflow.
func CountAddresses(cidr net.IPNet) int64 {
	ones, bits := cidr.Mask.Size()
	hostBits := bits - ones
	if hostBits >= 63 {
		return 1<<63 - 1
	}
	if hostBits < 0 {
		return 0
	}
	return int64(1) << uint(hostBits)
}

// addressCount returns the number of addresses in cidr as an exact big.Int.
func addressCount(cidr net.IPNet) *big.Int {
	ones, bits := cidr.Mask.Size()
	hostBits := bits - ones
	if hostBits < 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Lsh(big.NewInt(1), uint(hostBits))
}

// UtilizationPercent returns the allocated share of the parents' total address
// space as an integer in [0, 100], computed with arbitrary-precision
// arithmetic so it is accurate for IPv6 spaces larger than an int64. An empty
// pool reports 0.
//
// DO NOT USE THIS TO REPORT A POOL'S UTILIZATION. It sums the allocations, so
// an address covered by several of them counts once per allocation and an
// allocation lying outside the parents counts even though it consumes nothing.
// A pool where eight holders legitimately share one address — the same address
// in eight networks — reports eight times its real occupancy. Use Measure, or
// the figure Allocate already returns, both of which derive utilization from
// free space and count each address once.
//
// It survives as the reference implementation the benchmarks measure against,
// and as the thing whose disagreement with Measure is worth a test. It has no
// callers outside this package's own tests, and it should not gain one.
//
// # Do not delete it, and here is what deleting it costs
//
// "No production callers" has already been mistaken for "dead code" once, in a
// task filed to remove this function. Deleting it does not compile — seven test
// call sites depend on it — and two of them are the point:
//
//   - allocate_test.go, TestMeasure_CountsSharedAddressOnce:
//     asserts Measure reports 6% where this reports 50%. That contrast IS the
//     regression proof for the over-reporting defect. Remove the summing
//     implementation and the test degrades to "Measure returns 6", which is
//     equally true of an implementation that returns 6 for the wrong reason.
//   - arch_probe_test.go: the ten-second QEMU discriminator, kept deliberately
//     so nobody re-diagnoses emulated-amd64 heap corruption as an allocator bug.
//
// The rest are its own unit test and two benchmarks. So the honest summary is
// not "dead" but "deliberately unreachable from production and load-bearing in
// tests" — which is what an exported symbol with no production callers looks
// like when it is doing its job.
func UtilizationPercent(parents, existing []net.IPNet) int {
	total := new(big.Int)
	for _, p := range parents {
		total.Add(total, addressCount(p))
	}
	if total.Sign() == 0 {
		return 0
	}
	used := new(big.Int)
	for _, c := range existing {
		used.Add(used, addressCount(c))
	}
	// (used * 100) / total, integer division; clamp to [0, 100].
	pct := new(big.Int).Div(new(big.Int).Mul(used, big.NewInt(100)), total)
	switch {
	case pct.Sign() < 0:
		return 0
	case pct.Cmp(big.NewInt(100)) > 0:
		return 100
	default:
		return int(pct.Int64())
	}
}

// ----------------------------------------------------------------------------
// Internal helpers
// ----------------------------------------------------------------------------

type ipRange struct {
	start *big.Int
	end   *big.Int // inclusive
}

func sameFamily(a, b net.IP) bool {
	a4 := a.To4() != nil
	b4 := b.To4() != nil
	return a4 == b4
}

func ipToInt(ip net.IP) *big.Int {
	if v4 := ip.To4(); v4 != nil {
		return new(big.Int).SetBytes(v4)
	}
	return new(big.Int).SetBytes(ip.To16())
}

func intToIP(i *big.Int, bits int) net.IP {
	size := bits / 8
	out := make(net.IP, size)
	bytes := i.Bytes()
	if len(bytes) > size {
		// Caller error — return all zeros rather than panic.
		return out
	}
	copy(out[size-len(bytes):], bytes)
	return out
}

func blockSize(prefixLen, totalBits int) *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), uint(totalBits-prefixLen))
}

func cidrFirstAddr(cidr net.IPNet) *big.Int {
	return ipToInt(cidr.IP.Mask(cidr.Mask))
}

func cidrLastAddr(cidr net.IPNet) *big.Int {
	ones, bits := cidr.Mask.Size()
	size := blockSize(ones, bits)
	first := cidrFirstAddr(cidr)
	return new(big.Int).Sub(new(big.Int).Add(first, size), big.NewInt(1))
}

// cidrBounds returns the first and last address of cidr, deriving the last
// from the first rather than recomputing it. cidrFirstAddr masks and converts
// the address, so calling both accessors separately does that work twice for
// every allocation in a pool.
func cidrBounds(cidr net.IPNet) (start, end *big.Int) {
	ones, bits := cidr.Mask.Size()
	start = cidrFirstAddr(cidr)
	end = new(big.Int).Add(start, blockSize(ones, bits))
	end.Sub(end, big.NewInt(1))
	return start, end
}

func cidrSize(cidr net.IPNet) *big.Int {
	ones, bits := cidr.Mask.Size()
	return blockSize(ones, bits)
}

func cidrEquals(a, b net.IPNet) bool {
	if !sameFamily(a.IP, b.IP) {
		return false
	}
	ao, ab := a.Mask.Size()
	bo, bb := b.Mask.Size()
	if ao != bo || ab != bb {
		return false
	}
	return cidrFirstAddr(a).Cmp(cidrFirstAddr(b)) == 0
}

// alignUp returns the smallest big.Int >= v that is a multiple of
// 2^(totalBits-prefixLen).
func alignUp(v *big.Int, prefixLen, totalBits int) *big.Int {
	size := blockSize(prefixLen, totalBits)
	rem := new(big.Int).Mod(v, size)
	if rem.Sign() == 0 {
		return new(big.Int).Set(v)
	}
	return new(big.Int).Add(v, new(big.Int).Sub(size, rem))
}

func makeCIDR(start *big.Int, prefixLen, totalBits int) net.IPNet {
	ip := intToIP(start, totalBits)
	return net.IPNet{IP: ip, Mask: net.CIDRMask(prefixLen, totalBits)}
}

// filterWithin returns those CIDRs in cs that are contained in parent and
// share its address family.
func filterWithin(parent net.IPNet, cs []net.IPNet) []net.IPNet {
	var out []net.IPNet
	for _, c := range cs {
		if !sameFamily(parent.IP, c.IP) {
			continue
		}
		// Treat partial overlap or exact containment both as "within".
		if parent.Contains(c.IP) {
			out = append(out, c)
		}
	}
	return out
}

// freeRegions returns the maximal free address ranges inside parent, sorted
// ascending by start.
//
// Each entry's bounds are converted to big.Int once, before the sort, rather
// than inside the comparator. Comparing on the fly re-derived both bounds on
// every comparison, making the conversion cost n·log n instead of n and
// dominating the allocation profile of every caller.
func freeRegions(parent net.IPNet, within []net.IPNet) []ipRange {
	sorted := make([]ipRange, len(within))
	for i, e := range within {
		start, end := cidrBounds(e)
		sorted[i] = ipRange{start: start, end: end}
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].start.Cmp(sorted[j].start) < 0
	})

	parentStart := cidrFirstAddr(parent)
	parentEnd := cidrLastAddr(parent)

	cursor := new(big.Int).Set(parentStart)
	var regions []ipRange
	for _, e := range sorted {
		eStart := e.start
		eEnd := e.end
		if eEnd.Cmp(parentStart) < 0 || eStart.Cmp(parentEnd) > 0 {
			continue
		}
		if eStart.Cmp(cursor) > 0 {
			regions = append(regions, ipRange{
				start: new(big.Int).Set(cursor),
				end:   new(big.Int).Sub(eStart, big.NewInt(1)),
			})
		}
		next := new(big.Int).Add(eEnd, big.NewInt(1))
		if next.Cmp(cursor) > 0 {
			cursor = next
		}
	}
	if cursor.Cmp(parentEnd) <= 0 {
		regions = append(regions, ipRange{
			start: new(big.Int).Set(cursor),
			end:   new(big.Int).Set(parentEnd),
		})
	}
	return regions
}

// splitRegionIntoAlignedCIDRs greedily decomposes [start,end] into the
// smallest number of aligned CIDRs that cover the range exactly.
func splitRegionIntoAlignedCIDRs(start, end *big.Int, totalBits int) []net.IPNet {
	var out []net.IPNet
	cursor := new(big.Int).Set(start)
	for cursor.Cmp(end) <= 0 {
		// Largest prefix (i.e., smallest block) where cursor is aligned and
		// the block fits within [cursor,end].
		bestLen := totalBits
		for p := 0; p <= totalBits; p++ {
			size := blockSize(p, totalBits)
			if new(big.Int).Mod(cursor, size).Sign() != 0 {
				continue
			}
			blockEnd := new(big.Int).Sub(new(big.Int).Add(cursor, size), big.NewInt(1))
			if blockEnd.Cmp(end) <= 0 {
				bestLen = p
				break
			}
		}
		size := blockSize(bestLen, totalBits)
		out = append(out, makeCIDR(cursor, bestLen, totalBits))
		cursor = new(big.Int).Add(cursor, size)
	}
	return out
}
