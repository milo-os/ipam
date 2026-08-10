package allocator

// The bounded first-fit search: the database half of internal/allocation.Scan.
//
// The library walks allocations in ascending address order and stops at the
// first gap that fits. This file is what feeds it — a keyset-paged read of
// idx_ipam_cidr_alloc_pool_addr, plus the persisted floor that lets a
// sequentially filled pool skip everything below the fill line.
//
// It lives here rather than in internal/allocation because it is index-aware
// and holds a pgx.Tx, and that package compiles with only the standard library
// (constraint #5). The split is the point: the library owns what "first fit"
// means and is tested against the whole-set implementation; this file owns only
// how the rows arrive.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
)

// searchPageSize is how many allocations one read fetches.
//
// It trades round trips against rows that turn out not to be needed. With a
// warm floor the common case is one page containing one row — the block the
// previous claim took — so the size barely matters; it matters when the floor
// is cold or stale, where a bigger page means fewer round trips over ground
// that has to be walked anyway.
const searchPageSize = 256

// searchPageLimit bounds the number of pages one search may read.
//
// Not an expected condition: every page either advances the scan's cursor past
// at least one allocation or ends the parent, so a search terminates in at most
// ceil(allocations / searchPageSize) + parents pages. The limit exists so that
// a bug which fails to advance the cursor surfaces as a failed request naming
// this constant, rather than as a transaction spinning inside the pool lock and
// taking every other claim against that pool down with it.
const searchPageLimit = 100_000

// errSearchDidNotAdvance means the paging loop hit searchPageLimit. See above:
// it indicates a defect in this file or in Scan, never a property of the pool.
var errSearchDidNotAdvance = errors.New("bounded search exceeded its page limit without deciding")

// boundedFirstFit finds the lowest free block of prefixLen bits in the pool,
// reading only the allocations it has to examine.
//
// It returns the same block allocation.FindFirstAvailableBlock would return
// over the whole set — TestBoundedSearchAgreesWithWholeSetSearch asserts that
// against the wired path rather than against the library alone — and
// ErrPoolExhausted when there is no room.
//
// The second return is the floor to persist: the lowest free address the search
// walked to. It is meaningful only when the search succeeded from the floor it
// was given, which is why the caller passes the observed floor back into
// raiseSearchFloor rather than this function writing it.
func boundedFirstFit(
	ctx context.Context,
	tx pgx.Tx,
	poolKey, scopeDigest string,
	parents []net.IPNet,
	prefixLen int,
	floor net.IP,
) (*net.IPNet, net.IP, error) {
	defer metrics.ObserveQuery("bounded_first_fit", time.Now())

	scan, err := allocation.NewScan(parents, prefixLen, floor)
	if err != nil {
		return nil, nil, err
	}

	for pages := 0; !scan.Done(); pages++ {
		if pages >= searchPageLimit {
			return nil, nil, fmt.Errorf("%w: pool %q after %d pages", errSearchDidNotAdvance, poolKey, pages)
		}

		parent, from, ok := scan.Need()
		if !ok {
			break
		}

		// THE COVERING PROBE RUNS AT EVERY CURSOR POSITION, and getting this
		// wrong is the sharpest edge in the file. Two earlier versions were
		// wrong in the same direction — they handed out an address somebody
		// already held.
		//
		// PostgreSQL orders inet by network address and THEN by mask length, so
		// a block is BELOW the bare address it starts at:
		//
		//     '10.64.1.112/28'::cidr >= '10.64.1.112'::inet   ->  false
		//
		// An `allocated_cidr >= cursor` range scan therefore misses every block
		// that begins exactly at the cursor, and the cursor lands exactly on a
		// block's first address whenever two allocations are adjacent — which in
		// a sequentially filled pool is every time. It also misses any larger
		// block the cursor sits inside.
		//
		// So the range read cannot be the only read: `>>=`, the GiST-indexed
		// containment operator, supplies what it cannot see. Between them they
		// are complete, because a block whose last address is at or above the
		// cursor either contains the cursor or begins strictly above it.
		//
		// Probing once per PAGE is not enough either, which is the version that
		// looked right and was not. Feeding the probe advances the cursor, and
		// the page read then runs from the new position with no probe of its
		// own — so a block starting exactly there is invisible, the space it
		// occupies reads as a gap, and the search returns it. Hence: probe,
		// and if the cursor moved, go round again before reading a page.
		//
		// Within one address space the exclusion constraint on
		// (pool_key, scope_digest, allocated_cidr) catches the result as a
		// 23P01 rather than a double allocation. Across spaces it does not
		// compare the rows at all, so there the same bug is silent.
		covering, cerr := loadBlocksCovering(ctx, tx, poolKey, scopeDigest, parent, from)
		if cerr != nil {
			return nil, nil, cerr
		}
		if ferr := scan.Feed(covering); ferr != nil {
			return nil, nil, fmt.Errorf("feed covering blocks: %w", ferr)
		}
		if scan.Done() {
			break
		}
		if _, moved, still := scan.Need(); still && !moved.Equal(from) {
			// The probe consumed something. Re-probe at the new cursor rather
			// than reading a page from a position nothing has covered.
			continue
		}

		page, perr := loadBlocksFrom(ctx, tx, poolKey, scopeDigest, parent, from, searchPageSize)
		if perr != nil {
			return nil, nil, perr
		}
		if len(page) == 0 {
			// Nothing contains the cursor and nothing begins above it, so the
			// rest of this parent is free. This is the ONLY safe place to end a
			// parent: the probe immediately above is what proves the cursor
			// itself is uncovered.
			scan.End()
			continue
		}
		if ferr := scan.Feed(page); ferr != nil {
			return nil, nil, fmt.Errorf("feed page: %w", ferr)
		}
	}

	block, err := scan.Result()
	if err != nil {
		return nil, scan.FirstFree(), err
	}
	return &block, scan.FirstFree(), nil
}

