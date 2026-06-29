package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// tenantsFromPoolKey derives (project, org) labels for utilization gauges
// from a pool key. The key encodes only the immediate parent (project for
// project-scoped pools), so org is "" today; see tenant.Identity.Org for the
// long-term plan to populate it via a forwarded org extra.
func tenantsFromPoolKey(poolKey string) (project, org string) {
	return tenant.ProjectFromKey(poolKey), ""
}

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
func (a *PostgresPrefixAllocator) AllocatePrefix(ctx context.Context, tx pgx.Tx, poolKey string, prefixLen int, ipFamily string, claimKey string, ownerProject string) (string, error) {
	pool, err := lockAndDecodeIPPool(ctx, tx, poolKey)
	if err != nil {
		return "", err
	}

	parents, err := parsePoolCIDR(pool)
	if err != nil {
		return "", err
	}

	existing, err := loadExistingAllocations(ctx, tx, poolKey)
	if err != nil {
		return "", err
	}

	cidr, err := allocation.FindFirstAvailableBlock(parents, existing, prefixLen, allocation.Strategy(pool.Spec.Allocation.Strategy))
	if err != nil {
		if errors.Is(err, allocation.ErrPoolExhausted) {
			return "", ErrPoolExhausted
		}
		return "", fmt.Errorf("compute next prefix: %w", err)
	}

	if err := insertPrefixAllocation(ctx, tx, poolKey, cidr.String(), claimKey, ipFamily, ownerProject); err != nil {
		return "", err
	}

	// Pool capacity hasn't changed and the new allocation joins the existing
	// set, so the post-allocation utilization can be computed from data
	// already in scope without an extra DB round-trip.
	updated := append(append([]net.IPNet(nil), existing...), *cidr)
	if err := persistPoolCapacity(ctx, tx, pool, poolKey, parents, updated); err != nil {
		return "", fmt.Errorf("update pool capacity after allocation: %w", err)
	}
	publishPrefixUtilization(poolKey, ipFamily, parents, updated)

	klog.V(2).InfoS("Allocated prefix", "pool", poolKey, "cidr", cidr.String(), "claim", claimKey, "ownerProject", ownerProject)
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
// RETURNING surfaces the pool_key/ip_family so the post-release utilization
// gauge can be refreshed without an extra round-trip on the read path. Pool
// rows that have already been hard-deleted (orphaned allocations) yield zero
// rows from RETURNING; in that case the gauge update is silently skipped.
func (a *PostgresPrefixAllocator) Release(ctx context.Context, tx pgx.Tx, claimKey string) error {
	rows, err := tx.Query(ctx,
		`DELETE FROM ipam_cidr_allocations WHERE claim_key = $1
		 RETURNING pool_key, ip_family`, claimKey,
	)
	if err != nil {
		return fmt.Errorf("release prefix: %w", err)
	}
	type released struct {
		poolKey  string
		ipFamily string
	}
	var releases []released
	for rows.Next() {
		var r released
		if err := rows.Scan(&r.poolKey, &r.ipFamily); err != nil {
			rows.Close()
			return fmt.Errorf("scan released allocation: %w", err)
		}
		releases = append(releases, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate released allocations: %w", err)
	}

	for _, r := range releases {
		pool, perr := lockAndDecodeIPPool(ctx, tx, r.poolKey)
		if perr != nil {
			// Pool already gone (cascading delete); nothing to publish.
			if errors.Is(perr, ErrPoolNotFound) {
				continue
			}
			return fmt.Errorf("reload pool after release: %w", perr)
		}
		parents, perr := parsePoolCIDR(pool)
		if perr != nil {
			return fmt.Errorf("parse pool cidr after release: %w", perr)
		}
		remaining, perr := loadExistingAllocations(ctx, tx, r.poolKey)
		if perr != nil {
			return fmt.Errorf("reload allocations after release: %w", perr)
		}
		if perr := persistPoolCapacity(ctx, tx, pool, r.poolKey, parents, remaining); perr != nil {
			return fmt.Errorf("update pool capacity after release: %w", perr)
		}
		publishPrefixUtilization(r.poolKey, r.ipFamily, parents, remaining)
	}
	return nil
}

// persistPoolCapacity recomputes Total/Allocated/Available for the pool and
// writes the updated pool object back to ipam_objects (+ MODIFIED changelog)
// within the current transaction. Must be called inside the transaction that
// inserted or deleted the allocation row so the capacity stays consistent.
func persistPoolCapacity(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, poolKey string, parents, allocations []net.IPNet) error {
	setPoolCapacityStatus(pool, parents, allocations)
	data, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("marshal pool: %w", err)
	}
	if _, err := updateObject(ctx, tx, poolKey, data); err != nil {
		return fmt.Errorf("write pool: %w", err)
	}
	return nil
}

// setPoolCapacityStatus populates every utilization field on the pool's status
// from the parent ranges and current allocations. The integer Capacity counts
// saturate cleanly for address spaces wider than int64 (Total caps at MaxInt64,
// Allocated/Available never go negative); LargestFreePrefix and
// UtilizationPercent are computed with arbitrary-precision arithmetic and are
// the accurate signal for IPv6. IPFamily reflects the pool's effective family.
func setPoolCapacityStatus(pool *ipamv1alpha1.IPPool, parents, allocations []net.IPNet) {
	const maxInt64 = int64(math.MaxInt64)
	var total int64
	for _, p := range parents {
		c := allocation.CountAddresses(p)
		if total > maxInt64-c {
			total = maxInt64
			break
		}
		total += c
	}
	var allocated int64
	for _, a := range allocations {
		c := allocation.CountAddresses(a)
		if allocated > maxInt64-c {
			allocated = maxInt64
			break
		}
		allocated += c
	}
	allocated = min(allocated, total)
	available := max(total-allocated, 0)
	pool.Status.Capacity = ipamv1alpha1.PoolCapacity{
		Total:     total,
		Allocated: allocated,
		Available: available,
	}

	pool.Status.UtilizationPercent = int32(allocation.UtilizationPercent(parents, allocations))
	if prefixLen, ok := allocation.LargestFreePrefixLen(parents, allocations); ok {
		pool.Status.LargestFreePrefix = int32(prefixLen)
	} else {
		pool.Status.LargestFreePrefix = 0
	}
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

// loadExistingAllocations returns the CIDRs currently tracked against poolKey.
func loadExistingAllocations(ctx context.Context, tx pgx.Tx, poolKey string) ([]net.IPNet, error) {
	defer metrics.ObserveQuery("load_existing_allocations", time.Now())
	rows, err := tx.Query(ctx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE pool_key = $1`,
		poolKey,
	)
	if err != nil {
		return nil, fmt.Errorf("load existing allocations: %w", err)
	}
	defer rows.Close()

	var existing []net.IPNet
	for rows.Next() {
		var cidrStr string
		if err := rows.Scan(&cidrStr); err != nil {
			return nil, fmt.Errorf("scan allocation row: %w", err)
		}
		_, ipnet, err := net.ParseCIDR(cidrStr)
		if err != nil {
			return nil, fmt.Errorf("parse stored cidr %q: %w", cidrStr, err)
		}
		existing = append(existing, *ipnet)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate allocation rows: %w", err)
	}
	return existing, nil
}

// publishPrefixUtilization recomputes allocated/total for the supplied pool
// and updates the ipam_pool_utilization_ratio gauge. Both sums are computed
// in big.Int so very large IPv6 pools do not overflow int64; the final
// division is converted to float64 for the Prometheus gauge. A zero capacity
// pool publishes 0 rather than NaN — the gauge is documented as ratio in
// [0, 1] and dashboards/alerts assume a finite value.
func publishPrefixUtilization(poolKey, ipFamily string, parents, allocated []net.IPNet) {
	project, org := tenantsFromPoolKey(poolKey)
	total := new(big.Int)
	for _, p := range parents {
		ones, bits := p.Mask.Size()
		hostBits := bits - ones
		if hostBits < 0 {
			continue
		}
		size := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))
		total.Add(total, size)
	}
	used := new(big.Int)
	for _, c := range allocated {
		ones, bits := c.Mask.Size()
		hostBits := bits - ones
		if hostBits < 0 {
			continue
		}
		size := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))
		used.Add(used, size)
	}
	usedF, _ := new(big.Float).SetInt(used).Float64()
	totalF, _ := new(big.Float).SetInt(total).Float64()
	// Absolute counters are published alongside the ratio so dashboards can
	// distinguish small/full pools from large/half-full pools at a glance.
	// Float64 rather than int64: a /48 IPv6 pool has 2^80 addresses, well
	// past int64.
	metrics.SetPoolCapacity(poolKey, ipFamily, "ippools", project, org, totalF, usedF)
	if total.Sign() == 0 {
		metrics.SetPoolUtilization(poolKey, ipFamily, "ippools", project, org, 0)
		return
	}
	metrics.SetPoolUtilization(poolKey, ipFamily, "ippools", project, org, usedF/totalF)
}

// insertPrefixAllocation records a new allocation row.
func insertPrefixAllocation(ctx context.Context, tx pgx.Tx, poolKey, cidr, claimKey, ipFamily string, ownerProject string) error {
	defer metrics.ObserveQuery("insert_allocation", time.Now())
	_, err := tx.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, ip_family, reclaim_policy, owner_project)
		 VALUES ($1, $2, $3, $4, 'Delete', $5)`,
		poolKey, cidr, claimKey, ipFamily, ownerProject,
	)
	if err != nil {
		return fmt.Errorf("insert allocation: %w", err)
	}
	return nil
}
