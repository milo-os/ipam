package allocator

// Reclaiming the unprefixed keyspace (#88).
//
// # Why anything needs to exist here
//
// After platform-as-a-project (#65) every object IPAM stores lives under
// "project/<p>/". The unprefixed keyspace "/ipam.miloapis.com/..." is what a
// caller with no tenant used to write into, and nothing reaches it now — the
// untenanted-write gate refuses those requests at the handler chain.
//
// That leaves whatever was written before the gate closed, and it is
// unreachable in a way no API call can undo:
//
//	delete UNIMPERSONATED -> 403, the gate refuses the write
//	delete AS PLATFORM    -> 404, the platform keyspace does not contain it
//
// The identity that can see these objects may not write, and the identity that
// may write cannot see them. Both behaviours are individually correct; their
// intersection is what strands the objects. So the reclaim cannot go through
// the API at all, and this operates on the database directly — at the same
// privilege level `ipam migrate` already runs with.
//
// # Why this is a command and not a migration
//
// A goose migration runs unattended on every deployment. Migration 007
// deliberately REFUSES to proceed while unprefixed objects exist rather than
// deleting them, because on a real deployment they may be genuine pre-cutover
// objects an operator still intends to re-home — 007's header carries the
// rewrite for exactly that. A migration that deleted them would destroy that
// data silently, on a schedule nobody chose.
//
// So the destructive step is opt-in, a human runs it, and it reports before it
// acts.
//
// # The key shape this matches, and the one it must not
//
// Three key shapes appear in ipam_objects and only two are real objects:
//
//	project/<p>/ipam.miloapis.com/<resource>/<name>   a real object
//	/ipam.miloapis.com/<resource>/<name>              unprefixed — what this reclaims
//	/ippool/<name>                                    test residue (#77), NOT this
//
// The last is written by migrations/ Go tests into whatever schema they are
// handed, and it is a shape no registry produces. It is deliberately outside
// this predicate: mixing them would make a tool that deletes real objects and a
// tool that cleans up after tests the same tool, and the two want different
// levels of care. See docs/verification-conventions.md rule 11a, where
// confusing exactly these two shapes cost an hour.

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// unprefixedKeyPattern matches the keyspace no tenant can reach. The leading
// slash is what distinguishes it from "project/<p>/ipam.miloapis.com/...".
const unprefixedKeyPattern = "/ipam.miloapis.com/%"

// UnprefixedResidue is what the unprefixed keyspace holds, in the terms the
// reclaim acts on.
//
// It carries a sample of real keys rather than only counts, because a count is
// the thing that gets escalated and a predicate selecting the wrong population
// still returns real, plausible numbers. A caller reporting this should print
// the sample.
type UnprefixedResidue struct {
	// ObjectsByKind counts unprefixed objects per kind.
	ObjectsByKind map[string]int
	// Objects is the total across kinds.
	Objects int
	// Allocations counts rows in ipam_cidr_allocations that NAME an unprefixed
	// object — the carves those pools hold against their parents.
	Allocations int
	// AllocationsInside counts rows HELD IN an unprefixed pool. It is a subset
	// of the work but not of Allocations: a carve against a surviving parent is
	// counted in Allocations and not here, and it is the one that matters most.
	AllocationsInside int
	// IdentityRows counts ipam_pool_identity rows naming an unprefixed pool.
	IdentityRows int
	// SurvivingParents are pools that OUTLIVE the reclaim and are currently
	// holding carves for unprefixed pools. Their capacity status counts space
	// that is about to be returned, so each one is refreshed.
	//
	// This is the half that is easy to miss. The residue is not confined to the
	// unreachable keyspace: it consumes real address space in reachable,
	// operator-authored pools, and deleting only the unreachable objects would
	// leave those carves behind with nothing naming them.
	SurvivingParents []string
	// Sample is a handful of object keys, so a reader can confirm the predicate
	// selected what they think it did.
	Sample []string
}

// Empty reports that there is nothing to reclaim.
func (r *UnprefixedResidue) Empty() bool { return r.Objects == 0 && r.Allocations == 0 }

