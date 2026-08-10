package allocation

// Table-driven tests for internal/allocation. Companion to allocation_test.go;
// kept stdlib-only per the package invariant.

import (
	"errors"
	"net"
	"testing"
)

// ----------------------------------------------------------------------------
// FindFirstAvailableBlock — comprehensive table cases
// ----------------------------------------------------------------------------

func TestFindFirstAvailableBlock_Table(t *testing.T) {
	type tc struct {
		name     string
		parents  []string
		existing []string
		prefix   int
		strategy Strategy
		want     string // expected CIDR string; "" means expect error
		wantErr  error
	}

	cases := []tc{
		// Prefix-length walk /20..../28 from an empty /16 — confirms the
		// allocator scales across the typical operator request range.
		{name: "FirstFit_empty_/20", parents: []string{"10.0.0.0/16"}, prefix: 20, strategy: FirstFit, want: "10.0.0.0/20"},
		{name: "FirstFit_empty_/22", parents: []string{"10.0.0.0/16"}, prefix: 22, strategy: FirstFit, want: "10.0.0.0/22"},
		{name: "FirstFit_empty_/24", parents: []string{"10.0.0.0/16"}, prefix: 24, strategy: FirstFit, want: "10.0.0.0/24"},
		{name: "FirstFit_empty_/26", parents: []string{"10.0.0.0/16"}, prefix: 26, strategy: FirstFit, want: "10.0.0.0/26"},
		{name: "FirstFit_empty_/28", parents: []string{"10.0.0.0/16"}, prefix: 28, strategy: FirstFit, want: "10.0.0.0/28"},

		// Same-size as parent — single allocation consumes the whole pool.
		{name: "FirstFit_same_size_as_parent", parents: []string{"10.0.0.0/24"}, prefix: 24, strategy: FirstFit, want: "10.0.0.0/24"},

		// Same-size taken — pool exhausted.
		{name: "FirstFit_same_size_taken", parents: []string{"10.0.0.0/24"}, existing: []string{"10.0.0.0/24"}, prefix: 24, strategy: FirstFit, wantErr: ErrPoolExhausted},

		// Larger than parent — cannot fit.
		{name: "FirstFit_larger_than_parent", parents: []string{"10.0.0.0/24"}, prefix: 16, strategy: FirstFit, wantErr: ErrPoolExhausted},

		// Non-contiguous existing allocations leave aligned holes.
		{
			name:     "FirstFit_non_contiguous_picks_first_hole",
			parents:  []string{"10.0.0.0/22"}, // /22 = 4 /24s
			existing: []string{"10.0.0.0/24", "10.0.2.0/24"},
			prefix:   24, strategy: FirstFit, want: "10.0.1.0/24",
		},

		// BestFit: tightest hole wins. Two holes: /24 and /22.
		{
			name:    "BestFit_picks_smallest_hole",
			parents: []string{"10.0.0.0/16"},
			existing: []string{
				"10.0.0.0/24",
				"10.0.2.0/23",
				"10.0.8.0/21",
			},
			prefix: 24, strategy: BestFit, want: "10.0.1.0/24",
		},

		// BestFit with no fitting region.
		{
			name:     "BestFit_exhausted",
			parents:  []string{"10.0.0.0/29"},
			existing: []string{"10.0.0.0/30", "10.0.0.4/30"},
			prefix:   30, strategy: BestFit, wantErr: ErrPoolExhausted,
		},

		// LeastUtilized across two /16 parents — the empty parent must win.
		{
			name:     "LeastUtilized_picks_empty_parent",
			parents:  []string{"10.0.0.0/16", "10.1.0.0/16"},
			existing: []string{"10.0.0.0/24"},
			prefix:   24, strategy: LeastUtilized, want: "10.1.0.0/24",
		},

		// Default (empty) strategy falls back to FirstFit semantics.
		{
			name:     "Default_strategy_acts_as_first_fit",
			parents:  []string{"10.0.0.0/24"},
			existing: []string{"10.0.0.0/26"},
			prefix:   26, strategy: "", want: "10.0.0.64/26",
		},

		// IPv6 — basic and large pool.
		{name: "IPv6_/48_in_/32", parents: []string{"2001:db8::/32"}, prefix: 48, strategy: FirstFit, want: "2001:db8::/48"},
		{
			name:     "IPv6_/64_after_/48",
			parents:  []string{"2001:db8::/32"},
			existing: []string{"2001:db8::/48"},
			prefix:   64, strategy: FirstFit, want: "2001:db8:1::/64",
		},
		// IPv6 /32 holds 2^96 addresses — verifies the math/big path doesn't
		// overflow int64 anywhere along the way.
		{
			name:    "IPv6_large_pool_/32_subdivide_/40",
			parents: []string{"2001:db8::/32"},
			prefix:  40, strategy: FirstFit, want: "2001:db8::/40",
		},

		// Multi-parent FirstFit: first parent has space, no need to walk to
		// second.
		{
			name:    "FirstFit_multi_parent_picks_first",
			parents: []string{"10.0.0.0/24", "10.1.0.0/24"},
			prefix:  25, strategy: FirstFit, want: "10.0.0.0/25",
		},

		// Multi-parent FirstFit: first parent full → must spill to next.
		{
			name:     "FirstFit_multi_parent_spills_when_first_full",
			parents:  []string{"10.0.0.0/29", "10.1.0.0/24"},
			existing: []string{"10.0.0.0/30", "10.0.0.4/30"},
			prefix:   25, strategy: FirstFit, want: "10.1.0.0/25",
		},

		// No parents.
		{name: "no_parents", parents: nil, prefix: 24, strategy: FirstFit, wantErr: ErrNoParent},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parents := parseCIDRs(t, c.parents)
			existing := parseCIDRs(t, c.existing)
			got, err := FindFirstAvailableBlock(parents, existing, c.prefix, c.strategy)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cidrStr(*got) != c.want {
				t.Fatalf("got %s, want %s", cidrStr(*got), c.want)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Overlap / containment table
// ----------------------------------------------------------------------------

func TestCIDRsOverlap_Table(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"adjacent_/25_pair_does_not_overlap", "10.0.0.0/25", "10.0.0.128/25", false},
		{"adjacent_/24_pair_does_not_overlap", "10.0.0.0/24", "10.0.1.0/24", false},
		{"exact_duplicate_overlaps", "10.0.0.0/24", "10.0.0.0/24", true},
		{"superset_contains_subset", "10.0.0.0/16", "10.0.5.0/24", true},
		{"subset_contained_by_superset", "10.0.5.0/24", "10.0.0.0/16", true},
		{"split_/25_inside_/24_overlaps", "10.0.0.0/24", "10.0.0.128/25", true},
		{"family_mismatch_v4_v6", "10.0.0.0/24", "2001:db8::/32", false},
		{"family_mismatch_v6_v4", "2001:db8::/64", "10.0.0.0/24", false},
		{"ipv6_adjacent_no_overlap", "2001:db8::/49", "2001:db8:0:8000::/49", false},
		{"ipv6_exact_duplicate_overlaps", "2001:db8::/48", "2001:db8::/48", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CIDRsOverlap(mustCIDR(t, c.a), mustCIDR(t, c.b))
			if got != c.want {
				t.Fatalf("CIDRsOverlap(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// CountAddresses — including the int64-saturation branch
// ----------------------------------------------------------------------------

func TestCountAddresses_Table(t *testing.T) {
	cases := []struct {
		name string
		cidr string
		want int64
	}{
		{"v4_/32_one_addr", "10.0.0.0/32", 1},
		{"v4_/30_four_addrs", "10.0.0.0/30", 4},
		{"v4_/24_256_addrs", "10.0.0.0/24", 256},
		{"v4_/16_65k_addrs", "10.0.0.0/16", 65536},
		{"v6_/128_one_addr", "2001:db8::/128", 1},
		{"v6_/126_four_addrs", "2001:db8::/126", 4},
		{"v6_/64_2pow64_saturates_to_maxint64", "2001:db8::/64", 1<<63 - 1},
		{"v6_/0_saturates_to_maxint64", "::/0", 1<<63 - 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CountAddresses(mustCIDR(t, c.cidr))
			if got != c.want {
				t.Fatalf("CountAddresses(%s) = %d, want %d", c.cidr, got, c.want)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// SubtractCIDR — directly exercises splitRegionIntoAlignedCIDRs
// ----------------------------------------------------------------------------

func TestSubtractCIDR_Table(t *testing.T) {
	cases := []struct {
		name     string
		parent   string
		existing []string
		want     []string // unordered set of expected free CIDRs
	}{
		{
			name:   "empty_parent_returns_self",
			parent: "10.0.0.0/24",
			want:   []string{"10.0.0.0/24"},
		},
		{
			name:     "fully_allocated_returns_nothing",
			parent:   "10.0.0.0/24",
			existing: []string{"10.0.0.0/24"},
			want:     nil,
		},
		{
			name:     "single_/25_carve_yields_other_/25",
			parent:   "10.0.0.0/24",
			existing: []string{"10.0.0.0/25"},
			want:     []string{"10.0.0.128/25"},
		},
		{
			name:     "centered_carve_yields_aligned_split",
			parent:   "10.0.0.0/24",
			existing: []string{"10.0.0.64/26"},
			want:     []string{"10.0.0.0/26", "10.0.0.128/25"},
		},
		{
			name:     "ignores_v6_existing_in_v4_parent",
			parent:   "10.0.0.0/24",
			existing: []string{"2001:db8::/48"},
			want:     []string{"10.0.0.0/24"},
		},
		{
			name:     "ipv6_simple_/49_carve",
			parent:   "2001:db8::/48",
			existing: []string{"2001:db8::/49"},
			want:     []string{"2001:db8:0:8000::/49"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SubtractCIDR(mustCIDR(t, c.parent), parseCIDRs(t, c.existing))
			gotSet := map[string]bool{}
			for _, g := range got {
				gotSet[cidrStr(g)] = true
			}
			wantSet := map[string]bool{}
			for _, w := range c.want {
				wantSet[w] = true
			}
			if len(gotSet) != len(wantSet) {
				t.Fatalf("len mismatch: got %v want %v", gotSet, wantSet)
			}
			for w := range wantSet {
				if !gotSet[w] {
					t.Fatalf("missing expected CIDR %s in %v", w, gotSet)
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// CIDRPool — wrapper API + Largest/Fragmentation
// ----------------------------------------------------------------------------

func TestCIDRPool_Allocate_DelegatesToFinder(t *testing.T) {
	cases := []struct {
		name     string
		ranges   []string
		existing []string
		strategy Strategy
		prefix   int
		want     string
		wantErr  error
	}{
		{
			name:   "first_fit_default",
			ranges: []string{"10.0.0.0/24"},
			prefix: 25,
			want:   "10.0.0.0/25",
		},
		{
			name:     "best_fit_routes_through_pool",
			ranges:   []string{"10.0.0.0/16"},
			existing: []string{"10.0.0.0/24", "10.0.2.0/23", "10.0.8.0/21"},
			strategy: BestFit,
			prefix:   24,
			want:     "10.0.1.0/24",
		},
		{
			name:     "exhausted_propagates_error",
			ranges:   []string{"10.0.0.0/30"},
			existing: []string{"10.0.0.0/30"},
			strategy: FirstFit,
			prefix:   30,
			wantErr:  ErrPoolExhausted,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &CIDRPool{
				Ranges:   parseCIDRs(t, c.ranges),
				Existing: parseCIDRs(t, c.existing),
				Strategy: c.strategy,
			}
			got, err := p.Allocate(c.prefix)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cidrStr(*got) != c.want {
				t.Fatalf("got %s, want %s", cidrStr(*got), c.want)
			}
		})
	}
}

func TestCIDRPool_LargestFreeBlock_Table(t *testing.T) {
	cases := []struct {
		name     string
		ranges   []string
		existing []string
		want     string
		wantErr  error
	}{
		{
			name:   "empty_pool_returns_parent",
			ranges: []string{"10.0.0.0/24"},
			want:   "10.0.0.0/24",
		},
		{
			name:     "half_used_returns_other_half",
			ranges:   []string{"10.0.0.0/24"},
			existing: []string{"10.0.0.0/25"},
			want:     "10.0.0.128/25",
		},
		{
			name:     "fragmented_returns_largest_aligned_block",
			ranges:   []string{"10.0.0.0/24"},
			existing: []string{"10.0.0.0/26", "10.0.0.128/26"},
			// Free regions: 10.0.0.64/26 and 10.0.0.192/26 — both /26.
			want: "10.0.0.64/26",
		},
		{
			name:     "fully_allocated",
			ranges:   []string{"10.0.0.0/30"},
			existing: []string{"10.0.0.0/30"},
			wantErr:  ErrPoolExhausted,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &CIDRPool{
				Ranges:   parseCIDRs(t, c.ranges),
				Existing: parseCIDRs(t, c.existing),
			}
			got, err := p.LargestFreeBlock()
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cidrStr(*got) != c.want {
				t.Fatalf("got %s, want %s", cidrStr(*got), c.want)
			}
		})
	}
}

func TestCIDRPool_FragmentationPct_Table(t *testing.T) {
	cases := []struct {
		name     string
		ranges   []string
		existing []string
		// Fragmentation is 1 - largestFree/totalFree. We assert direction
		// (zero, positive) rather than exact float to keep the test stable.
		wantZero bool
		wantGT0  bool
	}{
		{
			name:     "empty_pool_is_unfragmented",
			ranges:   []string{"10.0.0.0/24"},
			wantZero: true,
		},
		{
			name:     "single_carve_is_unfragmented",
			ranges:   []string{"10.0.0.0/24"},
			existing: []string{"10.0.0.0/25"},
			wantZero: true,
		},
		{
			name:     "fully_allocated_returns_zero",
			ranges:   []string{"10.0.0.0/30"},
			existing: []string{"10.0.0.0/30"},
			wantZero: true,
		},
		{
			name:     "two_holes_is_fragmented",
			ranges:   []string{"10.0.0.0/24"},
			existing: []string{"10.0.0.64/26", "10.0.0.192/26"},
			wantGT0:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &CIDRPool{
				Ranges:   parseCIDRs(t, c.ranges),
				Existing: parseCIDRs(t, c.existing),
			}
			got := p.FragmentationPct()
			if c.wantZero && got != 0.0 {
				t.Fatalf("expected 0 fragmentation, got %f", got)
			}
			if c.wantGT0 && !(got > 0.0) {
				t.Fatalf("expected >0 fragmentation, got %f", got)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Helpers used only by this file
// ----------------------------------------------------------------------------

func parseCIDRs(t *testing.T, ss []string) []net.IPNet {
	t.Helper()
	if len(ss) == 0 {
		return nil
	}
	out := make([]net.IPNet, 0, len(ss))
	for _, s := range ss {
		out = append(out, mustCIDR(t, s))
	}
	return out
}
