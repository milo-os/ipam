package allocation

import (
	"errors"
	"math/big"
	"math/rand"
	"net"
	"sort"
	"testing"
)

// runScan drives a Scan the way a database-backed caller would. It returns the
// number of blocks handed over, which is the figure the exercise is about: a
// set-based search examines every block by definition.
func runScan(t *testing.T, s *Scan, all []net.IPNet, page int) (net.IPNet, int, error) {
	t.Helper()
	fed := 0
	for !s.Done() {
		parent, from, ok := s.Need()
		if !ok {
			break
		}
		// Stands in for the caller's query: blocks in this parent's family that
		// end at or above from, ascending. A real caller runs an index range
		// scan with a limit. Filtering the set here keeps this a differential
		// test rather than a second implementation.
		_, bits := parent.Mask.Size()
		fromInt := ipToInt(from)
		var matching []net.IPNet
		for _, b := range all {
			if _, bb := b.Mask.Size(); bb != bits {
				continue
			}
			if _, end := cidrBounds(b); end.Cmp(fromInt) < 0 {
				continue
			}
			matching = append(matching, b)
		}
		sortBlocks(matching)

		delivered := 0
		for start := 0; start < len(matching) && !s.Done(); start += page {
			end := min(start+page, len(matching))
			if err := s.Feed(matching[start:end]); err != nil {
				return net.IPNet{}, fed, err
			}
			fed += end - start
			delivered = end
		}
		_ = delivered
		if !s.Done() {
			s.End()
		}
	}
	block, err := s.Result()
	return block, fed, err
}

func sortBlocks(bs []net.IPNet) {
	sort.Slice(bs, func(i, j int) bool {
		si, _ := cidrBounds(bs[i])
		sj, _ := cidrBounds(bs[j])
		if c := si.Cmp(sj); c != 0 {
			return c < 0
		}
		oi, _ := bs[i].Mask.Size()
		oj, _ := bs[j].Mask.Size()
		return oi < oj // the order PostgreSQL's inet type gives: network, then masklen
	})
}

// The load-bearing test. The scan is worth having only if it is not a second,
// subtly different allocator, so it is checked against FindFirstAvailableBlock
// over randomised pools rather than a table of cases the author thought of. A
// disagreement is not a failure to tune away. The two compute the same
// function.
func TestScanAgreesWithWholeSetSearch(t *testing.T) {
	families := []struct {
		name    string
		parent  string
		minLen  int
		maxLen  int
		askLens []int
	}{
		{"v4", "10.0.0.0/16", 24, 30, []int{24, 26, 28, 30}},
		{"v6", "fd00::/40", 48, 56, []int{48, 52, 56}},
	}

	for _, f := range families {
		_, parent, err := net.ParseCIDR(f.parent)
		if err != nil {
			t.Fatalf("parse %s: %v", f.parent, err)
		}
		parents := []net.IPNet{*parent}

		for seed := range 200 {
			rng := rand.New(rand.NewSource(int64(seed)))
			existing := randomBlocks(t, rng, *parent, f.minLen, f.maxLen, rng.Intn(40))
			ask := f.askLens[rng.Intn(len(f.askLens))]

			want, wantErr := FindFirstAvailableBlock(parents, existing, ask, FirstFit)

			ordered := append([]net.IPNet(nil), existing...)
			sortBlocks(ordered)
			s, err := NewScan(parents, ask, nil)
			if err != nil {
				t.Fatalf("%s seed %d: NewScan: %v", f.name, seed, err)
			}
			got, _, gotErr := runScan(t, s, ordered, 1+rng.Intn(8))

			switch {
			case wantErr != nil && gotErr == nil:
				t.Fatalf("%s seed %d /%d: whole-set said %v, scan returned %v",
					f.name, seed, ask, wantErr, got)
			case wantErr == nil && gotErr != nil:
				t.Fatalf("%s seed %d /%d: whole-set returned %v, scan said %v",
					f.name, seed, ask, want, gotErr)
			case wantErr != nil && gotErr != nil:
				continue
			case got.String() != want.String():
				t.Fatalf("%s seed %d /%d: scan chose %v, whole-set chose %v (existing=%v)",
					f.name, seed, ask, got, want, existing)
			}
		}
	}
}

