package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tracing"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// PostgresPrefixAllocator implements PrefixAllocator atop ipam_objects and
// ipam_cidr_allocations. It performs the synchronous allocation sequence
// described in the architecture:
//
//	BEGIN
//	  SELECT data FROM ipam_objects WHERE key=$poolKey FOR UPDATE
//	  SELECT allocated_cidr FROM ipam_cidr_allocations WHERE pool_key=$poolKey
//	  -- in-Go: FindFirstAvailableBlock(parents, existing, prefixLen, strategy)
//	  INSERT INTO ipam_cidr_allocations (...)
//	COMMIT
//
// The pool row's lock is what serialises concurrent claims; the
// ipam_cidr_allocations rows are not individually locked, so the work is
// O(existing) per allocation rather than O(pool size).
type PostgresPrefixAllocator struct{}

// NewPostgresPrefixAllocator returns a stateless allocator.
func NewPostgresPrefixAllocator() *PostgresPrefixAllocator {
	return &PostgresPrefixAllocator{}
}

// AllocatePrefix implements PrefixAllocator.AllocatePrefix.
func (a *PostgresPrefixAllocator) AllocatePrefix(ctx context.Context, tx pgx.Tx, req PrefixRequest) (string, error) {
	poolKey, prefixLen := req.PoolKey, req.PrefixLen

	// Normalised once, here, and read from req by both the search and the
	// insert. A block searched under one digest and recorded under another
	// would not block the allocations it overlaps.
	if req.ScopeDigest == "" {
		req.ScopeDigest = scope.EmptyAddressSpaceDigest()
	}

	pool, err := lockAndDecodeIPPool(ctx, tx, poolKey)
	if err != nil {
		return "", err
	}

	parents, err := parsePoolCIDR(pool)
	if err != nil {
		return "", err
	}

	if err := materialiseReservations(ctx, tx, poolKey, pool, parents); err != nil {
		return "", err
	}

	var cidr *net.IPNet
	if req.Address != "" {
		// A named block replaces the search, and only the search: the pool row
		// is already locked and its reservations materialised above, so the
		// overlap check below sees every row a search would have, and
		// everything after it is the same write a searched block gets.
		cidr, err = requestedBlock(ctx, tx, req, parents)
		if err != nil {
			return "", err
		}
	} else {
		strategy := allocation.Strategy(pool.Spec.Allocation.Strategy)
		var fbSpan trace.Span
		ctx, fbSpan = tracing.Tracer().Start(ctx, tracing.SpanFindBlock)
		fbSpan.SetAttributes(attribute.String(tracing.AttrStrategy, string(strategy)))

		cidr, err = findBlock(ctx, tx, poolKey, req.ScopeDigest, parents, prefixLen, strategy)
		if err != nil {
			if errors.Is(err, allocation.ErrPoolExhausted) {
				fbSpan.SetAttributes(attribute.Bool(tracing.AttrExhausted, true))
				fbSpan.SetStatus(codes.Error, "pool exhausted")
				fbSpan.End()
				return "", ErrPoolExhausted
			}
			fbSpan.SetStatus(codes.Error, err.Error())
			fbSpan.End()
			return "", fmt.Errorf("compute next prefix: %w", err)
		}
		fbSpan.SetAttributes(attribute.String(tracing.AttrResultCIDR, cidr.String()))
		fbSpan.End()
	}

	// Before the insert, so the overlap query does not return the new block.
	consumed, err := consumptionAfterAllocate(ctx, tx, poolKey, parents, *cidr)
	if err != nil {
		return "", err
	}

	if err := insertPrefixAllocation(ctx, tx, req, cidr.String()); err != nil {
		return "", err
	}

	if err := writeConsumed(ctx, tx, poolKey, consumed); err != nil {
		return "", err
	}
	if err := persistPoolCapacity(ctx, tx, pool, poolKey, parents, consumed); err != nil {
		return "", fmt.Errorf("update pool capacity after allocation: %w", err)
	}

	klog.V(2).InfoS("Allocated prefix", "pool", poolKey, "cidr", cidr.String(), "claim", req.ClaimKey, "ownerProject", req.OwnerProject, "requested", req.Address != "")
	return cidr.String(), nil
}

