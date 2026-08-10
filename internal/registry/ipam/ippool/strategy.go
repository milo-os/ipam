// Package ippool provides REST storage for the cluster-scoped IPPool
// resource. Root pools are persisted directly by the underlying store; child
// pools (pools whose CIDR is sub-allocated out of a parent IPPool) are created
// synchronously through the AllocatingIPPoolREST wrapper so the response
// carries the assigned status.allocatedCIDR.
package ippool

import (
	"context"
	"fmt"
	"net"

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
	"go.miloapis.com/ipam/internal/registry/ipam/ipamvalidation"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// FieldIndexes are the SQL expression indexes backing IPPool field selectors.
// Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ippool_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPPool'`,
	},
	{
		// "What is carved from this pool", asked on every pool delete.
		IndexName:  "idx_ipam_ippool_parent_pool_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'parentPoolRef' ->> 'name')) WHERE kind = 'IPPool'`,
	},
	{
		// "Which pools offer class X" — parent resolution for a class with no
		// parentClassName. classNames is an array, so this is a containment
		// index rather than an equality one, and it must be gin for @> to use
		// it. ipam_pool_class_offer answers the same question with a cheap
		// count; this is what makes that table rebuildable from the objects.
		IndexName:  "idx_ipam_ippool_class_names",
		Expression: `USING gin ((ipam_data_to_jsonb(data) -> 'spec' -> 'classNames') jsonb_path_ops) WHERE kind = 'IPPool'`,
	},
	{
		// "Which pool serves this network in this location". ipam_pool_identity
		// is authoritative; this makes the answer reachable from the object
		// store too, which is what an on-call engineer has in front of them.
		IndexName:  "idx_ipam_ippool_scope_digest",
		Expression: `((ipam_data_to_jsonb(data) -> 'status' ->> 'scopeDigest')) WHERE kind = 'IPPool'`,
	},
}

type ipPoolStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type ipPoolStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) ipPoolStrategy {
	return ipPoolStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewStatusStrategy(typer runtime.ObjectTyper) ipPoolStatusStrategy {
	return ipPoolStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipPoolStrategy) NamespaceScoped() bool { return false }

// PrepareForCreate seeds Status based on whether this is a root pool
// (CIDR + IPFamily on the spec) or a child pool (parentPoolRef set).
// Root pools become Ready immediately — the apiserver allocates
// synchronously, so there is no controller that would later transition them
// from Pending. Child pools stay Pending until the Create handler runs the
// allocation transaction and overwrites Status with the assigned CIDR.
func (ipPoolStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	p := obj.(*ipam.IPPool)
	if p.Spec.Allocation.Strategy == "" {
		p.Spec.Allocation.Strategy = ipam.FirstFit
	}

	if p.Spec.ParentPoolRef != nil {
		// Child pool — Create handler populates status after allocation.
		p.Status = ipam.IPPoolStatus{Phase: ipam.PoolPending}
		return
	}

	// Root pool — compute canonical CIDR + total capacity now.
	p.Status = ipam.IPPoolStatus{Phase: ipam.PoolPending}
	if p.Spec.CIDR == "" {
		return
	}
	_, ipnet, err := net.ParseCIDR(p.Spec.CIDR)
	if err != nil {
		return
	}
	p.Status.AllocatedCIDR = ipnet.String()
	// Seed all utilization fields from an empty allocation set so the initial
	// status matches the post-allocation refresh and callers can observe
	// "available decreased" after the first allocation.
	setPoolStatusCapacity(p, []net.IPNet{*ipnet}, nil)
	p.Status.Phase = ipam.PoolReady
	p.Status.Conditions = []metav1.Condition{{
		Type:               "Allocated",
		Status:             metav1.ConditionTrue,
		Reason:             "AllocationSucceeded",
		Message:            "IPPool is ready for allocation",
		LastTransitionTime: metav1.Now(),
	}}
}

// PrepareForUpdate carries the stored status forward — a spec write does not
// get to set status — and then re-derives the one status field that is a pure
// function of the spec.
//
// # Why the digest is re-derived rather than carried
//
// status.scopeDigest is scope.PoolDigest(tenant, spec.scope). Both inputs can
// differ from what they were when the value was written, and until this existed
// the value was assigned in Create and never again, so it went stale silently in
// two different ways:
//
//   - spec.scope is mutable. It is deliberately absent from the immutable set in
//     ValidateUpdate, so an operator may re-scope a pool — and the digest went on
//     describing the scope the pool used to have.
//   - The tenant string moved underneath every existing platform pool when the
//     platform became a project. A re-homed pool carries the digest of the empty
//     scope under an empty tenant (migration 005's default, 6139457f…), and the
//     key rewrite could not fix it: a digest is a SHA-256 over a canonical form
//     the schema never stores, which is the same reason 005 and 006 could not
//     backfill digests either. Only a write through this path can heal it.
//
// This is the third instance of the same miss in this file — see the tasks for
// capacity status and for reservation edits, both of which were fields Create
// established and Update left alone. Anything else derived in Create belongs
// here too.
//
// Recomputing is a no-op on a healthy pool, which is the property that makes it
// safe to run on every update: the value only moves when it was already wrong.
func (ipPoolStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPPool)
	o := old.(*ipam.IPPool)
	n.Status = o.Status
	// From the object's own spec, not the stored one, so an edit to spec.scope
	// takes effect in the same write that makes it.
	n.Status.ScopeDigest = scope.PoolDigest(tenant.FromContext(ctx).Name, n.Spec.Scope)
}

