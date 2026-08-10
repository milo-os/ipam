package allocation

// Consumption, maintained one block at a time instead of measured over all of
// them.
//
// Measure answers "how much of this pool is gone" by walking every allocation
// and subtracting the free regions from the parents. The answer is exact, and
// it costs one full scan per claim: the caller loads the whole set to update
// one number. The instrument in
// internal/allocator/allocation_scaling_postgres_test.go reads that cost
// directly. Heap tuples per allocation come out as occupancy + 51 at every
// checkpoint.
//
// A DELTA NEEDS ONLY THE ALLOCATIONS THAT OVERLAP THE BLOCK BEING ADDED OR
// REMOVED. Every other allocation contributes the same amount before and
// after, so it cancels. A bounded index lookup therefore suffices where a full
// scan was required.
//
// # Why a delta is exact here, and summing sizes is not
//
// The obvious version, consumed += size(block), fails for the same reason
// UtilizationPercent fails, and in the same direction.
//
// The class model lets allocations overlap legitimately. Two networks may hold
// one address out of a shared range, which is one address with two holders
// rather than two addresses. Summing counts that address twice, so a /28
// shared by eight networks reads as eight times its real occupancy.
//
// # This is not the whole of the change it exists for
//
// Nothing calls it yet. Taking the traversal off the write path also needs
// somewhere to persist the running total, and the removal of
// status.largestFreePrefix — the one figure a delta cannot maintain, and
// therefore the reason the rest is possible at all (#99). This is the
// arithmetic, landed first and separately, because a wrong number here is a
// silently wrong utilization on every pool in the service and nothing would
// contradict it.

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
// Extra entries are harmless. A caller fetching a slightly wider set is
// therefore safe, and a caller fetching a narrower one over-counts.
//
// That requirement belongs to this function's contract, not to any one call
// site, which is why it appears here.
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
// It is deliberately the same computation as AddedConsumption rather than an
// inverse of it. Add-then-remove therefore returns the total exactly to where
// it started, for any block and any pool — asserted by
// TestConsumptionRoundTripsToZero. A maintained total that drifts is the
// characteristic failure of this approach, and it would surface as utilization
// slowly diverging from reality with nothing left to compare it against.
func RemovedConsumption(parents, overlapping []net.IPNet, block net.IPNet) (*big.Int, error) {
	return consumptionDelta(parents, overlapping, block)
}

// consumptionDelta is the difference the block makes to the pool's consumption,
// measured over the bounded set.
//
// consumptionDelta takes the difference of two Measures rather than computing
// a union by hand. That choice is worth keeping.
//
// Measure's free-region walk already computes the union of overlapping CIDRs
// against the parents correctly, including the awkward cases:
//
//   - blocks nested inside one another
//   - blocks straddling a parent boundary
//   - blocks lying outside the pool entirely
//
// A second implementation of that union is a second thing to get wrong, and it
// would be
// wrong only in the rare shapes, which is where nobody looks.
//
// The cost is two walks over `overlapping`, not over the pool. For a pool with
// one address space that set is at most a handful of rows; for a shared pool it
// is the number of spaces holding that exact block.
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