// InsertObject implements PrefixAllocator.InsertObject.
func (a *PostgresPrefixAllocator) InsertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error) {
	return insertObject(ctx, tx, key, kind, namespace, name, data)
}

// insertObject is the shared helper used by both PrefixAllocator and
// ASNAllocator implementations. The RETURNING clause hands back the rv that
// the sequence default assigned, so the caller can stamp it on the in-memory
// object before responding to the API client. The changelog row is inserted
// in the same tx so the watcher sees the new object on the next poll.
func insertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error) {
	defer metrics.ObserveQuery("insert_object", time.Now())
	labels := labelsFromData(data)
	if k := kindFromData(data); k != "" {
		kind = k
	}
	var rv int64
	err := tx.QueryRow(ctx,
		`INSERT INTO ipam_objects (key, kind, namespace, name, data, labels)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING resource_version`,
		key, kind, namespace, name, data, labels,
	).Scan(&rv)
	if err != nil {
		return 0, fmt.Errorf("insert object %q: %w", key, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ipam_changelog (key, resource_version, event_type, data)
		 VALUES ($1, $2, 'ADDED', $3)`,
		key, rv, data,
	); err != nil {
		return 0, fmt.Errorf("insert changelog for %q: %w", key, err)
	}
	return rv, nil
}

// kindFromData reads the kind out of a JSON-encoded API object. The document is
// the one source of the kind that no request path can clear, so the column
// always agrees with the row it describes.
func kindFromData(data []byte) string {
	var obj struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}
	return obj.Kind
}

// labelsFromData extracts metadata.labels from a JSON-encoded API object.
// Used by insertObject to populate the labels jsonb column without importing
// the codec or runtime packages into the allocator.
func labelsFromData(data []byte) []byte {
	var obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &obj); err != nil || len(obj.Metadata.Labels) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(obj.Metadata.Labels)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// Release implements PrefixAllocator.Release.
//
// Retention runs first, so the delete that follows sees only the rows that
// really are being freed. Both run under the locks of every pool the claim
// holds a row in, taken before either touches a row.
func (a *PostgresPrefixAllocator) Release(ctx context.Context, tx pgx.Tx, claimKey string) ([]string, error) {
	pools, err := lockPoolsOf(ctx, tx, "claim_key = $1", claimKey)
	if err != nil {
		return nil, err
	}
	retained, err := retainAllocations(ctx, tx, claimKey)
	if err != nil {
		return nil, err
	}
	if _, err := deleteLockedAllocations(ctx, tx, pools, "claim_key = $1", claimKey); err != nil {
		return nil, err
	}
	return retained, nil
}

// ReleaseAllocation implements PrefixAllocator.ReleaseAllocation.
func (a *PostgresPrefixAllocator) ReleaseAllocation(ctx context.Context, tx pgx.Tx, allocationKey string) error {
	return deleteAllocations(ctx, tx, "allocation_key = $1", allocationKey)
}

// ReleaseRetainedAllocation implements PrefixAllocator.ReleaseRetainedAllocation.
//
// The binding is read from the row, in the DELETE's own predicate, never from
// the IPAllocation object: the object is a copy a caller can write, and a copy
// that said "unbound" over a bound row would hand the address out twice.
func (a *PostgresPrefixAllocator) ReleaseRetainedAllocation(ctx context.Context, tx pgx.Tx, allocationKey string) error {
	pools, err := lockPoolsOf(ctx, tx, "allocation_key = $1", allocationKey)
	if err != nil {
		return err
	}
	freed, err := deleteLockedAllocations(ctx, tx, pools,
		"allocation_key = $1 AND claim_key IS NULL AND purpose = 'Claim'", allocationKey)
	if err != nil {
		return err
	}
	if freed > 0 {
		return nil
	}

	// Nothing freed: either there is no row, which leaves only the object to
	// remove, or the row is held.
	var claimKey *string
	err = tx.QueryRow(ctx,
		`SELECT claim_key FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocationKey,
	).Scan(&claimKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read allocation %q: %w", allocationKey, err)
	}
	holder := ""
	if claimKey != nil {
		holder = *claimKey
	}
	return &AllocationBoundError{AllocationKey: allocationKey, ClaimKey: holder}
}

// ReleasePoolReservations implements PrefixAllocator.ReleasePoolReservations.
//
// The pool is known, so it is locked by key rather than discovered from its
// rows: a reservation materialised after a discovery read would otherwise be
// deleted out from under the allocation that wrote it.
func (a *PostgresPrefixAllocator) ReleasePoolReservations(ctx context.Context, tx pgx.Tx, poolKey string) error {
	pool, err := lockAndDecodeIPPool(ctx, tx, poolKey)
	if err != nil && !errors.Is(err, ErrPoolNotFound) {
		return err
	}
	_, err = deleteLockedAllocations(ctx, tx, map[string]*ipamv1alpha1.IPPool{poolKey: pool},
		"pool_key = $1 AND purpose = 'Reservation'", poolKey)
	return err
}

// ReleaseChildPool frees what a child pool holds: the positions it reserves
// for itself and the block it occupies in its parent. The pool object is the
// caller's to delete, in the same transaction.
func ReleaseChildPool(ctx context.Context, tx pgx.Tx, alloc PrefixAllocator, poolKey string) error {
	if err := LockPoolLineage(ctx, tx, poolKey); err != nil {
		return fmt.Errorf("lock child pool and its parent: %w", err)
	}
	if err := alloc.ReleasePoolReservations(ctx, tx, poolKey); err != nil {
		return fmt.Errorf("release child-pool reservations: %w", err)
	}
	if _, err := alloc.Release(ctx, tx, poolKey); err != nil {
		return fmt.Errorf("release child-pool allocation: %w", err)
	}
	return nil
}

// LockPoolLineage locks the pool poolKey is carved from, then poolKey itself.
//
// A transaction that writes rows in more than one pool must lock them ancestor
// first, which is the order the cascade, PlaceScopeRange and ReleaseScopeRange
// take them in. Deleting a child pool frees its own reservations and then its
// place in its parent; done in that order, call by call, it locked the child
// before the parent, and deadlocked against a scope-range release that held
// the parent and wanted the child. ReleaseChildPool calls this first, so both
// locks are taken in the right order up front, and the release calls that
// follow re-lock what they already hold.
func LockPoolLineage(ctx context.Context, tx pgx.Tx, poolKey string) error {
	var source string
	err := tx.QueryRow(ctx,
		`SELECT pool_key FROM ipam_cidr_allocations WHERE allocation_key = $1`, poolKey,
	).Scan(&source)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("find the source of %q: %w", poolKey, err)
	}
	if source != "" {
		if err := lockPoolRow(ctx, tx, source); err != nil {
			return err
		}
	}
	return lockPoolRow(ctx, tx, poolKey)
}

