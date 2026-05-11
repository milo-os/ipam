package asnclaim

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

// FieldIndexes are the SQL expression indexes that back ASNClaim field
// selectors declared in SelectableFields. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_asnclaim_pool_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'poolRef' ->> 'name')) WHERE kind = 'ASNClaim'`,
	},
}

type asnClaimStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type asnClaimStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) asnClaimStrategy {
	return asnClaimStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewStatusStrategy(typer runtime.ObjectTyper) asnClaimStatusStrategy {
	return asnClaimStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (asnClaimStrategy) NamespaceScoped() bool { return true }

func (asnClaimStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	c := obj.(*ipam.ASNClaim)
	c.Status = ipam.ASNClaimStatus{Phase: ipam.ClaimPending}
}

func (asnClaimStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.ASNClaim)
	o := old.(*ipam.ASNClaim)
	n.Status = o.Status
}

func (asnClaimStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateASNClaim(obj.(*ipam.ASNClaim))
}

func (asnClaimStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string { return nil }
func (asnClaimStrategy) AllowCreateOnUpdate() bool                                     { return false }
func (asnClaimStrategy) AllowUnconditionalUpdate() bool                                { return true }
func (asnClaimStrategy) Canonicalize(_ runtime.Object)                                 {}

func (asnClaimStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.ASNClaim)
	o := old.(*ipam.ASNClaim)
	allErrs := validateASNClaim(n)
	if !equality.Semantic.DeepEqual(n.Spec.PoolRef, o.Spec.PoolRef) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "poolRef"), "poolRef is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.ClassRef, o.Spec.ClassRef) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "classRef"), "classRef is immutable"))
	}
	return allErrs
}

func (asnClaimStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateASNClaim(c *ipam.ASNClaim) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	if c.Spec.PoolRef == nil && c.Spec.ClassRef == nil {
		allErrs = append(allErrs, field.Required(specPath, "exactly one of poolRef or classRef must be specified"))
	}
	if c.Spec.PoolRef != nil && c.Spec.ClassRef != nil {
		allErrs = append(allErrs, field.Forbidden(specPath, "poolRef and classRef are mutually exclusive"))
	}
	return allErrs
}

func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	c, ok := obj.(*ipam.ASNClaim)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an ASNClaim")
	}
	return c.Labels, SelectableFields(c), nil
}

func SelectableFields(c *ipam.ASNClaim) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&c.ObjectMeta, true)
	// spec.poolRef.name lets watches/lists filter by the targeted ASN
	// pool. Empty when the claim used a classRef instead, which is the
	// right behavior (no fixed pool to filter by).
	poolRefName := ""
	if c.Spec.PoolRef != nil {
		poolRefName = c.Spec.PoolRef.Name
	}
	return generic.MergeFieldsSets(objectMetaFields, fields.Set{
		"spec.poolRef.name": poolRefName,
	})
}

func MatchASNClaim(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}

func (asnClaimStatusStrategy) NamespaceScoped() bool { return true }

func (asnClaimStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.ASNClaim)
	o := old.(*ipam.ASNClaim)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (asnClaimStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (asnClaimStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (asnClaimStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (asnClaimStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (asnClaimStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (asnClaimStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
