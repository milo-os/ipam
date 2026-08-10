package allocation

// Reservations withhold positions at the edges of a pool. A pool reserves
// `leading` blocks at the start of its lowest parent range and `trailing`
// blocks at the end of its highest, each block being one unit of
// unitPrefixLen bits. Runs spill across parents when one range cannot supply
// the whole count.
//
// The blocks this file returns are exactly the blocks the allocator would
// otherwise hand out at those positions — same size, same alignment — so a
// caller can persist them as ordinary allocations and let the existing
// overlap logic exclude them. Nothing here knows what a pool, a class, or a
// scope is; it takes CIDRs and integers and returns CIDRs.
//
// Results are deterministic: parents are ordered by address internally, so
// the same inputs yield the same blocks in the same order regardless of the
// order the caller supplies its ranges in.

import (
	"errors"
	"math/big"
	"net"
	"sort"
)

var (
	// ErrInvalidReservation indicates a negative reservation count, a unit
	// prefix length that cannot divide every parent, or parents that mix
	// address families or overlap each other.
	ErrInvalidReservation = errors.New("ipam: invalid reservation")
	// ErrReservationTooLarge indicates that leading+trailing exceeds the number
	// of units the parents contain.
	ErrReservationTooLarge = errors.New("ipam: reservation exceeds parent capacity")
)

// Reservation describes edge positions a pool withholds from allocation,
// counted in units of UnitPrefixLength bits.
//
// The zero value reserves nothing.
//
// Leading and Trailing are counts of blocks that will be materialised, so
// callers accepting them from an API must bound them; this package validates
// them against the parents' capacity, but a large IPv6 parent has capacity for
// more blocks than any process can hold.
type Reservation struct {
	// UnitPrefixLength is the prefix length of one reserved block. It must be
	// at least as long as every parent's own prefix length.
	UnitPrefixLength int
	// Leading is the number of units withheld at the start of the pool.
	Leading int
	// Trailing is the number of units withheld at the end of the pool.
	Trailing int
}

// IsZero reports whether the reservation withholds nothing. A zero-valued
// Reservation is a no-op: it produces no blocks and never errors, regardless
// of UnitPrefixLength.
func (r Reservation) IsZero() bool {
	return r.Leading == 0 && r.Trailing == 0
}

// BlocksIn returns the concrete blocks this reservation withholds from parents.
func (r Reservation) BlocksIn(parents []net.IPNet) ([]net.IPNet, error) {
	return ReservedBlocks(parents, r.Leading, r.Trailing, r.UnitPrefixLength)
}

// ReservedBlocks expands a pool's leading/trailing reservation into concrete
// blocks, counted in units of unitPrefixLen, so the caller can materialise
// each as a real allocation held by the pool.
//
// `leading` counts from the start of the lowest parent and `trailing` from the
// end of the highest, both by address, spilling into the next range when one
// cannot supply the whole count. Results are ascending by address: the leading
// blocks first, then the trailing ones. The function is deterministic — the
// same inputs always produce the same blocks, whatever order `parents` arrives
// in — so a caller may materialise them once at pool-provision time and
// recompute them later for comparison.
//
// With leading and trailing both zero the result is nil and nothing else is
// validated: reserving nothing is always a no-op.
//
// Errors:
//   - ErrNoParent if parents is empty.
//   - ErrInvalidReservation if leading or trailing is negative; if
//     unitPrefixLen is shorter than any parent's prefix (a unit wider than a
//     range is not a position in it) or wider than the address family; or if
//     parents mix address families or overlap one another.
//   - ErrReservationTooLarge if leading+trailing exceeds the number of units
//     the parents contain in total.
func ReservedBlocks(parents []net.IPNet, leading, trailing, unitPrefixLen int) ([]net.IPNet, error) {
	if leading < 0 || trailing < 0 {
		return nil, ErrInvalidReservation
	}
	if leading == 0 && trailing == 0 {
		return nil, nil
	}
	if len(parents) == 0 {
		return nil, ErrNoParent
	}

	sorted, bits, err := sortedDisjointParents(parents)
	if err != nil {
		return nil, err
	}

	// Every parent must be able to hold at least one whole unit. A range too
	// small to do so is a misconfiguration: skipping it silently would make
	// "leading" count positions the allocator does not agree exist.
	if unitPrefixLen > bits {
		return nil, ErrInvalidReservation
	}
	unitCounts := make([]*big.Int, len(sorted))
	total := new(big.Int)
	for i, p := range sorted {
		ones, _ := p.Mask.Size()
		if unitPrefixLen < ones {
			return nil, ErrInvalidReservation
		}
		unitCounts[i] = new(big.Int).Lsh(big.NewInt(1), uint(unitPrefixLen-ones))
		total.Add(total, unitCounts[i])
	}

	want := new(big.Int).Add(big.NewInt(int64(leading)), big.NewInt(int64(trailing)))
	if want.Cmp(total) > 0 {
		return nil, ErrReservationTooLarge
	}

	unitSize := blockSize(unitPrefixLen, bits)
	out := make([]net.IPNet, 0, leading+trailing)

	// Leading: walk parents low to high, taking units from the front of each
	// until the count is met.
	remaining := leading
	for i := 0; i < len(sorted) && remaining > 0; i++ {
		take := unitsToTake(remaining, unitCounts[i])
		cursor := cidrFirstAddr(sorted[i])
		for n := 0; n < take; n++ {
			out = append(out, makeCIDR(cursor, unitPrefixLen, bits))
			cursor = new(big.Int).Add(cursor, unitSize)
		}
		remaining -= take
	}

	// Trailing: walk parents high to low, taking units from the back of each.
	// Collected descending, then reversed so the whole result is ascending.
	var tail []net.IPNet
	remaining = trailing
	for i := len(sorted) - 1; i >= 0 && remaining > 0; i-- {
		take := unitsToTake(remaining, unitCounts[i])
		// Start `take` units back from one past this parent's last address.
		onePast := new(big.Int).Add(cidrLastAddr(sorted[i]), big.NewInt(1))
		cursor := new(big.Int).Sub(onePast, unitSize)
		for n := 0; n < take; n++ {
			tail = append(tail, makeCIDR(cursor, unitPrefixLen, bits))
			cursor = new(big.Int).Sub(cursor, unitSize)
		}
		remaining -= take
	}
	for i := len(tail) - 1; i >= 0; i-- {
		out = append(out, tail[i])
	}

	return out, nil
}

