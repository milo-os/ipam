package ipclass

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// SelectableFields says which fields a LIST may filter on, and the scheme's
// field-label conversion says which a request may name. Nothing in the type
// system connects them, and the two failure directions are not symmetric: a
// field selectable but not converted is rejected at request time, while one
// converted but not selectable is accepted and then matches nothing.
func TestEverySelectableFieldIsConvertible(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}

	// A class carrying every selectable field, so none is skipped for being
	// empty.
	sample := &ipam.IPClass{
		Spec: ipam.IPClassSpec{
			IPFamily:        ipam.IPv6,
			ParentClassName: "backbone",
		},
	}
	sample.Spec.Source = &ipam.ClassSourceRef{Project: "platform", Name: "public-unicast"}

	gvk := v1alpha1.SchemeGroupVersion.WithKind("IPClass")
	for label := range SelectableFields(sample) {
		if _, _, err := scheme.ConvertFieldLabel(gvk, label, "x"); err != nil {
			t.Errorf("SelectableFields offers %q but the scheme rejects it: %v", label, err)
		}
	}
}
