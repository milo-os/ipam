// Package ipclass provides REST storage for the IPClass resource, the policy
// object a claim names to say what kind of address it wants.
//
// A class is either a definition, which states the policy, or a reference,
// which names a definition in another project. The validation here is what
// keeps a reference from becoming a copy.
package ipclass

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/fieldindex"
	"go.miloapis.com/ipam/internal/registry/ipam/ipamvalidation"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// FieldIndexes are the SQL expression indexes backing IPClass field selectors
// declared in SelectableFields. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipclass_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPClass'`,
	},
	{
		IndexName:  "idx_ipam_ipclass_parent_class_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'parentClassName')) WHERE kind = 'IPClass'`,
	},
	{
		IndexName:  "idx_ipam_ipclass_source_project",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'source' ->> 'project')) WHERE kind = 'IPClass'`,
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

func (ipClassStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	c := obj.(*ipam.IPClass)
	c.Status = ipam.IPClassStatus{Phase: ipam.ClassPending}
}

func (ipClassStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPClass)
	o := old.(*ipam.IPClass)
	n.Status = o.Status
}

func (ipClassStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPClass(obj.(*ipam.IPClass))
}

func (ipClassStrategy) WarningsOnCreate(_ context.Context, obj runtime.Object) []string {
	return sharedPoolWarning(obj.(*ipam.IPClass))
}

// sharedPoolWarning states, once, what a provisioning class's poolPer means for
// consumers, when it does not name the reserved project role.
//
// It is a warning and not a rejection, and the reason is that the server cannot
// tell which answer is right. Shared is correct — required, even — for
// announceable public space: one /24 per location that every project draws
// from, where per-consumer blocks would exhaust an aggregate after 256 projects
// rather than 256 locations. Shared is wrong for a per-tenant prefix. The
// distinguishing fact is whether the roles named are project-scoped kinds, and
// answering that would need a table of kinds this service deliberately does not
// have: scope references are opaque {apiGroup, kind, name} strings.
//
// So the warning states the consequence rather than guessing at intent, and it
// fires exactly where the decision is made and is irreversible — poolPer is
// immutable, and the pools it identifies are never renumbered, so an author who
// meant the other thing has to replace the class rather than edit it. A
// warning costs a line of kubectl output and cannot refuse a valid class; the
// silence it replaces was how one /64 came to back two tenants' networks.
//
// Create only. An update cannot change poolPer, so repeating it on every status
// write would be noise about a decision nobody is making.
func sharedPoolWarning(c *ipam.IPClass) []string {
	if c.Spec.Source != nil || len(c.Spec.PoolPer) == 0 {
		return nil
	}
	roles, perConsumer := scope.PoolPerRoles(c.Spec.PoolPer)
	if perConsumer {
		return nil
	}
	return []string{fmt.Sprintf(
		"spec.poolPer does not name %q, so this class provisions one pool per {%s} SHARED by every consuming project. "+
			"That is correct for space every project draws from, such as an announceable public block, and wrong for a "+
			"per-tenant prefix. spec.poolPer is immutable, so add %q now if each project should get its own pool.",
		scope.ReservedRoleProject, strings.Join(roles, ", "), scope.ReservedRoleProject)}
}
func (ipClassStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipClassStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipClassStrategy) Canonicalize(_ runtime.Object)  {}

// ValidateUpdate rejects edits to the fields an existing allocation's identity
// was derived from. An allocation records the address it holds, not the policy
// that produced it, so a class edited underneath its allocations leaves them
// valid-looking and unreproducible.
func (ipClassStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPClass)
	o := old.(*ipam.IPClass)
	allErrs := validateIPClass(n)
	specPath := field.NewPath("spec")

	// Flipping between definition and reference, or re-pointing a reference,
	// changes which class every claim naming this one allocates under, and so
	// which pool and address space they already hold from.
	if !sourceEqual(n.Spec.Source, o.Spec.Source) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("source"), "spec.source is immutable"))
	}
	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamily"), "spec.ipFamily is immutable"))
	}
	if n.Spec.ParentClassName != o.Spec.ParentClassName {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("parentClassName"), "spec.parentClassName is immutable"))
	}
	if !stringsEqual(n.Spec.UniqueWithin, o.Spec.UniqueWithin) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("uniqueWithin"), "spec.uniqueWithin is immutable"))
	}
	if !stringsEqual(n.Spec.PoolPer, o.Spec.PoolPer) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("poolPer"), "spec.poolPer is immutable"))
	}
	return allErrs
}

func (ipClassStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string { return nil }

func validateIPClass(c *ipam.IPClass) field.ErrorList {
	if c.Spec.Source != nil {
		return validateReference(c)
	}
	return validateDefinition(c)
}

// validateReference enforces that a reference says only which class it points
// at.
//
// A reference that could also state UniqueWithin or PoolPer would be a copy,
// and two copies of one policy drift. The failure that produces is two holders
// of one address, reached on the success path, with each object valid in its
// own project.
func validateReference(c *ipam.IPClass) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	srcPath := specPath.Child("source")
	src := c.Spec.Source

	if src.Project == "" {
		allErrs = append(allErrs, field.Required(srcPath.Child("project"), "source.project is required"))
	} else if msgs := validation.IsDNS1123Subdomain(src.Project); len(msgs) > 0 {
		allErrs = append(allErrs, field.Invalid(srcPath.Child("project"), src.Project, msgs[0]))
	}
	if src.Name == "" {
		allErrs = append(allErrs, field.Required(srcPath.Child("name"), "source.name is required"))
	} else if msgs := validation.IsDNS1123Subdomain(src.Name); len(msgs) > 0 {
		allErrs = append(allErrs, field.Invalid(srcPath.Child("name"), src.Name, msgs[0]))
	}

	// Every field a definition may set, named individually so the error tells
	// the author which one to remove.
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"ipFamily", c.Spec.IPFamily != ""},
		{"parentClassName", c.Spec.ParentClassName != ""},
		{"poolPer", len(c.Spec.PoolPer) > 0},
		{"uniqueWithin", len(c.Spec.UniqueWithin) > 0},
		{"allowedPrefixLengths", c.Spec.AllowedPrefixLengths != nil},
		{"defaultPrefixLength", c.Spec.DefaultPrefixLength != 0},
		{"reservations", c.Spec.Reservations != nil},
		{"routing", c.Spec.Routing != ipam.RoutingSpec{}},
		{"strategy", c.Spec.Strategy != ""},
		{"reclaimPolicy", c.Spec.ReclaimPolicy != ""},
		{"retentionLease", c.Spec.RetentionLease != nil},
		{"provisioner", c.Spec.Provisioner != ""},
		{"parameters", len(c.Spec.Parameters) > 0},
	} {
		if f.set {
			allErrs = append(allErrs, field.Forbidden(specPath.Child(f.name),
				"a class with spec.source is a reference and states no policy of its own; "+
					"set this on the class it references"))
		}
	}
	return allErrs
}

func validateDefinition(c *ipam.IPClass) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	switch c.Spec.IPFamily {
	case "":
		allErrs = append(allErrs, field.Required(specPath.Child("ipFamily"), "ipFamily is required"))
	case ipam.IPv4, ipam.IPv6:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), c.Spec.IPFamily,
			[]string{string(ipam.IPv4), string(ipam.IPv6)}))
	}

	if c.Spec.ParentClassName == c.Name && c.Name != "" {
		allErrs = append(allErrs, field.Invalid(specPath.Child("parentClassName"), c.Spec.ParentClassName,
			"a class cannot be its own parent"))
	}

	if r := c.Spec.AllowedPrefixLengths; r != nil {
		rPath := specPath.Child("allowedPrefixLengths")
		if r.Min > r.Max {
			allErrs = append(allErrs, field.Invalid(rPath, fmt.Sprintf("%d-%d", r.Min, r.Max),
				"min must not exceed max"))
		}
		if d := c.Spec.DefaultPrefixLength; d != 0 && (d < r.Min || d > r.Max) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("defaultPrefixLength"), d,
				fmt.Sprintf("must fall within allowedPrefixLengths [%d, %d]", r.Min, r.Max)))
		}
	}

	// Reservations are applied to the pools a class provisions, so a class that
	// provisions none has nowhere to put them. Accepting the field there would
	// record a reservation that never reserves anything.
	if c.Spec.Reservations != nil && len(c.Spec.PoolPer) == 0 {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("reservations"),
			"reservations apply to the pools this class provisions; set spec.poolPer, "+
				"or state them on the pool itself"))
	}

	// The reserved role names the consuming project, which is meaningful only
	// as an axis of pool identity. uniqueWithin is already implicitly
	// per-project — an address space qualifies each ref by the claiming project
	// — so accepting it here would offer a second, redundant spelling of a
	// distinction the digest already makes, and an operator reading the two
	// fields could not tell which one was doing the work.
	for i, role := range c.Spec.UniqueWithin {
		if role == scope.ReservedRoleProject {
			allErrs = append(allErrs, field.Invalid(specPath.Child("uniqueWithin").Index(i), role,
				fmt.Sprintf("%q is a reserved scope role naming the consuming project; "+
					"uniqueWithin is already per-project, and the role is only meaningful in poolPer",
					scope.ReservedRoleProject)))
		}
	}

	// Apply the pool's own rule here, so a class cannot hold a reservation that
	// the pools it provisions would refuse.
	allErrs = append(allErrs, ipamvalidation.Reservations(
		c.Spec.Reservations, c.Spec.IPFamily, specPath.Child("reservations"))...)

	return allErrs
}

func sourceEqual(a, b *ipam.ClassSourceRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (ipClassStatusStrategy) NamespaceScoped() bool { return false }
func (ipClassStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPClass)
	o := old.(*ipam.IPClass)
	n.Spec = o.Spec
}
func (ipClassStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}
func (ipClassStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}
func (ipClassStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipClassStatusStrategy) AllowUnconditionalUpdate() bool { return false }
func (ipClassStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipClassStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("status")),
	}
}

func (ipClassStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
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
	sourceProject := ""
	if c.Spec.Source != nil {
		sourceProject = c.Spec.Source.Project
	}
	specific := fields.Set{
		"spec.ipFamily":        string(c.Spec.IPFamily),
		"spec.parentClassName": c.Spec.ParentClassName,
		"spec.source.project":  sourceProject,
	}
	return generic.MergeFieldsSets(objectMetaFields, specific)
}

func Match(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}
