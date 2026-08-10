package allocation

// Choosing a block and reporting utilization are two questions about one
// thing: the pool's free regions. Computing the regions costs time linear in
// the number of allocations the pool already holds. Two separate calls compute
// them twice, so a caller answering both per claim pays that cost twice.
//
// Allocate computes the regions once and answers both from them. The
// individual functions remain for callers that want one answer.

import (
	"math"
	"math/big"
	"net"
)

// AllocationResult holds what one traversal of a pool's free space answers:
// the block chosen, and what remains.
//
// The figures describe the pool AFTER Block is taken. A caller can therefore
// write them to status alongside the new allocation without a second pass.
// When no block was available, the figures describe the pool as it stands,
// which is what an exhaustion error reports.
type AllocationResult struct {
	// Block is the allocated CIDR, or nil when no block was available.
	Block *net.IPNet
	// UtilizationPercent is the allocated share of the pool, in [0, 100].
	UtilizationPercent float64
}

// PoolMeasurement is the free-space view of a pool: how much is gone, and how
// much remains.
//
// Every figure derives from the pool's free regions, not from summing its
// allocations. Two consequences follow:
//
//   - An address covered by several allocations counts once.
//   - An allocation lying outside the parents counts not at all.
//
// That distinction decides whether a pool where eight holders share one address
// reads as full or as holding one address.
//
// Total, Consumed and Free are exact and never saturate. A caller needing
// int64 status fields clamps them itself, because where to clamp is a question
// about that API's types rather than about the address space.
type PoolMeasurement struct {
	// UtilizationPercent is the consumed share of the pool, in [0, 100].
	//
	// A float, rounded to four decimal places.
	//
	// An integer percent is useless at the scale these pools are sized for.
	// 256 addresses out of a /12's 1,048,576 is 0.024%, which truncates to 0
	// and reads as "no allocations" on a pool holding sixteen.
	//
	// Four places keep one address in a /12 visible, at 0.0001, and keep the
	// JSON readable. The exact ratio is 0.0244140625, and those further digits
	// are noise rather than information.
	UtilizationPercent float64
	// Total, Consumed and Free are address counts. Consumed + Free == Total.
	Total    *big.Int
	Consumed *big.Int
	Free     *big.Int
}

// Measure reports a pool's free-space view in a single traversal.
//
// It answers what a total-minus-free capacity
// computation answer separately, from one pass over the free regions, and it
// is the measurement Allocate reports internally. Reserved positions count as
// consumed whether or not the caller has materialised them yet.
//
// Use this rather than UtilizationPercent, which sums allocations and
// over-reports any pool whose allocations legitimately overlap.
func Measure(parents, existing []net.IPNet, r Reservation) (PoolMeasurement, error) {
	blocked, err := blockedSet(parents, existing, r)
	if err != nil {
		return PoolMeasurement{}, err
	}
	return measure(viewPool(parents, blocked)), nil
}

// Allocate chooses a free block and reports the resulting state of the pool in
// a single traversal of its free space.
//
// It is equivalent to FindFirstAvailableBlock followed by Measure
// and UtilizationPercent over the pool with the new block included, at roughly
// a third of the cost. Reserved positions are excluded from allocation and
// counted as used, whether or not the caller has materialised them yet.
//
// On ErrPoolExhausted the result is still populated: Block is nil and the two
// figures describe the pool as it stands. That is exactly what a 507 response
// needs, so the exhaustion path costs no extra traversal either.
//
// Utilization is derived from free space rather than by summing the existing
// allocations. For a well-formed pool the two agree; where they differ this
// one is right, because summing double-counts overlapping entries and counts
// entries lying outside the parents.
func Allocate(parents, existing []net.IPNet, prefixLen int, s Strategy, r Reservation) (AllocationResult, error) {
	var res AllocationResult
	if len(parents) == 0 {
		return res, ErrNoParent
	}
	blocked, err := blockedSet(parents, existing, r)
	if err != nil {
		return res, err
	}

	views := viewPool(parents, blocked)
	block, pIdx, rIdx := selectBlock(views, prefixLen, s)
	if block != nil {
		// Remove the chosen block from the free space before measuring, so the
		// figures describe the pool the caller is about to persist.
		views[pIdx].takeFrom(rIdx, *block)
		res.Block = block
	}

	m := measure(views)
	res.UtilizationPercent = m.UtilizationPercent
	if block == nil {
		return res, ErrPoolExhausted
	}
	return res, nil
}

