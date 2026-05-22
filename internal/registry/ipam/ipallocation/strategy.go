package ipallocation

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
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/fieldindex"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// FieldIndexes are the SQL expression indexes backing IPAllocation field
// selectors declared in SelectableFields. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipallocation_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPAllocation'`,
	},
	{
		IndexName:  "idx_ipam_ipallocation_pool_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'poolRef' ->> 'name')) WHERE kind = 'IPAllocation'`,
	},
}

type ipAllocationStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type ipAllocationStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) ipAllocationStrategy {
	return ipAllocationStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewStatusStrategy(typer runtime.ObjectTyper) ipAllocationStatusStrategy {
	return ipAllocationStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipAllocationStrategy) NamespaceScoped() bool { return true }

func (ipAllocationStrategy) PrepareForCreate(_ context.Context, _ runtime.Object) {}

func (ipAllocationStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPAllocation)
	o := old.(*ipam.IPAllocation)
	n.Status = o.Status
}

func (ipAllocationStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPAllocation(obj.(*ipam.IPAllocation))
}

func (ipAllocationStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string {
	return nil
}
func (ipAllocationStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipAllocationStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipAllocationStrategy) Canonicalize(_ runtime.Object)  {}

func (ipAllocationStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPAllocation)
	o := old.(*ipam.IPAllocation)
	allErrs := validateIPAllocation(n)
	specPath := field.NewPath("spec")
	if n.Spec.CIDR != o.Spec.CIDR {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("cidr"), "spec.cidr is immutable"))
	}
	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamily"), "spec.ipFamily is immutable"))
	}
	if n.Spec.PoolRef.Name != o.Spec.PoolRef.Name {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("poolRef"), "spec.poolRef is immutable"))
	}
	return allErrs
}

func (ipAllocationStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

// ValidateDelete protects against direct user-initiated deletes of
// IPAllocation rows. The ipclaim Delete handler calls
// allocator.DeleteObject directly (bypassing strategy validation) when it
// tears down the claim, so this guard only fires for clients hitting the
// /ipallocations endpoint with `kubectl delete`.
func (ipAllocationStrategy) ValidateDelete(_ context.Context, obj runtime.Object) field.ErrorList {
	a := obj.(*ipam.IPAllocation)
	if a.Spec.PoolRef.Name != "" {
		return field.ErrorList{field.Forbidden(
			field.NewPath("spec", "poolRef"),
			"IPAllocation is managed by its owning IPClaim; delete the claim instead",
		)}
	}
	return nil
}

func validateIPAllocation(a *ipam.IPAllocation) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	if a.Spec.PoolRef.Name == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("poolRef", "name"), "poolRef.name is required"))
	}
	if a.Spec.IPFamily == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("ipFamily"), "ipFamily is required"))
	} else if a.Spec.IPFamily != ipam.IPv4 && a.Spec.IPFamily != ipam.IPv6 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), a.Spec.IPFamily, []string{string(ipam.IPv4), string(ipam.IPv6)}))
	}
	return allErrs
}

func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	a, ok := obj.(*ipam.IPAllocation)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPAllocation")
	}
	return a.Labels, SelectableFields(a), nil
}

func SelectableFields(a *ipam.IPAllocation) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&a.ObjectMeta, true)
	specific := fields.Set{
		"spec.ipFamily":     string(a.Spec.IPFamily),
		"spec.poolRef.name": a.Spec.PoolRef.Name,
	}
	return generic.MergeFieldsSets(objectMetaFields, specific)
}

func Match(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}

func (ipAllocationStatusStrategy) NamespaceScoped() bool { return true }

func (ipAllocationStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPAllocation)
	o := old.(*ipam.IPAllocation)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (ipAllocationStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (ipAllocationStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (ipAllocationStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipAllocationStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipAllocationStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipAllocationStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
