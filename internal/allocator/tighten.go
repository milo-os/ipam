package allocator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/metrics"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// TightenRetention brings forward the deadline of every allocation poolKey
// retains that its maxRetentionLease now cuts short, and publishes the new
// deadline on each IPAllocation. It returns how many deadlines moved, and how
// many it had to leave because their IPAllocation could not be written.
//
// A lease is fixed at retention, with this one exception, and it only ever
// tightens: the new deadline is the earlier of the old one and retention plus
// the cap, so loosening or removing the cap moves nothing, and running it twice
// is running it once. A deadline already passed is left alone; it is due
// either way. A row with no lease ('infinity') gets one, since the cap now
// bounds it.
//
// The cap is read from the pool row under its lock, not from the caller's
// copy, so two updates committing in either order leave the tighter cap
// applied. The sweeper applies the same step to every capped pool on each
// pass, so a row this leaves is retried there.
func TightenRetention(ctx context.Context, tx pgx.Tx, poolKey string) (moved, failed int, err error) {
	pool, err := lockAndDecodeIPPool(ctx, tx, poolKey)
	if errors.Is(err, ErrPoolNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return tightenLocked(ctx, tx, poolKey, pool)
}

// tightenPredicate selects the rows of pool $1 whose deadline a cap of $2
// microseconds cuts short.
const tightenPredicate = `pool_key = $1 AND claim_key IS NULL AND purpose = 'Claim'
		    AND retained_at IS NOT NULL AND expires_at IS NOT NULL AND expires_at > now()
		    AND ipam_ceil_second(retained_at + $2::bigint * interval '1 microsecond') < expires_at`

// tightenLocked is TightenRetention for a pool the caller holds locked.
//
// Each row's new deadline and its publication on the IPAllocation are written
// under one savepoint, so they commit together or not at all. A row whose
// IPAllocation cannot be written keeps its old, later deadline, which holds the
// address longer rather than releasing it before what its status says, and is
// counted in failed and retried on the next sweep pass.
func tightenLocked(ctx context.Context, tx pgx.Tx, poolKey string, pool *ipamv1alpha1.IPPool) (moved, failed int, err error) {
	// A cap stored before validation refused non-positive values is not one.
	if pool == nil || pool.Spec.MaxRetentionLease == nil || pool.Spec.MaxRetentionLease.Duration <= 0 {
		return 0, 0, nil
	}
	poolCap := pool.Spec.MaxRetentionLease.Microseconds()

	defer metrics.ObserveQuery("tighten_retention", time.Now())
	rows, err := tx.Query(ctx,
		`SELECT allocation_key FROM ipam_cidr_allocations WHERE `+tightenPredicate+` ORDER BY allocation_key`,
		poolKey, poolCap)
	if err != nil {
		return 0, 0, fmt.Errorf("find retention leases to tighten in %q: %w", poolKey, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, 0, fmt.Errorf("read retention leases to tighten in %q: %w", poolKey, err)
	}

	for _, key := range keys {
		err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
			var (
				retainedAt time.Time
				expiresAt  pgtype.Timestamptz
			)
			err := sp.QueryRow(ctx,
				`UPDATE ipam_cidr_allocations
				    SET expires_at = LEAST(expires_at, ipam_ceil_second(retained_at + $2::bigint * interval '1 microsecond'))
				  WHERE `+tightenPredicate+` AND allocation_key = $3
				 RETURNING retained_at, expires_at`, poolKey, poolCap, key,
			).Scan(&retainedAt, &expiresAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("tighten retention lease of %q: %w", key, err)
			}
			moved++
			if err := writeRetentionStatus(ctx, sp, retainedFrom(key, retainedAt, expiresAt)); err != nil {
				moved--
				return err
			}
			return nil
		})
		if err != nil {
			klog.ErrorS(err, "Could not apply a pool's cap to a retained allocation and publish it; it keeps its later deadline and is retried",
				"allocation", key, "pool", poolKey)
			failed++
		}
	}
	return moved, failed, nil
}
