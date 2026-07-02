package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// TestIPClassRoundTrip verifies the IPClass converters preserve every spec
// field across external → internal → external.
func TestIPClassRoundTrip(t *testing.T) {
	orig := &IPClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "public-egress",
			Annotations: map[string]string{IsDefaultClassAnnotation: "true"},
		},
		Spec: IPClassSpec{
			Provisioner:          NativeProvisioner,
			Parameters:           map[string]string{"k": "v"},
			IPFamily:             IPv4,
			Strategy:             LeastUtilized,
			AllowedPrefixLengths: PrefixLengthRange{Min: 24, Max: 28},
			DefaultPrefixLength:  26,
			ReclaimPolicy:        ReclaimRetain,
			Visibility:           "shared",
		},
	}

	var internal ipam.IPClass
	if err := convert_v1alpha1_IPClass_To_ipam(orig, &internal); err != nil {
		t.Fatalf("to internal: %v", err)
	}
	if internal.Spec.Provisioner != NativeProvisioner ||
		internal.Spec.IPFamily != ipam.IPv4 ||
		internal.Spec.Strategy != ipam.LeastUtilized ||
		internal.Spec.AllowedPrefixLengths.Min != 24 ||
		internal.Spec.AllowedPrefixLengths.Max != 28 ||
		internal.Spec.DefaultPrefixLength != 26 ||
		internal.Spec.ReclaimPolicy != ipam.ReclaimRetain ||
		internal.Spec.Visibility != "shared" ||
		internal.Spec.Parameters["k"] != "v" {
		t.Fatalf("internal spec mismatch: %+v", internal.Spec)
	}
	if internal.Annotations[ipam.IsDefaultClassAnnotation] != "true" {
		t.Fatalf("default annotation not preserved to internal")
	}

	var back IPClass
	if err := convert_ipam_IPClass_To_v1alpha1(&internal, &back); err != nil {
		t.Fatalf("to external: %v", err)
	}
	if back.Spec.Provisioner != orig.Spec.Provisioner ||
		back.Spec.IPFamily != orig.Spec.IPFamily ||
		back.Spec.Strategy != orig.Spec.Strategy ||
		back.Spec.AllowedPrefixLengths != orig.Spec.AllowedPrefixLengths ||
		back.Spec.DefaultPrefixLength != orig.Spec.DefaultPrefixLength ||
		back.Spec.ReclaimPolicy != orig.Spec.ReclaimPolicy ||
		back.Spec.Visibility != orig.Spec.Visibility ||
		back.Spec.Parameters["k"] != "v" {
		t.Fatalf("round-trip spec mismatch:\n got  %+v\n want %+v", back.Spec, orig.Spec)
	}
	if back.Annotations[IsDefaultClassAnnotation] != "true" {
		t.Fatalf("default annotation not preserved on round-trip")
	}
}

// TestClassNameFieldsRoundTrip verifies the new className/classNames fields on
// the existing resources survive conversion in both directions.
func TestClassNameFieldsRoundTrip(t *testing.T) {
	pool := &IPPool{Spec: IPPoolSpec{CIDR: "10.0.0.0/16", IPFamily: IPv4, ClassNames: []string{"a", "b"}}}
	var poolInt ipam.IPPool
	if err := convert_v1alpha1_IPPool_To_ipam(pool, &poolInt); err != nil {
		t.Fatalf("pool to internal: %v", err)
	}
	if len(poolInt.Spec.ClassNames) != 2 || poolInt.Spec.ClassNames[0] != "a" || poolInt.Spec.ClassNames[1] != "b" {
		t.Fatalf("pool classNames not preserved: %v", poolInt.Spec.ClassNames)
	}
	var poolBack IPPool
	if err := convert_ipam_IPPool_To_v1alpha1(&poolInt, &poolBack); err != nil {
		t.Fatalf("pool to external: %v", err)
	}
	if len(poolBack.Spec.ClassNames) != 2 {
		t.Fatalf("pool classNames lost on round-trip: %v", poolBack.Spec.ClassNames)
	}

	claim := &IPClaim{Spec: IPClaimSpec{IPFamily: IPv4, PrefixLength: 26, ClassName: "egress"}}
	var claimInt ipam.IPClaim
	if err := convert_v1alpha1_IPClaim_To_ipam(claim, &claimInt); err != nil {
		t.Fatalf("claim to internal: %v", err)
	}
	if claimInt.Spec.ClassName != "egress" {
		t.Fatalf("claim className not preserved: %q", claimInt.Spec.ClassName)
	}

	alloc := &IPAllocation{Spec: IPAllocationSpec{IPFamily: IPv4, PoolRef: LocalRef{Name: "p"}, ClassName: "egress"}}
	var allocInt ipam.IPAllocation
	if err := convert_v1alpha1_IPAllocation_To_ipam(alloc, &allocInt); err != nil {
		t.Fatalf("alloc to internal: %v", err)
	}
	if allocInt.Spec.ClassName != "egress" {
		t.Fatalf("allocation className not preserved: %q", allocInt.Spec.ClassName)
	}
	var allocBack IPAllocation
	if err := convert_ipam_IPAllocation_To_v1alpha1(&allocInt, &allocBack); err != nil {
		t.Fatalf("alloc to external: %v", err)
	}
	if allocBack.Spec.ClassName != "egress" {
		t.Fatalf("allocation className lost on round-trip: %q", allocBack.Spec.ClassName)
	}
}
