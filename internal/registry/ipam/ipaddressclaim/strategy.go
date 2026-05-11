package ipaddressclaim

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

// FieldIndexes are the SQL expression indexes that back IPAddressClaim field
// selectors declared in SelectableFields. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipaddressclaim_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPAddressClaim'`,
	},
	{
		IndexName:  "idx_ipam_ipaddressclaim_prefix_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'prefixRef' ->> 'name')) WHERE kind = 'IPAddressClaim'`,
	},
}

type ipAddressClaimStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type ipAddressClaimStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) ipAddressClaimStrategy {
	return ipAddressClaimStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewStatusStrategy(typer runtime.ObjectTyper) ipAddressClaimStatusStrategy {
	return ipAddressClaimStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipAddressClaimStrategy) NamespaceScoped() bool { return true }

func (ipAddressClaimStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	c := obj.(*ipam.IPAddressClaim)
	c.Status = ipam.IPAddressClaimStatus{Phase: ipam.ClaimPending}
}

func (ipAddressClaimStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPAddressClaim)
	o := old.(*ipam.IPAddressClaim)
	n.Status = o.Status
}

func (ipAddressClaimStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPAddressClaim(obj.(*ipam.IPAddressClaim))
}

func (ipAddressClaimStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string {
	return nil
}

func (ipAddressClaimStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipAddressClaimStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipAddressClaimStrategy) Canonicalize(_ runtime.Object)  {}

func (ipAddressClaimStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPAddressClaim)
	o := old.(*ipam.IPAddressClaim)
	allErrs := validateIPAddressClaim(n)
	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "ipFamily"), "ipFamily is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PrefixRef, o.Spec.PrefixRef) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "prefixRef"), "prefixRef is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PrefixSelector, o.Spec.PrefixSelector) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "prefixSelector"), "prefixSelector is immutable"))
	}
	return allErrs
}

func (ipAddressClaimStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateIPAddressClaim(c *ipam.IPAddressClaim) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	if c.Spec.IPFamily == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("ipFamily"), "ipFamily is required"))
	} else if c.Spec.IPFamily != ipam.IPv4 && c.Spec.IPFamily != ipam.IPv6 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), c.Spec.IPFamily, []string{string(ipam.IPv4), string(ipam.IPv6)}))
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
	c, ok := obj.(*ipam.IPAddressClaim)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPAddressClaim")
	}
	return c.Labels, SelectableFields(c), nil
}

func SelectableFields(c *ipam.IPAddressClaim) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&c.ObjectMeta, true)
	// spec.prefixRef.name surfaces the targeted pool for filtered
	// watches/lists (e.g. "show all address claims against this pool").
	// Empty for selector-based claims by design.
	prefixRefName := ""
	if c.Spec.PrefixRef != nil {
		prefixRefName = c.Spec.PrefixRef.Name
	}
	return generic.MergeFieldsSets(objectMetaFields, fields.Set{
		"spec.ipFamily":      string(c.Spec.IPFamily),
		"spec.prefixRef.name": prefixRefName,
	})
}

func MatchIPAddressClaim(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}

func (ipAddressClaimStatusStrategy) NamespaceScoped() bool { return true }

func (ipAddressClaimStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPAddressClaim)
	o := old.(*ipam.IPAddressClaim)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (ipAddressClaimStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (ipAddressClaimStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (ipAddressClaimStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipAddressClaimStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipAddressClaimStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipAddressClaimStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
