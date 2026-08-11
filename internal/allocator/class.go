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

// ErrChainedReference is returned when a reference names a class that is itself
// a reference.
var ErrChainedReference = errors.New("ipam: class reference names another reference")

// ErrChainTooDeep is returned when a class's parent chain exceeds
// MaxClassChainDepth. The registry rejects such a chain at write time; this
// makes a request thread terminate if one exists anyway.
var ErrChainTooDeep = errors.New("ipam: class chain too deep")

// MaxClassChainDepth bounds how many levels a cascade may provision. It matches
// the cap the IPClass registry enforces at write time.
const MaxClassChainDepth = 8

// ResolvedClass is a class definition together with the project it lives in. A
// name alone does not identify a class: the same name in two projects is two
// objects, and a reference resolves into a project that is not the caller's.
type ResolvedClass struct {
	*ipamv1alpha1.IPClass

	// Project holds the definition, so two projects referencing one class
	// resolve to one identity and share its address space.
	Project string
}

func classStorageKey(project, name string) string {
	return tenant.Identity{Name: project}.ResourceKey("ipclasses", name)
}

func classKeyPrefix(project string) string {
	return resourceKeyPrefixFor(project, "ipclasses")
}

func resourceKeyPrefixFor(project, resource string) string {
	return likeEscape(tenant.Identity{Name: project}.ResourceKey(resource, ""))
}

// likeEscape neutralises the characters LIKE treats as special. A project name
// containing % would otherwise match every project's key space.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// loadClassIn reads one IPClass without following a reference.
func loadClassIn(ctx context.Context, tx pgx.Tx, project, name string) (*ipamv1alpha1.IPClass, error) {
	defer metrics.ObserveQuery("load_class", time.Now())
	var data []byte
	err := tx.QueryRow(ctx,
		`SELECT data FROM ipam_objects WHERE key = $1`,
		classStorageKey(project, name),
	).Scan(&data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q in project %q", ErrClassNotFound, name, project)
		}
		return nil, fmt.Errorf("load class %q in project %q: %w", name, project, err)
	}
	var class ipamv1alpha1.IPClass
	if err := json.Unmarshal(data, &class); err != nil {
		return nil, fmt.Errorf("decode class %q in project %q: %w", name, project, err)
	}
	return &class, nil
}

// resolveClassIn reads a class and, if it is a reference, the definition it
// points at.
//
// A reference resolves in exactly one hop. A target that is itself a reference
// is an error: a chain would let the project at the far end change which class
// a claim allocates under, and pool identity keys off that class.
func resolveClassIn(ctx context.Context, tx pgx.Tx, project, name string) (*ResolvedClass, error) {
	class, err := loadClassIn(ctx, tx, project, name)
	if err != nil {
		return nil, err
	}
	if class.Spec.Source == nil {
		return &ResolvedClass{IPClass: class, Project: project}, nil
	}

	src := *class.Spec.Source
	target, err := loadClassIn(ctx, tx, src.Project, src.Name)
	if err != nil {
		return nil, fmt.Errorf("resolve class %q in project %q: %w", name, project, err)
	}
	if target.Spec.Source != nil {
		return nil, fmt.Errorf("%w: %q in project %q names %q in project %q",
			ErrChainedReference, name, project, src.Name, src.Project)
	}
	return &ResolvedClass{IPClass: target, Project: src.Project}, nil
}

// LoadClass reads the class a caller named, from the caller's own project.
func LoadClass(ctx context.Context, tx pgx.Tx, name string) (*ResolvedClass, error) {
	id, err := tenant.RequireTenant(ctx)
	if err != nil {
		return nil, err
	}
	return resolveClassIn(ctx, tx, id.Name, name)
}

