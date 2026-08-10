// Package ipclass provides REST storage for the cluster-scoped IPClass
// resource — the operator-authored policy object a consumer names to get an
// address.
//
// A class carries only what the allocator needs to hand an address out: which
// space it comes from (ParentClassName, or the pools offering it), and what it
// must not collide with (UniqueWithin). Nothing on a class selects an
// allocation; a claim binds one when it is created.
//
// Validation splits in two. Everything decidable from the object alone lives in
// ipClassStrategy below. Everything needing the rest of the catalog — the
// parent chain, the family agreement across it, the per-family default marker —
// lives in the Create/Update overrides in storage.go, because a strategy has no
// store to read.
package ipclass

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/fieldindex"
	"go.miloapis.com/ipam/internal/registry/ipam/ipamvalidation"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// FieldIndexes are the SQL expression indexes backing IPClass field selectors
// declared in SelectableFields. Applied idempotently by SyncIndexes.
//
// spec.parentClassName is indexed because the cascade resolves a chain upward
// on every first claim into a new scope, and the "is any class my child?"
// question is asked on every class write.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipclass_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPClass'`,
	},
	{
		IndexName:  "idx_ipam_ipclass_parent_class_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'parentClassName')) WHERE kind = 'IPClass'`,
	},
}

type ipClassStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type ipClassStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) ipClassStrategy {
	return ipClassStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewStatusStrategy(typer runtime.ObjectTyper) ipClassStatusStrategy {
	return ipClassStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipClassStrategy) NamespaceScoped() bool { return false }

// PrepareForCreate applies the class-level defaults and seeds status. A class
// is a policy object with no provisioning step of its own, so it becomes Ready
// immediately; the pool counts in status are recomputed at read time (class
// health is computed, never stored — a maintained counter would make one row
// every pool of the class contends on).
func (ipClassStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	c := obj.(*ipam.IPClass)
	if c.Spec.Strategy == "" {
		c.Spec.Strategy = ipam.PoolFirstFit
	}
	if c.Spec.ReclaimPolicy == "" {
		c.Spec.ReclaimPolicy = ipam.ReclaimDelete
	}
	if c.Spec.Routing.Internal == "" {
		c.Spec.Routing.Internal = ipam.InternalRoutingNone
	}
	if c.Spec.Routing.External == "" {
		c.Spec.Routing.External = ipam.ExternalRoutingNone
	}
	// RequiredScopeRoles is carried through rather than reset. The Create
	// override computed it from the class catalog a moment ago — the one place
	// the parent chain is in hand — and a strategy has no store to recompute it
	// from.
	c.Status = ipam.IPClassStatus{
		Phase:              ipam.ClassReady,
		RequiredScopeRoles: c.Status.RequiredScopeRoles,
	}
}

// PrepareForUpdate keeps status out of a spec write, with one exception:
// RequiredScopeRoles is derived from the spec being written, so carrying the old
// value forward would leave it describing the previous class. The Update
// override recomputes it before this runs.
func (ipClassStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPClass)
	o := old.(*ipam.IPClass)
	roles := n.Status.RequiredScopeRoles
	n.Status = o.Status
	n.Status.RequiredScopeRoles = roles
}

func (ipClassStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPClass(obj.(*ipam.IPClass))
}

