package allocation

import (
	"errors"
	"fmt"
	"math/big"
	"net"
)

// ErrScanClosed is returned when a Scan is fed after it has decided.
var ErrScanClosed = errors.New("scan has already decided")

// ErrScanOutOfOrder is returned when a block arrives below the one before it.
// Scan has already stepped past the address that block covers. Scan reports the
// error instead of sorting, because sorting would hide the unordered query and
// restore the whole-set cost.
var ErrScanOutOfOrder = errors.New("blocks must arrive in ascending address order")

// Scan is a first-fit search over one pool. The caller runs the loop and
// supplies allocations from its own storage:
//
//	for !s.Done() {
//		parent, from, ok := s.Need()
//		if !ok {
//			break
//		}
//		s.Feed(allocationsIn(parent, from))
//		s.End()
//	}
//	block, err := s.Result()
//
// The work scales with what the search examines rather than with what the pool
// holds, so a caller reads one page at a time and stops early.
//
// Scan answers what FirstFit answers, and nothing else. BestFit and
// LeastUtilized are defined over the whole pool, so no prefix of the
// allocations determines their answer.
//
// Reservations arrive through Feed like any other block, because this service
// stores them as rows.
type Scan struct {
	parents   []net.IPNet
	prefixLen int

	pIdx int // index into parents of the one being scanned
	bits int // address width of parents[pIdx]
	// pEnd is the last address of parents[pIdx]. filterWithin bounds the
	// set-based path; a streaming path must check each block against pEnd
	// instead, or it hands out a block past the end of the pool whenever a row
	// sits above the parent.
	pEnd *big.Int

	// cursor is the lowest address in parents[pIdx] not yet known to be taken.
	// Every block fed so far lies below it.
	cursor *big.Int
	last   *big.Int // start of the most recent block fed, for the ordering check

	// firstFree is the lowest free address at or above the floor, across every
	// parent examined.
	firstFree      *big.Int
	firstFreeBits  int
	firstFreeFound bool

	floor *big.Int // the caller's starting hint, nil for "from the beginning"

	result *net.IPNet
	done   bool
}

// NewScan starts a first-fit search for a block of prefixLen bits.
//
// floor is an optional hint that no free address exists below it:
//
//   - Too low costs a longer scan, so nil and a stale value are both safe.
//   - Too high silently skips free space.
//
// Nothing in this package computes a floor. The caller owns that claim and the
// invariant behind it.
func NewScan(parents []net.IPNet, prefixLen int, floor net.IP) (*Scan, error) {
	if len(parents) == 0 {
		return nil, ErrNoParent
	}
	s := &Scan{
		parents:   parents,
		prefixLen: prefixLen,
		pIdx:      -1,
	}
	if floor != nil {
		s.floor = ipToInt(floor)
	}
	s.advanceParent()
	return s, nil
}

// Need returns the parent to query and the address to start at. Supply every
// allocation that ends at or above from and starts at or below the end of
// parent, in ascending address order.
//
// ok is false once the scan has decided and needs nothing further.
func (s *Scan) Need() (parent net.IPNet, from net.IP, ok bool) {
	if s.done {
		return net.IPNet{}, nil, false
	}
	return s.parents[s.pIdx], intToIP(s.cursor, s.bits), true
}

