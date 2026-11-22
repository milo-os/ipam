package allocator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ClaimAllocation is a claim allocation row as a reclaim reads it: under its
// pool's lock and its own, so nothing it reports can change before the
// transaction ends.
type ClaimAllocation struct {
	AllocationKey string
	PoolKey       string
	// CIDR is the block the row holds, in canonical form.
	CIDR string
	// ClaimKey is the claim the row is bound to, or "" when it is retained.
	ClaimKey      string
	IPFamily      string
	ClassName     string
	ScopeDigest   string
	ReclaimPolicy ipamv1alpha1.ReclaimPolicy
}

// LockClaimAllocation implements PrefixAllocator.LockClaimAllocation.
//
// The pool is found with an unlocked read and locked before the row is read
// again FOR UPDATE, the order every other writer of allocation rows takes. A
// row released between the two reads is simply not found by the second, and
// the caller takes that as "nothing to reclaim".
func (a *PostgresPrefixAllocator) LockClaimAllocation(ctx context.Context, tx pgx.Tx, allocationKey string) (*ClaimAllocation, error) {
	pools, err := lockPoolsOf(ctx, tx, "allocation_key = $1 AND purpose = 'Claim'", allocationKey)
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		return nil, nil
	}
	locked := make([]string, 0, len(pools))
	for key := range pools {
		locked = append(locked, key)
	}

	defer metrics.ObserveQuery("select_claim_allocation_for_update", time.Now())
	var (
		row      ClaimAllocation
		claimKey *string
		policy   string
	)
	err = tx.QueryRow(ctx,
		`SELECT allocation_key, pool_key,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr),
		        claim_key, ip_family, class_name, scope_digest, reclaim_policy
		   FROM ipam_cidr_allocations
		  WHERE allocation_key = $1 AND purpose = 'Claim' AND pool_key = ANY($2::text[])
		    FOR UPDATE`, allocationKey, locked,
	).Scan(&row.AllocationKey, &row.PoolKey, &row.CIDR, &claimKey,
		&row.IPFamily, &row.ClassName, &row.ScopeDigest, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock allocation %q: %w", allocationKey, err)
	}
	if claimKey != nil {
		row.ClaimKey = *claimKey
	}
	row.ReclaimPolicy = ipamv1alpha1.ReclaimPolicy(policy)
	return &row, nil
}

// ReclaimAllocation implements PrefixAllocator.ReclaimAllocation.
//
// Binding is a compare-and-set on the row being unbound, so it cannot take a
// row another claim holds whatever the caller read before. Nothing else
// changes: the address was held all along, so consumption, pool capacity, the
// utilization gauges and the search floor stay as they are. retained_at is
// cleared by the ipam_set_retained_at trigger on this transition.
func (a *PostgresPrefixAllocator) ReclaimAllocation(ctx context.Context, tx pgx.Tx, allocationKey, claimKey string, policy ipamv1alpha1.ReclaimPolicy) error {
	defer metrics.ObserveQuery("reclaim_allocation", time.Now())
	if policy == "" {
		policy = ipamv1alpha1.ReclaimDelete
	}
	tag, err := tx.Exec(ctx,
		`UPDATE ipam_cidr_allocations SET claim_key = $2, reclaim_policy = $3
		  WHERE allocation_key = $1 AND claim_key IS NULL AND purpose = 'Claim'`,
		allocationKey, claimKey, string(policy),
	)
	if err != nil {
		return fmt.Errorf("reclaim allocation %q: %w", allocationKey, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var holder *string
	err = tx.QueryRow(ctx,
		`SELECT claim_key FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocationKey,
	).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("reclaim allocation %q: %w", allocationKey, ErrObjectNotFound)
	}
	if err != nil {
		return fmt.Errorf("read allocation %q: %w", allocationKey, err)
	}
	bound := &AllocationBoundError{AllocationKey: allocationKey}
	if holder != nil {
		bound.ClaimKey = *holder
	}
	return bound
}
