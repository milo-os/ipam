package allocator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/pkg/apis/ipam"
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

	// ClassRecorded reports whether the row records the class's defining
	// project and address scope; rows written before migration 006 do not.
	ClassRecorded bool
	ClassProject  string
	AddressScope  ipamv1alpha1.AddressScope
}

// Querier is the slice of a connection pool a read outside any transaction
// needs.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// HasClaimAllocation reports whether a claim allocation row exists under
// allocationKey. It takes no lock and needs no transaction: it is the cheap
// miss every create pays, and a hit is re-read under the pool's lock by
// LockClaimAllocation before anything is decided from it.
func HasClaimAllocation(ctx context.Context, db Querier, allocationKey string) (bool, error) {
	defer metrics.ObserveQuery("has_claim_allocation", time.Now())
	var exists bool
	err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ipam_cidr_allocations
		                 WHERE allocation_key = $1 AND purpose = 'Claim')`, allocationKey,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("look up allocation %q: %w", allocationKey, err)
	}
	return exists, nil
}

// LockClaimAllocation implements PrefixAllocator.LockClaimAllocation.
//
// The pool is found with an unlocked read and locked before the row is read
// again FOR UPDATE, the order every other writer of allocation rows takes. A
// row released between the two reads is simply not found by the second, and
// the caller takes that as "nothing to reclaim".
//
// A row whose pool row is missing has nothing to lock, and nothing decided
// from it would be serialised against the writers of that pool, so it is an
// error rather than a row. The foreign key on pool_key makes it unreachable.
func (a *PostgresPrefixAllocator) LockClaimAllocation(ctx context.Context, tx pgx.Tx, allocationKey string) (*ClaimAllocation, error) {
	pools, err := lockPoolsOf(ctx, tx, "allocation_key = $1 AND purpose = 'Claim'", allocationKey)
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		return nil, nil
	}
	locked := make([]string, 0, len(pools))
	for key, pool := range pools {
		if pool == nil {
			return nil, fmt.Errorf("%w: allocation %q names pool %q, which has no row to lock", ErrPoolNotFound, allocationKey, key)
		}
		locked = append(locked, key)
	}

	defer metrics.ObserveQuery("select_claim_allocation_for_update", time.Now())
	var (
		row                        ClaimAllocation
		claimKey                   *string
		policy                     string
		classProject, addressScope *string
	)
	err = tx.QueryRow(ctx,
		`SELECT allocation_key, pool_key,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr),
		        claim_key, ip_family, class_name, scope_digest, reclaim_policy,
		        class_project, address_scope
		   FROM ipam_cidr_allocations
		  WHERE allocation_key = $1 AND purpose = 'Claim' AND pool_key = ANY($2::text[])
		    FOR UPDATE`, allocationKey, locked,
	).Scan(&row.AllocationKey, &row.PoolKey, &row.CIDR, &claimKey,
		&row.IPFamily, &row.ClassName, &row.ScopeDigest, &policy,
		&classProject, &addressScope)
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
	if classProject != nil {
		row.ClassRecorded = true
		row.ClassProject = *classProject
		if addressScope != nil {
			row.AddressScope = ipamv1alpha1.AddressScope(*addressScope)
		}
	}
	return &row, nil
}

// PlacementMismatch is why a retained allocation's pool is not one a claim
// would be placed in, by the claim field that decides it.
type PlacementMismatch struct {
	Field  string
	Reason string
}

// ReclaimPlacement reports whether row's pool is the pool a claim of leaf with
// claimScope would be placed in now, without provisioning anything, or why
// not. The caller holds the pool's lock, so the pool read here is the one the
// reclaim binds into.
//
// Placement is decided the way create decides it, by the same predicates:
//
//   - a class with no parent draws from an operator pool offering it, in the
//     class's defining project and family, whose declared scope the claim
//     serves (offeringPools, poolServesScope). Any such pool will do; the
//     strategy chooses among them for a new block, and a block already held
//     in one of them is not moved.
//   - a chained class draws from the pool its parent chain resolves to for the
//     claim's scope (PlanCascade), which must be the pool the identity table
//     already names for that level. A pool for another location, network or
//     consumer is a different pool.
//
// A row that does not record its class's defining project is proven to be of
// this class only by the first rule: such a pool is always in the defining
// project. Under a chain it cannot be proven, because a parent may live in
// another project, and it is refused.
func ReclaimPlacement(ctx context.Context, tx pgx.Tx, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef, row *ClaimAllocation) (*PlacementMismatch, error) {
	defer metrics.ObserveQuery("reclaim_placement", time.Now())
	levels, err := PlanCascade(ctx, tx, leaf, claimScope)
	if err != nil {
		return nil, err
	}

	if len(levels) == 0 {
		offered, err := offeringPools(ctx, tx, leaf)
		if err != nil {
			return nil, err
		}
		for i := range offered {
			if offered[i].key != row.PoolKey {
				continue
			}
			if !poolServesScope(&offered[i].pool, claimScope) {
				return &PlacementMismatch{Field: "spec.scope", Reason: fmt.Sprintf(
					"it is held in pool %q, which does not serve this claim's scope", poolNameOf(row.PoolKey))}, nil
			}
			return nil, nil
		}
		return &PlacementMismatch{Field: "spec.className", Reason: fmt.Sprintf(
			"it is held in pool %q, which does not offer class %q", poolNameOf(row.PoolKey), leaf.Name)}, nil
	}

	if !row.ClassRecorded {
		return &PlacementMismatch{Field: "spec.className", Reason: fmt.Sprintf(
			"it was allocated before IPAM recorded which project defines its class, and class %q has a parent chain, so the class it was handed out under cannot be proven to be this one", leaf.Name)}, nil
	}
	holder := levels[len(levels)-1]
	resolved, err := lookupPoolIdentity(ctx, tx, holder.Class.Name, holder.ScopeDigest)
	if err != nil {
		return nil, err
	}
	if resolved != row.PoolKey {
		target := "no pool yet"
		if resolved != "" {
			target = fmt.Sprintf("pool %q", poolNameOf(resolved))
		}
		return &PlacementMismatch{Field: "spec.scope", Reason: fmt.Sprintf(
			"it is held in pool %q, and this claim's scope resolves class %q to %s", poolNameOf(row.PoolKey), leaf.Name, target)}, nil
	}
	return nil, nil
}

func poolNameOf(poolKey string) string {
	return poolKey[strings.LastIndex(poolKey, "/")+1:]
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
