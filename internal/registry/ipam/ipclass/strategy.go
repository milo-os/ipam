// Package ipclass provides REST storage for the cluster-scoped IPClass
// resource. An IPClass is a platform-owned allocation policy — the analog of a
// Kubernetes StorageClass. It carries no addresses and drives no allocation of
// its own, so the storage is plain CRUD (no allocator or db dependency); the
// IPClaim handler reads a class to place an allocation, and pools opt in to
// backing a class via spec.classNames.
package ipclass

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"

	"go.miloapis.com/ipam/internal/fieldindex"
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
		IndexName:  "idx_ipam_ipclass_provisioner",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'provisioner')) WHERE kind = 'IPClass'`,
	},
}

type ipClassStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) ipClassStrategy {
	return ipClassStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipClassStrategy) NamespaceScoped() bool { return false }

// PrepareForCreate defaults an empty provisioner to the native allocator, the
// only provisioner shipped today. Everything else is validated as-authored.
func (ipClassStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	c := obj.(*ipam.IPClass)
	if c.Spec.Provisioner == "" {
		c.Spec.Provisioner = ipam.NativeProvisioner
	}
}

// PrepareForUpdate keeps the same provisioner defaulting for updates that clear
// the field.
func (ipClassStrategy) PrepareForUpdate(_ context.Context, obj, _ runtime.Object) {
	c := obj.(*ipam.IPClass)
	if c.Spec.Provisioner == "" {
		c.Spec.Provisioner = ipam.NativeProvisioner
	}
}

func (ipClassStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPClass(obj.(*ipam.IPClass))
}

func (ipClassStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string { return nil }
func (ipClassStrategy) AllowCreateOnUpdate() bool                                     { return false }
func (ipClassStrategy) AllowUnconditionalUpdate() bool                                { return true }
func (ipClassStrategy) Canonicalize(_ runtime.Object)                                 {}

// ValidateUpdate re-runs the full field validation and freezes the two fields
// that would strand existing allocations if changed: the address family and
// the provisioner that satisfies the class.
func (ipClassStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPClass)
	o := old.(*ipam.IPClass)
	allErrs := validateIPClass(n)
	specPath := field.NewPath("spec")
	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamily"), "spec.ipFamily is immutable"))
	}
	if n.Spec.Provisioner != o.Spec.Provisioner {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("provisioner"), "spec.provisioner is immutable"))
	}
	return allErrs
}

func (ipClassStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

// familyMaxPrefix is the widest prefix length valid for a family: 32 bits for
// IPv4, 128 for IPv6.
func familyMaxPrefix(f ipam.IPFamily) int {
	if f == ipam.IPv6 {
		return 128
	}
	return 32
}

func validateIPClass(c *ipam.IPClass) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Provisioner — only the native allocator ships today; a class naming any
	// other provisioner is rejected until that provisioner exists.
	if c.Spec.Provisioner != ipam.NativeProvisioner {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("provisioner"),
			c.Spec.Provisioner, []string{ipam.NativeProvisioner}))
	}

	// IPFamily — a class is single-family and must declare it.
	switch c.Spec.IPFamily {
	case ipam.IPv4, ipam.IPv6:
		// ok
	case "":
		allErrs = append(allErrs, field.Required(specPath.Child("ipFamily"),
			"ipFamily is required; a class hands out a single address family"))
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"),
			c.Spec.IPFamily, []string{string(ipam.IPv4), string(ipam.IPv6)}))
	}

	// Strategy — optional, but must be a known strategy when set.
	switch c.Spec.Strategy {
	case "", ipam.FirstFit, ipam.BestFit, ipam.LeastUtilized:
		// ok
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("strategy"),
			c.Spec.Strategy, []string{string(ipam.FirstFit), string(ipam.BestFit), string(ipam.LeastUtilized)}))
	}

	// ReclaimPolicy — optional, but must be Delete or Retain when set.
	switch c.Spec.ReclaimPolicy {
	case "", ipam.ReclaimDelete, ipam.ReclaimRetain:
		// ok
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("reclaimPolicy"),
			c.Spec.ReclaimPolicy, []string{string(ipam.ReclaimDelete), string(ipam.ReclaimRetain)}))
	}

	// Visibility — reuses the pool sharing model.
	switch c.Spec.Visibility {
	case "", "platform", "consumer", "shared":
		// ok
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("visibility"),
			c.Spec.Visibility, []string{"", "platform", "consumer", "shared"}))
	}

	// AllowedPrefixLengths — bounds must be sane and lie within the family's
	// valid prefix range. Only enforce the family-relative ceiling when the
	// family itself is valid, to avoid piling confusing errors on a bad family.
	allowed := c.Spec.AllowedPrefixLengths
	prefixPath := specPath.Child("allowedPrefixLengths")
	familyOK := c.Spec.IPFamily == ipam.IPv4 || c.Spec.IPFamily == ipam.IPv6
	maxLen := familyMaxPrefix(c.Spec.IPFamily)

	if allowed.Min < 0 {
		allErrs = append(allErrs, field.Invalid(prefixPath.Child("min"), allowed.Min, "must be >= 0"))
	}
	if allowed.Max < 0 {
		allErrs = append(allErrs, field.Invalid(prefixPath.Child("max"), allowed.Max, "must be >= 0"))
	}
	if allowed.Min > 0 && allowed.Max > 0 && allowed.Min > allowed.Max {
		allErrs = append(allErrs, field.Invalid(prefixPath, allowed,
			"allowedPrefixLengths.min must be <= allowedPrefixLengths.max"))
	}
	if familyOK {
		if allowed.Min > maxLen {
			allErrs = append(allErrs, field.Invalid(prefixPath.Child("min"), allowed.Min,
				fmt.Sprintf("must be <= %d for %s", maxLen, c.Spec.IPFamily)))
		}
		if allowed.Max > maxLen {
			allErrs = append(allErrs, field.Invalid(prefixPath.Child("max"), allowed.Max,
				fmt.Sprintf("must be <= %d for %s", maxLen, c.Spec.IPFamily)))
		}
	}

	// DefaultPrefixLength — optional (0 means "no default; claims must ask");
	// when set it must fall inside the allowed range and the family bounds.
	if def := c.Spec.DefaultPrefixLength; def != 0 {
		defPath := specPath.Child("defaultPrefixLength")
		if def < 0 {
			allErrs = append(allErrs, field.Invalid(defPath, def, "must be >= 0"))
		}
		if familyOK && def > maxLen {
			allErrs = append(allErrs, field.Invalid(defPath, def,
				fmt.Sprintf("must be <= %d for %s", maxLen, c.Spec.IPFamily)))
		}
		if allowed.Min > 0 && def < allowed.Min {
			allErrs = append(allErrs, field.Invalid(defPath, def,
				fmt.Sprintf("must be >= allowedPrefixLengths.min (%d)", allowed.Min)))
		}
		if allowed.Max > 0 && def > allowed.Max {
			allErrs = append(allErrs, field.Invalid(defPath, def,
				fmt.Sprintf("must be <= allowedPrefixLengths.max (%d)", allowed.Max)))
		}
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
		"spec.ipFamily":    string(c.Spec.IPFamily),
		"spec.provisioner": c.Spec.Provisioner,
	}
	return generic.MergeFieldsSets(objectMetaFields, specific)
}

func Match(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}