// lockPoolsOf locks the pool row of every allocation matching cond, which must
// be a single-parameter predicate, and returns the decoded pools by key. A pool
// that no longer exists maps to nil.
//
// The locks are taken before any allocation row is written. AllocatePrefix
// locks the pool and then writes rows, so a path that wrote a row and then
// locked its pool would hold what an allocation waits for while waiting for
// what the allocation holds: Postgres breaks that cycle by killing one side
// with 40P01, and the caller sees a 500.
//
// Every predicate used with it identifies at most one row, so it locks at most
// one pool. The sort is a tie-break, not a lock order: key order is not
// ancestor order, and a transaction that locks several pools must take them
// ancestor first itself, before calling this (see LockPoolLineage).
//
// The read that finds the pools takes no lock, so a matching row committed
// after it can sit in a pool missing from the map. deleteLockedAllocations
// never deletes such a row: it is restricted to the pools locked here.
func lockPoolsOf(ctx context.Context, tx pgx.Tx, cond string, arg any) (map[string]*ipamv1alpha1.IPPool, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT pool_key FROM ipam_cidr_allocations WHERE `+cond, arg)
	if err != nil {
		return nil, fmt.Errorf("find pools to lock: %w", err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pool to lock: %w", err)
		}
		keys = append(keys, key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pools to lock: %w", err)
	}
	// Sorted here rather than by ORDER BY, so the order is the same bytewise
	// order whatever the database's collation.
	slices.Sort(keys)

	pools := make(map[string]*ipamv1alpha1.IPPool, len(keys))
	for _, key := range keys {
		pool, err := lockAndDecodeIPPool(ctx, tx, key)
		if err != nil {
			if errors.Is(err, ErrPoolNotFound) {
				pools[key] = nil
				continue
			}
			return nil, err
		}
		pools[key] = pool
	}
	return pools, nil
}

// retainAllocations unbinds the claim from the rows it holds under reclaim
// policy Retain and returns their allocation keys. The caller holds the locks
// of the pools those rows are in.
//
// Nothing else happens, and that is the point: the address is still held, so
// consumption, pool capacity, the utilization gauges and the search floor are
// all unchanged. retained_at is stamped by the ipam_set_retained_at trigger on
// this exact transition, so it is never written here.
func retainAllocations(ctx context.Context, tx pgx.Tx, claimKey string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`UPDATE ipam_cidr_allocations SET claim_key = NULL
		  WHERE claim_key = $1 AND reclaim_policy = 'Retain'
		 RETURNING allocation_key`, claimKey,
	)
	if err != nil {
		return nil, fmt.Errorf("retain allocation: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan retained allocation: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retained allocations: %w", err)
	}
	return keys, nil
}

// deleteAllocations frees every allocation row matching cond, which must be a
// single-parameter predicate, after locking the pools those rows are in.
func deleteAllocations(ctx context.Context, tx pgx.Tx, cond string, arg any) error {
	pools, err := lockPoolsOf(ctx, tx, cond, arg)
	if err != nil {
		return err
	}
	_, err = deleteLockedAllocations(ctx, tx, pools, cond, arg)
	return err
}

// deleteLockedAllocations frees every allocation row matching cond in one of
// the pools in pools, and republishes the capacity of each pool it touched,
// returning how many rows it freed. pools holds the pools the caller has
// locked; a row in any other pool is left alone, because deleting it would
// write a pool this transaction does not hold.
//
// RETURNING surfaces the pool_key/ip_family so the post-release utilization
// gauge can be refreshed without an extra round-trip on the read path. Pool
// rows that have already been hard-deleted (orphaned allocations) have nothing
// to publish; in that case the gauge update is silently skipped.
func deleteLockedAllocations(ctx context.Context, tx pgx.Tx, pools map[string]*ipamv1alpha1.IPPool, cond string, arg any) (int, error) {
	locked := make([]string, 0, len(pools))
	for key := range pools {
		locked = append(locked, key)
	}
	rows, err := tx.Query(ctx,
		`DELETE FROM ipam_cidr_allocations WHERE (`+cond+`) AND pool_key = ANY($2::text[])
		 RETURNING pool_key, ip_family,
		           host(allocated_cidr) || '/' || masklen(allocated_cidr)`, arg, locked,
	)
	if err != nil {
		return 0, fmt.Errorf("release prefix: %w", err)
	}
	type released struct {
		poolKey  string
		ipFamily string
		cidr     string
	}
	var releases []released
	for rows.Next() {
		var r released
		if err := rows.Scan(&r.poolKey, &r.ipFamily, &r.cidr); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan released allocation: %w", err)
		}
		releases = append(releases, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate released allocations: %w", err)
	}

	for _, r := range releases {
		// Pool already gone (cascading delete); nothing to publish.
		pool := pools[r.poolKey]
		if pool == nil {
			continue
		}
		parents, perr := parsePoolCIDR(pool)
		if perr != nil {
			return 0, fmt.Errorf("parse pool cidr after release: %w", perr)
		}
		_, block, perr := net.ParseCIDR(r.cidr)
		if perr != nil {
			return 0, fmt.Errorf("parse released cidr %q: %w", r.cidr, perr)
		}
		// After the delete, so the overlap query sees what still holds these
		// addresses.
		consumed, perr := consumptionAfterRelease(ctx, tx, r.poolKey, parents, *block)
		if perr != nil {
			return 0, perr
		}
		if perr := writeConsumed(ctx, tx, r.poolKey, consumed); perr != nil {
			return 0, perr
		}
		if perr := persistPoolCapacity(ctx, tx, pool, r.poolKey, parents, consumed); perr != nil {
			return 0, fmt.Errorf("update pool capacity after release: %w", perr)
		}
	}
	return len(releases), nil
}

// persistPoolCapacity recomputes Total/Allocated/Available for the pool and
// writes the updated pool object back to ipam_objects (+ MODIFIED changelog)
// within the current transaction. Must be called inside the transaction that
// inserted or deleted the allocation row so the capacity stays consistent.
func persistPoolCapacity(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, poolKey string, parents []net.IPNet, consumed *big.Int) error {
	setPoolCapacityStatus(pool, parents, consumed)
	data, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("marshal pool: %w", err)
	}
	if _, err := updateObject(ctx, tx, poolKey, data); err != nil {
		return fmt.Errorf("write pool: %w", err)
	}
	return nil
}

// setPoolCapacityStatus populates the pool's capacity and utilization from its
// parent ranges and current allocations.
//
// Every figure comes from one Measure, so the counts and the percentage cannot
// disagree. The counts are exact decimal strings: an IPv6 /20 holds 2^108
// addresses, which no int64 can express.
// setPoolCapacityStatus writes the capacity figures from the pool's running
// consumption total. Every figure derives from that one number, so status and
// the published metric cannot disagree.
func setPoolCapacityStatus(pool *ipamv1alpha1.IPPool, parents []net.IPNet, consumed *big.Int) {
	total := totalAddresses(parents)
	free := new(big.Int).Sub(total, consumed)
	if free.Sign() < 0 {
		free = new(big.Int)
	}
	pool.Status.Capacity = ipamv1alpha1.PoolCapacity{
		Total:     total.String(),
		Allocated: consumed.String(),
		Available: free.String(),
	}
	pool.Status.UtilizationPercent = utilizationPercent(total, consumed)
	pool.Status.IPFamily = ipamv1alpha1.IPFamily(effectivePoolFamily(pool))
}

// effectivePoolFamily returns a pool's address family: spec.ipFamily on root
// pools, otherwise derived from the carved status.allocatedCIDR. Returns the
// empty string when neither is resolvable.
func effectivePoolFamily(pool *ipamv1alpha1.IPPool) string {
	if pool.Spec.IPFamily != "" {
		return string(pool.Spec.IPFamily)
	}
	cidr := pool.Status.AllocatedCIDR
	if cidr == "" {
		cidr = pool.Spec.CIDR
	}
	if cidr == "" {
		return ""
	}
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	if ip.To4() != nil {
		return string(ipamv1alpha1.IPv4)
	}
	return string(ipamv1alpha1.IPv6)
}

// DeleteObject implements PrefixAllocator.DeleteObject.
func (a *PostgresPrefixAllocator) DeleteObject(ctx context.Context, tx pgx.Tx, key string) (int64, error) {
	return deleteObject(ctx, tx, key)
}

// GetObject implements PrefixAllocator.GetObject.
func (a *PostgresPrefixAllocator) GetObject(ctx context.Context, tx pgx.Tx, key string) ([]byte, error) {
	var data []byte
	err := tx.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, key).Scan(&data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrObjectNotFound, key)
		}
		return nil, fmt.Errorf("read object %q: %w", key, err)
	}
	return data, nil
}

// UpdateObject implements PrefixAllocator.UpdateObject.
func (a *PostgresPrefixAllocator) UpdateObject(ctx context.Context, tx pgx.Tx, key string, data []byte) (int64, error) {
	return updateObject(ctx, tx, key, data)
}

// updateObject rewrites the data column for an existing ipam_objects row,
// allocates a fresh resource_version from ipam_resource_version_seq, and
// inserts a MODIFIED changelog entry in the same transaction so watchers
// observe the update. Returns pgx.ErrNoRows wrapped if the key does not
// exist — the AllocatingREST Delete handlers always Get before Update so
// this branch indicates a concurrent delete the caller must surface.
func updateObject(ctx context.Context, tx pgx.Tx, key string, data []byte) (int64, error) {
	defer metrics.ObserveQuery("update_object", time.Now())
	labels := labelsFromData(data)
	var rv int64
	err := tx.QueryRow(ctx,
		`UPDATE ipam_objects
		    SET resource_version = nextval('ipam_resource_version_seq'),
		        data = $1,
		        labels = $2,
		        updated_at = NOW()
		  WHERE key = $3
		  RETURNING resource_version`,
		data, labels, key,
	).Scan(&rv)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("update object %q: not found", key)
		}
		return 0, fmt.Errorf("update object %q: %w", key, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ipam_changelog (key, resource_version, event_type, data)
		 VALUES ($1, $2, 'MODIFIED', $3)`,
		key, rv, data,
	); err != nil {
		return 0, fmt.Errorf("insert changelog for %q: %w", key, err)
	}
	return rv, nil
}

