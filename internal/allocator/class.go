package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ErrClassNotFound is returned when a named IPClass does not exist.
var ErrClassNotFound = errors.New("ipam: class not found")

// ErrNoDefaultClass is returned when a claim names no class and no class
// carries the default marker for the requested family.
var ErrNoDefaultClass = errors.New("ipam: no default class for address family")

// ErrChainTooDeep is returned when a class's parent chain exceeds
// MaxClassChainDepth. The class registry rejects such a chain at write time;
// this is the allocator's backstop against a chain that became too deep by some
// route the registry did not see, and it exists so a request thread terminates
// rather than walking forever.
var ErrChainTooDeep = errors.New("ipam: class chain too deep")

// MaxClassChainDepth bounds how many levels a cascade may provision. It matches
// the cap the IPClass registry enforces at write time.
const MaxClassChainDepth = 8

// classStorageKey is the ipam_objects key for an IPClass.
//
// Classes are operator-authored platform policy, so they live in the platform
// project — the one named by --platform-project — and a consumer naming a class
// reaches the same object whatever project they are in. That last property is
// unchanged; what changed is where "the platform" is. It used to be an
// unprefixed root, which no request through Milo's front gate can ever write to,
// because every such request carries a parent. The catalog now lives under
// "project/<platform>/" like every other object.
//
// It returns an error rather than falling back to the unprefixed key when no
// platform project is configured. The fallback is the tempting shape and it is
// the dangerous one: the lookup would succeed against an empty keyspace, every
// claim would fail with "class not found", and nothing would name the flag.
func classStorageKey(ctx context.Context, name string) (string, error) {
	id, err := tenant.PlatformIdentity(ctx)
	if err != nil {
		return "", err
	}
	return id.ResourceKey("ipclasses", name), nil
}

// classKeyPrefix is the LIKE pattern prefix every IPClass key shares —
// "project/<platform>/ipam.miloapis.com/ipclasses/" — for the one query that
// scans classes rather than naming one.
func classKeyPrefix(ctx context.Context) (string, error) {
	id, err := tenant.PlatformIdentity(ctx)
	if err != nil {
		return "", err
	}
	return likeEscape(id.ResourceKey("ipclasses", "")), nil
}

// resourceKeyPrefixFor is classKeyPrefix for an arbitrary project and resource:
// the LIKE-safe prefix every key of that resource in that project shares.
func resourceKeyPrefixFor(project, resource string) string {
	return likeEscape(tenant.Identity{Name: project}.ResourceKey(resource, ""))
}

// likeEscape neutralises the three characters LIKE gives meaning to, so a value
// interpolated into a pattern matches itself and nothing else.
//
// Every project name reaching these patterns is supposed to be a DNS-1123
// name, which contains none of them — the --platform-project flag is validated
// at startup and IPClass.spec.backingProjects at write time. This runs anyway
// because the cost is nothing and the failure mode is not "a malformed name is
// rejected" but "a name containing % matches every project's key space", which
// is a tenancy boundary being crossed by a wildcard nobody wrote deliberately.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// LoadClass reads one IPClass by name inside the supplied transaction.
//
// Classes are read inside the allocation transaction rather than cached,
// because the class fixes the shape of what is about to be written — the pool
// chain, the block size, the uniqueness rule — and a stale copy would produce a
// correctly-committed allocation of the wrong shape.
func LoadClass(ctx context.Context, tx pgx.Tx, name string) (*ipamv1alpha1.IPClass, error) {
	defer metrics.ObserveQuery("load_class", time.Now())
	key, err := classStorageKey(ctx, name)
	if err != nil {
		return nil, err
	}
	var data []byte
	err = tx.QueryRow(ctx,
		`SELECT data FROM ipam_objects WHERE key = $1`,
		key,
	).Scan(&data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrClassNotFound, name)
		}
		return nil, fmt.Errorf("load class %q: %w", name, err)
	}
	var class ipamv1alpha1.IPClass
	if err := json.Unmarshal(data, &class); err != nil {
		return nil, fmt.Errorf("decode class %q: %w", name, err)
	}
	return &class, nil
}