// ScanUnprefixed reports what the unprefixed keyspace holds without changing
// anything.
func ScanUnprefixed(ctx context.Context, tx pgx.Tx) (*UnprefixedResidue, error) {
	res := &UnprefixedResidue{ObjectsByKind: map[string]int{}}

	rows, err := tx.Query(ctx,
		`SELECT kind, count(*) FROM ipam_objects WHERE key LIKE $1 GROUP BY kind`,
		unprefixedKeyPattern)
	if err != nil {
		return nil, fmt.Errorf("count unprefixed objects: %w", err)
	}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan object counts: %w", err)
		}
		res.ObjectsByKind[kind] = n
		res.Objects += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count unprefixed objects: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE allocation_key LIKE $1`,
		unprefixedKeyPattern).Scan(&res.Allocations); err != nil {
		return nil, fmt.Errorf("count allocations naming unprefixed objects: %w", err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM ipam_cidr_allocations WHERE pool_key LIKE $1`,
		unprefixedKeyPattern).Scan(&res.AllocationsInside); err != nil {
		return nil, fmt.Errorf("count allocations inside unprefixed pools: %w", err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM ipam_pool_identity WHERE pool_key LIKE $1`,
		unprefixedKeyPattern).Scan(&res.IdentityRows); err != nil {
		return nil, fmt.Errorf("count unprefixed identity rows: %w", err)
	}

	// Parents that survive the reclaim and are holding residue carves. The
	// NOT LIKE is what makes them "surviving": a residue pool holding another
	// residue pool's carve is going away itself and needs no refresh.
	parentRows, err := tx.Query(ctx,
		`SELECT DISTINCT pool_key FROM ipam_cidr_allocations
		  WHERE allocation_key LIKE $1 AND pool_key NOT LIKE $1
		  ORDER BY pool_key`, unprefixedKeyPattern)
	if err != nil {
		return nil, fmt.Errorf("find surviving parents: %w", err)
	}
	for parentRows.Next() {
		var key string
		if err := parentRows.Scan(&key); err != nil {
			parentRows.Close()
			return nil, fmt.Errorf("scan parent key: %w", err)
		}
		res.SurvivingParents = append(res.SurvivingParents, key)
	}
	parentRows.Close()
	if err := parentRows.Err(); err != nil {
		return nil, fmt.Errorf("find surviving parents: %w", err)
	}

	sampleRows, err := tx.Query(ctx,
		`SELECT key FROM ipam_objects WHERE key LIKE $1 ORDER BY key LIMIT 5`,
		unprefixedKeyPattern)
	if err != nil {
		return nil, fmt.Errorf("sample unprefixed keys: %w", err)
	}
	for sampleRows.Next() {
		var key string
		if err := sampleRows.Scan(&key); err != nil {
			sampleRows.Close()
			return nil, fmt.Errorf("scan sample key: %w", err)
		}
		res.Sample = append(res.Sample, key)
	}
	sampleRows.Close()
	if err := sampleRows.Err(); err != nil {
		return nil, fmt.Errorf("sample unprefixed keys: %w", err)
	}
	sort.Strings(res.SurvivingParents)
	return res, nil
}

// ReclaimUnprefixed removes the unprefixed keyspace inside the caller's
// transaction and returns what it removed.
//
// # The order is forced by the schema, not chosen
//
// ipam_cidr_allocations.pool_key is ON DELETE RESTRICT, so deleting the objects
// first is refused outright — verified against a live database rather than read
// off the DDL. The allocation rows go first, and one predicate covers both
// populations because every row involved NAMES an unprefixed object in
// allocation_key, whether it is held inside a residue pool or against a
// surviving one.
//
// ipam_pool_identity.pool_key is ON DELETE CASCADE DEFERRABLE, so those rows go
// with the objects and are not deleted explicitly. Deleting them by hand would
// work and would also hide it if that constraint ever changed.
//
// # Why capacity is refreshed and not left to the next allocation
//
// A surviving parent's status.capacity counts the carves this releases. Nothing
// re-derives it until something else allocates from that pool, so a reclaim that
// stopped at the deletes would leave a pool reporting space it no longer holds —
// the same shape as #47, reintroduced by the tool meant to clean up. The refresh
// is inside the same transaction, so a caller either gets both or neither.
func ReclaimUnprefixed(ctx context.Context, tx pgx.Tx) (*UnprefixedResidue, error) {
	res, err := ScanUnprefixed(ctx, tx)
	if err != nil {
		return nil, err
	}
	if res.Empty() {
		return res, nil
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM ipam_cidr_allocations WHERE allocation_key LIKE $1`,
		unprefixedKeyPattern); err != nil {
		return nil, fmt.Errorf("release carves naming unprefixed pools: %w", err)
	}
	// Allocations held INSIDE a residue pool whose own allocation_key is not
	// unprefixed — a claim made against one of these pools before the gate
	// closed. The first delete does not cover them and the FK would refuse the
	// object delete while they remain.
	if _, err := tx.Exec(ctx,
		`DELETE FROM ipam_cidr_allocations WHERE pool_key LIKE $1`,
		unprefixedKeyPattern); err != nil {
		return nil, fmt.Errorf("release allocations inside unprefixed pools: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM ipam_objects WHERE key LIKE $1`,
		unprefixedKeyPattern); err != nil {
		return nil, fmt.Errorf("delete unprefixed objects: %w", err)
	}

	for _, parent := range res.SurvivingParents {
		if err := RefreshPoolCapacity(ctx, tx, parent); err != nil {
			return nil, fmt.Errorf("refresh capacity for %q: %w", parent, err)
		}
	}
	return res, nil
}

// RefreshPoolCapacity re-derives a pool's capacity status from the allocations
// it currently holds and writes it back.
//
// It exists for callers that change a pool's allocations by a route the
// allocator does not own — the reclaim above being the only one today. Every
// normal path recomputes capacity as part of the write that moved it, which is
// why this had no reason to exist before.
//
// A no-op on a healthy pool, which is what makes it safe to run unconditionally:
// the value only moves when it was already wrong.
func RefreshPoolCapacity(ctx context.Context, tx pgx.Tx, poolKey string) error {
	pool, err := lockAndDecodeIPPool(ctx, tx, poolKey)
	if err != nil {
		return err
	}
	parents, err := parsePoolCIDRs(pool)
	if err != nil {
		return err
	}
	all, err := loadAllAllocations(ctx, tx, poolKey)
	if err != nil {
		return err
	}
	return writePoolCapacity(ctx, tx, pool, poolKey, parents, all)
}
