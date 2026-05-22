package ipclaim

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

// FieldIndexes are the SQL expression indexes backing IPClaim field selectors
// declared in SelectableFields. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipclaim_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPClaim'`,
	},
	{
		IndexName:  "idx_ipam_ipclaim_pool_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'poolRef' ->> 'name')) WHERE kind = 'IPClaim'`,
	},
}

type ipClaimStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type ipClaimStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) ipClaimStrategy {
	return ipClaimStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewStatusStrategy(typer runtime.ObjectTyper) ipClaimStatusStrategy {
	return ipClaimStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipClaimStrategy) NamespaceScoped() bool { return true }

func (ipClaimStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	c := obj.(*ipam.IPClaim)
	c.Status = ipam.IPClaimStatus{Phase: ipam.ClaimPending}
}

func (ipClaimStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPClaim)
	o := old.(*ipam.IPClaim)
	n.Status = o.Status
}

func (ipClaimStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPClaim(obj.(*ipam.IPClaim))
}

func (ipClaimStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string {
	return nil
}

func (ipClaimStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipClaimStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipClaimStrategy) Canonicalize(_ runtime.Object)  {}

func (ipClaimStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPClaim)
	o := old.(*ipam.IPClaim)
	allErrs := validateIPClaim(n)

	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "ipFamily"), "ipFamily is immutable"))
	}
	if n.Spec.PrefixLength != o.Spec.PrefixLength {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "prefixLength"), "prefixLength is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PoolRef, o.Spec.PoolRef) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "poolRef"), "poolRef is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PoolSelector, o.Spec.PoolSelector) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "poolSelector"), "poolSelector is immutable"))
	}
	return allErrs
}

func (ipClaimStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateIPClaim(c *ipam.IPClaim) field.ErrorList {
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
	if c.Spec.PoolRef == nil && c.Spec.PoolSelector == nil {
		allErrs = append(allErrs, field.Required(specPath, "exactly one of poolRef or poolSelector must be specified"))
	}
	if c.Spec.PoolRef != nil && c.Spec.PoolSelector != nil {
		allErrs = append(allErrs, field.Forbidden(specPath, "poolRef and poolSelector are mutually exclusive"))
	}
	return allErrs
}

func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	c, ok := obj.(*ipam.IPClaim)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPClaim")
	}
	return c.Labels, SelectableFields(c), nil
}

func SelectableFields(c *ipam.IPClaim) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&c.ObjectMeta, true)
	// spec.poolRef.name lets clients filter watches/lists by the targeted
	// pool. Empty when the claim used a poolSelector instead, which is the
	// right behaviour (no fixed pool to filter by).
	poolRefName := ""
	if c.Spec.PoolRef != nil {
		poolRefName = c.Spec.PoolRef.Name
	}
	specific := fields.Set{
		"spec.ipFamily":     string(c.Spec.IPFamily),
		"spec.poolRef.name": poolRefName,
	}
	return generic.MergeFieldsSets(objectMetaFields, specific)
}

func MatchIPClaim(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}

func (ipClaimStatusStrategy) NamespaceScoped() bool { return true }

func (ipClaimStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPClaim)
	o := old.(*ipam.IPClaim)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (ipClaimStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (ipClaimStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (ipClaimStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipClaimStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipClaimStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipClaimStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
