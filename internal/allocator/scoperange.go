package allocator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// ErrClassHoldsNoRange is returned for a scope-range request against a class
// that provisions no pools. poolPer is what makes a class provision one, so a
// class without it holds nothing a claim could bind: the pool such a request
// would build is a pool no allocation is ever served from.
var ErrClassHoldsNoRange = errors.New("ipam: class provisions no pools, so it holds no range")

// ErrScopeRangeHeld is returned when the range is already bound to a different
// claim.
var ErrScopeRangeHeld = errors.New("ipam: range already held by another claim")

// ErrScopeRangeOccupied is returned when a release would free a range that
// still has allocations or child pools inside it.
var ErrScopeRangeOccupied = errors.New("ipam: range still has allocations inside it")

// ErrScopeRangeCarveMissing is returned when the pool a range resolves to has
// no carve row against its source. A cascade-provisioned pool always writes
// one in the transaction that creates it, so this means the pool was authored
// by an operator rather than provisioned — and an operator's pool is not a
// range any claim may take away.
var ErrScopeRangeCarveMissing = errors.New("ipam: pool holds no carve against a source pool")

// ResolveScopeRange provisions the pool a class holds for a scope, and every
// level above it, returning that pool's key.
//
// It is ResolvePool with the leaf included: the same plan, the same identity
// rows, the same racing behaviour. That is deliberate and is the point of the
// feature — a range provisioned here is indistinguishable from one the cascade
// would have built under a later Block claim, so the later claim adopts it via
// lookupPoolIdentity rather than provisioning a second pool beside it.
func ResolveScopeRange(ctx context.Context, db TxBeginner, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef) (string, error) {
	if len(leaf.Spec.PoolPer) == 0 {
		return "", fmt.Errorf("%w: class %q names no poolPer", ErrClassHoldsNoRange, leaf.Name)
	}

	start := time.Now()
	result, provisioned := "error", false
	defer func() { metrics.ObserveCascadeResolution(result, provisioned, start) }()

	planTx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin scope-range planning transaction: %w", err)
	}
	levels, err := PlanScopeRangeCascade(ctx, planTx, leaf, claimScope)
	_ = planTx.Rollback(ctx)
	if err != nil {
		return "", err
	}

	source, err := discoverInTx(ctx, db, levels[0].Class, claimScope)
	if err != nil {
		return "", err
	}
	for i := range levels {
		poolKey, created, err := ensureLevel(ctx, db, levels[i], source)
		if err != nil {
			return "", fmt.Errorf("provision pool for class %q: %w", levels[i].Class.Name, err)
		}
		provisioned = provisioned || created
		source = poolKey
	}
	result = "success"
	return source, nil
}

// BindScopeRange makes claimKey the holder of the carve a provisioned pool
// occupies in its source, returning the range's CIDR.
//
// The carve row is the binding point rather than a table of its own because it
// is already the one row that exists exactly while the range does, is already
// unique per pool, and already carries a claim_key column that is unique across
// the table. A pool the cascade provisioned holds its own carve — claim_key
// equals allocation_key — and that self-held state is what "unheld" means here,
// so the WHERE clause is the mutual exclusion and no second claim can take a
// range out from under the first.
func BindScopeRange(ctx context.Context, tx pgx.Tx, poolKey, claimKey string) (string, error) {
	var cidr string
	err := tx.QueryRow(ctx,
		`UPDATE ipam_cidr_allocations
		    SET claim_key = $2
		  WHERE allocation_key = $1 AND purpose = 'PoolCarve' AND claim_key = allocation_key
		 RETURNING host(allocated_cidr) || '/' || masklen(allocated_cidr)`,
		poolKey, claimKey,
	).Scan(&cidr)
	if err == nil {
		return cidr, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("bind scope range %q: %w", poolKey, err)
	}

	// No row updated: either somebody holds it, or there is no carve at all.
	var holder *string
	err = tx.QueryRow(ctx,
		`SELECT claim_key, host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE allocation_key = $1 AND purpose = 'PoolCarve'`,
		poolKey,
	).Scan(&holder, &cidr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrScopeRangeCarveMissing, poolKey)
	}
	if err != nil {
		return "", fmt.Errorf("read scope range holder for %q: %w", poolKey, err)
	}
	held := ""
	if holder != nil {
		held = *holder
	}
	return "", fmt.Errorf("%w: %s is held by %q", ErrScopeRangeHeld, poolKey, held)
}

