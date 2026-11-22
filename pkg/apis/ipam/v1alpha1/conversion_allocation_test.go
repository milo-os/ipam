package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// The IPAllocation converters are hand-written too. The retention timestamps
// are the deadline an owner reads, so losing one in either direction would
// show a retained allocation as held with no lease.
func TestIPAllocationRetentionTimesSurviveRoundTrip(t *testing.T) {
	retained := metav1.NewTime(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))
	expires := metav1.NewTime(retained.Add(time.Hour))
	in := &IPAllocation{Status: IPAllocationStatus{RetainedAt: &retained, ExpiresAt: &expires}}

	var internal ipam.IPAllocation
	if err := convert_v1alpha1_IPAllocation_To_ipam(in, &internal); err != nil {
		t.Fatalf("to internal: %v", err)
	}
	var out IPAllocation
	if err := convert_ipam_IPAllocation_To_v1alpha1(&internal, &out); err != nil {
		t.Fatalf("to versioned: %v", err)
	}
	if out.Status.RetainedAt == nil || !out.Status.RetainedAt.Equal(&retained) {
		t.Errorf("retainedAt = %v, want %v", out.Status.RetainedAt, retained)
	}
	if out.Status.ExpiresAt == nil || !out.Status.ExpiresAt.Equal(&expires) {
		t.Errorf("expiresAt = %v, want %v", out.Status.ExpiresAt, expires)
	}
	if out.Status.RetainedAt == in.Status.RetainedAt {
		t.Error("retainedAt is shared with the input rather than copied")
	}
}