func (ipClassStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string { return nil }
func (ipClassStrategy) AllowCreateOnUpdate() bool                                     { return false }
func (ipClassStrategy) AllowUnconditionalUpdate() bool                                { return true }
func (ipClassStrategy) Canonicalize(_ runtime.Object)                                 {}

// ValidateUpdate enforces the immutable set. Each of these fields is immutable
// for the same underlying reason: allocations already handed out were placed
// according to its value, and changing it strands them somewhere the class no
// longer describes.
//
//   - ipFamily: an allocation is an address of one family, and nothing can
//     convert one.
//   - parentClassName: allocations sit inside the old parent's space, outside
//     the declared ancestry the moment it changes.
//   - uniqueWithin: this field states the uniqueness guarantee, and the
//     allocator's search follows from it. Narrowing it retroactively asserts a
//     guarantee that was never enforced when the existing allocations were made.
//   - poolPer: it decides which pools exist. Changing it orphans every pool
//     already provisioned under the old projection, along with everything
//     carved from them.
//   - provisioner: a class's allocations were realised by one component; another
//     one cannot adopt them.
func (ipClassStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPClass)
	o := old.(*ipam.IPClass)
	allErrs := validateIPClass(n)
	specPath := field.NewPath("spec")

	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamily"),
			"spec.ipFamily is immutable"))
	}
	if n.Spec.ParentClassName != o.Spec.ParentClassName {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("parentClassName"),
			"spec.parentClassName is immutable: changing it strands every existing allocation outside its declared ancestry"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.UniqueWithin, o.Spec.UniqueWithin) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("uniqueWithin"),
			"spec.uniqueWithin is immutable: it states the uniqueness guarantee that existing allocations were made under"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PoolPer, o.Spec.PoolPer) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("poolPer"),
			"spec.poolPer is immutable: it decides which pools exist, and changing it orphans every pool already provisioned under the old projection"))
	}
	if n.Spec.Provisioner != o.Spec.Provisioner {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("provisioner"),
			"spec.provisioner is immutable"))
	}
	return allErrs
}

func (ipClassStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

// maxPrefixLenFor is the widest prefix length an address of the given family
// can carry.
func maxPrefixLenFor(family ipam.IPFamily) int32 {
	if family == ipam.IPv6 {
		return 128
	}
	return 32
}

// validateIPClass checks everything decidable without reading other classes.
// Cross-class rules — parent existence, family agreement along the chain,
// prefix lengths against the parent's, cycles, and the per-family default
// marker — are enforced in storage.go where a store is available.
func validateIPClass(c *ipam.IPClass) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	switch c.Spec.IPFamily {
	case "":
		allErrs = append(allErrs, field.Required(specPath.Child("ipFamily"),
			"ipFamily is required: a class hands out one address family, and dual-stack is two classes"))
	case ipam.IPv4, ipam.IPv6:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), c.Spec.IPFamily,
			[]string{string(ipam.IPv4), string(ipam.IPv6)}))
	}

	if c.Spec.ParentClassName == c.Name && c.Name != "" {
		allErrs = append(allErrs, field.Invalid(specPath.Child("parentClassName"), c.Spec.ParentClassName,
			"a class cannot be its own parent"))
	}

	allErrs = append(allErrs, validateRoleList(c.Spec.PoolPer, specPath.Child("poolPer"))...)
	allErrs = append(allErrs, validateRoleList(c.Spec.UniqueWithin, specPath.Child("uniqueWithin"))...)

	maxLen := maxPrefixLenFor(c.Spec.IPFamily)
	if r := c.Spec.AllowedPrefixLengths; r != nil {
		lenPath := specPath.Child("allowedPrefixLengths")
		if r.Min < 0 || r.Min > maxLen {
			allErrs = append(allErrs, field.Invalid(lenPath.Child("min"), r.Min,
				fmt.Sprintf("must be in [0, %d] for %s", maxLen, c.Spec.IPFamily)))
		}
		if r.Max < 0 || r.Max > maxLen {
			allErrs = append(allErrs, field.Invalid(lenPath.Child("max"), r.Max,
				fmt.Sprintf("must be in [0, %d] for %s", maxLen, c.Spec.IPFamily)))
		}
		if r.Min > r.Max {
			allErrs = append(allErrs, field.Invalid(lenPath, r,
				"min must be <= max"))
		}
	}

	if c.Spec.DefaultPrefixLength != 0 {
		defPath := specPath.Child("defaultPrefixLength")
		if c.Spec.DefaultPrefixLength < 0 || c.Spec.DefaultPrefixLength > maxLen {
			allErrs = append(allErrs, field.Invalid(defPath, c.Spec.DefaultPrefixLength,
				fmt.Sprintf("must be in [0, %d] for %s", maxLen, c.Spec.IPFamily)))
		} else if r := c.Spec.AllowedPrefixLengths; r != nil &&
			(c.Spec.DefaultPrefixLength < r.Min || c.Spec.DefaultPrefixLength > r.Max) {
			allErrs = append(allErrs, field.Invalid(defPath, c.Spec.DefaultPrefixLength,
				fmt.Sprintf("must fall within allowedPrefixLengths [%d, %d]", r.Min, r.Max)))
		}
	}

	switch c.Spec.Strategy {
	case "", ipam.PoolFirstFit, ipam.PoolLeastUtilized:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("strategy"), c.Spec.Strategy,
			[]string{string(ipam.PoolFirstFit), string(ipam.PoolLeastUtilized)}))
	}

	switch c.Spec.ReclaimPolicy {
	case "", ipam.ReclaimDelete, ipam.ReclaimRetain:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("reclaimPolicy"), c.Spec.ReclaimPolicy,
			[]string{string(ipam.ReclaimDelete), string(ipam.ReclaimRetain)}))
	}

	switch c.Spec.Routing.Internal {
	case "", ipam.InternalRoutingNone, ipam.InternalRoutingHost:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("routing", "internal"), c.Spec.Routing.Internal,
			[]string{string(ipam.InternalRoutingNone), string(ipam.InternalRoutingHost)}))
	}

	switch c.Spec.Routing.External {
	case "", ipam.ExternalRoutingNone, ipam.ExternalRoutingAggregate:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("routing", "external"), c.Spec.Routing.External,
			[]string{string(ipam.ExternalRoutingNone), string(ipam.ExternalRoutingAggregate)}))
	}

	switch c.Spec.Visibility {
	case "", ipam.VisibilityPlatform, ipam.VisibilityConsumer, ipam.VisibilityShared:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("visibility"), c.Spec.Visibility,
			[]string{ipam.VisibilityPlatform, ipam.VisibilityConsumer, ipam.VisibilityShared}))
	}

	allErrs = append(allErrs, ipamvalidation.Reservations(
		c.Spec.Reservations, c.Spec.IPFamily, specPath.Child("reservations"))...)
	allErrs = append(allErrs, ipamvalidation.Lease(
		c.Spec.RetentionLease, specPath.Child("retentionLease"))...)

	// Reservations describe the pools this class provisions. A class that
	// provisions nothing — no PoolPer — has nowhere to apply them, so stating
	// them is a mistake worth catching rather than a no-op worth tolerating:
	// the author meant a reservation to happen somewhere, and it would not.
	if c.Spec.Reservations != nil && len(c.Spec.PoolPer) == 0 {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("reservations"),
			"spec.reservations applies to the pools this class provisions, and this class provisions none; set spec.poolPer, or state the reservation on the pool itself"))
	}

	if v, ok := c.Annotations[ipam.IsDefaultClassAnnotation]; ok && v != "true" && v != "false" {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("metadata", "annotations").Key(ipam.IsDefaultClassAnnotation), v,
			`must be "true" or "false"`))
	}

	return allErrs
}

