package ipprefix

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

// FieldIndexes are the SQL expression indexes that back IPPrefix field
// selectors declared in SelectablePrefixFields. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipprefix_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPPrefix'`,
	},
	{
		IndexName:  "idx_ipam_ipprefix_class_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'classRef' ->> 'name')) WHERE kind = 'IPPrefix'`,
	},
}

type ipPrefixStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type ipPrefixStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewPrefixStrategy(typer runtime.ObjectTyper) ipPrefixStrategy {
	return ipPrefixStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewPrefixStatusStrategy(typer runtime.ObjectTyper) ipPrefixStatusStrategy {
	return ipPrefixStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipPrefixStrategy) NamespaceScoped() bool { return false }

func (ipPrefixStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	p := obj.(*ipam.IPPrefix)
	// Default the allocation strategy so the field is visible in
	// `kubectl get ipprefix -o yaml`. The allocator silently falls back
	// to FirstFit when the field is empty, but operators reasoning about
	// behaviour should not have to know that — making it explicit also
	// surfaces it on the audit log.
	if p.Spec.Allocation.Strategy == "" {
		p.Spec.Allocation.Strategy = ipam.FirstFit
	}
	// This apiserver allocates synchronously; there is no controller that
	// later transitions Pending → Ready. Compute the canonical CIDR and
	// total capacity here so the persisted row is immediately usable as a
	// pool. If the CIDR is invalid, fall back to Pending — Validate will
	// reject the create on the next step in the strategy chain.
	p.Status = ipam.IPPrefixStatus{Phase: ipam.PrefixPending}
	if p.Spec.CIDR == "" {
		return
	}
	_, ipnet, err := net.ParseCIDR(p.Spec.CIDR)
	if err != nil {
		return
	}
	p.Status.CIDR = ipnet.String()
	p.Status.Capacity = ipam.PrefixCapacity{Total: allocation.CountAddresses(*ipnet)}
	p.Status.Phase = ipam.PrefixReady
	p.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "PrefixReady",
		Message:            "IPPrefix is ready for allocation",
		LastTransitionTime: metav1.Now(),
	}}
}

func (ipPrefixStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPPrefix)
	o := old.(*ipam.IPPrefix)
	n.Status = o.Status
}

func (ipPrefixStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPPrefix(obj.(*ipam.IPPrefix))
}

func (ipPrefixStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string { return nil }
func (ipPrefixStrategy) AllowCreateOnUpdate() bool                                    { return false }
func (ipPrefixStrategy) AllowUnconditionalUpdate() bool                               { return true }
func (ipPrefixStrategy) Canonicalize(_ runtime.Object)                                {}

func (ipPrefixStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPPrefix)
	o := old.(*ipam.IPPrefix)
	allErrs := validateIPPrefix(n)
	if n.Spec.CIDR != o.Spec.CIDR {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "cidr"), "spec.cidr is immutable"))
	}
	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "ipFamily"), "spec.ipFamily is immutable"))
	}
	if n.Spec.ClassRef != o.Spec.ClassRef {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "classRef"), "spec.classRef is immutable"))
	}
	return allErrs
}

func (ipPrefixStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateIPPrefix(p *ipam.IPPrefix) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	if p.Spec.CIDR == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("cidr"), "cidr is required"))
	} else if _, _, err := net.ParseCIDR(p.Spec.CIDR); err != nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("cidr"), p.Spec.CIDR, fmt.Sprintf("invalid CIDR: %v", err)))
	}
	if p.Spec.IPFamily == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("ipFamily"), "ipFamily is required"))
	} else if p.Spec.IPFamily != ipam.IPv4 && p.Spec.IPFamily != ipam.IPv6 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), p.Spec.IPFamily, []string{string(ipam.IPv4), string(ipam.IPv6)}))
	}
	if p.Spec.ClassRef.Name == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("classRef", "name"), "classRef.name is required"))
	}
	if p.Spec.Allocation.MinPrefixLength > 0 && p.Spec.Allocation.MaxPrefixLength > 0 &&
		p.Spec.Allocation.MinPrefixLength > p.Spec.Allocation.MaxPrefixLength {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("allocation"), p.Spec.Allocation,
			"minPrefixLength must be <= maxPrefixLength",
		))
	}
	return allErrs
}

func GetPrefixAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	p, ok := obj.(*ipam.IPPrefix)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPPrefix")
	}
	return p.Labels, SelectablePrefixFields(p), nil
}

func SelectablePrefixFields(p *ipam.IPPrefix) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&p.ObjectMeta, true)
	specific := fields.Set{
		"spec.ipFamily":     string(p.Spec.IPFamily),
		"spec.classRef.name": p.Spec.ClassRef.Name,
	}
	return generic.MergeFieldsSets(objectMetaFields, specific)
}

func MatchIPPrefix(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetPrefixAttrs}
}

func (ipPrefixStatusStrategy) NamespaceScoped() bool { return false }

func (ipPrefixStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPPrefix)
	o := old.(*ipam.IPPrefix)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (ipPrefixStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (ipPrefixStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (ipPrefixStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipPrefixStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipPrefixStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipPrefixStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
