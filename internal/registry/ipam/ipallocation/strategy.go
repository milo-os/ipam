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
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/fieldindex"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// FieldIndexes are the SQL expression indexes backing IPAllocation field
// selectors declared in SelectableFields. Applied idempotently by SyncIndexes.
// FieldIndexes are the SQL expression indexes backing IPAllocation field
// selectors. Applied idempotently by SyncIndexes at startup.
//
// These must stay in step with migrations/002_class_model.sql. SyncIndexes runs
// immediately after goose with CREATE INDEX IF NOT EXISTS, so an entry here for
// an index the migration drops would recreate it seconds later and make the
// migration a no-op — and a path the migration indexes but this list omits is
// only indexed until someone rebuilds from Go.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipallocation_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPAllocation'`,
	},
	{
		IndexName:  "idx_ipam_ipallocation_pool_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'poolRef' ->> 'name')) WHERE kind = 'IPAllocation'`,
	},
	{
		// "Everything this class has handed out" — the class inventory, and the
		// unit quota attributes usage to.
		IndexName:  "idx_ipam_ipallocation_class_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'className')) WHERE kind = 'IPAllocation'`,
	},
	{
		// "Which allocation is this claim bound to" on release, and — with the
		// expression IS NULL — the retained-with-no-claim list a lease expiry
		// sweep walks.
		IndexName:  "idx_ipam_ipallocation_claim_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'claimRef' ->> 'name')) WHERE kind = 'IPAllocation'`,
	},
	{
		// "Every allocation in this address space" — the inventory view, and the
		// cross-check that the exclusion constraint and the object store agree.
		IndexName:  "idx_ipam_ipallocation_scope_digest",
		Expression: `((ipam_data_to_jsonb(data) -> 'status' ->> 'scopeDigest')) WHERE kind = 'IPAllocation'`,
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

// ValidateUpdate freezes the whole spec.
//
// An IPAllocation is a RECORD of a completed allocation, not a request for one.
// Every field in its spec is derived by the server at bind time from the claim
// and its class, so no client has standing to edit any of it. The system's own
// mutations — clearing claimRef under Retain, rebinding it on reclaim, the
// lease sweeper's marks — write through allocator.updateObject inside the
// allocation transaction and never reach this strategy, so freezing here costs
// them nothing.
//
// # Why the whole spec, compared structurally, rather than a list of fields
//
// This used to name ipFamily and poolRef, which left className, purpose, scope,
// claimRef, reclaimPolicy and ownerRef editable. spec.scope being editable is
// what made status.scopeDigest able to go stale (#82), the same asymmetry #79
// fixed on IPPool. A named list is a list someone has to remember to extend: add
// a spec field and it is mutable by default, silently. A structural comparison
// covers new fields the moment they exist.
//
// # Why freezing rather than recomputing the digest
//
// The obvious reading of #82 is to copy #79's fix — recompute status.scopeDigest
// from spec.Scope in PrepareForUpdate. That would be wrong here, and the
// difference is worth stating because the code shapes are identical.
//
// Nothing reads IPAllocation.Spec.Scope. Every digest in the service derives
// from the CLAIM's scope: claim.Spec.Scope becomes uniqueDigest, which is
// written to this object, to ipam_cidr_allocations.scope_digest, and to the
// reclaim request. The stored column is what the exclusion constraint compares,
// and it is what status.scopeDigest is documented to mean — "the value
// uniqueness is enforced on". Recomputing status from a spec field nothing else
// consults would make it disagree with that column: a confident wrong answer to
// exactly the question asked during an incident, in place of a merely stale one.
//
// Applying a fix across a semantic boundary because the code shape matched is
// how #64 happened. Freezing removes the staleness without inventing a second
// source of truth.
func (ipAllocationStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPAllocation)
	o := old.(*ipam.IPAllocation)
	allErrs := validateIPAllocation(n)

	if !equality.Semantic.DeepEqual(n.Spec, o.Spec) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"),
			"spec is immutable: an IPAllocation records an allocation the server "+
				"already made. To release the address, delete the owning IPClaim; "+
				"to force-release a retained one, delete the IPAllocation"))
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
		// See the IPPool strategy: idx_ipam_ipallocation_scope_digest exists to
		// serve this and nothing declared it.
		"status.scopeDigest": a.Status.ScopeDigest,
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
