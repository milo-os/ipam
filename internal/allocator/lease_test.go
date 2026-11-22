package allocator

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestEffectiveLease(t *testing.T) {
	for _, tc := range []struct {
		name              string
		classLease, poolC *metav1.Duration
		want              *time.Duration
	}{
		{"neither", nil, nil, nil},
		{"class only", dur(time.Hour), nil, ptr(time.Hour)},
		{"pool only", nil, dur(time.Hour), ptr(time.Hour)},
		{"pool shorter", dur(2 * time.Hour), dur(time.Hour), ptr(time.Hour)},
		{"class shorter", dur(time.Hour), dur(2 * time.Hour), ptr(time.Hour)},
		{"stored zero class lease is unset", dur(0), dur(time.Hour), ptr(time.Hour)},
		{"stored negative cap is unset", dur(time.Hour), dur(-time.Hour), ptr(time.Hour)},
		{"both non-positive", dur(0), dur(-time.Minute), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveLease(tc.classLease, tc.poolC)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("EffectiveLease = %v, want %v", got, tc.want)
			}
		})
	}
}

// Only a Retain the class would not have chosen needs a lease. An unset class
// policy is Delete.
func TestRetainOverridesClass(t *testing.T) {
	const (
		none   = ipamv1alpha1.ReclaimPolicy("")
		del    = ipamv1alpha1.ReclaimDelete
		retain = ipamv1alpha1.ReclaimRetain
	)
	for _, tc := range []struct {
		class, claim ipamv1alpha1.ReclaimPolicy
		want         bool
	}{
		{none, retain, true},
		{del, retain, true},
		{retain, retain, false},
		{retain, none, false},
		{del, none, false},
		{none, none, false},
		{retain, del, false},
		{del, del, false},
	} {
		class := &ipamv1alpha1.IPClass{Spec: ipamv1alpha1.IPClassSpec{ReclaimPolicy: tc.class}}
		if got := RetainOverridesClass(class, tc.claim); got != tc.want {
			t.Errorf("class %q, claim %q: RetainOverridesClass = %v, want %v", tc.class, tc.claim, got, tc.want)
		}
	}
}
