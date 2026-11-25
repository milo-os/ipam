package allocator

// The database half of internal/allocation.Scan. That package owns what "first
// fit" means and compiles with only the standard library; this file owns how
// the rows arrive.

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

const searchPageSize = 256

// searchPageLimit is unreachable in a correct search. It stops a cursor that
// fails to advance from spinning inside the pool lock.
const searchPageLimit = 100_000

var errSearchDidNotAdvance = errors.New("bounded search exceeded its page limit without deciding")

// boundedFirstFit finds the lowest free block of prefixLen bits, reading only
// the allocations it examines. It returns the block FindFirstAvailableBlock
// would return over the whole set, or ErrPoolExhausted, and the floor to
// persist.
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

		// Probe at every cursor position, not once per page. PostgreSQL orders
		// inet by address then mask length, so a block sorts below the address
		// it starts at and the range read misses whatever covers the cursor.
		// Skipping a probe makes occupied space read as a gap — silently, when
		// the two allocations are in different address spaces.
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
			continue // the probe moved the cursor; probe again before paging
		}

		page, perr := loadBlocksFrom(ctx, tx, poolKey, scopeDigest, parent, from, searchPageSize)
		if perr != nil {
			return nil, nil, perr
		}
		if len(page) == 0 {
			// The only safe place to end a parent: the probe above is what
			// proves the cursor itself is uncovered.
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

// spaceAll is the search digest of a caller that must avoid every allocation in
// the pool whatever space holds it — a carve, which takes address space out of
// the pool for a child pool to own, so no space may still be holding any of it.
//
// It is not an address space and nothing is ever written under it.
const spaceAll = ""

// searchFilter selects the blocks a search must treat as occupied. A Claim's
// block belongs to one address space and blocks only that space; reservations
// and the carves backing child pools block every space, because that space has
// left the pool. Under spaceAll every block is occupied.
//
// Do not add an owner_project term. A search narrower than the constraint
// presents as unexplained exhaustion rather than as an error.
const searchFilter = `pool_key = $1 AND ($2 = '' OR purpose <> 'Claim' OR scope_digest = $2)`

// loadBlocksCovering returns the allocations that contain from. It is a
// correctness lookup, not an optimisation.
//
// PostgreSQL orders inet by network address then mask length, so 10.0.0.0/24
// sorts before 10.0.0.5: a /24 covering the cursor is below it in the ordering
// and the range read never sees it. Missing one means treating occupied space
// as free.
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

// loadBlocksFrom returns up to limit allocations at or above from, ascending.
//
// The `<<= parent` term is not merely a filter: without it a pool whose rows
// lie above the parent under scan fills a page with blocks the scan ignores,
// and the cursor never advances.
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

// rowQuerier lets readSearchFloor take a pool or a transaction. Nothing else
// here is widened: every other function writes, and a write outside the claim's
// transaction would not be atomic with it.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockSearchFloorSQL locks a floor row and reads it. Every caller holds the
// pool row's lock first, so every caller acquires (pool_A, floor_A),
// (pool_B, floor_B) and two transactions can only deadlock by taking pools in
// different orders.
const lockSearchFloorSQL = `SELECT host(floor) FROM ipam_pool_search_floor
	  WHERE pool_key = $1 AND scope_digest = $2
	  FOR UPDATE`

// floorRead is what lockSearchFloorSQL returned: the floor, or that there is
// no row yet.
type floorRead struct {
	found bool
	floor net.IP
}

// scanSearchFloor reads lockSearchFloorSQL's row, however it was sent.
func scanSearchFloor(row pgx.Row, poolKey string) (floorRead, error) {
	var text string
	if err := row.Scan(&text); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return floorRead{}, nil
		}
		return floorRead{}, fmt.Errorf("lock search floor for %q: %w", poolKey, err)
	}
	// A value that does not parse leaves the floor nil: search from the base.
	return floorRead{found: true, floor: net.ParseIP(text)}, nil
}

