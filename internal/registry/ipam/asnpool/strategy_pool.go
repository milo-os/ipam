package asnpool

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// FieldIndexes are the SQL expression indexes that back ASNPool field
// selectors declared in GetPoolAttrs. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_asnpool_class_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'classRef' ->> 'name')) WHERE kind = 'ASNPool'`,
	},
}

type asnPoolStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type asnPoolStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewPoolStrategy(typer runtime.ObjectTyper) asnPoolStrategy {
	return asnPoolStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewPoolStatusStrategy(typer runtime.ObjectTyper) asnPoolStatusStrategy {
	return asnPoolStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (asnPoolStrategy) NamespaceScoped() bool { return false }

func (asnPoolStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	p := obj.(*ipam.ASNPool)
	// Synchronous apiserver: there is no controller to set Ready later.
	// Compute total capacity from spec.ranges and stamp a Ready condition
	// when the ranges are well-formed; Validate rejects malformed ones.
	var total int64
	wellFormed := len(p.Spec.Ranges) > 0
	for _, r := range p.Spec.Ranges {
		if r.Start <= 0 || r.End < r.Start {
			wellFormed = false
			continue
		}
		total += r.End - r.Start + 1
	}
	p.Status = ipam.ASNPoolStatus{
		Capacity: ipam.ASNCapacity{Total: total},
	}
	if wellFormed {
		p.Status.Conditions = []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "PoolReady",
			Message:            "ASNPool is ready for allocation",
			LastTransitionTime: metav1.Now(),
		}}
	}
}

func (asnPoolStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.ASNPool)
	o := old.(*ipam.ASNPool)
	n.Status = o.Status
}

func (asnPoolStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateASNPool(obj.(*ipam.ASNPool))
}

func (asnPoolStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string { return nil }
func (asnPoolStrategy) AllowCreateOnUpdate() bool                                    { return false }
func (asnPoolStrategy) AllowUnconditionalUpdate() bool                               { return true }
func (asnPoolStrategy) Canonicalize(_ runtime.Object)                                {}

func (asnPoolStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.ASNPool)
	o := old.(*ipam.ASNPool)
	allErrs := validateASNPool(n)
	if n.Spec.ClassRef != o.Spec.ClassRef {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "classRef"), "spec.classRef is immutable"))
	}
	return allErrs
}

func (asnPoolStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

// asnMax is the largest valid 4-byte ASN per RFC 6793. Pool ranges that
// exceed this are rejected at create — without the bound, allocator output
// would silently include values that are not real ASNs.
const asnMax = int64(4_294_967_295)

func validateASNPool(p *ipam.ASNPool) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	if len(p.Spec.Ranges) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("ranges"), "at least one range is required"))
	}
	for i, r := range p.Spec.Ranges {
		rPath := specPath.Child("ranges").Index(i)
		if r.Start <= 0 {
			allErrs = append(allErrs, field.Invalid(rPath.Child("start"), r.Start, "start must be > 0"))
		}
		if r.Start > asnMax {
			allErrs = append(allErrs, field.Invalid(rPath.Child("start"), r.Start, "start exceeds maximum 32-bit ASN value (4294967295)"))
		}
		if r.End < r.Start {
			allErrs = append(allErrs, field.Invalid(rPath.Child("end"), r.End, "end must be >= start"))
		}
		if r.End > asnMax {
			allErrs = append(allErrs, field.Invalid(rPath.Child("end"), r.End, "end exceeds maximum 32-bit ASN value (4294967295)"))
		}
	}
	if p.Spec.ClassRef.Name == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("classRef", "name"), "classRef.name is required"))
	}
	return allErrs
}

func GetPoolAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	p, ok := obj.(*ipam.ASNPool)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an ASNPool")
	}
	return p.Labels, generic.MergeFieldsSets(generic.ObjectMetaFieldsSet(&p.ObjectMeta, false), fields.Set{
		"spec.classRef.name": p.Spec.ClassRef.Name,
	}), nil
}

func MatchASNPool(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetPoolAttrs}
}

func (asnPoolStatusStrategy) NamespaceScoped() bool { return false }

func (asnPoolStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.ASNPool)
	o := old.(*ipam.ASNPool)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (asnPoolStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (asnPoolStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (asnPoolStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (asnPoolStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (asnPoolStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (asnPoolStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
