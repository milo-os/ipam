package allocator

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func classLeasing(d *metav1.Duration) *ipamv1alpha1.IPClass {
	return &ipamv1alpha1.IPClass{Spec: ipamv1alpha1.IPClassSpec{RetentionLease: d}}
}

func poolCapping(d *metav1.Duration) *ipamv1alpha1.IPPool {
	return &ipamv1alpha1.IPPool{Spec: ipamv1alpha1.IPPoolSpec{MaxRetentionLease: d}}
}

func TestEffectiveLease(t *testing.T) {
	day := 24 * time.Hour

	tests := []struct {
		name  string
		class *ipamv1alpha1.IPClass
		pool  *ipamv1alpha1.IPPool
		want  *time.Duration
	}{
		{
			// The default, and the one that must not change by accident:
			// shipping the lease feature releases nothing until someone opts in.
			name:  "both unset means no expiry",
			class: classLeasing(nil),
			pool:  poolCapping(nil),
			want:  nil,
		},
		{
			name:  "the class states it and nothing caps it",
			class: classLeasing(dur(30 * day)),
			pool:  poolCapping(nil),
			want:  ptr(30 * day),
		},
		{
			// The case that makes the cap worth having: an operator hardening a
			// scarce range does not have to edit every class drawing from it.
			name:  "the pool caps a class that states none",
			class: classLeasing(nil),
			pool:  poolCapping(dur(7 * day)),
			want:  ptr(7 * day),
		},
		{
			// The pool is the thing that runs out, so it wins.
			name:  "the pool's shorter cap wins",
			class: classLeasing(dur(90 * day)),
			pool:  poolCapping(dur(7 * day)),
			want:  ptr(7 * day),
		},
		{
			// A generous pool does not extend a strict class.
			name:  "a longer pool cap does not extend the class",
			class: classLeasing(dur(day)),
			pool:  poolCapping(dur(90 * day)),
			want:  ptr(day),
		},
		{
			name:  "equal values resolve to that value",
			class: classLeasing(dur(day)),
			pool:  poolCapping(dur(day)),
			want:  ptr(day),
		},
		{
			name:  "nil objects are tolerated",
			class: nil,
			pool:  nil,
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveLease(tt.class, tt.pool)
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("EffectiveLease = %v, want no expiry", *got)
			case tt.want != nil && got == nil:
				t.Fatalf("EffectiveLease = no expiry, want %v", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Fatalf("EffectiveLease = %v, want %v", *got, *tt.want)
			}
		})
	}
}

// The clock is the whole reason migration 004 exists. Measuring from allocation
// would expire an address that was allocated long ago and retained a moment ago,
// which is worst for exactly the long-lived addresses retention protects.
func TestLeaseExpiryMeasuresFromRetentionNotAllocation(t *testing.T) {
	lease := 30 * 24 * time.Hour
	retainedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	expiry, ok := LeaseExpiry(retainedAt, &lease)
	if !ok {
		t.Fatal("a retained allocation with a lease must have an expiry")
	}
	if want := retainedAt.Add(lease); !expiry.Equal(want) {
		t.Errorf("expiry = %s, want %s", expiry, want)
	}

	// An address allocated a year before it was retained still gets a full
	// lease, because the function is not given the allocation time at all.
	if expiry.Before(retainedAt) {
		t.Error("expiry precedes retention")
	}
}

func TestLeaseExpiryWithoutALeaseNeverExpires(t *testing.T) {
	if _, ok := LeaseExpiry(time.Now(), nil); ok {
		t.Error("an allocation with no lease must never report an expiry")
	}
}

// A zero retainedAt means the row was never retained — a bound allocation, a
// reservation, a pool carve. None of those expire, and treating a zero time as
// "the epoch" would make every one of them instantly overdue.
func TestLeaseExpiryOfANeverRetainedAllocation(t *testing.T) {
	lease := time.Hour
	if _, ok := LeaseExpiry(time.Time{}, &lease); ok {
		t.Error("an allocation that was never retained must not report an expiry")
	}
}

// Zero is rejected rather than read as "expire immediately". Reading it that way
// would make Retain behave as Delete while still saying Retain, and there is
// already a policy that means release immediately.
func TestValidateLease(t *testing.T) {
	if err := ValidateLease(nil, "spec.retentionLease"); err != nil {
		t.Errorf("unset must be valid — it is how no-expiry is expressed: %v", err)
	}
	if err := ValidateLease(dur(time.Hour), "spec.retentionLease"); err != nil {
		t.Errorf("a positive lease must be valid: %v", err)
	}
	if err := ValidateLease(dur(0), "spec.retentionLease"); err == nil {
		t.Error("a zero lease must be rejected")
	}
	if err := ValidateLease(dur(-time.Hour), "spec.retentionLease"); err == nil {
		t.Error("a negative lease must be rejected")
	}
}

func ptr(d time.Duration) *time.Duration { return &d }
