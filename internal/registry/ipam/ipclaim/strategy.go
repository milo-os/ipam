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
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// FieldIndexes are the SQL expression indexes backing IPClaim field selectors
// declared in SelectableFields. Applied idempotently by SyncIndexes.
//
// The spec.ipFamily and spec.poolRef.name indexes migration 002 drops are
// deliberately absent: a claim no longer names a pool, and nothing lists claims
// by family — they are listed by class. Leaving a Go declaration behind for a
// dropped index would have SyncIndexes recreate it seconds after the migration
// removed it.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipclaim_class_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'className')) WHERE kind = 'IPClaim'`,
	},
	{
		IndexName:  "idx_ipam_ipclaim_status_pool_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'status' -> 'poolRef' ->> 'name')) WHERE kind = 'IPClaim'`,
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

// ValidateUpdate freezes everything that determined where the address came
// from.
//
// A claim binds its allocation once, when the claim is created, and nothing
// recomputes the pairing afterwards — the claim object *is* the identity. So a
// claim whose class, family, requested size, or scope changed after allocation
// is not a claim for different space; it is a claim that no longer describes the
// space it holds. The scope refs in particular: a claim whose network or
// location changed after allocation is incoherent, because the address it holds
// was chosen to be unique in the old one.
func (ipClaimStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPClaim)
	o := old.(*ipam.IPClaim)
	allErrs := validateIPClaim(n)
	specPath := field.NewPath("spec")

	if n.Spec.ClassName != o.Spec.ClassName {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("className"),
			"spec.className is immutable: a claim binds its allocation once, at creation"))
	}
	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamily"),
			"spec.ipFamily is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PrefixLength, o.Spec.PrefixLength) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("prefixLength"),
			"spec.prefixLength is immutable"))
	}
	if n.Spec.Address != o.Spec.Address {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("address"),
			"spec.address is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.Scope, o.Spec.Scope) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("scope"),
			"spec.scope is immutable: the address this claim holds was chosen to be unique within it"))
	}
	return allErrs
}

func (ipClaimStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

// validateIPClaim checks what a claim can be judged on without reading its
// class. The rules that need the class — that the requested size falls in the
// class's allowed range, and that the scope carries every role the class and its
// parent chain require — are enforced in the Create handler, which has a
// transaction to read the class in.
//
// Those are deliberately errors rather than defaults. A claim missing a role its
// class needs is rejected by name rather than falling back to a wider
// comparison, because a wider comparison would look correct while refusing
// addresses the narrow one was meant to allow — surfacing as unexplained
// exhaustion rather than as a missing field.
func validateIPClaim(c *ipam.IPClaim) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// A claim identifies the space it wants either by naming a class or by
	// naming a family and taking that family's default. With neither there is
	// nothing to resolve.
	if c.Spec.ClassName == "" && c.Spec.IPFamily == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("className"),
			"one of spec.className or spec.ipFamily is required"))
	}
	if c.Spec.IPFamily != "" && c.Spec.IPFamily != ipam.IPv4 && c.Spec.IPFamily != ipam.IPv6 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), c.Spec.IPFamily,
			[]string{string(ipam.IPv4), string(ipam.IPv6)}))
	}

	if c.Spec.PrefixLength != nil {
		length := *c.Spec.PrefixLength
		maxLen := int32(128)
		if c.Spec.IPFamily == ipam.IPv4 {
			maxLen = 32
		}
		if length <= 0 || length > maxLen {
			allErrs = append(allErrs, field.Invalid(specPath.Child("prefixLength"), length,
				fmt.Sprintf("must be in [1, %d]", maxLen)))
		}
	}

	if err := scope.Validate(c.Spec.Scope); err != nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("scope"), c.Spec.Scope, err.Error()))
	}

	switch c.Spec.ReclaimPolicy {
	case "", ipam.ReclaimDelete, ipam.ReclaimRetain:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("reclaimPolicy"), c.Spec.ReclaimPolicy,
			[]string{string(ipam.ReclaimDelete), string(ipam.ReclaimRetain)}))
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

// SelectableFields exposes the class a claim names and the pool it landed in.
//
// The pool is a status field, which is the point: the consumer does not choose
// it, but an operator draining a pool needs to list everything drawing from one.
func SelectableFields(c *ipam.IPClaim) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&c.ObjectMeta, true)
	poolRefName := ""
	if c.Status.PoolRef != nil {
		poolRefName = c.Status.PoolRef.Name
	}
	specific := fields.Set{
		"spec.className":      c.Spec.ClassName,
		"status.poolRef.name": poolRefName,
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
