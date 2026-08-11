package allocator

import (
	"context"
	"net"
	"testing"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/testdb"
)

// The bounded search must return the block the whole-set search would return.
// It is worth having only if it is not a second, subtly different allocator, so
// every step is checked against FindFirstAvailableBlock over the full set.
func TestBoundedSearchAgreesWithWholeSetSearch(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	const cidr = "10.50.0.0/16"
	poolKey := seedPool(t, db, "bounded-agree", cidr)
	parents := []net.IPNet{mustNet(t, cidr)}

	for i := range 40 {
		// What the whole-set search would choose, before the allocation runs.
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		existing, err := loadExistingAllocations(ctx, tx, poolKey)
		if err != nil {
			t.Fatalf("load allocations: %v", err)
		}
		want, err := allocation.FindFirstAvailableBlock(parents, existing, 26, allocation.FirstFit)
		if err != nil {
			t.Fatalf("step %d: whole-set search: %v", i, err)
		}
		_ = tx.Rollback(ctx)

		got := allocate(t, db, poolKey, claimName(i), 26)
		if got != want.String() {
			t.Fatalf("step %d: bounded search chose %s, whole-set search chose %s", i, got, want)
		}
	}
}

// The floor records where a search stopped, so the next one does not re-read
// what it already stepped over. Without it a bounded search of a sequentially
// filled pool is still linear: the first free block is at the end.
func TestTheFloorRisesAsThePoolFills(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	const cidr = "10.51.0.0/16"
	poolKey := seedPool(t, db, "bounded-floor", cidr)
	digest := scope.EmptyAddressSpaceDigest()

	var floors []string
	for i := range 5 {
		allocate(t, db, poolKey, claimName(i), 24)

		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		floor, err := readSearchFloor(ctx, tx, poolKey, digest)
		_ = tx.Rollback(ctx)
		if err != nil {
			t.Fatalf("read floor: %v", err)
		}
		if floor == nil {
			t.Fatalf("step %d: no floor recorded", i)
		}
		floors = append(floors, floor.String())
	}

	for i := 1; i < len(floors); i++ {
		if !ipLess(floors[i-1], floors[i]) {
			t.Errorf("floor did not rise: %v", floors)
			break
		}
	}
}

// A floor must never rise above a free address, because the search never looks
// below it. Releasing the first allocation has to bring it back down.
func TestReleasingBelowTheFloorMakesTheSpaceReachableAgain(t *testing.T) {
	db := testdb.Pool(t)

	const cidr = "10.52.0.0/16"
	poolKey := seedPool(t, db, "bounded-release", cidr)

	first := allocate(t, db, poolKey, claimName(0), 24)
	for i := 1; i < 5; i++ {
		allocate(t, db, poolKey, claimName(i), 24)
	}

	release(t, db, claimName(0))

	// The next allocation must land back in the hole, not past the floor.
	if got := allocate(t, db, poolKey, claimName(99), 24); got != first {
		t.Errorf("after releasing %s the next /24 went to %s; the floor is above free space", first, got)
	}
}

func claimName(i int) string {
	return "bounded-claim-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func ipLess(a, b string) bool {
	ipa, ipb := net.ParseIP(a).To16(), net.ParseIP(b).To16()
	for i := range ipa {
		if ipa[i] != ipb[i] {
			return ipa[i] < ipb[i]
		}
	}
	return false
}
