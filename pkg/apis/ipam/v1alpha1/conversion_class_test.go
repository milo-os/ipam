package v1alpha1

import (
	"testing"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// The IPClass converters are hand-written, so a field added to one spec and not
// to both converters is lost silently in whichever direction was missed.
func TestIPClassSourceSurvivesRoundTrip(t *testing.T) {
	in := &IPClass{}
	in.Spec.Source = &ClassSourceRef{Project: "platform", Name: "public-unicast-ipv4"}

	var internal ipam.IPClass
	if err := convert_v1alpha1_IPClass_To_ipam(in, &internal); err != nil {
		t.Fatalf("to internal: %v", err)
	}
	if internal.Spec.Source == nil {
		t.Fatal("Source lost converting to the internal type")
	}

	var out IPClass
	if err := convert_ipam_IPClass_To_v1alpha1(&internal, &out); err != nil {
		t.Fatalf("to versioned: %v", err)
	}
	if out.Spec.Source == nil {
		t.Fatal("Source lost converting back to v1alpha1")
	}
	if *out.Spec.Source != *in.Spec.Source {
		t.Errorf("Source = %+v, want %+v", *out.Spec.Source, *in.Spec.Source)
	}
}

// A definition class has no Source, and converting must not invent one.
func TestIPClassWithoutSourceStaysNil(t *testing.T) {
	var internal ipam.IPClass
	if err := convert_v1alpha1_IPClass_To_ipam(&IPClass{}, &internal); err != nil {
		t.Fatalf("to internal: %v", err)
	}
	if internal.Spec.Source != nil {
		t.Errorf("Source = %+v, want nil", internal.Spec.Source)
	}
}

// Source is a pointer, so a shallow copy would alias it and let a mutation of
// the converted object reach back into the original.
func TestIPClassSourceIsCopiedNotAliased(t *testing.T) {
	in := &IPClass{}
	in.Spec.Source = &ClassSourceRef{Project: "platform", Name: "public-unicast-ipv4"}

	var internal ipam.IPClass
	if err := convert_v1alpha1_IPClass_To_ipam(in, &internal); err != nil {
		t.Fatalf("to internal: %v", err)
	}
	internal.Spec.Source.Project = "someone-else"

	if in.Spec.Source.Project != "platform" {
		t.Errorf("mutating the converted Source changed the original: %q", in.Spec.Source.Project)
	}
}