// deleteObject is the shared helper used by both allocator implementations.
// A single CTE atomically removes the ipam_objects row and inserts a DELETED
// changelog entry carrying the pre-delete payload as a tombstone. If the
// object was already gone the inner SELECT returns no rows, the INSERT
// inserts nothing, and QueryRow returns pgx.ErrNoRows — which we map to a
// (0, nil) return so callers can distinguish "already deleted" from a real
// failure.
func deleteObject(ctx context.Context, tx pgx.Tx, key string) (int64, error) {
	var rv int64
	err := tx.QueryRow(ctx,
		`WITH deleted AS (
		     DELETE FROM ipam_objects WHERE key = $1 RETURNING data
		 )
		 INSERT INTO ipam_changelog (key, resource_version, event_type, data)
		 SELECT $1, nextval('ipam_resource_version_seq'), 'DELETED', data FROM deleted
		 RETURNING resource_version`,
		key,
	).Scan(&rv)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already gone; emit nothing rather than a DELETE for a key the
			// watcher never saw an ADD for.
			return 0, nil
		}
		return 0, fmt.Errorf("delete object %q: %w", key, err)
	}
	return rv, nil
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// lockAndDecodeIPPool acquires a row-level lock on the pool row in
// ipam_objects and decodes its data column as an IPPool. Status.AllocatedCIDR
// is preferred (populated for child pools after provisioning); Spec.CIDR is
// the fallback used by root pools whose CIDR is operator-supplied.
func lockAndDecodeIPPool(ctx context.Context, tx pgx.Tx, poolKey string) (*ipamv1alpha1.IPPool, error) {
	defer metrics.ObserveQuery("select_pool_for_update", time.Now())
	var data []byte
	err := tx.QueryRow(ctx,
		`SELECT data FROM ipam_objects WHERE key = $1 FOR UPDATE`,
		poolKey,
	).Scan(&data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPoolNotFound
		}
		return nil, fmt.Errorf("lock pool row: %w", err)
	}

	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, fmt.Errorf("decode pool object: %w", err)
	}
	return &pool, nil
}

