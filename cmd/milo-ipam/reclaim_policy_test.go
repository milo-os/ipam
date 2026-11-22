package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// The release prompt says what the server will do, and the server reads the
// policy recorded on the allocation. A class edited since the claim bound, or
// a reference class that states no policy, must not change the answer.
func TestEffectiveReclaimPolicyReadsTheAllocation(t *testing.T) {
	boundClaim := func() *ipamv1alpha1.IPClaim {
		return &ipamv1alpha1.IPClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec:       ipamv1alpha1.IPClaimSpec{ClassName: "public"},
			Status: ipamv1alpha1.IPClaimStatus{
				BoundAllocationRef: &ipamv1alpha1.LocalRef{Name: "alloc-web"},
			},
		}
	}
	class := func(policy ipamv1alpha1.ReclaimPolicy) *ipamv1alpha1.IPClass {
		return &ipamv1alpha1.IPClass{
			ObjectMeta: metav1.ObjectMeta{Name: "public"},
			Spec:       ipamv1alpha1.IPClassSpec{ReclaimPolicy: policy},
		}
	}
	allocation := func(policy ipamv1alpha1.ReclaimPolicy) *ipamv1alpha1.IPAllocation {
		return &ipamv1alpha1.IPAllocation{
			ObjectMeta: metav1.ObjectMeta{Name: "alloc-web", Namespace: "default"},
			Spec:       ipamv1alpha1.IPAllocationSpec{ReclaimPolicy: policy},
		}
	}

	t.Run("class edited to Delete after the claim bound under Retain", func(t *testing.T) {
		cs := newFakeClientset(class(ipamv1alpha1.ReclaimDelete), allocation(ipamv1alpha1.ReclaimRetain))
		if got := effectiveReclaimPolicy(cs, boundClaim()); got != ipamv1alpha1.ReclaimRetain {
			t.Errorf("policy = %s, want Retain from the allocation", got)
		}
	})
	t.Run("reference class with no policy of its own", func(t *testing.T) {
		cs := newFakeClientset(class(""), allocation(ipamv1alpha1.ReclaimRetain))
		if got := effectiveReclaimPolicy(cs, boundClaim()); got != ipamv1alpha1.ReclaimRetain {
			t.Errorf("policy = %s, want Retain from the allocation", got)
		}
	})
	t.Run("no allocation to read falls back to the class", func(t *testing.T) {
		cs := newFakeClientset(class(ipamv1alpha1.ReclaimRetain))
		if got := effectiveReclaimPolicy(cs, boundClaim()); got != ipamv1alpha1.ReclaimRetain {
			t.Errorf("policy = %s, want Retain from the class", got)
		}
	})
}