// lockSearchFloor takes the floor row's lock and returns its value. Call it
// immediately after locking the pool row (see lockSearchFloorSQL).
//
// The row is created if absent so there is always something to lock. It is
// read first, because it is absent only on a space's first search, and seeding
// it on every search was a round trip spent under the pool's lock for nothing.
func lockSearchFloor(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, base net.IP) (net.IP, error) {
	read, err := scanSearchFloor(tx.QueryRow(ctx, lockSearchFloorSQL, poolKey, scopeDigest), poolKey)
	if err != nil {
		return nil, err
	}
	return seededSearchFloor(ctx, tx, poolKey, scopeDigest, base, read)
}

// seededSearchFloor returns the floor read found, or seeds the row at base and
// locks it when read found none.
//
// Seeding needs no lock of its own: every writer of a pool's floors holds the
// pool's lock, which this transaction does, so nothing can seed it in between.
func seededSearchFloor(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, base net.IP, read floorRead) (net.IP, error) {
	if read.found {
		return read.floor, nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ipam_pool_search_floor (pool_key, scope_digest, floor)
		 VALUES ($1, $2, $3::inet)
		 ON CONFLICT (pool_key, scope_digest) DO NOTHING`,
		poolKey, scopeDigest, base.String()); err != nil {
		return nil, fmt.Errorf("seed search floor for %q: %w", poolKey, err)
	}
	read, err := scanSearchFloor(tx.QueryRow(ctx, lockSearchFloorSQL, poolKey, scopeDigest), poolKey)
	if err != nil {
		return nil, err
	}
	// Not found only if the pool row vanished under us and the FK cascaded
	// the floor away. Searching from the base is correct.
	return read.floor, nil
}

// readSearchFloor returns the floor without locking it, for tests and
// diagnostics. The allocation path uses lockSearchFloor.
//
// nil means "start at the pool's base", which is always correct and only
// slower. A floor above a free address makes that address unreachable, so every
// failure mode here resolves downwards.
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

// raiseSearchFloor records where the next search may start. observed is the
// floor this search began from, nil when it began at the base.
//
// The write is conditional on the floor not having moved: if a release lowered
// it meanwhile, overwriting would put the freed address below the floor where
// no later search looks. Losing that race costs one slower scan.
func raiseSearchFloor(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, observed, next net.IP) error {
	raise := floorRaise{poolKey: poolKey, scopeDigest: scopeDigest, observed: observed, next: next}
	sql, args, ok := raise.statement()
	if !ok {
		return nil
	}
	_, err := tx.Exec(ctx, sql, args...)
	return raise.wrap(err)
}

// floorRaise is a raiseSearchFloor not yet sent, so it can ride in a batch.
type floorRaise struct {
	poolKey, scopeDigest string
	observed, next       net.IP
}

// statement is the SQL the raise sends, or ok false when there is nothing to
// write.
func (r floorRaise) statement() (sql string, args []any, ok bool) {
	if r.next == nil {
		// No free address found, so nothing was learned. Raising the floor here
		// would make an exhausted pool exhausted even after a release.
		return "", nil, false
	}
	if r.observed == nil {
		// First search for this space. ON CONFLICT DO NOTHING rather than DO
		// UPDATE: if a row appeared while this search ran, it was written by
		// something with a fresher view than ours.
		return `INSERT INTO ipam_pool_search_floor (pool_key, scope_digest, floor)
			 VALUES ($1, $2, $3::inet)
			 ON CONFLICT (pool_key, scope_digest) DO NOTHING`,
			[]any{r.poolKey, r.scopeDigest, r.next.String()}, true
	}
	return `UPDATE ipam_pool_search_floor
		    SET floor = $3::inet, updated_at = NOW()
		  WHERE pool_key = $1 AND scope_digest = $2
		    AND floor = $4::inet`,
		[]any{r.poolKey, r.scopeDigest, r.next.String(), r.observed.String()}, true
}

func (r floorRaise) wrap(err error) error {
	if err == nil {
		return nil
	}
	if r.observed == nil {
		return fmt.Errorf("seed search floor for %q: %w", r.poolKey, err)
	}
	return fmt.Errorf("raise search floor for %q: %w", r.poolKey, err)
}