// unitsToTake returns how many of `want` units can come from a range holding
// `available` of them. `available` is a big.Int because an IPv6 range can hold
// more units than an int can express; `want` never exceeds an int, so the
// result is safely narrowed.
func unitsToTake(want int, available *big.Int) int {
	if available.Cmp(big.NewInt(int64(want))) >= 0 {
		return want
	}
	return int(available.Int64())
}

// sortedDisjointParents returns parents ordered ascending by address, together
// with the address family's bit width. It rejects a mixed-family or
// self-overlapping set: both make "the start of the pool" ambiguous and would
// let a reservation and an allocation disagree about what a position is.
func sortedDisjointParents(parents []net.IPNet) ([]net.IPNet, int, error) {
	sorted := make([]net.IPNet, len(parents))
	copy(sorted, parents)

	_, bits := sorted[0].Mask.Size()
	if bits == 0 {
		// Mask.Size reports (0,0) for a non-canonical mask.
		return nil, 0, ErrInvalidReservation
	}
	for _, p := range sorted[1:] {
		if _, b := p.Mask.Size(); b != bits {
			return nil, 0, ErrInvalidReservation
		}
		if !sameFamily(sorted[0].IP, p.IP) {
			return nil, 0, ErrInvalidReservation
		}
	}

	sort.Slice(sorted, func(i, j int) bool {
		return cidrFirstAddr(sorted[i]).Cmp(cidrFirstAddr(sorted[j])) < 0
	})
	for i := 1; i < len(sorted); i++ {
		if CIDRsOverlap(sorted[i-1], sorted[i]) {
			return nil, 0, ErrInvalidReservation
		}
	}
	return sorted, bits, nil
}

// IsBlockAvailable reports whether the exact block `want` fits inside one of
// the parents and overlaps nothing in existing. This is the specific-address
// path — a claim naming an address rather than a size, which cannot go through
// FindFirstAvailableBlock.
//
// `want` must be contained whole: a block straddling the edge of a parent is
// unavailable even though its first address is inside one. Reserved positions
// are ordinary members of existing, so a claim naming a reserved address is
// refused by the same check that refuses an allocated one.
func IsBlockAvailable(parents, existing []net.IPNet, want net.IPNet) bool {
	contained := false
	for _, p := range parents {
		if cidrContains(p, want) {
			contained = true
			break
		}
	}
	if !contained {
		return false
	}
	for _, e := range existing {
		if CIDRsOverlap(want, e) {
			return false
		}
	}
	return true
}

// cidrContains reports whether child lies entirely within parent. Unlike
// net.IPNet.Contains, which tests a single address, this compares both ends so
// a block overhanging the parent's last address is not mistaken for one inside
// it.
func cidrContains(parent, child net.IPNet) bool {
	if !sameFamily(parent.IP, child.IP) {
		return false
	}
	return cidrFirstAddr(child).Cmp(cidrFirstAddr(parent)) >= 0 &&
		cidrLastAddr(child).Cmp(cidrLastAddr(parent)) <= 0
}