// parsePoolCIDR returns the parent CIDR (single-element slice). IPPool
// pools always have a single CIDR; the slice form matches
// allocation.FindFirstAvailableBlock's parameter shape.
func parsePoolCIDR(pool *ipamv1alpha1.IPPool) ([]net.IPNet, error) {
	cidrStr := pool.Spec.CIDR
	if pool.Status.AllocatedCIDR != "" {
		cidrStr = pool.Status.AllocatedCIDR
	}
	_, ipnet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return nil, fmt.Errorf("parse pool CIDR %q: %w", cidrStr, err)
	}
	return []net.IPNet{*ipnet}, nil
}

// loadAllocationsInSpace returns the blocks a new allocation in scopeDigest
// must avoid: the claims of that space, plus the reservations and pool carves
// that hold their address in every space.
//
// searchFilter is the same predicate the paged search applies, so the two
// strategy paths see one set of occupied blocks.
func loadAllocationsInSpace(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string) ([]net.IPNet, error) {
	defer metrics.ObserveQuery("load_allocations_in_space", time.Now())
	return queryAllocationCIDRs(ctx, tx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE `+searchFilter,
		poolKey, scopeDigest)
}

// loadExistingAllocations returns the CIDRs currently tracked against poolKey,
// across every address space in it. It measures occupancy, which counts an
// address once however many spaces hold it; it is not a search.
func loadExistingAllocations(ctx context.Context, tx pgx.Tx, poolKey string) ([]net.IPNet, error) {
	defer metrics.ObserveQuery("load_existing_allocations", time.Now())
	return queryAllocationCIDRs(ctx, tx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE pool_key = $1`,
		poolKey)
}