func (ipPoolStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPPool(obj.(*ipam.IPPool))
}

func (ipPoolStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string { return nil }
func (ipPoolStrategy) AllowCreateOnUpdate() bool                                     { return false }
func (ipPoolStrategy) AllowUnconditionalUpdate() bool                                { return true }
func (ipPoolStrategy) Canonicalize(_ runtime.Object)                                 {}

// ValidateUpdate enforces the immutable spec fields.
//
// # If you take a field OUT of this list, check what status derives from it
//
// The list below is not only a validation rule; it is the reason most of status
// can be carried forward untouched by PrepareForUpdate. Every derived status
// field on this type draws on something frozen here — allocatedCIDR on
// spec.cidr, ipFamily on spec.ipFamily, the capacity figures on ranges and
// allocation rows that no edit can move — so re-deriving them would be a
// guaranteed no-op.
//
// status.scopeDigest was the exception, and it was the exception for exactly one
// reason: spec.scope is NOT in this list. It is the only status field derived
// from a mutable spec field, and it was the only one that went stale. It was
// found late because the platform-as-a-project cutover made a second instance of
// it visible; the mutability was the real defect and had been there the whole
// time.
//
// So the invariant, stated where someone will be standing when it matters:
// **relaxing an immutability rule here obliges you to recompute whatever status
// derives from that field in PrepareForUpdate.** Adding a rule is free; removing
// one is not.
func (ipPoolStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPPool)
	o := old.(*ipam.IPPool)
	allErrs := validateIPPool(n)
	specPath := field.NewPath("spec")
	if n.Spec.CIDR != o.Spec.CIDR {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("cidr"), "spec.cidr is immutable"))
	}
	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamily"), "spec.ipFamily is immutable"))
	}
	if !localRefEqual(n.Spec.ParentPoolRef, o.Spec.ParentPoolRef) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("parentPoolRef"), "spec.parentPoolRef is immutable"))
	}
	if n.Spec.PrefixLength != o.Spec.PrefixLength {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("prefixLength"), "spec.prefixLength is immutable"))
	}
	// Immutable because changing it never did anything. Reservations are
	// materialised into real allocation rows by ProvisionReservations on the
	// create path, and nothing re-provisions or releases them afterwards — so an
	// edit was accepted, returned 200, and left the pool holding exactly the
	// blocks it started with. Worse, status.capacity is derived from those rows
	// and so kept agreeing with them, which made the dropped edit look applied.
	//
	// BUILT AS A SUBRESOURCE ON 2026-08-10 AND REMOVED THE SAME DAY. Do not
	// rebuild it without new evidence — the reasoning is recorded here rather
	// than in a deleted commit precisely so the next person does not redo it.
	//
	// The shape was POST ippools/<name>/reservations, reconciling the rows and
	// the spec in one transaction under the pool row lock, refusing a widening
	// that would strand allocations and naming the holders. It worked and it was
	// tested. It was removed because the USE CASE did not survive examination:
	//
	//   - No operator ever asked. Every reservation change in this repo's
	//     history is self-generated backlog, and the task proposing this was
	//     filed as a follow-up to the immutability fix rather than by a user.
	//   - The cases split by whether the pool is populated, and the split runs
	//     the wrong way. Empty or young pool: the edit works, and recreating the
	//     pool was already cheap. Populated pool: widening is REFUSED, so the
	//     operator's remedy is still "release the claims in the way" — most of
	//     the pain of the advice below.
	//   - So the capability was most useful where it was least needed, and
	//     degraded to this message exactly where it would have helped.
	//
	// Nearly all the machinery served widening — the policy decision, the
	// availability check the exclusion constraint cannot back, the refusal path
	// — and that is the half that most often cannot act. If this comes back,
	// NARROWING ALONE is the honest feature: always safe, no policy question,
	// no refusal path.
	//
	// This removes no capability an operator had; it tells them the truth about
	// the one they did not. Reconciling an edit is a real feature with a policy
	// question at its centre — what to do with allocations sitting in newly
	// reserved space — and that decision belongs in its own change rather than
	// being invented inside a validation rule. Relaxing this later is additive;
	// retracting a reconciliation that stranded somebody's addresses is not.
	if !reservationSpecEqual(n.Spec.Reservations, o.Spec.Reservations) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("reservations"),
			"spec.reservations is fixed when the pool is created and cannot be changed: "+
				"the reserved blocks are materialised as allocations at creation and are not re-derived. "+
				"Create a new pool with the reservations you want, and migrate claims to it"))
	}
	return allErrs
}

func reservationSpecEqual(a, b *ipam.ReservationSpec) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Leading == b.Leading &&
			a.Trailing == b.Trailing &&
			a.UnitPrefixLength == b.UnitPrefixLength
	}
}

