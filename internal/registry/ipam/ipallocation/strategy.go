package ipallocation

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

func NewStrategy(typer runtime.ObjectTyper) ipAllocationStrategy {
	return ipAllocationStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
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
func (ipAllocationStrategy) AllowCreateOnUpdate(_ context.Context) bool      { return false }
func (ipAllocationStrategy) AllowUnconditionalUpdate(_ context.Context) bool { return true }
func (ipAllocationStrategy) Canonicalize(_ runtime.Object)                   {}

// ValidateUpdate refuses any change to spec. An allocation's spec is the
// server's record of what the allocation row holds and for whom, and every
// field of it is written in the transaction that writes the row. An edited
// claimRef in particular would make a bound allocation read as releasable.
// Labels and annotations remain the caller's.
func (ipAllocationStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPAllocation)
	o := old.(*ipam.IPAllocation)
	allErrs := validateIPAllocation(n)
	if !equality.Semantic.DeepEqual(n.Spec, o.Spec) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"),
			"spec is immutable; it records the allocation the server made"))
	}
	return allErrs
}

func (ipAllocationStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

// ValidateDelete protects a bound allocation from being deleted out from
// under the claim that holds it. The ipclaim Delete handler calls
// allocator.DeleteObject directly (bypassing strategy validation) when it
// tears down the claim, and ReleasingREST.Delete refuses a bound allocation
// itself, against the row as well as the object; this states the same rule
// for any path that reaches the store directly.
//
// A retained allocation has no claim — spec.claimRef is nil — and deleting it
// is the only way to give its address back. Refusing that would make Retain a
// one-way door.
func (ipAllocationStrategy) ValidateDelete(_ context.Context, obj runtime.Object) field.ErrorList {
	a := obj.(*ipam.IPAllocation)
	if a.Spec.ClaimRef != nil && a.Spec.ClaimRef.Name != "" {
		return field.ErrorList{field.Forbidden(
			field.NewPath("spec", "claimRef"),
			fmt.Sprintf("IPAllocation is bound to IPClaim %q; delete the claim instead", a.Spec.ClaimRef.Name),
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
