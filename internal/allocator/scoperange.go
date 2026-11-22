package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
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

// ErrSharedPoolUnplaced is returned when a ScopeRange claim names its range
// and a level of the chain that would have to be created is shared by every
// consumer of its class. One consumer naming where a shared pool goes would be
// choosing for all of them, so only a per-consumer level is placed; a shared
// level must already exist and contain the range.
var ErrSharedPoolUnplaced = errors.New("ipam: a shared pool cannot be placed by one consumer")

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

	source, err := discoverInTx(ctx, db, levels[0].Class, claimScope, nil)
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
//
// Every pool the release will write is locked first, root-first: the source
// the range is carved from, the range, then its descendants level by level.
// That is the order the cascade and PlaceScopeRange take pool locks in, an
// ancestor before any descendant, so a release racing a placement or a claim
// into the same tree waits for it rather than deadlocking against it. The walk
// below then frees leaf-first, holding locks it already has.
func ReleaseScopeRange(ctx context.Context, tx pgx.Tx, alloc PrefixAllocator, poolKey string) error {
	if err := lockRangeTree(ctx, tx, poolKey); err != nil {
		return err
	}
	return releaseRangeTree(ctx, tx, alloc, poolKey, 0)
}

// lockRangeTree takes the row lock on poolKey's source pool, poolKey, and
// every unheld pool provisioned beneath it, root-first. A pool's children are
// read only once it is locked: nothing can carve a new child from a pool
// without its lock, so the set read is the set the release will see.
func lockRangeTree(ctx context.Context, tx pgx.Tx, poolKey string) error {
	var source string
	err := tx.QueryRow(ctx,
		`SELECT pool_key FROM ipam_cidr_allocations
		  WHERE allocation_key = $1 AND purpose = 'PoolCarve'`, poolKey).Scan(&source)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("find the source of %q: %w", poolKey, err)
	}
	if source != "" {
		if err := lockPoolRow(ctx, tx, source); err != nil {
			return err
		}
	}

	level := []string{poolKey}
	for depth := 0; len(level) > 0; depth++ {
		if depth > MaxClassChainDepth {
			return fmt.Errorf("lock range %s: pool tree deeper than %d levels", poolKey, MaxClassChainDepth)
		}
		var next []string
		for _, key := range level {
			if err := lockPoolRow(ctx, tx, key); err != nil {
				return err
			}
			children, err := unheldChildPools(ctx, tx, key)
			if err != nil {
				return err
			}
			next = append(next, children...)
		}
		level = next
	}
	return nil
}

func lockPoolRow(ctx context.Context, tx pgx.Tx, poolKey string) error {
	defer metrics.ObserveQuery("select_pool_for_update", time.Now())
	if _, err := tx.Exec(ctx, `SELECT 1 FROM ipam_objects WHERE key = $1 FOR UPDATE`, poolKey); err != nil {
		return fmt.Errorf("lock pool %q: %w", poolKey, err)
	}
	return nil
}