func (ipPoolStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateIPPool(p *ipam.IPPool) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	isChild := p.Spec.ParentPoolRef != nil
	hasChildLen := p.Spec.PrefixLength != 0
	hasRootCIDR := p.Spec.CIDR != ""
	hasRootFamily := p.Spec.IPFamily != ""

	switch {
	case isChild:
		// Child pool — root fields must be absent, child fields required.
		if hasRootCIDR {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("cidr"),
				"spec.cidr must not be set on a child pool (computed from parent allocation)"))
		}
		if hasRootFamily {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamily"),
				"spec.ipFamily must not be set on a child pool (inherited from parent)"))
		}
		if p.Spec.ParentPoolRef.Name == "" {
			allErrs = append(allErrs, field.Required(specPath.Child("parentPoolRef", "name"),
				"parentPoolRef.name is required for child pools"))
		}
		if !hasChildLen {
			allErrs = append(allErrs, field.Required(specPath.Child("prefixLength"),
				"prefixLength is required for child pools"))
		} else if p.Spec.PrefixLength < 1 || p.Spec.PrefixLength > 128 {
			allErrs = append(allErrs, field.Invalid(specPath.Child("prefixLength"), p.Spec.PrefixLength,
				"prefixLength must be in [1, 128]"))
		}
	default:
		// Root pool — child fields must be absent, root fields required.
		if hasChildLen {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("prefixLength"),
				"spec.prefixLength must not be set without spec.parentPoolRef"))
		}
		if !hasRootCIDR {
			allErrs = append(allErrs, field.Required(specPath.Child("cidr"),
				"cidr is required for root pools"))
		} else if _, _, err := net.ParseCIDR(p.Spec.CIDR); err != nil {
			allErrs = append(allErrs, field.Invalid(specPath.Child("cidr"), p.Spec.CIDR,
				fmt.Sprintf("invalid CIDR: %v", err)))
		}
		if !hasRootFamily {
			allErrs = append(allErrs, field.Required(specPath.Child("ipFamily"),
				"ipFamily is required for root pools"))
		} else if p.Spec.IPFamily != ipam.IPv4 && p.Spec.IPFamily != ipam.IPv6 {
			allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), p.Spec.IPFamily,
				[]string{string(ipam.IPv4), string(ipam.IPv6)}))
		}
	}

	if p.Spec.Allocation.MinPrefixLength > 0 && p.Spec.Allocation.MaxPrefixLength > 0 &&
		p.Spec.Allocation.MinPrefixLength > p.Spec.Allocation.MaxPrefixLength {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("allocation"), p.Spec.Allocation,
			"minPrefixLength must be <= maxPrefixLength",
		))
	}

	allErrs = append(allErrs, ipamvalidation.Reservations(
		p.Spec.Reservations, ipam.IPFamily(effectiveFamilyForValidation(p)),
		specPath.Child("reservations"))...)
	allErrs = append(allErrs, ipamvalidation.Lease(
		p.Spec.MaxRetentionLease, specPath.Child("maxRetentionLease"))...)

	switch p.Spec.Visibility {
	case "", "platform", "consumer", "shared":
		// ok
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("visibility"), p.Spec.Visibility,
			[]string{"", "platform", "consumer", "shared"}))
	}

	return allErrs
}

func localRefEqual(a, b *ipam.LocalRef) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Name == b.Name
	}
}

func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	p, ok := obj.(*ipam.IPPool)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPPool")
	}
	return p.Labels, SelectableFields(p), nil
}

func SelectableFields(p *ipam.IPPool) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&p.ObjectMeta, false)
	parentName := ""
	if p.Spec.ParentPoolRef != nil {
		parentName = p.Spec.ParentPoolRef.Name
	}
	specific := fields.Set{
		"spec.ipFamily":           string(p.Spec.IPFamily),
		"spec.parentPoolRef.name": parentName,
		// Declared because idx_ipam_ippool_scope_digest exists to serve it and
		// migration 002 advertises the query by name. It was indexed and
		// converted by nobody, so the index was maintained on every write and
		// could never be read.
		"status.scopeDigest": p.Status.ScopeDigest,
	}
	return generic.MergeFieldsSets(objectMetaFields, specific)
}

func Match(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}

func (ipPoolStatusStrategy) NamespaceScoped() bool { return false }

func (ipPoolStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPPool)
	o := old.(*ipam.IPPool)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (ipPoolStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (ipPoolStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (ipPoolStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipPoolStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipPoolStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipPoolStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}

// effectiveFamilyForValidation returns the family a pool's own fields imply, for
// validation that runs before the parent chain is resolved. A child pool
// inherits its family from its parent and cannot state one, so validation of a
// child falls back to the widest bounds — the real family is checked in Create,
// where the parent is in hand.
func effectiveFamilyForValidation(p *ipam.IPPool) string {
	if p.Spec.IPFamily != "" {
		return string(p.Spec.IPFamily)
	}
	return string(ipam.IPv6)
}