// LoadDefaultClass returns the class carrying the default marker for family.
//
// The platform is IPv6-first and an interface that says nothing gets IPv6, so
// this is the common path rather than a fallback: most claims name a family and
// no class at all.
//
// The registry enforces at most one default per family at write time. If two
// somehow exist, this returns the first by name so the choice is at least
// deterministic — an arbitrary but stable class beats a claim that succeeds and
// fails by turns.
func LoadDefaultClass(ctx context.Context, tx pgx.Tx, family ipamv1alpha1.IPFamily) (*ipamv1alpha1.IPClass, error) {
	defer metrics.ObserveQuery("load_default_class", time.Now())
	// The key predicate below is not an optimisation. Without it this query
	// reads every tenant's key space and returns the first IPClass anywhere in
	// the database carrying the default marker — so a project able to create an
	// IPClass in its own project could decide what every other project's
	// unqualified claim allocates from, and could do it by choosing a name that
	// sorts first. The catalog is platform policy; the query has to say so.
	prefix, err := classKeyPrefix(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT data FROM ipam_objects
		  WHERE kind = 'IPClass'
		    AND key LIKE $3
		    AND ipam_data_to_jsonb(data) -> 'metadata' -> 'annotations' ->> $1 = 'true'
		    AND ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily' = $2
		  ORDER BY key
		  LIMIT 1`,
		ipamv1alpha1.IsDefaultClassAnnotation, string(family), prefix+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("query default class for %s: %w", family, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("query default class for %s: %w", family, err)
		}
		return nil, fmt.Errorf("%w: %s", ErrNoDefaultClass, family)
	}
	var data []byte
	if err := rows.Scan(&data); err != nil {
		return nil, fmt.Errorf("scan default class: %w", err)
	}
	var class ipamv1alpha1.IPClass
	if err := json.Unmarshal(data, &class); err != nil {
		return nil, fmt.Errorf("decode default class for %s: %w", family, err)
	}
	return &class, nil
}

// ResolveClass picks the class a claim allocates under: the one it names, or
// the default for the family it asked for.
func ResolveClass(ctx context.Context, tx pgx.Tx, className string, family ipamv1alpha1.IPFamily) (*ipamv1alpha1.IPClass, error) {
	if className != "" {
		return LoadClass(ctx, tx, className)
	}
	if family == "" {
		return nil, fmt.Errorf("%w: claim names neither a class nor an address family", ErrNoDefaultClass)
	}
	return LoadDefaultClass(ctx, tx, family)
}

// LoadAncestry walks a class's parent chain and returns the *provisioning*
// classes, nearest first: the class that provisions the pool the claim draws
// from, then the class that provisions that pool's source, and so on to the
// root of the chain.
//
// The leaf class itself is deliberately absent. A leaf provisions nothing — it
// binds an allocation directly — so it contributes no level to the cascade. A
// class with no parent yields an empty ancestry, and its claims draw straight
// from whichever operator-authored pool offers it, which is the whole IPv4
// endpoint story from the design doc.
func LoadAncestry(ctx context.Context, tx pgx.Tx, leaf *ipamv1alpha1.IPClass) ([]*ipamv1alpha1.IPClass, error) {
	var chain []*ipamv1alpha1.IPClass
	seen := map[string]bool{leaf.Name: true}

	current := leaf
	for len(chain) <= MaxClassChainDepth {
		parentName := current.Spec.ParentClassName
		if parentName == "" {
			return chain, nil
		}
		if seen[parentName] {
			return nil, fmt.Errorf("%w: cycle at class %q", ErrChainTooDeep, parentName)
		}
		seen[parentName] = true

		parent, err := LoadClass(ctx, tx, parentName)
		if err != nil {
			return nil, fmt.Errorf("resolve parent of %q: %w", current.Name, err)
		}
		if parent.Spec.IPFamily != leaf.Spec.IPFamily {
			// The registry rejects this at class-write time. Reaching it here
			// means the catalog changed underneath a rule that was supposed to
			// be immutable, and continuing would carve an address of one family
			// out of a pool of another.
			return nil, fmt.Errorf("class %q hands out %s but its ancestor %q hands out %s",
				leaf.Name, leaf.Spec.IPFamily, parent.Name, parent.Spec.IPFamily)
		}
		chain = append(chain, parent)
		current = parent
	}
	return nil, fmt.Errorf("%w: class %q exceeds %d levels", ErrChainTooDeep, leaf.Name, MaxClassChainDepth)
}

// EffectivePrefixLength resolves the block size a claim of this class gets:
// the size it asked for, or the class's default. It also enforces the class's
// allowed range, because a claim outside it is a client error rather than an
// allocation that happens to be the wrong size.
func EffectivePrefixLength(class *ipamv1alpha1.IPClass, requested *int32) (int, error) {
	var length int32
	switch {
	case requested != nil:
		length = *requested
	case class.Spec.DefaultPrefixLength != 0:
		length = class.Spec.DefaultPrefixLength
	case class.Spec.AllowedPrefixLengths != nil && class.Spec.AllowedPrefixLengths.Min == class.Spec.AllowedPrefixLengths.Max:
		// A fixed-size class states its size once, as min == max, and setting
		// defaultPrefixLength as well would be the same fact written twice.
		length = class.Spec.AllowedPrefixLengths.Min
	default:
		return 0, fmt.Errorf("class %q sets no defaultPrefixLength and the claim requested none", class.Name)
	}

	if r := class.Spec.AllowedPrefixLengths; r != nil && (length < r.Min || length > r.Max) {
		return 0, fmt.Errorf("prefixLength %d is outside class %q allowedPrefixLengths [%d, %d]",
			length, class.Name, r.Min, r.Max)
	}
	if length <= 0 {
		return 0, fmt.Errorf("prefixLength must be greater than 0, got %d", length)
	}
	maxLen := int32(32)
	if class.Spec.IPFamily == ipamv1alpha1.IPv6 {
		maxLen = 128
	}
	if length > maxLen {
		return 0, fmt.Errorf("prefixLength %d exceeds %d for %s", length, maxLen, class.Spec.IPFamily)
	}
	return int(length), nil
}

// EffectiveReclaimPolicy resolves the policy an allocation is recorded with:
// the claim's override, else the class default, else Delete.
//
// The result is written onto the allocation rather than read back through the
// class at release time, because the allocation outlives the claim that chose
// it — and, under Retain, outlives any reference to why.
func EffectiveReclaimPolicy(class *ipamv1alpha1.IPClass, override ipamv1alpha1.ReclaimPolicy) ipamv1alpha1.ReclaimPolicy {
	if override != "" {
		return override
	}
	if class.Spec.ReclaimPolicy != "" {
		return class.Spec.ReclaimPolicy
	}
	return ipamv1alpha1.ReclaimDelete
}