// searchFilter is the class model's central rule, in the form both reads below
// use.
//
// A Claim's block belongs to one address space and blocks only that space;
// everything else the pool holds — its reservations, and the carves backing
// child pools — blocks every space, because that space has really left the
// pool. loadAllocationsInScope carries the long form of this reasoning and the
// standing instruction not to add an owner_project term; the same applies here,
// and the two predicates must not drift apart. A search narrower than the
// exclusion constraint presents as unexplained exhaustion, not as an error.
const searchFilter = `pool_key = $1 AND (purpose <> 'Claim' OR scope_digest = $2)`

// loadBlocksCovering returns the allocations that contain `from`.
//
// These cannot come from the ordered range read. PostgreSQL orders inet by
// network address and then by mask length, so 10.0.0.0/24 sorts BEFORE
// 10.0.0.5 — a /24 covering the cursor is below it in the ordering and a
// `>= from` scan never sees it. Missing one means treating occupied space as
// free, so this is a correctness lookup rather than an optimisation.
//
// `>>=` is the GiST-indexed containment operator, and the result is normally
// empty or a single row.
func loadBlocksCovering(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, parent net.IPNet, from net.IP) ([]net.IPNet, error) {
	return queryAllocationCIDRs(ctx, tx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE `+searchFilter+`
		    AND allocated_cidr >>= $3::inet
		    AND allocated_cidr <<= $4::cidr
		  ORDER BY allocated_cidr`,
		poolKey, scopeDigest, from.String(), parent.String())
}

// loadBlocksFrom returns up to limit allocations at or above `from`, ascending.
//
// This is the read the whole change exists for: an index range scan on
// (pool_key, allocated_cidr) with a LIMIT, so the rows fetched are the rows the
// search examines rather than every row the pool holds.
//
// The `<<= parent` term is not merely a filter. Without it a pool whose rows
// lie above the parent under scan can fill a page with blocks the scan
// correctly ignores, the cursor never advances, and the loop spins until
// searchPageLimit. Every row for a pool is carved from that pool, so
// restricting to the parent discards nothing real.
func loadBlocksFrom(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, parent net.IPNet, from net.IP, limit int) ([]net.IPNet, error) {
	return queryAllocationCIDRs(ctx, tx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE `+searchFilter+`
		    AND allocated_cidr >= $3::inet
		    AND allocated_cidr <<= $4::cidr
		  ORDER BY allocated_cidr
		  LIMIT $5`,
		poolKey, scopeDigest, from.String(), parent.String(), limit)
}

// ----------------------------------------------------------------------------
// the floor
// ----------------------------------------------------------------------------