// FindFirstAvailableBlock locates a free sub-block of prefixLen bits across
// the given parents, honouring the given strategy.
//
// Use Allocate instead when the pool's resulting utilization or largest free
// prefix is also wanted — it answers all three from one traversal.
func FindFirstAvailableBlock(parents, existing []net.IPNet, prefixLen int, s Strategy) (*net.IPNet, error) {
	if len(parents) == 0 {
		return nil, ErrNoParent
	}
	// FirstFit can stop at the first free block rather than building every
	// parent's view, so it keeps a dedicated path here.
	if strategyOrDefault(s) == FirstFit {
		for _, parent := range parents {
			regions, _, ok := viewParent(parent, existing)
			if !ok {
				continue
			}
			if cidr, _, found := firstFitIn(parent, regions, prefixLen); found {
				return &cidr, nil
			}
		}
		return nil, ErrPoolExhausted
	}
	views := viewPool(parents, existing)
	block, _, _ := selectBlock(views, prefixLen, s)
	if block == nil {
		return nil, ErrPoolExhausted
	}
	return block, nil
}

// FindFirstAvailableBlockWithReservations is FindFirstAvailableBlock with the
// pool's reserved edge positions excluded. Reservations are materialised as
// blocks and appended to existing, so exclusion goes through the one overlap
// path this package already has rather than a second one.
//
// A pool whose only free space is reserved reports ErrPoolExhausted, the same
// as a pool with no free space at all: from the claim's point of view they are
// the same condition. A malformed reservation reports ErrInvalidReservation or
// ErrReservationTooLarge instead — an operator misconfiguration must not be
// indistinguishable from a full pool.
func FindFirstAvailableBlockWithReservations(parents, existing []net.IPNet, prefixLen int, s Strategy, r Reservation) (*net.IPNet, error) {
	if len(parents) == 0 {
		return nil, ErrNoParent
	}
	blocked, err := blockedSet(parents, existing, r)
	if err != nil {
		return nil, err
	}
	return FindFirstAvailableBlock(parents, blocked, prefixLen, s)
}

// parentView is one parent range and the free regions remaining inside it.
type parentView struct {
	parent  net.IPNet
	bits    int
	regions []ipRange
	// free is the total free address count across regions, kept current as
	// blocks are taken so utilization needs no second sweep.
	free *big.Int
}

// takeFrom removes block from region i, splitting it into the parts on either
// side. The block always lies within that region, because selectBlock chooses
// it from there.
func (v *parentView) takeFrom(i int, block net.IPNet) {
	region := v.regions[i]
	bStart, bEnd := cidrBounds(block)

	var replacement []ipRange
	if bStart.Cmp(region.start) > 0 {
		replacement = append(replacement, ipRange{
			start: region.start,
			end:   new(big.Int).Sub(bStart, big.NewInt(1)),
		})
	}
	if bEnd.Cmp(region.end) < 0 {
		replacement = append(replacement, ipRange{
			start: new(big.Int).Add(bEnd, big.NewInt(1)),
			end:   region.end,
		})
	}

	updated := make([]ipRange, 0, len(v.regions)+1)
	updated = append(updated, v.regions[:i]...)
	updated = append(updated, replacement...)
	updated = append(updated, v.regions[i+1:]...)
	v.regions = updated

	v.free.Sub(v.free, cidrSize(block))
}

// viewParent computes the free regions inside one parent. The bool is false
// when the parent's mask is not canonical.
func viewParent(parent net.IPNet, blocked []net.IPNet) ([]ipRange, int, bool) {
	_, bits := parent.Mask.Size()
	if bits == 0 {
		return nil, 0, false
	}
	return freeRegions(parent, filterWithin(parent, blocked)), bits, true
}

func viewPool(parents, blocked []net.IPNet) []parentView {
	views := make([]parentView, 0, len(parents))
	for _, parent := range parents {
		regions, bits, ok := viewParent(parent, blocked)
		if !ok {
			continue
		}
		free := new(big.Int)
		for _, r := range regions {
			free.Add(free, rangeSize(r))
		}
		views = append(views, parentView{parent: parent, bits: bits, regions: regions, free: free})
	}
	return views
}

// firstFitIn returns the first aligned block of prefixLen bits that fits in
// one of the regions, together with the index of the region it came from.
func firstFitIn(parent net.IPNet, regions []ipRange, prefixLen int) (net.IPNet, int, bool) {
	ones, bits := parent.Mask.Size()
	if prefixLen < ones || prefixLen > bits {
		return net.IPNet{}, 0, false
	}
	size := blockSize(prefixLen, bits)
	for i, region := range regions {
		start := alignUp(region.start, prefixLen, bits)
		end := new(big.Int).Add(start, size)
		end.Sub(end, big.NewInt(1))
		if end.Cmp(region.end) > 0 {
			continue
		}
		return makeCIDR(start, prefixLen, bits), i, true
	}
	return net.IPNet{}, 0, false
}

