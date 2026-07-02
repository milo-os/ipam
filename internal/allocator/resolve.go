package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// LoadIPClass loads the named IPClass from the caller's scope. IPClass is
// cluster-scoped at the API layer but, like IPPool, is persisted under the
// tenant prefix of the control-plane it was created through, so it is
// addressed with the same ownerProject prefix the pools use. Returns
// ErrClassNotFound when no such class exists.
func LoadIPClass(ctx context.Context, tx pgx.Tx, ownerProject, name string) (*ipamv1alpha1.IPClass, error) {
	defer metrics.ObserveQuery("load_ip_class", time.Now())

	key := tenant.Identity{Name: ownerProject}.ResourceKey("ipclasses", name)
	var data []byte
	err := tx.QueryRow(ctx,
		`SELECT data FROM ipam_objects WHERE key = $1 AND kind = 'IPClass'`,
		key,
	).Scan(&data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrClassNotFound
		}
		return nil, fmt.Errorf("load IPClass %q: %w", name, err)
	}
	var class ipamv1alpha1.IPClass
	if err := json.Unmarshal(data, &class); err != nil {
		return nil, fmt.Errorf("decode IPClass %q: %w", name, err)
	}
	return &class, nil
}

// FindDefaultIPClass returns the IPClass in the caller's scope marked as the
// platform default via the is-default-class annotation. Returns
// ErrClassNotFound when no class is marked default. The single-default
// invariant is enforced on write (ipclass storage), so the first match is the
// only match.
func FindDefaultIPClass(ctx context.Context, tx pgx.Tx, ownerProject string) (*ipamv1alpha1.IPClass, error) {
	defer metrics.ObserveQuery("find_default_ip_class", time.Now())

	keys, datas, err := listPools(ctx, tx, "IPClass", ownerProject)
	if err != nil {
		return nil, err
	}
	for i, key := range keys {
		var class ipamv1alpha1.IPClass
		if err := json.Unmarshal(datas[i], &class); err != nil {
			return nil, fmt.Errorf("decode IPClass %q: %w", key, err)
		}
		if class.Annotations[ipamv1alpha1.IsDefaultClassAnnotation] == "true" {
			return &class, nil
		}
	}
	return nil, ErrClassNotFound
}

// ResolveIPPoolForClass returns the storage key of an IPPool that backs the
// named class for the given address family. Candidate pools are drawn from the
// caller's own project scope AND the platform scope — consumer projects usually
// own no pools, so a class's backing capacity typically lives platform-owned.
// Candidates are those whose spec.classNames contains className and whose
// effective family matches ipFamily; among them the class's placement strategy
// chooses deterministically (ties, and FirstFit, break by storage key so
// repeated claims are stable). Returns ErrNoPoolForClass when no pool backs the
// class. Cross-project (projectRef/SAR) resolution is intentionally not done
// here — that is a deferred follow-up.
func ResolveIPPoolForClass(ctx context.Context, tx pgx.Tx, className, ownerProject, ipFamily, strategy string) (string, error) {
	defer metrics.ObserveQuery("resolve_ip_pool_for_class", time.Now())

	var keys []string
	var pools []ipamv1alpha1.IPPool
	for _, scope := range poolScopes(ownerProject) {
		sKeys, datas, err := listPools(ctx, tx, "IPPool", scope)
		if err != nil {
			return "", err
		}
		for i, key := range sKeys {
			var p ipamv1alpha1.IPPool
			if err := json.Unmarshal(datas[i], &p); err != nil {
				return "", fmt.Errorf("decode IPPool %q: %w", key, err)
			}
			keys = append(keys, key)
			pools = append(pools, p)
		}
	}
	// Merged scopes are each individually key-sorted but not globally; sort the
	// union so the first-fit-by-key tie-break stays deterministic.
	sortPoolsByKey(keys, pools)

	key, ok := pickPoolForClass(keys, pools, className, ipFamily, strategy)
	if !ok {
		return "", ErrNoPoolForClass
	}
	return key, nil
}

// poolScopes returns the storage scopes to search for a class's backing pools:
// the platform scope alone for a platform caller, or the caller's own project
// scope followed by the platform scope for a project caller.
func poolScopes(ownerProject string) []string {
	if ownerProject == "" {
		return []string{""}
	}
	return []string{ownerProject, ""}
}

// sortPoolsByKey sorts the parallel (keys, pools) slices in place by key.
func sortPoolsByKey(keys []string, pools []ipamv1alpha1.IPPool) {
	sort.Sort(&poolsByKey{keys: keys, pools: pools})
}

type poolsByKey struct {
	keys  []string
	pools []ipamv1alpha1.IPPool
}

func (p *poolsByKey) Len() int           { return len(p.keys) }
func (p *poolsByKey) Less(i, j int) bool { return p.keys[i] < p.keys[j] }
func (p *poolsByKey) Swap(i, j int) {
	p.keys[i], p.keys[j] = p.keys[j], p.keys[i]
	p.pools[i], p.pools[j] = p.pools[j], p.pools[i]
}

// pickPoolForClass chooses the backing pool for a class from candidate pools
// (keys and pools are parallel, sorted by key). It is pure so the placement
// policy is unit-testable without a database. Candidates are those whose
// spec.classNames offers className and whose family matches ipFamily (empty
// ipFamily skips the family filter); among them the strategy's score picks the
// winner, and because keys arrive sorted a strict "<" comparison breaks ties by
// lowest key — deterministic first-fit-by-key.
func pickPoolForClass(keys []string, pools []ipamv1alpha1.IPPool, className, ipFamily, strategy string) (string, bool) {
	bestKey := ""
	bestScore := 0
	found := false
	for i := range pools {
		p := &pools[i]
		if !poolBacksClass(p, className) {
			continue
		}
		if ipFamily != "" && effectivePoolFamily(p) != ipFamily {
			continue
		}
		score := poolScore(strategy, p)
		if !found || score < bestScore {
			found = true
			bestScore = score
			bestKey = keys[i]
		}
	}
	return bestKey, found
}

// poolBacksClass reports whether a pool offers its capacity to className.
func poolBacksClass(pool *ipamv1alpha1.IPPool, className string) bool {
	for _, c := range pool.Spec.ClassNames {
		if c == className {
			return true
		}
	}
	return false
}

// poolScore ranks a candidate pool for a placement strategy; lower is better.
// FirstFit (and any unknown/empty strategy) scores every pool equally so the
// lowest storage key wins. LeastUtilized prefers the pool with the smallest
// utilization. BestFit prefers the tightest pool — the one whose largest free
// block is smallest — so wide pools are kept intact for larger requests.
func poolScore(strategy string, pool *ipamv1alpha1.IPPool) int {
	switch ipamv1alpha1.Strategy(strategy) {
	case ipamv1alpha1.LeastUtilized:
		return int(pool.Status.UtilizationPercent)
	case ipamv1alpha1.BestFit:
		// largestFreePrefix is a mask length: larger value = smaller free
		// block = tighter fit. Zero means exhausted/unknown, ranked worst.
		lf := int(pool.Status.LargestFreePrefix)
		if lf == 0 {
			return 128
		}
		return 128 - lf
	default: // FirstFit and empty
		return 0
	}
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
	case "IPClass":
		return "ipclasses"
	}
	// Conservative fallback — lowercase + "s" — never reached for the kinds
	// this resolver supports today, but defends against future kinds being
	// added without updating the table.
	return strings.ToLower(kind) + "s"
}