// Pins the floor's contract in both directions, because the two failure modes
// are not symmetric:
//
//   - Too low costs a longer scan and nothing else.
//   - Too high skips free space silently. No error, no exhaustion, just an
//     address nobody is handed.
//
// The unsafe direction is demonstrated rather than left to be discovered by
// wondering where a /28 went.
func TestScanFromFloorSkipsWhatItIsToldToSkip(t *testing.T) {
	_, parent, _ := net.ParseCIDR("10.0.0.0/24")
	parents := []net.IPNet{*parent}
	// 10.0.0.0/26 taken; 10.0.0.64/26 free; 10.0.0.128/25 taken.
	existing := []net.IPNet{
		mustCIDR(t, "10.0.0.0/26"),
		mustCIDR(t, "10.0.0.128/25"),
	}

	for _, tc := range []struct {
		name  string
		floor string
		want  string
	}{
		{"no floor finds the hole", "", "10.0.0.64/26"},
		{"floor below the hole finds it", "10.0.0.0", "10.0.0.64/26"},
		{"floor at the hole finds it", "10.0.0.64", "10.0.0.64/26"},
		{"floor above the hole misses it, and the pool reads as full", "10.0.0.128", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var floor net.IP
			if tc.floor != "" {
				floor = net.ParseIP(tc.floor)
			}
			s, err := NewScan(parents, 26, floor)
			if err != nil {
				t.Fatalf("NewScan: %v", err)
			}
			got, _, gotErr := runScan(t, s, existing, 4)
			if tc.want == "" {
				if gotErr == nil {
					t.Fatalf("expected exhaustion above the floor, got %v", got)
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("unexpected error: %v", gotErr)
			}
			if got.String() != tc.want {
				t.Fatalf("got %v, want %s", got, tc.want)
			}
		})
	}
}

// Guards the distinction the floor depends on.
//
// A /26 search steps over a free /30. If the scan reported where the /26 landed
// as the next floor, every later search would skip that /30, including the /30
// searches it would have suited exactly. FirstFree must be the first free
// address.
func TestScanFirstFreeIsAnAddressNotABlock(t *testing.T) {
	_, parent, _ := net.ParseCIDR("10.0.0.0/24")
	// 10.0.0.0/25 is taken except for a free /30 at 10.0.0.4, so the first free
	// address is 10.0.0.4 while the first free /26 is 10.0.0.128.
	existing := []net.IPNet{
		mustCIDR(t, "10.0.0.0/30"),
		mustCIDR(t, "10.0.0.8/29"),
		mustCIDR(t, "10.0.0.16/28"),
		mustCIDR(t, "10.0.0.32/27"),
		mustCIDR(t, "10.0.0.64/26"),
	}
	sortBlocks(existing)

	s, err := NewScan([]net.IPNet{*parent}, 26, nil)
	if err != nil {
		t.Fatalf("NewScan: %v", err)
	}
	got, _, gotErr := runScan(t, s, existing, 2)
	if gotErr != nil {
		t.Fatalf("unexpected error: %v", gotErr)
	}
	if got.String() != "10.0.0.128/26" {
		t.Fatalf("chose %v, want 10.0.0.128/26", got)
	}
	if ff := s.FirstFree(); ff == nil || ff.String() != "10.0.0.4" {
		t.Fatalf("FirstFree = %v, want 10.0.0.4 — the first free ADDRESS, not where the /26 landed", ff)
	}
}

// The scan advances a cursor and never looks back, so a block arriving below
// the one before it covers addresses the scan has already stepped over, and
// those addresses would be handed out. Scan must refuse rather than sort:
// sorting hides a caller whose query lost its ORDER BY, and restores the cost
// the scan exists to remove.
func TestScanRejectsOutOfOrderBlocks(t *testing.T) {
	// The request is a /24, so the gap below the first block cannot satisfy it
	// and the scan is still running when the out-of-order block arrives. A
	// request that fits in that gap would decide first and never see the second
	// block, which is correct behaviour but tests nothing here.
	_, parent, _ := net.ParseCIDR("10.0.0.0/23")
	s, err := NewScan([]net.IPNet{*parent}, 24, nil)
	if err != nil {
		t.Fatalf("NewScan: %v", err)
	}
	err = s.Feed([]net.IPNet{
		mustCIDR(t, "10.0.0.128/25"),
		mustCIDR(t, "10.0.0.0/28"),
	})
	if !errors.Is(err, ErrScanOutOfOrder) {
		t.Fatalf("expected ErrScanOutOfOrder, got %v", err)
	}
}

