package main

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// The lease line says what happens to the address and when: a deadline says
// the address is released then, one already passed says it is due, and no
// lease says what will release the address instead.
func TestLeaseTextSaysWhatHappensAndWhen(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time { v := metav1.NewTime(now.Add(d)); return &v }
	for _, tc := range []struct {
		name   string
		status ipamv1alpha1.IPAllocationStatus
		want   []string
	}{
		{"future", ipamv1alpha1.IPAllocationStatus{RetainedAt: at(-time.Hour), ExpiresAt: at(3 * time.Hour)},
			[]string{"runs out 2026-11-21T15:00:00Z", "in 3h", "released back to the pool then"}},
		{"past", ipamv1alpha1.IPAllocationStatus{RetainedAt: at(-time.Hour), ExpiresAt: at(-10 * time.Minute)},
			[]string{"ran out", "10m ago", "due for release"}},
		{"none", ipamv1alpha1.IPAllocationStatus{RetainedAt: at(-time.Hour)}, []string{"none", "released or reclaimed"}},
		{"pending", ipamv1alpha1.IPAllocationStatus{RetainedAt: at(-time.Hour), Conditions: []metav1.Condition{{
			Type: ipamv1alpha1.AllocationConditionRetained, Status: metav1.ConditionTrue, Reason: ipamv1alpha1.RetainedReasonLeasePending,
		}}}, []string{"not yet evaluated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := leaseText(&ipamv1alpha1.IPAllocation{Status: tc.status}, now)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("leaseText = %q, want it to contain %q", got, w)
				}
			}
		})
	}
}