// Feed consumes allocations from the parent Need named, in ascending address
// order.
//
// Feed returns as soon as the answer is decided, which may be part-way through
// the page. Check Done rather than assuming Scan wanted every block.
//
// Scan skips blocks below the cursor instead of rejecting them. A block nested
// inside one it has already stepped over is legitimate and still ascending.
func (s *Scan) Feed(blocks []net.IPNet) error {
	if s.done {
		return ErrScanClosed
	}
	for i := range blocks {
		if s.done {
			return nil
		}
		if err := s.feedOne(blocks[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scan) feedOne(block net.IPNet) error {
	_, bits := block.Mask.Size()
	if bits == 0 {
		return fmt.Errorf("%w: block %v has no canonical mask", ErrScanOutOfOrder, block)
	}
	if bits != s.bits {
		// A block of another address family cannot constrain this parent. Scan
		// ignores it rather than rejecting it, so a caller need not split its
		// query by family.
		return nil
	}

	start, end := cidrBounds(block)
	if s.last != nil && start.Cmp(s.last) < 0 {
		return fmt.Errorf("%w: %v starts below the previous block", ErrScanOutOfOrder, block)
	}
	s.last = start

	// A block above the parent constrains nothing. Callers query by pool, and a
	// pool with several CIDRs holds rows above each of them. This return also
	// bounds every fit to the parent, so tryFit needs no clamp of its own.
	if start.Cmp(s.pEnd) > 0 {
		return nil
	}
	if end.Cmp(s.cursor) < 0 {
		// Entirely below the cursor: already accounted for.
		return nil
	}

	if start.Cmp(s.cursor) > 0 {
		// The addresses in [cursor, start) are free.
		s.noteFirstFree(s.cursor)
		if s.tryFit(s.cursor, new(big.Int).Sub(start, big.NewInt(1))) {
			return nil
		}
	}
	next := new(big.Int).Add(end, big.NewInt(1))
	if next.Cmp(s.cursor) > 0 {
		s.cursor = next
	}
	return nil
}

// End reports that the query returned no more allocations. Scan then fits a
// block in whatever is left of this parent, or moves to the next one.
func (s *Scan) End() {
	if s.done {
		return
	}
	_, parentEnd := cidrBounds(s.parents[s.pIdx])
	if s.cursor.Cmp(parentEnd) <= 0 {
		s.noteFirstFree(s.cursor)
		if s.tryFit(s.cursor, parentEnd) {
			return
		}
	}
	s.advanceParent()
}

// Done reports whether the scan has an answer.
func (s *Scan) Done() bool { return s.done }

// Result returns the chosen block, or ErrPoolExhausted when no parent had room
// for one of this size.
func (s *Scan) Result() (net.IPNet, error) {
	if !s.done {
		return net.IPNet{}, errors.New("scan is not finished; feed until Done")
	}
	if s.result == nil {
		return net.IPNet{}, ErrPoolExhausted
	}
	return *s.result, nil
}

// FirstFree returns the lowest free address the scan saw at or above its floor,
// or nil if it saw none. A caller caches this value as the next search's floor.
//
// The result is an address, not the address at which a block of this size fits.
// A /28 search steps over a free /30, and recording where the /28 landed would
// skip that /30 for every later search.
func (s *Scan) FirstFree() net.IP {
	if !s.firstFreeFound {
		return nil
	}
	return intToIP(s.firstFree, s.firstFreeBits)
}

// tryFit places a block of the requested size in [lo, hi] if one fits, and
// finishes the scan when it does.
func (s *Scan) tryFit(lo, hi *big.Int) bool {
	ones, _ := s.parents[s.pIdx].Mask.Size()
	if s.prefixLen < ones || s.prefixLen > s.bits {
		return false
	}
	start := alignUp(lo, s.prefixLen, s.bits)
	end := new(big.Int).Add(start, blockSize(s.prefixLen, s.bits))
	end.Sub(end, big.NewInt(1))
	if end.Cmp(hi) > 0 {
		return false
	}
	block := makeCIDR(start, s.prefixLen, s.bits)
	s.result = &block
	s.done = true
	return true
}

func (s *Scan) noteFirstFree(addr *big.Int) {
	if s.firstFreeFound {
		return
	}
	s.firstFreeFound = true
	s.firstFree = new(big.Int).Set(addr)
	s.firstFreeBits = s.bits
}

// advanceParent moves to the next parent not entirely below the floor.
func (s *Scan) advanceParent() {
	for {
		s.pIdx++
		if s.pIdx >= len(s.parents) {
			s.done = true
			return
		}
		start, end := cidrBounds(s.parents[s.pIdx])
		_, bits := s.parents[s.pIdx].Mask.Size()
		if bits == 0 {
			// A non-canonical mask, which the set-based path also skips.
			continue
		}
		if s.floor != nil {
			if s.floor.Cmp(end) > 0 {
				continue // entirely below the floor
			}
			if s.floor.Cmp(start) > 0 {
				start = new(big.Int).Set(s.floor)
			}
		}
		s.bits = bits
		s.cursor = start
		s.pEnd = end
		s.last = nil
		return
	}
}
