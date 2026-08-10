package allocation

// First-fit without the whole set.
//
// FindFirstAvailableBlock and Allocate take every allocation in the pool as a
// slice. That is the honest shape for a library with no database under it, and
// it is why allocation cost is quadratic in pool occupancy: the caller loads N
// rows to hand out allocation N+1. Measured — see
// internal/allocator/allocation_scaling_postgres_test.go — a pool at 4,000
// allocations reads 4,050 heap tuples per claim and allocates 10 MB of Go heap
// doing it, both rising in step with occupancy.
//
// Scan inverts the flow. Instead of being given the set, it names the smallest
// span it still needs to know about, consumes blocks in ascending address
// order, and stops as soon as the answer is decided. A caller with an ordered
// index over the allocations can then fetch a page at a time and stop early, so
// the work is proportional to what the search EXAMINES rather than to what
// exists.
//
// It answers exactly what FirstFit answers, and nothing else. BestFit and
// LeastUtilized are defined over the whole pool — "the smallest region that
// fits" and "the emptiest parent" cannot be known from a prefix of the
// allocations — so they keep the set-based path and keep its cost. That is not
// an omission to be fixed later; it is what those strategies mean.
//
// # This is not yet on the hot path, and what it still needs
//
// Nothing in internal/allocator calls Scan. Wiring it is not a matter of
// swapping the call, and the two missing pieces are worth stating here rather
// than in a task nobody will find:
//
//  1. An index ordered by (pool_key, allocated_cidr). The one that exists is
//     (pool_key, scope_digest) INCLUDE (allocated_cidr), and an INCLUDE column
//     cannot order a scan — so today's caller has no way to deliver blocks
//     ascending without sorting them, which means loading them all.
//
//  2. A persisted floor per (pool, address space). WITHOUT ONE, A BOUNDED
//     SEARCH IS STILL LINEAR IN THE COMMON CASE: a pool filled sequentially has
//     its first free block at the end, so a scan from the base examines every
//     allocation to reach it. The scan removes the Go-side cost and leaves the
//     exponent. FirstFree exists to be persisted as that floor.
//
// And a third thing that is not this package's to fix: the capacity recompute
// runs on the same path and measures every allocation in the pool after every
// insert (see writePoolCapacity). Making the search bounded while that stays
// leaves the request quadratic. `consumed` can be maintained incrementally;
// status.largestFreePrefix comes from the same traversal, is user-visible, and
// feeds the 507 body — so that one is a decision, not a refactor.
//
// # Reservations are not a parameter here
//
// FindFirstAvailableBlockWithReservations materialises reserved positions and
// appends them to the set. A Scan's caller is reading rows, and in this service
// reservations ARE rows (purpose='Reservation'), so they arrive through Feed
// like anything else. Adding a Reservation parameter would let a caller that
// already supplies them supply them twice, which is silent: the second copy
// changes no answer, right up until the day reservations stop being
// materialised and the two sources disagree.

import (
	"errors"
	"fmt"
	"math/big"
	"net"
)

// ErrScanClosed is returned when a Scan is fed after it has decided. It is a
// programming error rather than a pool condition.
var ErrScanClosed = errors.New("scan has already decided")

// ErrScanOutOfOrder is returned when a block arrives below the one before it.
//
// The scan's whole advantage rests on ascending order — it advances a cursor
// and never looks back, so a block arriving late is a block it has already
// stepped over, and the address it covers would be handed out. This errors
// rather than sorting: a caller whose query lost its ordering must be told,
// because sorting here would hide it and reintroduce the whole-set cost that
// the scan exists to avoid.
var ErrScanOutOfOrder = errors.New("blocks must arrive in ascending address order")

// Scan is a first-fit search over one pool, driven by the caller.
//
// The cycle is: Need reports the span to fetch, Feed consumes a page of blocks
// in that span, End reports that the span held no more. When Done, Result has
// the answer.
type Scan struct {
	parents   []net.IPNet
	prefixLen int

	pIdx int // index into parents of the span being scanned
	bits int // address width of parents[pIdx]
	// pEnd is the last address of parents[pIdx]. The set-based path gets the
	// equivalent for free from filterWithin; a streaming one has to check it
	// per block, and not checking it hands out a block past the end of the
	// pool when a row sits above the parent.
	pEnd *big.Int

	// cursor is the lowest address in parents[pIdx] not yet known to be taken.
	// Every block fed so far lies below it.
	cursor *big.Int
	// last is the start of the most recent block fed, for the ordering check.
	last *big.Int

	// firstFree is the lowest free address at or above the starting floor,
	// across every parent examined. It is what a caller caches as the next
	// run's floor.
	firstFree      *big.Int
	firstFreeBits  int
	firstFreeFound bool

	floor *big.Int // the caller's starting hint, nil for "from the beginning"

	result *net.IPNet
	done   bool
}

// NewScan starts a first-fit search for a block of prefixLen bits.
//
// floor is an optional hint: the caller's belief that no free address exists
// below it. A floor that is too LOW only costs a longer scan, so nil and a
// stale value are both safe. A floor that is too HIGH silently skips free
// space, which is why nothing in this package computes one — the caller owns
// that claim and the invariant behind it.
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

// Need reports the span the caller must supply blocks for: every allocation
// that ends at or above `from` and starts at or below the end of `parent`, in
// ascending order.
//
// ok is false when the scan has decided and needs nothing further.
func (s *Scan) Need() (parent net.IPNet, from net.IP, ok bool) {
	if s.done {
		return net.IPNet{}, nil, false
	}
	return s.parents[s.pIdx], intToIP(s.cursor, s.bits), true
}

// Feed consumes blocks for the current span, in ascending address order.
//
// It returns as soon as the answer is decided, so a caller holding a page it
// has not finished handing over should check Done rather than assuming the
// whole page was wanted. Blocks below the cursor are skipped rather than
// rejected: a block nested inside one already stepped over is legitimate and
// still ascending.
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
		// Another address family cannot constrain this parent. Ignored rather
		// than rejected so a caller need not split its query by family.
		return nil
	}

	start, end := cidrBounds(block)
	if s.last != nil && start.Cmp(s.last) < 0 {
		return fmt.Errorf("%w: %v starts below the previous block", ErrScanOutOfOrder, block)
	}
	s.last = start

	// Blocks starting above the parent constrain nothing — the same rule
	// filterWithin applies on the set-based path. A caller's query is by pool,
	// and a pool's rows are not guaranteed to lie inside the parent it is being
	// searched against: a pool with several CIDRs has rows above each of them.
	//
	// This return is also what bounds every fit to the parent. tryFit's upper
	// limit is either this block's start minus one or, from End, the parent's
	// last address — so with stray blocks turned away here, no fit can be
	// proposed past the end of the parent, and tryFit needs no clamp of its own.
	// One was written and removed: deleting it changed no test, because this
	// return had already made it unreachable.
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

// End reports that the current span holds no more blocks at or above the
// cursor. The scan either fits a block in the tail of this parent or moves to
// the next one.
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

// FirstFree is the lowest free address the scan saw at or above its floor, or
// nil when it saw none.
//
// This is the value worth caching as the next search's floor. It is the first
// free ADDRESS, not the first address a block of this size fits at: a /28
// search steps over a free /30, and recording where the /28 landed would skip
// that /30 for every later search, including the ones it would have suited.
// That distinction is the whole reason this is a separate accessor rather than
// being derived from Result.
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

// advanceParent moves to the next parent whose range is not entirely below the
// floor, and finishes the scan when there is none.
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
			// Non-canonical mask; the set-based path skips these too.
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