// A caller queries by pool, and a pool with several CIDRs has rows above any
// one of them, so blocks outside the parent under scan are normal input.
// Without the check a stray block reads as "everything up to here is free" and
// the scan proposes a block past the end of the pool. The randomised generator
// never places one, so this is the only test covering it.
func TestScanIgnoresBlocksAboveTheParent(t *testing.T) {
	_, parent, _ := net.ParseCIDR("10.0.0.0/24")
	blocks := []net.IPNet{
		mustCIDR(t, "10.0.0.0/25"),
		mustCIDR(t, "10.0.0.128/25"),
		// Another of the pool's CIDRs, far above this parent.
		mustCIDR(t, "10.9.0.0/28"),
	}

	s, err := NewScan([]net.IPNet{*parent}, 28, nil)
	if err != nil {
		t.Fatalf("NewScan: %v", err)
	}
	got, _, gotErr := runScan(t, s, blocks, 8)
	if gotErr == nil {
		t.Fatalf("parent is fully allocated; scan handed out %v", got)
	}
	if !errors.Is(gotErr, ErrPoolExhausted) {
		t.Fatalf("got %v, want ErrPoolExhausted", gotErr)
	}
}

// The reason the type exists.
//
// A pool holding 2,000 blocks with a hole near the start should decide after
// reading a handful, not after reading all of them. The assertion counts blocks
// fed, which is what a caller's query fetches. It does not measure elapsed
// time, which at this size measures the test harness.
func TestScanExaminesOnlyWhatItNeeds(t *testing.T) {
	_, parent, _ := net.ParseCIDR("10.0.0.0/8")
	parents := []net.IPNet{*parent}

	// 2,000 sequential /28s, with the fourth one missing.
	var existing []net.IPNet
	base := ipToInt(parent.IP)
	step := big.NewInt(16)
	for i := range 2000 {
		if i == 3 {
			continue
		}
		at := new(big.Int).Add(base, new(big.Int).Mul(big.NewInt(int64(i)), step))
		existing = append(existing, makeCIDR(at, 28, 32))
	}
	sortBlocks(existing)

	s, err := NewScan(parents, 28, nil)
	if err != nil {
		t.Fatalf("NewScan: %v", err)
	}
	got, fed, gotErr := runScan(t, s, existing, 16)
	if gotErr != nil {
		t.Fatalf("unexpected error: %v", gotErr)
	}
	if got.String() != "10.0.0.48/28" {
		t.Fatalf("chose %v, want 10.0.0.48/28", got)
	}
	// One page of 16 is the most it should need; the whole-set path reads all
	// 1,999. The bound is on the page size rather than on 4 because a caller
	// hands over whole pages.
	if fed > 16 {
		t.Fatalf("fed %d blocks to decide a hole at position 3; the point of the scan is that this is bounded", fed)
	}
	t.Logf("decided after %d of %d blocks", fed, len(existing))
}

// randomBlocks lays out non-overlapping blocks of random sizes inside parent,
// leaving random gaps. Non-overlapping because that is what the exclusion
// constraint on (pool_key, scope_digest, allocated_cidr) guarantees within one
// address space, which is the population a Scan is ever pointed at.
func randomBlocks(t *testing.T, rng *rand.Rand, parent net.IPNet, minLen, maxLen, n int) []net.IPNet {
	t.Helper()
	_, bits := parent.Mask.Size()
	start, end := cidrBounds(parent)
	cursor := new(big.Int).Set(start)

	var out []net.IPNet
	for range n {
		// A gap, sometimes.
		if rng.Intn(3) == 0 {
			cursor.Add(cursor, big.NewInt(int64(rng.Intn(64)+1)))
		}
		length := minLen + rng.Intn(maxLen-minLen+1)
		at := alignUp(cursor, length, bits)
		last := new(big.Int).Add(at, blockSize(length, bits))
		last.Sub(last, big.NewInt(1))
		if last.Cmp(end) > 0 {
			break
		}
		out = append(out, makeCIDR(at, length, bits))
		cursor = new(big.Int).Add(last, big.NewInt(1))
	}
	return out
}