// LoadDefaultClass returns the class carrying the default marker for family in
// the caller's project.
//
// Family is matched after resolution, not in the query: a reference states
// nothing but the class it points at, so the family lives on the definition.
func LoadDefaultClass(ctx context.Context, tx pgx.Tx, family ipamv1alpha1.IPFamily) (*ResolvedClass, error) {
	defer metrics.ObserveQuery("load_default_class", time.Now())
	id, err := tenant.RequireTenant(ctx)
	if err != nil {
		return nil, err
	}

	// The key predicate is not an optimisation. Without it this returns the
	// first default-marked IPClass anywhere in the database, so any project
	// able to write one decides what every other project's claims allocate
	// from.
	rows, err := tx.Query(ctx,
		`SELECT data FROM ipam_objects
		  WHERE kind = 'IPClass'
		    AND key LIKE $2
		    AND ipam_data_to_jsonb(data) -> 'metadata' -> 'annotations' ->> $1 = 'true'
		  ORDER BY key`,
		ipamv1alpha1.IsDefaultClassAnnotation, classKeyPrefix(id.Name)+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("query default class for %s: %w", family, err)
	}

	var names []string
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan default class: %w", err)
		}
		var class ipamv1alpha1.IPClass
		if err := json.Unmarshal(data, &class); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode default class for %s: %w", family, err)
		}
		names = append(names, class.Name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query default class for %s: %w", family, err)
	}

	// After the rows are consumed: resolution queries the same transaction.
	for _, name := range names {
		resolved, err := resolveClassIn(ctx, tx, id.Name, name)
		if err != nil {
			return nil, err
		}
		if resolved.Spec.IPFamily == family {
			return resolved, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoDefaultClass, family)
}

// ResolveClass picks the class a claim allocates under: the one it names, or
// the default for the family it asked for.
func ResolveClass(ctx context.Context, tx pgx.Tx, className string, family ipamv1alpha1.IPFamily) (*ResolvedClass, error) {
	if className != "" {
		return LoadClass(ctx, tx, className)
	}
	if family == "" {
		return nil, fmt.Errorf("%w: claim names neither a class nor an address family", ErrNoDefaultClass)
	}
	return LoadDefaultClass(ctx, tx, family)
}

// LoadAncestry walks a class's parent chain and returns the provisioning
// classes, nearest first.
//
// Each parent is named relative to the class that names it, so a chain crossing
// into another project keeps every level keyed to its own definition's project.
//
// The leaf is deliberately absent: it binds an allocation directly and
// provisions nothing. A class with no parent yields an empty ancestry.
func LoadAncestry(ctx context.Context, tx pgx.Tx, leaf *ResolvedClass) ([]*ResolvedClass, error) {
	var chain []*ResolvedClass
	// Keyed by project and name: only a repeat of the same object is a cycle.
	seen := map[string]bool{classStorageKey(leaf.Project, leaf.Name): true}

	current := leaf
	for len(chain) <= MaxClassChainDepth {
		parentName := current.Spec.ParentClassName
		if parentName == "" {
			return chain, nil
		}

		parent, err := resolveClassIn(ctx, tx, current.Project, parentName)
		if err != nil {
			return nil, fmt.Errorf("resolve parent of %q: %w", current.Name, err)
		}
		key := classStorageKey(parent.Project, parent.Name)
		if seen[key] {
			return nil, fmt.Errorf("%w: cycle at class %q in project %q", ErrChainTooDeep, parent.Name, parent.Project)
		}
		seen[key] = true

		if parent.Spec.IPFamily != leaf.Spec.IPFamily {
			// Continuing would carve an address of one family out of a pool of
			// another.
			return nil, fmt.Errorf("class %q hands out %s but its ancestor %q hands out %s",
				leaf.Name, leaf.Spec.IPFamily, parent.Name, parent.Spec.IPFamily)
		}
		chain = append(chain, parent)
		current = parent
	}
	return nil, fmt.Errorf("%w: class %q exceeds %d levels", ErrChainTooDeep, leaf.Name, MaxClassChainDepth)
}

// EffectivePrefixLength resolves the block size a claim gets: the size it
// requested, or the class's default, within the class's allowed range.
func EffectivePrefixLength(class *ipamv1alpha1.IPClass, requested *int32) (int, error) {
	var length int32
	switch {
	case requested != nil:
		length = *requested
	case class.Spec.DefaultPrefixLength != 0:
		length = class.Spec.DefaultPrefixLength
	case class.Spec.AllowedPrefixLengths != nil && class.Spec.AllowedPrefixLengths.Min == class.Spec.AllowedPrefixLengths.Max:
		// A fixed-size class states its size once, as min == max.
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
// the claim's override, else the class default, else Delete. It is written onto
// the allocation because the allocation outlives the claim that chose it.
func EffectiveReclaimPolicy(class *ipamv1alpha1.IPClass, override ipamv1alpha1.ReclaimPolicy) ipamv1alpha1.ReclaimPolicy {
	if override != "" {
		return override
	}
	if class.Spec.ReclaimPolicy != "" {
		return class.Spec.ReclaimPolicy
	}
	return ipamv1alpha1.ReclaimDelete
}