// validateRoleList checks a list of scope role names. Roles are opaque to the
// allocator — it never learns what a network or a location is — but they are
// map keys, so they must be non-empty and distinct or a claim's scope cannot be
// projected onto them unambiguously.
func validateRoleList(roles []string, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	seen := make(map[string]bool, len(roles))
	for i, role := range roles {
		if role == "" {
			allErrs = append(allErrs, field.Required(path.Index(i), "role name must not be empty"))
			continue
		}
		if seen[role] {
			allErrs = append(allErrs, field.Duplicate(path.Index(i), role))
			continue
		}
		seen[role] = true
	}
	return allErrs
}

func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	c, ok := obj.(*ipam.IPClass)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPClass")
	}
	return c.Labels, SelectableFields(c), nil
}

func SelectableFields(c *ipam.IPClass) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&c.ObjectMeta, false)
	specific := fields.Set{
		"spec.ipFamily":        string(c.Spec.IPFamily),
		"spec.parentClassName": c.Spec.ParentClassName,
	}
	return generic.MergeFieldsSets(objectMetaFields, specific)
}

func Match(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}

func (ipClassStatusStrategy) NamespaceScoped() bool { return false }

func (ipClassStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPClass)
	o := old.(*ipam.IPClass)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (ipClassStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (ipClassStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (ipClassStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipClassStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipClassStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipClassStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
