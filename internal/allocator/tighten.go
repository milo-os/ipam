package allocator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"go.miloapis.com/ipam/internal/metrics"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// TightenRetention brings forward the deadline of every allocation poolKey
// retains that its maxRetentionLease now cuts short, and publishes the new
// deadline on each IPAllocation. It returns how many deadlines moved.
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
// pass, so a call that fails here is made good there.
func TightenRetention(ctx context.Context, tx pgx.Tx, poolKey string) (int, error) {
	pool, err := lockAndDecodeIPPool(ctx, tx, poolKey)
	if errors.Is(err, ErrPoolNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return tightenLocked(ctx, tx, poolKey, pool)
}

// tightenLocked is TightenRetention for a pool the caller holds locked.
//
// A deadline that moves is published on its IPAllocation under a savepoint of
// its own: an object that cannot be written costs its own status, not the cap,
// which is the operator's declared maximum and is applied to the row either
// way.
func tightenLocked(ctx context.Context, tx pgx.Tx, poolKey string, pool *ipamv1alpha1.IPPool) (int, error) {
	// A cap stored before validation refused non-positive values is not one.
	if pool == nil || pool.Spec.MaxRetentionLease == nil || pool.Spec.MaxRetentionLease.Duration <= 0 {
		return 0, nil
	}

	defer metrics.ObserveQuery("tighten_retention", time.Now())
	rows, err := tx.Query(ctx,
		`UPDATE ipam_cidr_allocations
		    SET expires_at = LEAST(expires_at, ipam_ceil_second(retained_at + $2::bigint * interval '1 microsecond'))
		  WHERE pool_key = $1 AND claim_key IS NULL AND purpose = 'Claim'
		    AND retained_at IS NOT NULL AND expires_at IS NOT NULL AND expires_at > now()
		    AND ipam_ceil_second(retained_at + $2::bigint * interval '1 microsecond') < expires_at
		 RETURNING allocation_key, retained_at, expires_at`,
		poolKey, pool.Spec.MaxRetentionLease.Microseconds(),
	)
	if err != nil {
		return 0, fmt.Errorf("tighten retention leases in %q: %w", poolKey, err)
	}
	tightened, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RetainedAllocation, error) {
		var (
			key        string
			retainedAt time.Time
			expiresAt  pgtype.Timestamptz
		)
		if err := row.Scan(&key, &retainedAt, &expiresAt); err != nil {
			return RetainedAllocation{}, err
		}
		return retainedFrom(key, retainedAt, expiresAt), nil
	})
	if err != nil {
		return 0, fmt.Errorf("read tightened retention leases in %q: %w", poolKey, err)
	}
	for _, r := range tightened {
		publishRetention(ctx, tx, r)
	}
	return len(tightened), nil
}