// rowQuerier is the single method readSearchFloor needs, so a test can ask the
// pool the same question the allocator asks its transaction. Both pgx.Tx and
// *pgxpool.Pool satisfy it; nothing else in this file is widened, because
// everything else writes and a write outside the claim's transaction would not
// be atomic with it.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockSearchFloor takes the floor row's lock and returns its value.
//
// # This must be called immediately after the pool row is locked, and nowhere else
//
// The floor row is a SECOND lock on the allocation path, and a second lock is
// where deadlocks live. Reading it without locking, then updating it after the
// search, leaves a window in which this transaction holds its POOL row and
// acquires the FLOOR row late — while another transaction, further along, holds
// that floor row and is reaching for a pool row. Postgres breaks the cycle with
// SQLSTATE 40P01 and the caller gets a 500 on a claim (#93).
//
// Taking the floor immediately after its own pool makes the acquisition order
// (pool_A, floor_A), (pool_B, floor_B), … for every caller. Two transactions can
// then only deadlock by taking POOLS in different orders, which is a
// pre-existing property of the cascade and not something this table introduces.
//
// The row is created if absent so there is always something to lock. That
// insert is inside the caller's transaction, so a rolled-back claim leaves no
// row behind, and the seeded value is the safest one there is — see migration
// 009 on why a floor may only ever be too low.
func lockSearchFloor(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, base net.IP) (net.IP, error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO ipam_pool_search_floor (pool_key, scope_digest, floor)
		 VALUES ($1, $2, $3::inet)
		 ON CONFLICT (pool_key, scope_digest) DO NOTHING`,
		poolKey, scopeDigest, base.String()); err != nil {
		return nil, fmt.Errorf("seed search floor for %q: %w", poolKey, err)
	}
	var text string
	err := tx.QueryRow(ctx,
		`SELECT host(floor) FROM ipam_pool_search_floor
		  WHERE pool_key = $1 AND scope_digest = $2
		  FOR UPDATE`,
		poolKey, scopeDigest).Scan(&text)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Only reachable if the pool row vanished under us and the FK
			// cascaded the floor away. Searching from the base is correct.
			return nil, nil
		}
		return nil, fmt.Errorf("lock search floor for %q: %w", poolKey, err)
	}
	ip := net.ParseIP(text)
	if ip == nil {
		return nil, nil
	}
	return ip, nil
}

// readSearchFloor returns the lowest address in this pool and address space that
// could still be free, or nil when nothing is recorded.
//
// Read-only, and used by tests and diagnostics. The allocation path uses
// lockSearchFloor instead — see its comment for why the lock is not optional.
//
// nil means "start at the pool's base", which is always correct and only ever
// slower. Every failure mode of this table resolves to nil or to a value below
// the truth; see migration 009 for why that direction is the only safe one.
func readSearchFloor(ctx context.Context, q rowQuerier, poolKey, scopeDigest string) (net.IP, error) {
	var text string
	err := q.QueryRow(ctx,
		`SELECT host(floor) FROM ipam_pool_search_floor
		  WHERE pool_key = $1 AND scope_digest = $2`,
		poolKey, scopeDigest).Scan(&text)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read search floor for %q: %w", poolKey, err)
	}
	ip := net.ParseIP(text)
	if ip == nil {
		// A value that does not parse is a cache entry, not an invariant. Refuse
		// to guess and search from the base instead of erroring the claim: the
		// address the caller asked for does not depend on this table.
		return nil, nil
	}
	return ip, nil
}

// raiseSearchFloor records where the next search may start, and refuses to do so
// if anything lowered the floor while this one was running.
//
// observed is the floor the search began from, nil when it began at the base.
// next is the lowest free address the search walked to.
//
// # Why this is a compare-and-set
//
// A release commits, the trigger in migration 009 lowers the floor to the freed
// address, and this write would otherwise overwrite it with a value justified
// only by ground the search walked BEFORE the release. The freed address ends up
// below the floor, no later search looks there, and it is gone — silently, with
// the pool eventually reporting full while holding room.
//
// The CAS makes the search the loser in that race, which is the correct loser:
// its floor is stale, the release's is not. A failed update costs one slower
// scan. Do not "simplify" this to an unconditional upsert — it reads as
// equivalent and it is the difference between a slow search and a lost address.
func raiseSearchFloor(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, observed, next net.IP) error {
	if next == nil {
		// The search found no free address at or above its floor. It has learned
		// nothing about where the next one starts, and raising the floor to the
		// end of the pool would make an exhausted pool permanently exhausted even
		// after a release. Leave it alone.
		return nil
	}
	if observed == nil {
		// First search for this space. ON CONFLICT DO NOTHING rather than DO
		// UPDATE: if a row appeared while this search ran, it was written by
		// something with a fresher view than ours.
		_, err := tx.Exec(ctx,
			`INSERT INTO ipam_pool_search_floor (pool_key, scope_digest, floor)
			 VALUES ($1, $2, $3::inet)
			 ON CONFLICT (pool_key, scope_digest) DO NOTHING`,
			poolKey, scopeDigest, next.String())
		if err != nil {
			return fmt.Errorf("seed search floor for %q: %w", poolKey, err)
		}
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE ipam_pool_search_floor
		    SET floor = $3::inet, updated_at = NOW()
		  WHERE pool_key = $1 AND scope_digest = $2
		    AND floor = $4::inet`,
		poolKey, scopeDigest, next.String(), observed.String())
	if err != nil {
		return fmt.Errorf("raise search floor for %q: %w", poolKey, err)
	}
	return nil
}
