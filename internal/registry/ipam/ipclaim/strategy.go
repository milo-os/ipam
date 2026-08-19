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

func (ipClaimStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPClaim)
	o := old.(*ipam.IPClaim)
	allErrs := validateIPClaim(n)

	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "ipFamily"), "ipFamily is immutable"))
	}
	if !equality.Semantic.DeepEqual(n.Spec.PrefixLength, o.Spec.PrefixLength) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "prefixLength"), "prefixLength is immutable"))
	}
	if n.Spec.ClassName != o.Spec.ClassName {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "className"), "className is immutable"))
	}
	// The scope decides which pool serves the claim and which allocations it
	// must not collide with. Editing it would move a bound address into a
	// different address space without moving the address.
	if !equality.Semantic.DeepEqual(n.Spec.Scope, o.Spec.Scope) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "scope"), "scope is immutable"))
	}
	return allErrs
}

func (ipClaimStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateIPClaim(c *ipam.IPClaim) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// A claim names a class, or a family so the default class for it can be
	// found. The class supplies everything else, including the block size, so
	// nothing here can validate a prefix length against a range it cannot see.
	if c.Spec.ClassName == "" && c.Spec.IPFamily == "" {
		allErrs = append(allErrs, field.Required(specPath, "one of className or ipFamily is required"))
	}
	if c.Spec.IPFamily != "" && c.Spec.IPFamily != ipam.IPv4 && c.Spec.IPFamily != ipam.IPv6 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), c.Spec.IPFamily,
			[]string{string(ipam.IPv4), string(ipam.IPv6)}))
	}
	if p := c.Spec.PrefixLength; p != nil {
		maxLen := int32(32)
		if c.Spec.IPFamily == ipam.IPv6 {
			maxLen = 128
		}
		if *p <= 0 || *p > maxLen {
			allErrs = append(allErrs, field.Invalid(specPath.Child("prefixLength"), *p,
				fmt.Sprintf("must be between 1 and %d", maxLen)))
		}
	}
	for role, ref := range c.Spec.Scope {
		// The reserved roles say who a class's pools belong to, and the server
		// reads the consuming project off the request rather than the body.
		// Accepting them here is the whole attack: a claimant could otherwise
		// name another project's pool by writing a scope reference. Rejecting
		// is not merely safer than ignoring — a claim that names one has
		// misunderstood which pool it will reach, and silently dropping the
		// field would confirm the misunderstanding.
		if scope.IsReservedRole(role) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("scope", role), ref,
				fmt.Sprintf("%q is a reserved poolPer role and not a scope reference; "+
					"which pool a claim reaches is declared by its class and taken from the request", role)))
			continue
		}
		if ref.Kind == "" || ref.Name == "" {
			allErrs = append(allErrs, field.Invalid(specPath.Child("scope", role), ref,
				"each scope reference needs a kind and a name"))
		}
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
	// The pool is resolved by the allocator, so it is filterable on STATUS
	// rather than on spec. Empty until the claim is bound.
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