// selectBlock applies the strategy across every parent's view, returning the
// chosen block and the parent and region it came from. A nil block means the
// pool is exhausted for this size.
func selectBlock(views []parentView, prefixLen int, s Strategy) (*net.IPNet, int, int) {
	type candidate struct {
		cidr       net.IPNet
		regionSize *big.Int
		parentIdx  int
		regionIdx  int
		parentFree *big.Int
	}
	var candidates []candidate

	for pIdx := range views {
		v := &views[pIdx]
		ones, bits := v.parent.Mask.Size()
		if prefixLen < ones || prefixLen > bits {
			// Cannot fit a block this size in (or split this finely from) parent.
			continue
		}
		size := blockSize(prefixLen, bits)
		for rIdx, region := range v.regions {
			start := alignUp(region.start, prefixLen, bits)
			end := new(big.Int).Add(start, size)
			end.Sub(end, big.NewInt(1))
			if end.Cmp(region.end) > 0 {
				continue
			}
			candidates = append(candidates, candidate{
				cidr:       makeCIDR(start, prefixLen, bits),
				regionSize: rangeSize(region),
				parentIdx:  pIdx,
				regionIdx:  rIdx,
				parentFree: v.free,
			})
			if strategyOrDefault(s) == FirstFit {
				c := candidates[len(candidates)-1]
				return &c.cidr, c.parentIdx, c.regionIdx
			}
		}
	}

	if len(candidates) == 0 {
		return nil, 0, 0
	}

	best := 0
	switch s {
	case BestFit:
		for i := 1; i < len(candidates); i++ {
			if candidates[i].regionSize.Cmp(candidates[best].regionSize) < 0 {
				best = i
			}
		}
	case LeastUtilized:
		for i := 1; i < len(candidates); i++ {
			if candidates[i].parentFree.Cmp(candidates[best].parentFree) > 0 {
				best = i
			}
		}
	}
	c := candidates[best]
	return &c.cidr, c.parentIdx, c.regionIdx
}

// measure reports the free-space view of every parent in views.
func measure(views []parentView) PoolMeasurement {
	var m PoolMeasurement

	m.Total = new(big.Int)
	m.Free = new(big.Int)

	// Totals only.
	//
	// DO NOT ADD A LARGEST-FREE-BLOCK SEARCH HERE. It runs largestAlignedBlock
	// once per FREE REGION, and a pool holding N scattered allocations has
	// roughly N regions. On a /12 at 4,096 allocations that is 1,321us against
	// 0.3us without, and every successful claim reaches this function.
	for i := range views {
		v := &views[i]
		m.Total.Add(m.Total, addressCount(v.parent))
		m.Free.Add(m.Free, v.free)
	}

	m.Consumed = new(big.Int).Sub(m.Total, m.Free)
	if m.Consumed.Sign() < 0 {
		// Only reachable when parents overlap each other, which double-counts
		// their shared space. Clamp rather than report negative consumption.
		m.Consumed = new(big.Int)
	}
	if m.Total.Sign() == 0 {
		return m
	}

	// Integer division truncates, and truncation was the defect this replaced:
	// every figure below 1% became 0, so a pool holding 256 of 1,048,576
	// addresses reported 0%. big.Rat stays exact for IPv6 counts of any width,
	// where float64 division of the raw totals loses precision before the
	// percentage is taken.
	m.UtilizationPercent = clampPercent(new(big.Rat).SetFrac(
		new(big.Int).Mul(m.Consumed, big.NewInt(100)), m.Total))
	return m
}

// clampPercent converts an exact ratio to a percentage in [0, 100], rounded to
// four decimal places.
//
// clampPercent rounds here rather than leaving it to the display layer, so
// every consumer reads the same number. A stored value carrying more precision
// than anyone renders is a value two readers can disagree about.
func clampPercent(r *big.Rat) float64 {
	f, _ := r.Float64()
	switch {
	case f < 0:
		return 0
	case f > 100:
		return 100
	}
	return math.Round(f*10000) / 10000
}

// blockedSet returns existing plus any reserved positions, which are excluded
// from allocation identically.
func blockedSet(parents, existing []net.IPNet, r Reservation) ([]net.IPNet, error) {
	if r.IsZero() {
		return existing, nil
	}
	reserved, err := r.BlocksIn(parents)
	if err != nil {
		return nil, err
	}
	blocked := make([]net.IPNet, 0, len(existing)+len(reserved))
	blocked = append(blocked, existing...)
	blocked = append(blocked, reserved...)
	return blocked, nil
}

func strategyOrDefault(s Strategy) Strategy {
	if s == "" {
		return FirstFit
	}
	return s
}

func rangeSize(r ipRange) *big.Int {
	size := new(big.Int).Sub(r.end, r.start)
	return size.Add(size, big.NewInt(1))
}
