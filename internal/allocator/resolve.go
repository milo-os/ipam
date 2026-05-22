package allocator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ResolveIPPool returns the storage key of an IPPool that satisfies the
// supplied label selector. It lists pools belonging to the caller's project
// (or the platform scope when ownerProject is empty), decodes each into an
// IPPool, applies the selector, and returns the first match by storage key.
//
// The first-match policy is deliberately simple: it is deterministic across
// callers, requires no per-pool capacity probe, and lets operators steer
// allocation by naming pools in the order they want them filled. A future
// "spread by free space" strategy can be added behind the same signature
// without changing the storage layer's call sites.
//
// ipFamily, when non-empty, filters pools whose `spec.ipFamily` does not
// match. Pass "" to skip the family filter (e.g. for IPAddressClaim where
// the ipFamily comes from the resolved pool itself).
//
// Returns ErrPoolNotFound if no pool matches the selector.
func ResolveIPPool(ctx context.Context, tx pgx.Tx, selector *metav1.LabelSelector, ownerProject, ipFamily string) (string, error) {
	defer metrics.ObserveQuery("resolve_ip_pool", time.Now())

	sel, err := labelSelectorOrEverything(selector)
	if err != nil {
		return "", fmt.Errorf("compile label selector: %w", err)
	}

	keys, datas, err := listPools(ctx, tx, "IPPool", ownerProject)
	if err != nil {
		return "", err
	}

	for i, key := range keys {
		var pool ipamv1alpha1.IPPool
		if err := json.Unmarshal(datas[i], &pool); err != nil {
			return "", fmt.Errorf("decode IPPool %q: %w", key, err)
		}
		if ipFamily != "" && string(pool.Spec.IPFamily) != ipFamily {
			continue
		}
		if !sel.Matches(labels.Set(pool.Labels)) {
			continue
		}
		return key, nil
	}
	return "", ErrPoolNotFound
}

// listPools loads (key, data) for every ipam_objects row of the given kind
// belonging to the supplied project. Platform-scoped requests
// (ownerProject == "") see only platform pools; project-scoped requests see
// only their own project's pools. Cross-project shared pools are addressed
// via spec.prefixSelector.projectRef rather than being globally visible —
// see ResolvePrefixPoolWithProject for that path.
//
// The query uses the existing kind index plus a key-prefix LIKE filter; both
// are indexed (idx_ipam_objects_kind, idx_ipam_objects_key_prefix), so the
// scan stays O(matching pools) rather than O(all objects).
func listPools(ctx context.Context, tx pgx.Tx, kind, ownerProject string) ([]string, [][]byte, error) {
	prefix := tenant.Identity{Name: ownerProject}.ApplyPrefix("/ipam.miloapis.com/" + plural(kind) + "/")
	rows, err := tx.Query(ctx,
		`SELECT key, data FROM ipam_objects WHERE kind = $1 AND key LIKE $2 ORDER BY key`,
		kind, prefix+"%",
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s pools: %w", kind, err)
	}
	defer rows.Close()

	var keys []string
	var datas [][]byte
	for rows.Next() {
		var key string
		var data []byte
		if err := rows.Scan(&key, &data); err != nil {
			return nil, nil, fmt.Errorf("scan %s pool row: %w", kind, err)
		}
		keys = append(keys, key)
		datas = append(datas, data)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate %s pool rows: %w", kind, err)
	}
	return keys, datas, nil
}

// labelSelectorOrEverything compiles selector into a labels.Selector. A nil
// or empty selector matches every pool — operators sometimes want a claim
// to land in any pool of the resource type.
func labelSelectorOrEverything(selector *metav1.LabelSelector) (labels.Selector, error) {
	if selector == nil {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(selector)
}

// plural maps an internal Kind name to the plural form used in storage keys.
// The set is small enough to enumerate; doing so avoids pulling kubebuilder's
// pluraliser in here.
func plural(kind string) string {
	switch kind {
	case "IPPool":
		return "ippools"
	}
	// Conservative fallback — lowercase + "s" — never reached for the kinds
	// this resolver supports today, but defends against future kinds being
	// added without updating the table.
	return strings.ToLower(kind) + "s"
}
