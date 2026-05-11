package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// PostgresASNAllocator implements ASNAllocator atop ipam_objects and
// ipam_asn_allocations using the same pool-row-lock pattern as the prefix
// allocator.
type PostgresASNAllocator struct{}

// NewPostgresASNAllocator returns a stateless ASN allocator.
func NewPostgresASNAllocator() *PostgresASNAllocator {
	return &PostgresASNAllocator{}
}

// AllocateASN implements ASNAllocator.AllocateASN.
func (a *PostgresASNAllocator) AllocateASN(ctx context.Context, tx pgx.Tx, poolKey string, claimKey string, ownerProject string) (int64, error) {
	pool, err := lockAndDecodeASNPool(ctx, tx, poolKey)
	if err != nil {
		return 0, err
	}

	ranges := make([]allocation.ASNRange, 0, len(pool.Spec.Ranges))
	for _, r := range pool.Spec.Ranges {
		ranges = append(ranges, allocation.ASNRange{Start: r.Start, End: r.End})
	}

	existing, err := loadExistingASNs(ctx, tx, poolKey)
	if err != nil {
		return 0, err
	}

	pool2 := allocation.ASNPool{Ranges: ranges, Existing: existing}
	asn, err := pool2.Allocate()
	if err != nil {
		if errors.Is(err, allocation.ErrPoolExhausted) {
			return 0, ErrPoolExhausted
		}
		return 0, fmt.Errorf("compute next asn: %w", err)
	}

	insertStart := time.Now()
	_, err = tx.Exec(ctx,
		`INSERT INTO ipam_asn_allocations (pool_key, asn, claim_key, reclaim_policy, owner_project)
		 VALUES ($1, $2, $3, 'Delete', $4)`,
		poolKey, asn, claimKey, ownerProject,
	)
	metrics.ObserveQuery("insert_allocation", insertStart)
	if err != nil {
		return 0, fmt.Errorf("insert asn allocation: %w", err)
	}

	publishASNUtilization(poolKey, ranges, int64(len(existing)+1))

	klog.V(2).InfoS("Allocated ASN", "pool", poolKey, "asn", asn, "claim", claimKey, "ownerProject", ownerProject)
	return asn, nil
}

// InsertObject implements ASNAllocator.InsertObject. Shares the helper used
// by the prefix allocator so claim rows land in ipam_objects with a real
// resource_version stamped onto the in-memory object before the response
// returns.
func (a *PostgresASNAllocator) InsertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error) {
	return insertObject(ctx, tx, key, kind, namespace, name, data)
}

// Release implements ASNAllocator.Release.
//
// RETURNING surfaces the pool_key so the post-release utilization gauge can
// be refreshed without an additional read. Pools that have been hard-deleted
// out from under the allocation row are skipped silently.
func (a *PostgresASNAllocator) Release(ctx context.Context, tx pgx.Tx, claimKey string) error {
	rows, err := tx.Query(ctx,
		`DELETE FROM ipam_asn_allocations WHERE claim_key = $1
		 RETURNING pool_key`, claimKey,
	)
	if err != nil {
		return fmt.Errorf("release asn: %w", err)
	}
	var poolKeys []string
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan released asn: %w", err)
		}
		poolKeys = append(poolKeys, pk)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate released asns: %w", err)
	}

	for _, pk := range poolKeys {
		pool, perr := lockAndDecodeASNPool(ctx, tx, pk)
		if perr != nil {
			if errors.Is(perr, ErrPoolNotFound) {
				continue
			}
			return fmt.Errorf("reload asn pool after release: %w", perr)
		}
		ranges := make([]allocation.ASNRange, 0, len(pool.Spec.Ranges))
		for _, r := range pool.Spec.Ranges {
			ranges = append(ranges, allocation.ASNRange{Start: r.Start, End: r.End})
		}
		remaining, perr := loadExistingASNs(ctx, tx, pk)
		if perr != nil {
			return fmt.Errorf("reload asn allocations after release: %w", perr)
		}
		publishASNUtilization(pk, ranges, int64(len(remaining)))
	}
	return nil
}

// publishASNUtilization recomputes allocated/total for the supplied ASN pool
// and updates the ipam_pool_utilization_ratio gauge plus the absolute
// ipam_pool_capacity_total / ipam_pool_allocated_total gauges. ASN pools use
// the "ASN" ip_family label so they do not collide with prefix pools in the
// PromQL view. Tenant labels (project / org) are derived from the pool key
// via tenantsFromPoolKey so the gauge can be sliced per-tenant in dashboards.
func publishASNUtilization(poolKey string, ranges []allocation.ASNRange, allocated int64) {
	project, org := tenantsFromPoolKey(poolKey)
	var total int64
	for _, r := range ranges {
		if r.End < r.Start {
			continue
		}
		total += r.End - r.Start + 1
	}
	metrics.SetPoolCapacity(poolKey, "ASN", "asnpools", project, org, float64(total), float64(allocated))
	if total == 0 {
		metrics.SetPoolUtilization(poolKey, "ASN", "asnpools", project, org, 0)
		return
	}
	metrics.SetPoolUtilization(poolKey, "ASN", "asnpools", project, org, float64(allocated)/float64(total))
}

// DeleteObject implements ASNAllocator.DeleteObject. Shares the helper used
// by the prefix allocator so the claim row is removed and a DELETED
// changelog entry is recorded in the same transaction.
func (a *PostgresASNAllocator) DeleteObject(ctx context.Context, tx pgx.Tx, key string) (int64, error) {
	return deleteObject(ctx, tx, key)
}

// UpdateObject implements ASNAllocator.UpdateObject. Shares the helper used
// by the prefix allocator so the claim row is rewritten with a fresh
// resource_version and a MODIFIED changelog entry is recorded in the same
// transaction.
func (a *PostgresASNAllocator) UpdateObject(ctx context.Context, tx pgx.Tx, key string, data []byte) (int64, error) {
	return updateObject(ctx, tx, key, data)
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

func lockAndDecodeASNPool(ctx context.Context, tx pgx.Tx, poolKey string) (*ipamv1alpha1.ASNPool, error) {
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
		return nil, fmt.Errorf("lock asn pool row: %w", err)
	}
	var pool ipamv1alpha1.ASNPool
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, fmt.Errorf("decode asn pool: %w", err)
	}
	return &pool, nil
}

// loadExistingASNs returns the currently allocated ASNs sorted ascending —
// the contract required by allocation.ASNPool.
func loadExistingASNs(ctx context.Context, tx pgx.Tx, poolKey string) ([]int64, error) {
	defer metrics.ObserveQuery("load_existing_allocations", time.Now())
	rows, err := tx.Query(ctx,
		`SELECT asn FROM ipam_asn_allocations WHERE pool_key = $1 ORDER BY asn ASC`,
		poolKey,
	)
	if err != nil {
		return nil, fmt.Errorf("load existing asns: %w", err)
	}
	defer rows.Close()

	var existing []int64
	for rows.Next() {
		var asn int64
		if err := rows.Scan(&asn); err != nil {
			return nil, fmt.Errorf("scan asn row: %w", err)
		}
		existing = append(existing, asn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate asn rows: %w", err)
	}
	return existing, nil
}