// insertPrefixAllocation records a new allocation row.
//
// class_name and reclaim_policy are written from the request, not left to the
// column defaults: a retained row is read after its claim is gone, and what it
// was handed out under has to be readable from the row itself.
//
// scope_digest is the digest findBlock searched under, so the row lands in the
// address space the search proved free.
func insertPrefixAllocation(ctx context.Context, tx pgx.Tx, req PrefixRequest, cidr string) error {
	defer metrics.ObserveQuery("insert_allocation", time.Now())
	allocationKey := req.AllocationKey
	if allocationKey == "" {
		allocationKey = req.ClaimKey
	}
	policy := req.ReclaimPolicy
	if policy == "" {
		policy = ipamv1alpha1.ReclaimDelete
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family,
		     purpose, class_name, scope_digest, reclaim_policy, owner_project)
		 VALUES ($1, $2, $3, $4, $5, 'Claim', $6, $7, $8, $9)`,
		req.PoolKey, cidr, req.ClaimKey, allocationKey, req.IPFamily,
		req.ClassName, req.ScopeDigest, string(policy), req.OwnerProject,
	)
	if err != nil {
		// A named block was checked free under the pool's lock, so this
		// means a writer the lock does not exclude got there first. It is
		// the same refusal the check would have given, not a server fault.
		if req.Address != "" && isOverlapViolation(err) {
			return &AddressInUseError{Address: cidr}
		}
		return fmt.Errorf("insert allocation: %w", err)
	}
	return nil
}

// overlapConstraint is the exclusion constraint that keeps two allocations of
// one address space in one pool from overlapping.
const overlapConstraint = "ipam_cidr_alloc_no_overlap"

// isOverlapViolation reports whether err is that constraint refusing a row
// (SQLSTATE 23P01, exclusion_violation).
func isOverlapViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01" && pgErr.ConstraintName == overlapConstraint
}

// totalAddresses counts the addresses the pool's parents cover.
func totalAddresses(parents []net.IPNet) *big.Int {
	total := new(big.Int)
	for _, p := range parents {
		ones, bits := p.Mask.Size()
		if hostBits := bits - ones; hostBits >= 0 {
			total.Add(total, new(big.Int).Lsh(big.NewInt(1), uint(hostBits)))
		}
	}
	return total
}

// utilizationPercent reports consumed as a share of total, to four decimal
// places. An integer percent is useless at these sizes: 256 addresses out of a
// /12 is 0.024%, which truncates to zero and reads as an empty pool.
func utilizationPercent(total, consumed *big.Int) float64 {
	if total.Sign() == 0 {
		return 0
	}
	ratio := new(big.Float).Quo(new(big.Float).SetInt(consumed), new(big.Float).SetInt(total))
	pct, _ := ratio.Float64()
	return math.Round(pct*100*10000) / 10000
}

// findBlock chooses the block to hand out within one address space.
//
// Both strategy paths search under scopeDigest, so a pool whose strategy
// changes hands out the same set of blocks. A whole-pool search would report a
// pool exhausted on the strength of allocations another address space holds.
//
// FirstFit reads the allocations a page at a time, from a floor recording where
// the last search stopped, so the read is bounded by what the search examines.
// The other strategies are defined over the whole pool — "the smallest region
// that fits" and "the emptiest parent" cannot be known from a prefix of the
// allocations — so they load the space and keep its cost.
func findBlock(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, parents []net.IPNet, prefixLen int, strategy allocation.Strategy) (*net.IPNet, error) {
	if strategy != "" && strategy != allocation.FirstFit {
		existing, err := loadAllocationsInSpace(ctx, tx, poolKey, scopeDigest)
		if err != nil {
			return nil, err
		}
		return allocation.FindFirstAvailableBlock(parents, existing, prefixLen, strategy)
	}

	if scopeDigest == spaceAll {
		// A search over every space has no floor to stand on. One would have to
		// be lowered whenever any space released, and the trigger that lowers
		// floors moves only the released row's own space — leaving a floor
		// above free addresses, the one direction that loses them.
		cidr, _, err := boundedFirstFit(ctx, tx, poolKey, spaceAll, parents, prefixLen, nil)
		return cidr, err
	}

	// After the pool row, never before it. Every caller takes (pool, floor) in
	// that order, so two transactions can only deadlock by taking POOLS in
	// different orders.
	observed, err := lockSearchFloor(ctx, tx, poolKey, scopeDigest, parents[0].IP)
	if err != nil {
		return nil, err
	}

	cidr, next, err := boundedFirstFit(ctx, tx, poolKey, scopeDigest, parents, prefixLen, observed)
	if err != nil {
		return nil, err
	}
	if err := raiseSearchFloor(ctx, tx, poolKey, scopeDigest, observed, next); err != nil {
		return nil, err
	}
	return cidr, nil
}
