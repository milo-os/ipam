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

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/fieldindex"
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
		IndexName:  "idx_ipam_ippool_parent_pool_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'parentPoolRef' ->> 'name')) WHERE kind = 'IPPool'`,
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
	// Use CountAddresses so the initial Total uses the same unit as the
	// post-allocation persistPoolCapacity refresh; seed Available so callers
	// can observe "available decreased" after the first allocation.
	total := allocation.CountAddresses(*ipnet)
	p.Status.Capacity = ipam.PoolCapacity{Total: total, Available: total}
	p.Status.Phase = ipam.PoolReady
	p.Status.Conditions = []metav1.Condition{{
		Type:               "Allocated",
		Status:             metav1.ConditionTrue,
		Reason:             "AllocationSucceeded",
		Message:            "IPPool is ready for allocation",
		LastTransitionTime: metav1.Now(),
	}}
}

func (ipPoolStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPPool)
	o := old.(*ipam.IPPool)
	n.Status = o.Status
}

func (ipPoolStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPPool(obj.(*ipam.IPPool))
}

func (ipPoolStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string { return nil }
func (ipPoolStrategy) AllowCreateOnUpdate() bool                                     { return false }
func (ipPoolStrategy) AllowUnconditionalUpdate() bool                                { return true }
func (ipPoolStrategy) Canonicalize(_ runtime.Object)                                 {}

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
	return allErrs
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
		"spec.ipFamily":            string(p.Spec.IPFamily),
		"spec.parentPoolRef.name":  parentName,
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
