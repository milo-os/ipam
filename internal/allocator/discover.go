package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ErrNoOfferingPool is returned when no pool serves a claim of this class.
var ErrNoOfferingPool = errors.New("ipam: no pool offers this class")

// DiscoverPool returns the key of the pool a claim of this class allocates
// from.
//
// Pools offer the ROOT of the class's parent chain, not the leaf. A leaf binds
// an allocation out of what its ancestry provisions, so an operator backs the
// chain once at the top rather than once per class hanging off it.
func DiscoverPool(ctx context.Context, tx pgx.Tx, class *ResolvedClass, claimScope map[string]ipam.ScopeRef) (string, error) {
	defer metrics.ObserveQuery("discover_pool", time.Now())

	root, err := chainRoot(ctx, tx, class)
	if err != nil {
		return "", err
	}
	offered, err := offeringPools(ctx, tx, root)
	if err != nil {
		return "", err
	}

	var candidates []offeringPool
	for _, c := range offered {
		// The one filter that depends on the claim rather than on the class.
		if !poolServesScope(&c.pool, claimScope) {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("%w: class %q in project %q", ErrNoOfferingPool, class.Name, class.Project)
	}

	best := 0
	for i := 1; i < len(candidates); i++ {
		if preferPool(class.Spec.Strategy, &candidates[i].pool, &candidates[best].pool) {
			best = i
		}
	}
	return candidates[best].key, nil
}

// chainRoot returns the class whose pools back the chain: the furthest ancestor,
// or the class itself when it has no parent.
func chainRoot(ctx context.Context, tx pgx.Tx, leaf *ResolvedClass) (*ResolvedClass, error) {
	ancestry, err := LoadAncestry(ctx, tx, leaf)
	if err != nil {
		return nil, err
	}
	if len(ancestry) == 0 {
		return leaf, nil
	}
	return ancestry[len(ancestry)-1], nil
}

type offeringPool struct {
	key  string
	pool ipamv1alpha1.IPPool
}

// offeringPools lists the pools published to a class, filtered by everything
// that depends only on the class: the project that may back it, and the address
// family it hands out.
//
// A pool may only back a class in the project holding that class's definition.
// The offer table keys on class NAME alone, so without the project restriction
// a pool in any project could publish itself to the name "backbone" and serve
// claims of somebody else's class.
func offeringPools(ctx context.Context, tx pgx.Tx, class *ResolvedClass) ([]offeringPool, error) {
	pattern := resourceKeyPrefixFor(class.Project, "ippools") + "%"

	rows, err := tx.Query(ctx,
		`SELECT o.pool_key, obj.data
		   FROM ipam_pool_class_offer o
		   JOIN ipam_objects obj ON obj.key = o.pool_key
		  WHERE o.class_name = $1
		    AND ipam_data_to_jsonb(obj.data) ->> 'kind' = 'IPPool'
		    AND obj.key LIKE $2
		  ORDER BY o.pool_key`,
		class.Name, pattern,
	)
	if err != nil {
		return nil, fmt.Errorf("list pools offering class %q: %w", class.Name, err)
	}
	defer rows.Close()

	var out []offeringPool
	for rows.Next() {
		var key string
		var data []byte
		if err := rows.Scan(&key, &data); err != nil {
			return nil, fmt.Errorf("scan pool row: %w", err)
		}
		var pool ipamv1alpha1.IPPool
		if err := json.Unmarshal(data, &pool); err != nil {
			return nil, fmt.Errorf("decode pool %q: %w", key, err)
		}
		if effectivePoolFamily(&pool) != string(class.Spec.IPFamily) {
			continue
		}
		out = append(out, offeringPool{key: key, pool: pool})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pool rows: %w", err)
	}
	return out, nil
}

// poolServesScope reports whether a pool's declared scope is satisfied by the
// claim's.
//
// A pool states the roles it is specific to. A claim must name the same object
// for each: a pool for location "lon1" serves only claims in lon1. Roles the
// pool does not state are unconstrained, so a pool with no scope serves every
// claim of its class.
func poolServesScope(pool *ipamv1alpha1.IPPool, claimScope map[string]ipam.ScopeRef) bool {
	for role, declared := range pool.Spec.Scope {
		got, ok := claimScope[role]
		if !ok {
			return false
		}
		if got.APIGroup != declared.APIGroup || got.Kind != declared.Kind || got.Name != declared.Name {
			return false
		}
	}
	return true
}

// preferPool reports whether candidate beats best under the class's strategy.
func preferPool(strategy ipamv1alpha1.PoolSelectionStrategy, candidate, best *ipamv1alpha1.IPPool) bool {
	if strategy == ipamv1alpha1.PoolLeastUtilized {
		return candidate.Status.UtilizationPercent < best.Status.UtilizationPercent
	}
	// First by key order, which offeringPools already sorted by. Deterministic
	// across callers, and it lets an operator steer allocation by naming pools
	// in the order they want them filled.
	return false
}