// FindScopeRange returns the key of the pool claimKey holds, or "" if it holds
// none.
//
// It reads the carve row by its holder rather than replanning the cascade, so a
// release does not depend on the class chain still resolving the way it did
// when the range was taken. A class renamed or reparented after the fact would
// otherwise strand the pool with nothing able to free it.
func FindScopeRange(ctx context.Context, tx pgx.Tx, claimKey string) (string, error) {
	var poolKey string
	err := tx.QueryRow(ctx,
		`SELECT allocation_key FROM ipam_cidr_allocations
		  WHERE claim_key = $1 AND purpose = 'PoolCarve'`,
		claimKey,
	).Scan(&poolKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find scope range held by %q: %w", claimKey, err)
	}
	return poolKey, nil
}

// ReleaseScopeRange frees the range poolKey names, and every pool the cascade
// provisioned beneath it: each pool's own reserved positions, the carve it
// occupies in its source, and the pool object itself. Identity rows go with
// their objects, on the foreign key's ON DELETE CASCADE, so the next claim for
// any of those scopes provisions a fresh pool rather than adopting one that no
// longer exists.
//
// Descendants go because they are derived, not authored. A per-region subnet
// pool exists only as somewhere a block of this range could come from; it
// outlives the last endpoint in it, so refusing on its account would mean a
// range that ever had one endpoint could never be given back. Nothing chose
// those pools, and nothing is holding them.
//
// A range with a live ALLOCATION anywhere beneath it is a different matter and
// is refused with ErrScopeRangeOccupied. Freeing it would return addresses to
// the source that something still holds, and the next tenant handed that range
// would be handed their addresses with it. A descendant range some other claim
// holds is refused for the same reason. Reservations do not count: they belong
// to the pool and are released with it.
//
// The whole walk runs in the caller's transaction, so a refusal at any depth
// leaves nothing released.
func ReleaseScopeRange(ctx context.Context, tx pgx.Tx, alloc PrefixAllocator, poolKey string) error {
	return releaseRangeTree(ctx, tx, alloc, poolKey, 0)
}

func releaseRangeTree(ctx context.Context, tx pgx.Tx, alloc PrefixAllocator, poolKey string, depth int) error {
	if depth > MaxClassChainDepth {
		return fmt.Errorf("release range %s: pool tree deeper than %d levels", poolKey, MaxClassChainDepth)
	}

	rows, err := tx.Query(ctx,
		`SELECT allocation_key, purpose, COALESCE(claim_key, '')
		   FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND purpose <> 'Reservation'`,
		poolKey)
	if err != nil {
		return fmt.Errorf("read what is inside %q: %w", poolKey, err)
	}
	var (
		children  []string
		occupants int
	)
	for rows.Next() {
		var allocationKey, purpose, claimKey string
		if err := rows.Scan(&allocationKey, &purpose, &claimKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan occupant of %q: %w", poolKey, err)
		}
		// A carve held by a claim is somebody's range, not derived space this
		// release may reclaim.
		if purpose == string(ipam.PurposePoolCarve) && claimKey == allocationKey {
			children = append(children, allocationKey)
			continue
		}
		occupants++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate occupants of %q: %w", poolKey, err)
	}
	if occupants > 0 {
		return fmt.Errorf("%w: %d allocation(s) inside %s", ErrScopeRangeOccupied, occupants, poolKey)
	}

	for _, child := range children {
		if err := releaseRangeTree(ctx, tx, alloc, child, depth+1); err != nil {
			return err
		}
	}

	if err := alloc.ReleasePoolReservations(ctx, tx, poolKey); err != nil {
		return fmt.Errorf("release range reservations: %w", err)
	}
	// By allocation_key, not by claim: the carve's claim binding is the thing
	// being torn down.
	if err := alloc.ReleaseAllocation(ctx, tx, poolKey); err != nil {
		return fmt.Errorf("release range carve: %w", err)
	}
	if _, err := alloc.DeleteObject(ctx, tx, poolKey); err != nil {
		return fmt.Errorf("delete range pool: %w", err)
	}
	return nil
}
