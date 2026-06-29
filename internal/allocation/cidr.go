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
}

// Allocate returns the next free aligned sub-block of prefixLen bits using
// the pool's strategy.
func (p *CIDRPool) Allocate(prefixLen int) (*net.IPNet, error) {
	return FindFirstAvailableBlock(p.Ranges, p.Existing, prefixLen, p.Strategy)
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

// LargestFreeBlock returns the biggest free contiguous CIDR (smallest prefix
// length) currently available within the pool. Returns ErrPoolExhausted if
// the pool is fully allocated.
func (p *CIDRPool) LargestFreeBlock() (*net.IPNet, error) {
	var best *net.IPNet
	var bestSize *big.Int
	for _, parent := range p.Ranges {
		_, bits := parent.Mask.Size()
		within := filterWithin(parent, p.Existing)
		regions := freeRegions(parent, within)
		for _, region := range regions {
			cidr, ok := largestAlignedBlock(region.start, region.end, bits)
			if !ok {
				continue
			}
			if best == nil || cidrSize(cidr).Cmp(bestSize) > 0 {
				blockCopy := cidr
				best = &blockCopy
				bestSize = cidrSize(cidr)
			}
		}
	}
	if best == nil {
		return nil, ErrPoolExhausted
	}
	return best, nil
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

// FindFirstAvailableBlock locates a free sub-block of prefixLen bits across
// the given parents, honouring the given strategy.
func FindFirstAvailableBlock(parents, existing []net.IPNet, prefixLen int, s Strategy) (*net.IPNet, error) {
	if len(parents) == 0 {
		return nil, ErrNoParent
	}
	if s == "" {
		s = FirstFit
	}

	type candidate struct {
		cidr        net.IPNet
		regionSize  *big.Int // size of the surrounding free region
		parentIndex int
		parentFree  *big.Int // free addresses remaining in parent
	}
	var candidates []candidate

	for idx, parent := range parents {
		ones, bits := parent.Mask.Size()
		if prefixLen < ones || prefixLen > bits {
			// Cannot fit a block this size in (or split this finely from) parent.
			continue
		}
		within := filterWithin(parent, existing)
		regions := freeRegions(parent, within)
		size := blockSize(prefixLen, bits)
		parentFree := new(big.Int)
		for _, r := range regions {
			rs := new(big.Int).Sub(new(big.Int).Add(r.end, big.NewInt(1)), r.start)
			parentFree.Add(parentFree, rs)
		}
		for _, region := range regions {
			start := alignUp(region.start, prefixLen, bits)
			end := new(big.Int).Sub(new(big.Int).Add(start, size), big.NewInt(1))
			if end.Cmp(region.end) > 0 {
				continue
			}
			regionSize := new(big.Int).Sub(new(big.Int).Add(region.end, big.NewInt(1)), region.start)
			candidates = append(candidates, candidate{
				cidr:        makeCIDR(start, prefixLen, bits),
				regionSize:  regionSize,
				parentIndex: idx,
				parentFree:  new(big.Int).Set(parentFree),
			})
			if s == FirstFit {
				// Early exit — first free block is sufficient.
				cidr := candidates[len(candidates)-1].cidr
				return &cidr, nil
			}
		}
	}

	if len(candidates) == 0 {
		return nil, ErrPoolExhausted
	}

	switch s {
	case BestFit:
		best := 0
		for i := 1; i < len(candidates); i++ {
			if candidates[i].regionSize.Cmp(candidates[best].regionSize) < 0 {
				best = i
			}
		}
		c := candidates[best].cidr
		return &c, nil
	case LeastUtilized:
		best := 0
		for i := 1; i < len(candidates); i++ {
			if candidates[i].parentFree.Cmp(candidates[best].parentFree) > 0 {
				best = i
			}
		}
		c := candidates[best].cidr
		return &c, nil
	default:
		c := candidates[0].cidr
		return &c, nil
	}
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

// LargestFreePrefixLen returns the prefix length of the largest free aligned
// block available across parents after removing existing allocations. The
// second return value is false when the pool is fully allocated (no free
// block). The result is family-agnostic and never overflows.
func LargestFreePrefixLen(parents, existing []net.IPNet) (int, bool) {
	pool := &CIDRPool{Ranges: parents, Existing: existing}
	block, err := pool.LargestFreeBlock()
	if err != nil {
		return 0, false
	}
	ones, _ := block.Mask.Size()
	return ones, true
}

// UtilizationPercent returns the allocated share of the parents' total address
// space as an integer in [0, 100], computed with arbitrary-precision
// arithmetic so it is accurate for IPv6 spaces larger than an int64. An empty
// pool reports 0.
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
func freeRegions(parent net.IPNet, within []net.IPNet) []ipRange {
	sorted := make([]net.IPNet, len(within))
	copy(sorted, within)
	sort.Slice(sorted, func(i, j int) bool {
		return cidrFirstAddr(sorted[i]).Cmp(cidrFirstAddr(sorted[j])) < 0
	})

	parentStart := cidrFirstAddr(parent)
	parentEnd := cidrLastAddr(parent)

	cursor := new(big.Int).Set(parentStart)
	var regions []ipRange
	for _, e := range sorted {
		eStart := cidrFirstAddr(e)
		eEnd := cidrLastAddr(e)
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

// largestAlignedBlock returns the largest aligned CIDR that fits inside
// [start,end] (inclusive) for the given address-family bit width. The
// returned CIDR uses the same address family as derived from totalBits.
func largestAlignedBlock(start, end *big.Int, totalBits int) (net.IPNet, bool) {
	if start.Cmp(end) > 0 {
		return net.IPNet{}, false
	}
	// Try smaller prefix lengths (larger blocks) first.
	for p := 0; p <= totalBits; p++ {
		size := blockSize(p, totalBits)
		aligned := alignUp(start, p, totalBits)
		blockEnd := new(big.Int).Sub(new(big.Int).Add(aligned, size), big.NewInt(1))
		if blockEnd.Cmp(end) <= 0 {
			return makeCIDR(aligned, p, totalBits), true
		}
	}
	return net.IPNet{}, false
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