// unheldChildPools returns the pools carved from poolKey that no claim holds:
// the derived pools a release takes with it.
func unheldChildPools(ctx context.Context, tx pgx.Tx, poolKey string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT allocation_key FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND purpose = 'PoolCarve' AND claim_key = allocation_key
		  ORDER BY allocation_key`, poolKey)
	if err != nil {
		return nil, fmt.Errorf("list pools carved from %q: %w", poolKey, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan pool carved from %q: %w", poolKey, err)
		}
		out = append(out, key)
	}
	return out, rows.Err()
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

// PlaceScopeRange is ResolveScopeRange for a claim that names its range: every
// level of the chain is found or created so that it holds want, and want
// becomes the class's own pool. It returns that pool's key.
//
// Walked root-first, a level is one of:
//
//   - present and containing want: reused, exactly as an unaddressed claim
//     would reuse it;
//   - present elsewhere: ErrAddressOutsidePool, naming the pool and its range.
//     A pool is never renumbered, so a range placed before this claim, by
//     first-free or by another claim, stays where it is. The class's own pool
//     must equal want, not merely contain it;
//   - missing and per-consumer: carved at the block of its size containing
//     want, out of the level above. A collision there is the AddressInUse or
//     AddressReserved a named claim gets, from the same classification;
//   - missing and shared: ErrSharedPoolUnplaced.
//
// The root level's source is the operator-authored pool that contains the
// root's block (DiscoverPoolFor). want must already be the class's pool size
// and aligned, which allocation.RequestedBlock establishes.
//
// Unlike ResolveScopeRange every level is written in the caller's transaction,
// so a refusal at any depth leaves nothing behind, and a dry-run that rolls
// back has placed nothing. The order of locks is the cascade's: per level, the
// identity row and then the source pool. A claim losing the identity race to
// a concurrent one reads the winner's pool and is held to it like any present
// level, so it is refused rather than placed elsewhere, and it never carves.
func PlaceScopeRange(ctx context.Context, tx pgx.Tx, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef, want net.IPNet) (string, error) {
	if len(leaf.Spec.PoolPer) == 0 {
		return "", fmt.Errorf("%w: class %q names no poolPer", ErrClassHoldsNoRange, leaf.Name)
	}

	start := time.Now()
	result, provisioned := "error", false
	defer func() { metrics.ObserveCascadeResolution(result, provisioned, start) }()

	levels, err := PlanScopeRangeCascade(ctx, tx, leaf, claimScope)
	if err != nil {
		return "", err
	}
	blocks, err := levelBlocks(levels, want)
	if err != nil {
		return "", err
	}

	// A level read as present can be released before its lock is taken: the
	// release holds the lock, commits, and the pool is gone. The walk is then
	// repeated from the top in a fresh savepoint, which sees the release and
	// places the level anew, rather than refusing a claim the next attempt
	// would satisfy.
	for attempt := 1; ; attempt++ {
		poolKey, created, err := placeChain(ctx, tx, levels, blocks, want, claimScope)
		if errors.Is(err, ErrPoolNotFound) && attempt < placeAttempts {
			klog.V(2).InfoS("A pool was released while a range was placed inside it; placing again",
				"class", leaf.Name, "attempt", attempt)
			continue
		}
		if err != nil {
			return "", err
		}
		provisioned = created
		result = "success"
		return poolKey, nil
	}
}

// placeAttempts bounds how many times a placement is repeated after a level
// it found was released underneath it.
const placeAttempts = 3

// placeChain is one walk of PlaceScopeRange, in a savepoint of tx so that a
// walk abandoned part-way leaves nothing and holds no lock.
func placeChain(ctx context.Context, tx pgx.Tx, levels []CascadeLevel, blocks []net.IPNet, want net.IPNet, claimScope map[string]ipam.ScopeRef) (string, bool, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin placement savepoint: %w", err)
	}
	source, err := DiscoverPoolFor(ctx, sp, levels[0].Class, claimScope, &blocks[0])
	if err != nil {
		_ = sp.Rollback(ctx)
		return "", false, err
	}
	provisioned := false
	for i := range levels {
		poolKey, created, err := placeLevel(ctx, sp, levels[i], source, blocks[i], want, i == len(levels)-1)
		if err != nil {
			_ = sp.Rollback(ctx)
			return "", false, fmt.Errorf("place pool for class %q: %w", levels[i].Class.Name, err)
		}
		provisioned = provisioned || created
		source = poolKey
	}
	if err := sp.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("release placement savepoint: %w", err)
	}
	return source, provisioned, nil
}

// levelBlocks returns, for each level, the block of that level's pool size
// containing want.
func levelBlocks(levels []CascadeLevel, want net.IPNet) ([]net.IPNet, error) {
	blocks := make([]net.IPNet, len(levels))
	for i, level := range levels {
		prefixLen, err := EffectivePrefixLength(level.Class.IPClass, nil)
		if err != nil {
			return nil, fmt.Errorf("resolve block size for class %q: %w", level.Class.Name, err)
		}
		block, err := allocation.ContainingBlock(want, prefixLen)
		if err != nil {
			return nil, fmt.Errorf("%w: class %q carves /%d pools, which cannot hold %s",
				ErrAddressOutsidePool, level.Class.Name, prefixLen, want.String())
		}
		blocks[i] = block
	}
	return blocks, nil
}

// placeLevel finds or creates one level's pool at block, reporting whether
// this call created it. leaf marks the class's own pool, which must be exactly
// want.
func placeLevel(ctx context.Context, tx pgx.Tx, level CascadeLevel, sourcePoolKey string, block, want net.IPNet, leaf bool) (poolKey string, provisioned bool, err error) {
	outcome := levelError
	defer func() { metrics.RecordCascadeLevel(level.Class.Name, outcome) }()

	existing, err := lookupPoolIdentity(ctx, tx, level.Class.Name, level.ScopeDigest)
	if err != nil {
		return "", false, err
	}
	if existing != "" {
		if err := checkPlaced(ctx, tx, level, existing, want, leaf); err != nil {
			return "", false, err
		}
		outcome = levelReused
		return existing, false, nil
	}

	// Before the identity row is claimed, so a refusal holds nothing.
	if level.Tenancy.Consumer == "" {
		return "", false, fmt.Errorf("%w: class %q holds one pool for this scope for every consumer, and it does not exist yet; "+
			"claim the range without spec.address to have it placed, then name ranges inside it",
			ErrSharedPoolUnplaced, level.Class.Name)
	}

	poolKey, won, err := claimPoolIdentity(ctx, tx, level.Class.Name, level.ScopeDigest, level.PoolKey)
	if err != nil {
		return "", false, err
	}
	if !won {
		// The winner committed its pool with the identity row. It is held to
		// the range like any pool that was already there.
		if err := checkPlaced(ctx, tx, level, poolKey, want, leaf); err != nil {
			return "", false, err
		}
		outcome = levelLost
		return poolKey, false, nil
	}

	if err := provisionPool(ctx, tx, level, sourcePoolKey, &block, tenant.FromContext(ctx).Name); err != nil {
		return "", false, err
	}
	outcome = levelProvisioned
	klog.V(2).InfoS("Placed pool at the range a claim named",
		"class", level.Class.Name, "pool", poolKey, "cidr", block.String(), "source", sourcePoolKey)
	return poolKey, true, nil
}

// checkPlaced holds an existing pool to the range a claim named: it must
// contain want, and when it is the class's own pool, be exactly want.
func checkPlaced(ctx context.Context, tx pgx.Tx, level CascadeLevel, poolKey string, want net.IPNet, leaf bool) error {
	parents, err := readPoolRange(ctx, tx, poolKey)
	if err != nil {
		return err
	}
	ok := allocation.IsBlockAvailable(parents, nil, want)
	if leaf {
		ok = len(parents) == 1 && parents[0].String() == want.String()
	}
	if ok {
		return nil
	}
	relation := "does not contain"
	if leaf {
		relation = "is not"
	}
	return fmt.Errorf("%w: class %q already placed IPPool %q for this scope at %s, which %s %s; a placed pool is never moved",
		ErrAddressOutsidePool, level.Class.Name, poolNameFromKey(poolKey), parentsString(parents), relation, want.String())
}

// readPoolRange reads a pool's range without locking its row. A provisioned
// pool's range is fixed when it is written, so nothing a lock would exclude
// can change it.
func readPoolRange(ctx context.Context, tx pgx.Tx, poolKey string) ([]net.IPNet, error) {
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, poolKey).Scan(&data); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPoolNotFound
		}
		return nil, fmt.Errorf("read pool %q: %w", poolKey, err)
	}
	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, fmt.Errorf("decode pool %q: %w", poolKey, err)
	}
	return parsePoolCIDR(&pool)
}
