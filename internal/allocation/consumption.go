package allocation

// Consumption maintained one block at a time, rather than measured over all of
// them.
//
// Measure walks every allocation in the pool to answer how much is gone, so a
// caller updating one number loads the whole set. A delta needs only the
// allocations that OVERLAP the block being added or removed: every other
// allocation contributes the same amount before and after, so it cancels. A
// bounded index lookup replaces a full scan.
//
// # Why a delta and not consumed += size(block)
//
// Allocations may legitimately overlap. Two networks can hold one address out
// of a shared range, which is one address with two holders rather than two
// addresses. Summing counts it twice, so a /28 shared by eight networks reads
// as eight times its real occupancy.
//
// # Not wired yet
//
// Taking the traversal off the write path also needs somewhere to persist the
// running total. status.largestFreePrefix was the one figure a delta cannot
// maintain, and removing it is what makes the rest possible.

import (
	"math/big"
	"net"
)

// AddedConsumption reports how many addresses become consumed when block is
// added to a pool that already holds `overlapping`.
//
// parents are the pool's ranges.
//
// `overlapping` must contain EVERY existing allocation that intersects block.
// Extra entries are harmless, so a caller fetching a wider set is safe and a
// caller fetching a narrower one over-counts.
func AddedConsumption(parents, overlapping []net.IPNet, block net.IPNet) (*big.Int, error) {
	return consumptionDelta(parents, overlapping, block)
}

// RemovedConsumption reports how many addresses stop being consumed when block
// is removed from a pool whose remaining allocations are `overlapping`.
//
// It takes the same set with the released block itself excluded: the addresses
// freed are those it covered that nothing else still covers. A retained
// allocation is still held, so this must not be called for one.
//
// Deliberately the same computation as AddedConsumption rather than an inverse
// of it, so add-then-remove returns the total exactly to where it started for
// any block and any pool. TestConsumptionRoundTripsToZero asserts that. Drift
// is the characteristic failure of a maintained total, and it surfaces as
// utilization diverging from reality with nothing left to compare against.
func RemovedConsumption(parents, overlapping []net.IPNet, block net.IPNet) (*big.Int, error) {
	return consumptionDelta(parents, overlapping, block)
}

// consumptionDelta is the difference the block makes to the pool's consumption,
// measured over the bounded set.
//
// It takes the difference of two Measures rather than computing a union by
// hand. Measure's free-region walk already handles blocks nested inside one
// another, blocks straddling a parent boundary, and blocks outside the pool. A
// second implementation would be wrong only in those rare shapes.
//
// The cost is two walks over `overlapping`, not over the pool.
func consumptionDelta(parents, overlapping []net.IPNet, block net.IPNet) (*big.Int, error) {
	before, err := Measure(parents, overlapping, Reservation{})
	if err != nil {
		return nil, err
	}
	after, err := Measure(parents, append(append([]net.IPNet{}, overlapping...), block), Reservation{})
	if err != nil {
		return nil, err
	}
	delta := new(big.Int).Sub(after.Consumed, before.Consumed)
	if delta.Sign() < 0 {
		// Not reachable: adding a block cannot reduce consumption.
		//
		// Clamped rather than returned negative. A negative delta applied to a
		// running total corrupts that total permanently. A pool reading
		// slightly empty is recoverable; a poisoned total is not.
		return new(big.Int), nil
	}
	return delta, nil
}
