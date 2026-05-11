package asnpool

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

type asnPoolClassStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func NewClassStrategy(typer runtime.ObjectTyper) asnPoolClassStrategy {
	return asnPoolClassStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (asnPoolClassStrategy) NamespaceScoped() bool                                  { return false }
func (asnPoolClassStrategy) PrepareForCreate(_ context.Context, _ runtime.Object)   {}
func (asnPoolClassStrategy) PrepareForUpdate(_ context.Context, _, _ runtime.Object) {}
func (asnPoolClassStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateASNPoolClass(obj.(*ipam.ASNPoolClass))
}
func (asnPoolClassStrategy) WarningsOnCreate(_ context.Context, _ runtime.Object) []string {
	return nil
}
func (asnPoolClassStrategy) AllowCreateOnUpdate() bool      { return false }
func (asnPoolClassStrategy) AllowUnconditionalUpdate() bool { return true }
func (asnPoolClassStrategy) Canonicalize(_ runtime.Object)  {}
func (asnPoolClassStrategy) ValidateUpdate(_ context.Context, obj, _ runtime.Object) field.ErrorList {
	return validateASNPoolClass(obj.(*ipam.ASNPoolClass))
}
func (asnPoolClassStrategy) WarningsOnUpdate(_ context.Context, _, _ runtime.Object) []string {
	return nil
}

func validateASNPoolClass(c *ipam.ASNPoolClass) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	if c.Spec.Visibility != "" && c.Spec.Visibility != "platform" && c.Spec.Visibility != "consumer" && c.Spec.Visibility != "shared" {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("visibility"), c.Spec.Visibility, []string{"platform", "consumer", "shared"}))
	}
	return allErrs
}

func GetClassAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	c, ok := obj.(*ipam.ASNPoolClass)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not an ASNPoolClass")
	}
	return c.Labels, generic.ObjectMetaFieldsSet(&c.ObjectMeta, false), nil
}

func MatchASNPoolClass(label labels.Selector, fld fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: fld, GetAttrs: GetClassAttrs}
}
