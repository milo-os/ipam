package allocation

// Table-driven tests for reservation math. Companion to allocation_test.go;
// kept stdlib-only per the package invariant.

import (
	"errors"
	"net"
	"testing"
)

func mustCIDRs(t *testing.T, ss []string) []net.IPNet {
	t.Helper()
	var out []net.IPNet
	for _, s := range ss {
		out = append(out, mustCIDR(t, s))
	}
	return out
}

func cidrStrs(ns []net.IPNet) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, cidrStr(n))
	}
	return out
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReservedBlocks_Table(t *testing.T) {
	type tc struct {
		name     string
		parents  []string
		unit     int
		leading  int
		trailing int
		want     []string
		wantErr  error
	}

	cases := []tc{
		// The worked example from the enhancement doc: a subnet whose first
		// block holds the gateway and the all-zeros address.
		{
			name: "leading_one_v6_endpoint", parents: []string{"fd20:a1b:2c3d:1::/64"}, unit: 96,
			leading: 1,
			want:    []string{"fd20:a1b:2c3d:1::/96"},
		},
		{
			name: "leading_two_trailing_two_v4", parents: []string{"10.128.0.0/20"}, unit: 32,
			leading: 2, trailing: 2,
			want: []string{"10.128.0.0/32", "10.128.0.1/32", "10.128.15.254/32", "10.128.15.255/32"},
		},
		{
			name: "trailing_only", parents: []string{"10.0.0.0/24"}, unit: 26,
			trailing: 1,
			want:     []string{"10.0.0.192/26"},
		},
		{
			name: "unit_equal_to_parent_reserves_whole_parent", parents: []string{"10.0.0.0/24"}, unit: 24,
			leading: 1,
			want:    []string{"10.0.0.0/24"},
		},

		// Zero reservations are a clean no-op — no blocks, no validation, even
		// with inputs that would otherwise be rejected.
		{name: "zero_is_noop", parents: []string{"10.0.0.0/24"}, unit: 32, want: nil},
		{name: "zero_is_noop_even_with_bad_unit", parents: []string{"10.0.0.0/24"}, unit: 8, want: nil},
		{name: "zero_is_noop_with_unset_unit", parents: []string{"10.0.0.0/24"}, unit: 0, want: nil},
		{name: "zero_is_noop_with_no_parents", unit: 32, want: nil},

		{name: "no_parents", unit: 32, leading: 1, wantErr: ErrNoParent},

		// ---- multi-parent -------------------------------------------------
		// Leading counts from the lowest parent, trailing from the highest.
		{
			name: "multi_parent_leading_and_trailing", parents: []string{"10.0.0.0/24", "10.0.1.0/24"}, unit: 26,
			leading: 1, trailing: 1,
			want: []string{"10.0.0.0/26", "10.0.1.192/26"},
		},
		// Supplied out of address order — the answer must not change.
		{
			name: "multi_parent_out_of_order", parents: []string{"10.0.1.0/24", "10.0.0.0/24"}, unit: 26,
			leading: 1, trailing: 1,
			want: []string{"10.0.0.0/26", "10.0.1.192/26"},
		},
		// Non-contiguous ranges: still one pool, still one pair of edges.
		{
			name: "multi_parent_non_contiguous", parents: []string{"10.0.0.0/24", "192.168.7.0/24"}, unit: 26,
			leading: 1, trailing: 1,
			want: []string{"10.0.0.0/26", "192.168.7.192/26"},
		},
		// A run spills into the next range when one cannot supply the count.
		{
			name: "multi_parent_leading_spills", parents: []string{"10.0.0.0/30", "10.0.1.0/30"}, unit: 32,
			leading: 6,
			want: []string{
				"10.0.0.0/32", "10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32",
				"10.0.1.0/32", "10.0.1.1/32",
			},
		},
		{
			name: "multi_parent_trailing_spills", parents: []string{"10.0.0.0/30", "10.0.1.0/30"}, unit: 32,
			trailing: 6,
			want: []string{
				"10.0.0.2/32", "10.0.0.3/32",
				"10.0.1.0/32", "10.0.1.1/32", "10.0.1.2/32", "10.0.1.3/32",
			},
		},
		// Capacity is the sum across ranges, and the two runs may meet exactly.
		{
			name: "multi_parent_exact_capacity", parents: []string{"10.0.0.0/31", "10.0.1.0/31"}, unit: 32,
			leading: 2, trailing: 2,
			want: []string{"10.0.0.0/32", "10.0.0.1/32", "10.0.1.0/32", "10.0.1.1/32"},
		},
		{
			name: "multi_parent_over_capacity", parents: []string{"10.0.0.0/31", "10.0.1.0/31"}, unit: 32,
			leading: 3, trailing: 2, wantErr: ErrReservationTooLarge,
		},
		// Differing-size ranges are fine as long as each holds a whole unit.
		{
			name: "multi_parent_differing_sizes", parents: []string{"10.0.0.0/28", "10.1.0.0/16"}, unit: 30,
			leading: 1, trailing: 1,
			want: []string{"10.0.0.0/30", "10.1.255.252/30"},
		},
		// ...but a range too small to hold one unit is a misconfiguration, not
		// a range to skip silently.
		{
			name: "multi_parent_range_smaller_than_unit", parents: []string{"10.0.0.0/28", "10.1.0.0/16"}, unit: 26,
			leading: 1, wantErr: ErrInvalidReservation,
		},
		// Overlapping ranges make "the start of the pool" ambiguous.
		{
			name: "multi_parent_overlapping", parents: []string{"10.0.0.0/24", "10.0.0.0/25"}, unit: 26,
			leading: 1, wantErr: ErrInvalidReservation,
		},
		// Mixed families cannot be ordered against each other.
		{
			name: "multi_parent_mixed_families", parents: []string{"10.0.0.0/24", "fd00::/64"}, unit: 96,
			leading: 1, wantErr: ErrInvalidReservation,
		},

		// The whole pool may be reserved, exactly.
		{
			name: "exact_capacity_all_leading", parents: []string{"10.0.0.0/30"}, unit: 32,
			leading: 4,
			want:    []string{"10.0.0.0/32", "10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"},
		},
		{
			name: "exact_capacity_split", parents: []string{"10.0.0.0/30"}, unit: 32,
			leading: 2, trailing: 2,
			want: []string{"10.0.0.0/32", "10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"},
		},

		// One past capacity — the runs would overlap, so it is rejected rather
		// than silently deduplicated.
		{
			name: "over_capacity", parents: []string{"10.0.0.0/30"}, unit: 32,
			leading: 3, trailing: 2, wantErr: ErrReservationTooLarge,
		},
		{
			name: "over_capacity_leading_alone", parents: []string{"10.0.0.0/24"}, unit: 26,
			leading: 5, wantErr: ErrReservationTooLarge,
		},

		// A unit wider than the parent is not a position in the parent. Error
		// rather than clamp: clamping would hand back a block outside the
		// parent, or silently reserve more than asked.
		{
			name: "unit_wider_than_parent", parents: []string{"10.0.0.0/24"}, unit: 16,
			leading: 1, wantErr: ErrInvalidReservation,
		},
		{
			name: "unit_wider_than_parent_v6", parents: []string{"fd00::/64"}, unit: 48,
			leading: 1, wantErr: ErrInvalidReservation,
		},
		{
			name: "unit_beyond_family_width", parents: []string{"10.0.0.0/24"}, unit: 33,
			leading: 1, wantErr: ErrInvalidReservation,
		},
		{
			name: "unit_beyond_family_width_v6", parents: []string{"fd00::/64"}, unit: 129,
			leading: 1, wantErr: ErrInvalidReservation,
		},
		{
			name: "negative_leading", parents: []string{"10.0.0.0/24"}, unit: 32,
			leading: -1, wantErr: ErrInvalidReservation,
		},
		{
			name: "negative_trailing", parents: []string{"10.0.0.0/24"}, unit: 32,
			trailing: -1, wantErr: ErrInvalidReservation,
		},

		// IPv6 spaces wider than an int64: a /8 of /128 units holds 2^120
		// positions. The capacity check must not narrow that to an int.
		{
			name: "v6_huge_unit_count_leading", parents: []string{"2001:db8::/8"}, unit: 128,
			leading: 2,
			want:    []string{"2000::/128", "2000::1/128"},
		},
		{
			name: "v6_huge_unit_count_trailing", parents: []string{"2000::/8"}, unit: 128,
			trailing: 1,
			want:     []string{"20ff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/128"},
		},
		{
			name: "v6_full_width_parent", parents: []string{"::/0"}, unit: 128,
			leading: 1, trailing: 1,
			want: []string{"::/128", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/128"},
		},
		{
			name: "v4_full_width_parent", parents: []string{"0.0.0.0/0"}, unit: 32,
			leading: 1, trailing: 1,
			want: []string{"0.0.0.0/32", "255.255.255.255/32"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parents := mustCIDRs(t, c.parents)
			got, err := ReservedBlocks(parents, c.leading, c.trailing, c.unit)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("expected %v, got %v (blocks %v)", c.wantErr, err, cidrStrs(got))
				}
				if got != nil {
					t.Fatalf("expected no blocks on error, got %v", cidrStrs(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !equalStrs(cidrStrs(got), c.want) {
				t.Fatalf("expected %v, got %v", c.want, cidrStrs(got))
			}
		})
	}
}

// The registry materialises reserved blocks once at pool-provision time and
// must get the same set from any later call. Determinism has to survive the
// caller reordering its ranges, since nothing guarantees a stable order out of
// the database.
func TestReservedBlocks_DeterministicAcrossParentOrder(t *testing.T) {
	orders := [][]string{
		{"10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/24"},
		{"10.0.2.0/24", "10.0.0.0/24", "10.0.1.0/24"},
		{"10.0.1.0/24", "10.0.2.0/24", "10.0.0.0/24"},
		{"10.0.2.0/24", "10.0.1.0/24", "10.0.0.0/24"},
	}
	want := []string{"10.0.0.0/26", "10.0.0.64/26", "10.0.2.192/26"}

	for _, order := range orders {
		got, err := ReservedBlocks(mustCIDRs(t, order), 2, 1, 26)
		if err != nil {
			t.Fatalf("%v: unexpected err: %v", order, err)
		}
		if !equalStrs(cidrStrs(got), want) {
			t.Fatalf("%v: expected %v, got %v", order, want, cidrStrs(got))
		}
	}

	// Repeated calls with identical inputs are identical.
	parents := mustCIDRs(t, orders[0])
	first, err := ReservedBlocks(parents, 2, 1, 26)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := ReservedBlocks(parents, 2, 1, 26)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !equalStrs(cidrStrs(again), cidrStrs(first)) {
			t.Fatalf("call %d differed: %v vs %v", i, cidrStrs(again), cidrStrs(first))
		}
	}

	// The caller's slice must come back untouched — sorting happens on a copy.
	if cidrStr(parents[0]) != "10.0.0.0/24" || cidrStr(parents[2]) != "10.0.2.0/24" {
		t.Fatalf("caller's parents were reordered: %v", cidrStrs(parents))
	}
}

// A reserved block must be exactly the block the allocator would otherwise
// hand out at that position — same size, same alignment. If the two disagreed,
// persisting a reservation would not actually block the allocation it targets.
func TestReservedBlocks_MatchAllocatorOutput(t *testing.T) {
	cases := []struct {
		parents []string
		unit    int
	}{
		{[]string{"10.0.0.0/16"}, 24},
		{[]string{"10.0.0.0/24"}, 32},
		{[]string{"192.168.4.0/22"}, 30},
		{[]string{"fd20:a1b:2c3d:1::/64"}, 96},
		{[]string{"fd00::/48"}, 64},
		// Multi-parent, ascending — the order FirstFit scans in.
		{[]string{"10.0.0.0/30", "10.0.1.0/24"}, 32},
	}

	for _, c := range cases {
		t.Run(c.parents[0], func(t *testing.T) {
			parents := mustCIDRs(t, c.parents)
			reserved, err := ReservedBlocks(parents, 6, 0, c.unit)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			// Allocate unit-sized blocks from an empty pool; they must be the
			// same blocks in the same order.
			var existing []net.IPNet
			for i, want := range reserved {
				got, err := FindFirstAvailableBlock(parents, existing, c.unit, FirstFit)
				if err != nil {
					t.Fatalf("allocation %d: %v", i, err)
				}
				if cidrStr(*got) != cidrStr(want) {
					t.Fatalf("allocation %d: allocator gave %s, reservation gave %s", i, cidrStr(*got), cidrStr(want))
				}
				existing = append(existing, *got)
			}
		})
	}
}

func TestIsBlockAvailable_Table(t *testing.T) {
	type tc struct {
		name     string
		parents  []string
		existing []string
		want     string
		expect   bool
	}

	cases := []tc{
		{name: "empty_pool", parents: []string{"10.0.0.0/16"}, want: "10.0.1.0/24", expect: true},
		{name: "no_overlap", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.2.0/24", "10.0.3.0/24"}, want: "10.0.1.0/24", expect: true},
		{name: "exact_match_taken", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.1.0/24"}, want: "10.0.1.0/24", expect: false},
		{name: "inside_existing", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.0.0/20"}, want: "10.0.1.128/25", expect: false},
		{name: "contains_existing", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.1.128/25"}, want: "10.0.0.0/20", expect: false},
		{name: "adjacent_is_available", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.1.0/24"}, want: "10.0.2.0/24", expect: true},
		{name: "later_entry_collides", parents: []string{"10.0.0.0/16"}, existing: []string{"10.0.1.0/24", "10.0.5.0/25"}, want: "10.0.5.0/24", expect: false},

		// Containment. A block outside every parent is not available however
		// empty the pool is — this is the check a claim naming an arbitrary
		// address most needs.
		{name: "outside_parent", parents: []string{"10.0.0.0/16"}, want: "10.1.0.0/24", expect: false},
		{name: "no_parents_at_all", want: "10.0.1.0/24", expect: false},
		{name: "equal_to_parent", parents: []string{"10.0.0.0/24"}, want: "10.0.0.0/24", expect: true},
		// Wider than the parent: its first address is inside, its last is not.
		// net.IPNet.Contains(want.IP) would wrongly accept this, which is why
		// containment compares both ends. (A block *narrower* than the parent
		// is always either contained or disjoint, never straddling, because
		// both are aligned.)
		{name: "wider_than_parent", parents: []string{"10.0.0.0/24"}, want: "10.0.0.0/16", expect: false},
		{name: "wider_than_parent_offset", parents: []string{"10.0.1.0/24"}, want: "10.0.0.0/16", expect: false},

		// Multi-parent: containment in any one range is enough.
		{name: "second_parent", parents: []string{"10.0.0.0/24", "192.168.0.0/24"}, want: "192.168.0.128/25", expect: true},
		{name: "between_parents", parents: []string{"10.0.0.0/24", "192.168.0.0/24"}, want: "172.16.0.0/24", expect: false},

		// Cross-family.
		{name: "v6_available", parents: []string{"fd00::/48"}, want: "fd00::/64", expect: true},
		{name: "v6_taken", parents: []string{"fd00::/48"}, existing: []string{"fd00::/60"}, want: "fd00::/64", expect: false},
		{name: "v4_want_in_v6_pool", parents: []string{"fd00::/48"}, want: "10.0.0.0/24", expect: false},
		{name: "v6_existing_never_blocks_v4", parents: []string{"10.0.0.0/16"}, existing: []string{"fd00::/64"}, want: "10.0.1.0/24", expect: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsBlockAvailable(mustCIDRs(t, c.parents), mustCIDRs(t, c.existing), mustCIDR(t, c.want))
			if got != c.expect {
				t.Fatalf("expected %v, got %v", c.expect, got)
			}
		})
	}
}

// A reserved position is an ordinary allocation, so the specific-address path
// refuses it for the same reason it refuses a claimed one.
func TestIsBlockAvailable_RefusesReservedPositions(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.0.0.0/24"})
	reserved, err := ReservedBlocks(parents, 2, 1, 26)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	for _, block := range reserved {
		if IsBlockAvailable(parents, reserved, block) {
			t.Fatalf("reserved block %s reported available", cidrStr(block))
		}
	}
	// The one position neither reserved nor allocated.
	if !IsBlockAvailable(parents, reserved, mustCIDR(t, "10.0.0.128/26")) {
		t.Fatal("expected 10.0.0.128/26 to be available")
	}
}

func TestFindFirstAvailableBlockWithReservations_Table(t *testing.T) {
	type tc struct {
		name     string
		parents  []string
		existing []string
		prefix   int
		strategy Strategy
		res      Reservation
		want     string
		wantErr  error
	}

	cases := []tc{
		// No reservation — identical to the unreserved path.
		{
			name: "zero_reservation_is_noop", parents: []string{"10.0.0.0/16"},
			prefix: 24, strategy: FirstFit, want: "10.0.0.0/24",
		},

		// Claim size equals unit size: allocation starts past the reservation.
		{
			name: "leading_one_skips_first_block", parents: []string{"10.0.0.0/16"},
			prefix: 24, strategy: FirstFit,
			res:  Reservation{UnitPrefixLength: 24, Leading: 1},
			want: "10.0.1.0/24",
		},
		{
			name: "leading_two_skips_two_blocks", parents: []string{"fd20:a1b:2c3d:1::/64"},
			prefix: 96, strategy: FirstFit,
			res:  Reservation{UnitPrefixLength: 96, Leading: 2},
			want: "fd20:a1b:2c3d:1:0:2::/96",
		},

		// Claims larger than the unit: a leading reservation of 2 /32s inside a
		// /24 makes the first free /30 the second one, because the first
		// overlaps the reservation. This is the pool-serves-several-sizes case.
		{
			name: "unit_narrower_than_claim", parents: []string{"10.0.0.0/24"},
			prefix: 30, strategy: FirstFit,
			res:  Reservation{UnitPrefixLength: 32, Leading: 2},
			want: "10.0.0.4/30",
		},
		{
			name: "unit_narrower_than_claim_exact_fit", parents: []string{"10.0.0.0/24"},
			prefix: 30, strategy: FirstFit,
			res:  Reservation{UnitPrefixLength: 32, Leading: 4},
			want: "10.0.0.4/30",
		},

		// Claims smaller than the unit: a /28 reservation blocks the whole /28
		// even though the claim is a /30.
		{
			name: "unit_wider_than_claim", parents: []string{"10.0.0.0/24"},
			prefix: 30, strategy: FirstFit,
			res:  Reservation{UnitPrefixLength: 28, Leading: 1},
			want: "10.0.0.16/30",
		},

		// Trailing reservations only bite at the end of the pool.
		{
			name: "trailing_ignored_until_full", parents: []string{"10.0.0.0/24"},
			prefix: 26, strategy: FirstFit,
			res:  Reservation{UnitPrefixLength: 26, Trailing: 1},
			want: "10.0.0.0/26",
		},
		{
			name: "trailing_blocks_last", parents: []string{"10.0.0.0/24"},
			existing: []string{"10.0.0.0/26", "10.0.0.64/26", "10.0.0.128/26"},
			prefix:   26, strategy: FirstFit,
			res:     Reservation{UnitPrefixLength: 26, Trailing: 1},
			wantErr: ErrPoolExhausted,
		},

		// Reservations interact with existing allocations, not instead of them.
		{
			name: "reservation_plus_existing", parents: []string{"10.0.0.0/22"},
			existing: []string{"10.0.1.0/24"},
			prefix:   24, strategy: FirstFit,
			res:  Reservation{UnitPrefixLength: 24, Leading: 1},
			want: "10.0.2.0/24",
		},

		// Reserving the whole pool exhausts it — same condition as a full pool,
		// from the claim's point of view.
		{
			name: "fully_reserved_is_exhausted", parents: []string{"10.0.0.0/30"},
			prefix: 32, strategy: FirstFit,
			res:     Reservation{UnitPrefixLength: 32, Leading: 2, Trailing: 2},
			wantErr: ErrPoolExhausted,
		},

		// A multi-parent pool has one pair of edges, not one per range: only
		// the lowest range loses its head and only the highest loses its tail.
		{
			name: "multi_parent_edges_only", parents: []string{"10.0.0.0/24", "10.0.1.0/24"},
			existing: []string{"10.0.0.64/26", "10.0.0.128/26", "10.0.0.192/26"},
			prefix:   26, strategy: FirstFit,
			res:  Reservation{UnitPrefixLength: 26, Leading: 1, Trailing: 1},
			want: "10.0.1.0/26",
		},
		{
			name: "multi_parent_trailing_at_high_end", parents: []string{"10.0.0.0/24", "10.0.1.0/24"},
			existing: []string{
				"10.0.0.64/26", "10.0.0.128/26", "10.0.0.192/26",
				"10.0.1.0/26", "10.0.1.64/26", "10.0.1.128/26",
			},
			prefix: 26, strategy: FirstFit,
			res:     Reservation{UnitPrefixLength: 26, Leading: 1, Trailing: 1},
			wantErr: ErrPoolExhausted,
		},

		// Strategies still apply on top of reservations. Two holes remain, the
		// /24-sized one is tighter than the tail.
		{
			name: "bestfit_with_reservation", parents: []string{"10.0.0.0/16"},
			existing: []string{"10.0.2.0/23", "10.0.8.0/21"},
			prefix:   24, strategy: BestFit,
			res:  Reservation{UnitPrefixLength: 24, Leading: 1},
			want: "10.0.1.0/24",
		},

		// Validation errors propagate rather than being swallowed into
		// exhaustion — an operator misconfiguration must not read as a full
		// pool.
		{
			name: "invalid_unit_propagates", parents: []string{"10.0.0.0/24"},
			prefix: 26, strategy: FirstFit,
			res:     Reservation{UnitPrefixLength: 16, Leading: 1},
			wantErr: ErrInvalidReservation,
		},
		{
			name: "too_large_propagates", parents: []string{"10.0.0.0/24"},
			prefix: 26, strategy: FirstFit,
			res:     Reservation{UnitPrefixLength: 26, Leading: 3, Trailing: 3},
			wantErr: ErrReservationTooLarge,
		},

		// No parents is still ErrNoParent, checked before the reservation.
		{
			name: "no_parents", prefix: 24, strategy: FirstFit,
			res:     Reservation{UnitPrefixLength: 24, Leading: 1},
			wantErr: ErrNoParent,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FindFirstAvailableBlockWithReservations(
				mustCIDRs(t, c.parents), mustCIDRs(t, c.existing), c.prefix, c.strategy, c.res)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("expected %v, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cidrStr(*got) != c.want {
				t.Fatalf("expected %s, got %s", c.want, cidrStr(*got))
			}
		})
	}
}

// Reserved blocks are persisted as real allocations, so a caller that both
// stores them in Existing and keeps the Reservation set must get the same
// answer as one that does only one of the two.
func TestReservation_IdempotentWithPersistedBlocks(t *testing.T) {
	parents := mustCIDRs(t, []string{"10.0.0.0/22"})
	res := Reservation{UnitPrefixLength: 24, Leading: 1, Trailing: 1}

	reserved, err := res.BlocksIn(parents)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	fromReservation, err := FindFirstAvailableBlockWithReservations(parents, nil, 24, FirstFit, res)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	fromExisting, err := FindFirstAvailableBlock(parents, reserved, 24, FirstFit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	both, err := FindFirstAvailableBlockWithReservations(parents, reserved, 24, FirstFit, res)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if cidrStr(*fromReservation) != "10.0.1.0/24" {
		t.Fatalf("expected 10.0.1.0/24, got %s", cidrStr(*fromReservation))
	}
	if cidrStr(*fromExisting) != cidrStr(*fromReservation) || cidrStr(*both) != cidrStr(*fromReservation) {
		t.Fatalf("paths disagree: reservation=%s existing=%s both=%s",
			cidrStr(*fromReservation), cidrStr(*fromExisting), cidrStr(*both))
	}
}

func TestCIDRPool_Allocate_HonoursReservation(t *testing.T) {
	pool := &CIDRPool{
		Ranges:      mustCIDRs(t, []string{"10.0.0.0/16"}),
		Strategy:    FirstFit,
		Reservation: Reservation{UnitPrefixLength: 24, Leading: 2, Trailing: 1},
	}
	got, err := pool.Allocate(24)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "10.0.2.0/24" {
		t.Fatalf("expected 10.0.2.0/24, got %s", cidrStr(*got))
	}

	// The zero-valued Reservation on a pool must not change behaviour.
	plain := &CIDRPool{Ranges: pool.Ranges, Strategy: FirstFit}
	got, err = plain.Allocate(24)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "10.0.0.0/24" {
		t.Fatalf("expected 10.0.0.0/24, got %s", cidrStr(*got))
	}
}

func TestReservation_IsZero(t *testing.T) {
	cases := []struct {
		name string
		res  Reservation
		want bool
	}{
		{name: "zero_value", res: Reservation{}, want: true},
		{name: "unit_only", res: Reservation{UnitPrefixLength: 96}, want: true},
		{name: "leading", res: Reservation{UnitPrefixLength: 96, Leading: 1}, want: false},
		{name: "trailing", res: Reservation{UnitPrefixLength: 96, Trailing: 1}, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.res.IsZero(); got != c.want {
				t.Fatalf("expected %v, got %v", c.want, got)
			}
		})
	}
}

// FirstFit scans parents in the order the caller supplies them, not in address
// order. Reservations are computed in address order, so the two agree only for
// an ascending slice. This test pins that behaviour, because the reservation
// math depends on it and the requirement on callers must not change silently.
func TestFindFirstAvailableBlock_MultiParent(t *testing.T) {
	t.Run("firstfit_follows_slice_order_not_address_order", func(t *testing.T) {
		parents := mustCIDRs(t, []string{"10.0.2.0/24", "10.0.0.0/24"})
		got, err := FindFirstAvailableBlock(parents, nil, 26, FirstFit)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if cidrStr(*got) != "10.0.2.0/26" {
			t.Fatalf("expected 10.0.2.0/26 (slice order), got %s", cidrStr(*got))
		}
	})

	t.Run("skips_parent_too_small_for_request", func(t *testing.T) {
		parents := mustCIDRs(t, []string{"10.0.0.0/28", "10.1.0.0/16"})
		got, err := FindFirstAvailableBlock(parents, nil, 24, FirstFit)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if cidrStr(*got) != "10.1.0.0/24" {
			t.Fatalf("expected 10.1.0.0/24, got %s", cidrStr(*got))
		}
	})

	t.Run("spills_into_next_parent_when_first_is_full", func(t *testing.T) {
		parents := mustCIDRs(t, []string{"10.0.0.0/24", "10.0.1.0/24"})
		existing := mustCIDRs(t, []string{"10.0.0.0/24"})
		got, err := FindFirstAvailableBlock(parents, existing, 24, FirstFit)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if cidrStr(*got) != "10.0.1.0/24" {
			t.Fatalf("expected 10.0.1.0/24, got %s", cidrStr(*got))
		}
	})

	t.Run("exhausts_across_all_parents", func(t *testing.T) {
		parents := mustCIDRs(t, []string{"10.0.0.0/24", "10.0.1.0/24"})
		existing := mustCIDRs(t, []string{"10.0.0.0/24", "10.0.1.0/24"})
		if _, err := FindFirstAvailableBlock(parents, existing, 24, FirstFit); !errors.Is(err, ErrPoolExhausted) {
			t.Fatalf("expected ErrPoolExhausted, got %v", err)
		}
	})

	t.Run("mixed_families_each_serve_their_own", func(t *testing.T) {
		parents := mustCIDRs(t, []string{"10.0.0.0/24", "fd00::/48"})
		got, err := FindFirstAvailableBlock(parents, nil, 64, FirstFit)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if cidrStr(*got) != "fd00::/64" {
			t.Fatalf("expected fd00::/64, got %s", cidrStr(*got))
		}
	})

	// Overlapping parents double-count their shared space: the same block is
	// offered from both ranges. Reservation rejects such a set outright; the
	// search does not, so callers must not build one.
	t.Run("overlapping_parents_double_count", func(t *testing.T) {
		parents := mustCIDRs(t, []string{"10.0.0.0/25", "10.0.0.0/24"})
		existing := mustCIDRs(t, []string{"10.0.0.0/26"})
		got, err := FindFirstAvailableBlock(parents, existing, 26, FirstFit)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if cidrStr(*got) != "10.0.0.64/26" {
			t.Fatalf("expected 10.0.0.64/26, got %s", cidrStr(*got))
		}
		if _, err := ReservedBlocks(parents, 1, 0, 26); !errors.Is(err, ErrInvalidReservation) {
			t.Fatalf("expected reservation to reject overlapping parents, got %v", err)
		}
	})
}
