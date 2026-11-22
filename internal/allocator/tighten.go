package allocator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"go.miloapis.com/ipam/internal/metrics"
)

// TightenRetention brings forward the deadline of every allocation poolKey
// retains that its maxRetentionLease now cuts short, and publishes the new
// deadline on each IPAllocation. It returns how many deadlines moved, and how
// many of those an observing sweeper had already counted past, which the
// caller records as would-be releases once the transaction commits.
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
// applied.
func TightenRetention(ctx context.Context, tx pgx.Tx, poolKey string) (moved, countedPast int, err error) {
	pool, err := lockAndDecodeIPPool(ctx, tx, poolKey)
	if errors.Is(err, ErrPoolNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	// A cap stored before validation refused non-positive values is not one.
	if pool.Spec.MaxRetentionLease == nil || pool.Spec.MaxRetentionLease.Duration <= 0 {
		return 0, 0, nil
	}

	defer metrics.ObserveQuery("tighten_retention", time.Now())
	rows, err := tx.Query(ctx,
		`UPDATE ipam_cidr_allocations
		    SET expires_at = LEAST(expires_at, date_trunc('second', retained_at) + $2::bigint * interval '1 microsecond')
		  WHERE pool_key = $1 AND claim_key IS NULL AND purpose = 'Claim'
		    AND retained_at IS NOT NULL AND expires_at IS NOT NULL AND expires_at > now()
		    AND date_trunc('second', retained_at) + $2::bigint * interval '1 microsecond' < expires_at
		 RETURNING allocation_key, retained_at, expires_at`,
		poolKey, pool.Spec.MaxRetentionLease.Microseconds(),
	)
	if err != nil {
		return 0, 0, fmt.Errorf("tighten retention leases in %q: %w", poolKey, err)
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
		return 0, 0, fmt.Errorf("read tightened retention leases in %q: %w", poolKey, err)
	}
	if len(tightened) == 0 {
		return 0, 0, nil
	}
	for _, r := range tightened {
		if err := writeRetentionStatus(ctx, tx, r); err != nil {
			return 0, 0, err
		}
	}

	// A deadline moved to before where an observing sweeper stopped counting
	// would never be counted by it. The state row is held shared so the count
	// cannot move past these deadlines while they are being written.
	var (
		mode string
		st   sweepState
	)
	err = tx.QueryRow(ctx,
		`SELECT mode, counted_through, counted_through_key FROM ipam_retention_sweep_state WHERE id = 1 FOR SHARE`,
	).Scan(&mode, &st.countedThrough, &st.countedThroughKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return len(tightened), 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read sweep state: %w", err)
	}
	if SweepMode(mode) == SweepObserve {
		for _, r := range tightened {
			if st.countedBefore(r.ExpiresAt, r.AllocationKey) {
				countedPast++
			}
		}
	}
	return len(tightened), countedPast, nil
}
