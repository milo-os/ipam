package ipprefix

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

type ipPrefixClassStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewClassStrategy(typer runtime.ObjectTyper) ipPrefixClassStrategy {
	return ipPrefixClassStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (ipPrefixClassStrategy) NamespaceScoped() bool                                 { return false }
func (ipPrefixClassStrategy) PrepareForCreate(_ context.Context, _ runtime.Object)  {}
func (ipPrefixClassStrategy) PrepareForUpdate(_ context.Context, _, _ runtime.Object) {}
func (ipPrefixClassStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateIPPrefixClass(obj.(*ipam.IPPrefixClass))
}
func (ipPrefixClassStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string {
	return nil
}
func (ipPrefixClassStrategy) AllowCreateOnUpdate() bool      { return false }
func (ipPrefixClassStrategy) AllowUnconditionalUpdate() bool { return true }
func (ipPrefixClassStrategy) Canonicalize(_ runtime.Object)  {}
func (ipPrefixClassStrategy) ValidateUpdate(_ context.Context, obj, _ runtime.Object) field.ErrorList {
	return validateIPPrefixClass(obj.(*ipam.IPPrefixClass))
}
func (ipPrefixClassStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateIPPrefixClass(c *ipam.IPPrefixClass) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	if c.Spec.Visibility != "" && c.Spec.Visibility != "platform" && c.Spec.Visibility != "consumer" && c.Spec.Visibility != "shared" {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("visibility"), c.Spec.Visibility, []string{"platform", "consumer", "shared"}))
	}
	return allErrs
}

func GetClassAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	c, ok := obj.(*ipam.IPPrefixClass)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an IPPrefixClass")
	}
	return c.Labels, generic.ObjectMetaFieldsSet(&c.ObjectMeta, false), nil
}

func MatchIPPrefixClass(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetClassAttrs}
}
