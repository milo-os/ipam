package ipaddress

import (
	"context"
	"fmt"
	"net"

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

// FieldIndexes are the SQL expression indexes that back IPAddress field
// selectors declared in SelectableFields. Applied idempotently by SyncIndexes.
var FieldIndexes = []fieldindex.FieldIndex{
	{
		IndexName:  "idx_ipam_ipaddress_address",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'address')) WHERE kind = 'IPAddress'`,
	},
	{
		IndexName:  "idx_ipam_ipaddress_ip_family",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily')) WHERE kind = 'IPAddress'`,
	},
	{
		IndexName:  "idx_ipam_ipaddress_prefix_ref_name",
		Expression: `((ipam_data_to_jsonb(data) -> 'spec' -> 'prefixRef' ->> 'name')) WHERE kind = 'IPAddress'`,
	},
}

type ipAddressStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

type ipAddressStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewStrategy(typer runtime.ObjectTyper) ipAddressStrategy {
	return ipAddressStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func NewStatusStrategy(typer runtime.ObjectTyper) ipAddressStatusStrategy {
	return ipAddressStatusStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipAddressStrategy) NamespaceScoped() bool { return true }

func (ipAddressStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	a := obj.(*ipam.IPAddress)
	a.Status = ipam.IPAddressStatus{}
}

func (ipAddressStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPAddress)
	o := old.(*ipam.IPAddress)
	n.Status = o.Status
}

func (ipAddressStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPAddress(obj.(*ipam.IPAddress))
}

func (ipAddressStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string { return nil }
func (ipAddressStrategy) AllowCreateOnUpdate() bool                                    { return false }
func (ipAddressStrategy) AllowUnconditionalUpdate() bool                               { return true }
func (ipAddressStrategy) Canonicalize(_ runtime.Object)                                {}

func (ipAddressStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n := obj.(*ipam.IPAddress)
	o := old.(*ipam.IPAddress)
	allErrs := validateIPAddress(n)
	if n.Spec.Address != o.Spec.Address {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "address"), "spec.address is immutable"))
	}
	if n.Spec.IPFamily != o.Spec.IPFamily {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "ipFamily"), "spec.ipFamily is immutable"))
	}
	if n.Spec.PrefixRef != o.Spec.PrefixRef {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "prefixRef"), "spec.prefixRef is immutable"))
	}
	return allErrs
}

func (ipAddressStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateIPAddress(a *ipam.IPAddress) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	var parsed net.IP
	if a.Spec.Address == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("address"), "address is required"))
	} else {
		parsed = net.ParseIP(a.Spec.Address)
		if parsed == nil {
			allErrs = append(allErrs, field.Invalid(specPath.Child("address"), a.Spec.Address, "invalid IP address"))
		}
	}
	if a.Spec.IPFamily != ipam.IPv4 && a.Spec.IPFamily != ipam.IPv6 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("ipFamily"), a.Spec.IPFamily, []string{string(ipam.IPv4), string(ipam.IPv6)}))
	}
	// Cross-check: an IPv4 address must be claimed as IPFamily=IPv4 and
	// an IPv6-only address as IPFamily=IPv6. Without this check the
	// allocator and consumers downstream would index by ipFamily and
	// silently miss the address. net.ParseIP returns a 16-byte slice
	// even for IPv4 addresses, so use To4() to discriminate.
	if parsed != nil && a.Spec.IPFamily != "" {
		isV4 := parsed.To4() != nil
		switch {
		case isV4 && a.Spec.IPFamily != ipam.IPv4:
			allErrs = append(allErrs, field.Invalid(specPath.Child("ipFamily"), a.Spec.IPFamily,
				fmt.Sprintf("address %q is IPv4 but ipFamily is %s", a.Spec.Address, a.Spec.IPFamily)))
		case !isV4 && a.Spec.IPFamily != ipam.IPv6:
			allErrs = append(allErrs, field.Invalid(specPath.Child("ipFamily"), a.Spec.IPFamily,
				fmt.Sprintf("address %q is IPv6 but ipFamily is %s", a.Spec.Address, a.Spec.IPFamily)))
		}
	}
	if a.Spec.PrefixRef.Name == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("prefixRef", "name"), "prefixRef.name is required"))
	}
	return allErrs
}

func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	a, ok := obj.(*ipam.IPAddress)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPAddress")
	}
	return a.Labels, SelectableFields(a), nil
}

func SelectableFields(a *ipam.IPAddress) fields.Set {
	objectMetaFields := generic.ObjectMetaFieldsSet(&a.ObjectMeta, true)
	return generic.MergeFieldsSets(objectMetaFields, fields.Set{
		"spec.address":       a.Spec.Address,
		"spec.ipFamily":      string(a.Spec.IPFamily),
		"spec.prefixRef.name": a.Spec.PrefixRef.Name,
	})
}

func MatchIPAddress(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetAttrs}
}

func (ipAddressStatusStrategy) NamespaceScoped() bool { return true }

func (ipAddressStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n := obj.(*ipam.IPAddress)
	o := old.(*ipam.IPAddress)
	n.Spec = o.Spec
	n.Labels = o.Labels
	n.Annotations = o.Annotations
}

func (ipAddressStatusStrategy) ValidateUpdate(_ context.Context, _, _ runtime.Object) field.ErrorList {
	return nil
}

func (ipAddressStatusStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func (ipAddressStatusStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipAddressStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipAddressStatusStrategy) Canonicalize(_ runtime.Object)  {}

func (ipAddressStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"ipam.miloapis.com/v1alpha1": fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}
