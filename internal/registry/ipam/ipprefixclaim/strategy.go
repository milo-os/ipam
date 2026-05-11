package ipprefixclaim

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
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// FieldIndexes are the SQL expression indexes that back IPPrefixClaim field
// selectors declared in SelectableFields. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipprefixclaim_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPPrefixClaim'`,
	},
	{
		IndexName:  "idx_ipam_ipprefixclaim_prefix_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'prefixRef' ->> 'name')) WHERE kind = 'IPPrefixClaim'`,
	},
}

type ipPrefixClaimStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type ipPrefixClaimStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) ipPrefixClaimStrategy {
	return ipPrefixClaimStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewStatusStrategy(typer runtime.ObjectTyper) ipPrefixClaimStatusStrategy {
	return ipPrefixClaimStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipPrefixClaimStrategy) NamespaceScoped() bool { return true }

func (ipPrefixClaimStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	c := obj.(*ipam.IPPrefixClaim)
	c.Status = ipam.IPPrefixClaimStatus{Phase: ipam.ClaimPending}
}

func (ipPrefixClaimStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPPrefixClaim)
	o := old.(*ipam.IPPrefixClaim)
	n.Status = o.Status
}

func (ipPrefixClaimStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPPrefixClaim(obj.(*ipam.IPPrefixClaim))
}

func (ipPrefixClaimStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string {
	return nil
}

func (ipPrefixClaimStrategy) AllowCreateOnUpdate() bool        { return false }
func (ipPrefixClaimStrategy) AllowUnconditionalUpdate() bool   { return true }
func (ipPrefixClaimStrategy) Canonicalize(_ runtime.Object)    {}

func (ipPrefixClaimStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPPrefixClaim)
	o := old.(*ipam.IPPrefixClaim)
	allErrs := validateIPPrefixClaim(n)

	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "ipFamily"), "ipFamily is immutable"))
	}
	if n.Spec.PrefixLength != o.Spec.PrefixLength {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "prefixLength"), "prefixLength is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PrefixRef, o.Spec.PrefixRef) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "prefixRef"), "prefixRef is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PrefixSelector, o.Spec.PrefixSelector) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "prefixSelector"), "prefixSelector is immutable"))
	}
	return allErrs
}

func (ipPrefixClaimStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateIPPrefixClaim(c *ipam.IPPrefixClaim) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	if c.Spec.IPFamily == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("ipFamily"), "ipFamily is required"))
	} else if c.Spec.IPFamily != ipam.IPv4 && c.Spec.IPFamily != ipam.IPv6 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), c.Spec.IPFamily, []string{string(ipam.IPv4), string(ipam.IPv6)}))
	}
	if c.Spec.PrefixLength <= 0 {
		allErrs = append(allErrs, field.Invalid(specPath.Child("prefixLength"), c.Spec.PrefixLength, "prefixLength must be greater than 0"))
	}
	maxLen := 32
	if c.Spec.IPFamily == ipam.IPv6 {
		maxLen = 128
	}
	if c.Spec.PrefixLength > maxLen {
		allErrs = append(allErrs, field.Invalid(specPath.Child("prefixLength"), c.Spec.PrefixLength, fmt.Sprintf("prefixLength must not exceed %d for %s", maxLen, c.Spec.IPFamily)))
	}
	if c.Spec.PrefixRef == nil && c.Spec.PrefixSelector == nil {
		allErrs = append(allErrs, field.Required(specPath, "exactly one of prefixRef or prefixSelector must be specified"))
	}
	if c.Spec.PrefixRef != nil && c.Spec.PrefixSelector != nil {
		allErrs = append(allErrs, field.Forbidden(specPath, "prefixRef and prefixSelector are mutually exclusive"))
	}
	return allErrs
}

func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	c, ok := obj.(*ipam.IPPrefixClaim)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPPrefixClaim")
	}
	return c.Labels, SelectableFields(c), nil
}

func SelectableFields(c *ipam.IPPrefixClaim) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&c.ObjectMeta, true)
	// spec.prefixRef.name lets clients filter watches/lists by the
	// targeted pool — useful for operator dashboards and "what claims
	// reference this pool" queries. Empty when the claim used a
	// prefixSelector instead, which is the right behavior (no fixed
	// pool to filter by).
	prefixRefName := ""
	if c.Spec.PrefixRef != nil {
		prefixRefName = c.Spec.PrefixRef.Name
	}
	specific := fields.Set{
		"spec.ipFamily":      string(c.Spec.IPFamily),
		"spec.prefixRef.name": prefixRefName,
	}
	return generic.MergeFieldsSets(objectMetaFields, specific)
}

func MatchIPPrefixClaim(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}

func (ipPrefixClaimStatusStrategy) NamespaceScoped() bool { return true }

func (ipPrefixClaimStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPPrefixClaim)
	o := old.(*ipam.IPPrefixClaim)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (ipPrefixClaimStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (ipPrefixClaimStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (ipPrefixClaimStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipPrefixClaimStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipPrefixClaimStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipPrefixClaimStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
