package allocation

import (
	"math/big"
	"math/rand"
	"net"
	"testing"
)

// overlappingWith is the set a caller's index lookup would return: every block
// that intersects `block`. The production caller does this with a GiST
// containment query; here it is a filter, which is what makes the test a
// differential one rather than a reimplementation of the thing under test.
func overlappingWith(all []net.IPNet, block net.IPNet) []net.IPNet {
	var out []net.IPNet
	for _, b := range all {
		if CIDRsOverlap(b, block) {
			out = append(out, b)
		}
	}
	return out
}

// TestAddedConsumptionAgreesWithMeasure is the load-bearing test.
//
// It builds a pool one allocation at a time, accumulating deltas from the
// BOUNDED set, and after every step compares the running total against Measure
// over the WHOLE set. If the two ever diverge, the incremental scheme is
// unsound and the pool's utilization would drift with nothing to contradict it.
//
// The population deliberately includes overlapping blocks — nested, identical
// and partial — because non-overlapping allocations make summing sizes correct
// by accident, and a suite built only on those cannot tell the two schemes
// apart. That is the same trap as a fixture set where every claim shares one
// address-space digest.
func TestAddedConsumptionAgreesWithMeasure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent string
		sizes  []int
		bits   int
	}{
		{"v4 mixed sizes", "10.0.0.0/16", []int{24, 26, 28, 30}, 32},
		{"v6 mixed sizes", "fd00::/40", []int{48, 52, 56}, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, parent, err := net.ParseCIDR(tc.parent)
			if err != nil {
				t.Fatalf("parse %s: %v", tc.parent, err)
			}
			parents := []net.IPNet{*parent}
			rng := rand.New(rand.NewSource(11))

			var held []net.IPNet
			running := new(big.Int)

			for step := range 120 {
				block := randomBlockIn(t, rng, *parent, tc.sizes, tc.bits)

				delta, derr := AddedConsumption(parents, overlappingWith(held, block), block)
				if derr != nil {
					t.Fatalf("step %d: AddedConsumption: %v", step, derr)
				}
				running.Add(running, delta)
				held = append(held, block)

				m, merr := Measure(parents, held, Reservation{})
				if merr != nil {
					t.Fatalf("step %d: Measure: %v", step, merr)
				}
				if running.Cmp(m.Consumed) != 0 {
					t.Fatalf("step %d: running total %s, Measure says %s (block %v)",
						step, running, m.Consumed, block)
				}
			}
			t.Logf("%d allocations, consumption tracked exactly: %s addresses", len(held), running)
		})
	}
}

// TestAddedConsumptionCountsSharedSpaceOnce is the case that separates this from
// the obvious implementation.
//
// Two holders of the SAME block is one address gone, not two. Summing sizes —
// which is what anyone writes first, and what UtilizationPercent still does —
// reports double. The assertion is on the second add returning exactly zero.
func TestAddedConsumptionCountsSharedSpaceOnce(t *testing.T) {
	_, parent, _ := net.ParseCIDR("10.128.0.0/20")
	parents := []net.IPNet{*parent}
	shared := mustCIDR(t, "10.128.0.0/28")

	first, err := AddedConsumption(parents, nil, shared)
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if first.Cmp(big.NewInt(16)) != 0 {
		t.Fatalf("first add consumed %s, want 16", first)
	}

	// A second network takes the identical block out of the same shared range.
	second, err := AddedConsumption(parents, []net.IPNet{shared}, shared)
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if second.Sign() != 0 {
		t.Fatalf("second holder of the same /28 consumed %s more addresses; shared space must count once", second)
	}

	// And a block nested inside one already held adds nothing either.
	nested, err := AddedConsumption(parents, []net.IPNet{shared}, mustCIDR(t, "10.128.0.4/30"))
	if err != nil {
		t.Fatalf("nested add: %v", err)
	}
	if nested.Sign() != 0 {
		t.Fatalf("a /30 inside a held /28 consumed %s more addresses", nested)
	}
}

// TestConsumptionRoundTripsToZero guards against drift.
//
// A running total that is nearly right is the failure this scheme risks, and it
// is undetectable in production once the traversal it replaced is gone — there
// would be nothing left to disagree with it. Add then remove must return the
// total to exactly where it started, for every block, whatever else the pool
// holds.
func TestConsumptionRoundTripsToZero(t *testing.T) {
	_, parent, _ := net.ParseCIDR("10.0.0.0/16")
	parents := []net.IPNet{*parent}
	rng := rand.New(rand.NewSource(23))
	sizes := []int{24, 26, 28, 30}

	var held []net.IPNet
	for range 40 {
		held = append(held, randomBlockIn(t, rng, *parent, sizes, 32))
	}

	for i := range 60 {
		block := randomBlockIn(t, rng, *parent, sizes, 32)

		added, err := AddedConsumption(parents, overlappingWith(held, block), block)
		if err != nil {
			t.Fatalf("%d: add: %v", i, err)
		}
		with := append(append([]net.IPNet{}, held...), block)
		removed, err := RemovedConsumption(parents, overlappingWith(held, block), block)
		if err != nil {
			t.Fatalf("%d: remove: %v", i, err)
		}
		_ = with
		if added.Cmp(removed) != 0 {
			t.Fatalf("%d: adding %v consumed %s but removing it frees %s; the total would drift by %s per cycle",
				i, block, added, removed, new(big.Int).Sub(added, removed))
		}
	}
}

// TestConsumptionIgnoresBlocksOutsideThePool matches Measure, which counts only
// what lies inside the parents. A row outside the pool it names is malformed
// data, and it must not inflate the pool's occupancy.
func TestConsumptionIgnoresBlocksOutsideThePool(t *testing.T) {
	_, parent, _ := net.ParseCIDR("10.0.0.0/24")
	parents := []net.IPNet{*parent}

	outside, err := AddedConsumption(parents, nil, mustCIDR(t, "192.0.2.0/28"))
	if err != nil {
		t.Fatalf("outside: %v", err)
	}
	if outside.Sign() != 0 {
		t.Fatalf("a block outside the pool consumed %s addresses of it", outside)
	}
}

// randomBlockIn places an aligned block of a random size anywhere in parent,
// with no attempt to avoid the ones already placed — overlap is the point.
func randomBlockIn(t *testing.T, rng *rand.Rand, parent net.IPNet, sizes []int, bits int) net.IPNet {
	t.Helper()
	length := sizes[rng.Intn(len(sizes))]
	start, end := cidrBounds(parent)
	span := new(big.Int).Sub(end, start)
	if span.Sign() <= 0 {
		return parent
	}
	off := new(big.Int).Rand(rng, span)
	at := alignUp(new(big.Int).Add(start, off), length, bits)
	last := new(big.Int).Add(at, blockSize(length, bits))
	last.Sub(last, big.NewInt(1))
	if last.Cmp(end) > 0 {
		at = alignUp(start, length, bits)
	}
	return makeCIDR(at, length, bits)
}
