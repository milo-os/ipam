package ipamvalidation

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestRetentionLease(t *testing.T) {
	path := field.NewPath("spec", "retentionLease")
	for _, tc := range []struct {
		name    string
		lease   *metav1.Duration
		wantErr string
	}{
		{name: "unset means no expiry", lease: nil},
		{name: "one second", lease: &metav1.Duration{Duration: time.Second}},
		{name: "a day", lease: &metav1.Duration{Duration: 24 * time.Hour}},
		{name: "exactly the ceiling", lease: &metav1.Duration{Duration: 8760 * time.Hour}},

		{name: "zero is a placeholder, not release-at-once", lease: &metav1.Duration{}, wantErr: "must be a positive duration"},
		{name: "negative", lease: &metav1.Duration{Duration: -time.Hour}, wantErr: "must be a positive duration"},
		{name: "past the ceiling", lease: &metav1.Duration{Duration: 8760*time.Hour + time.Second}, wantErr: "must be at most 8760h0m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := RetentionLease(tc.lease, path)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want exactly one", errs)
			}
			if errs[0].Field != path.String() || errs[0].Type != field.ErrorTypeInvalid {
				t.Errorf("error %v, want Invalid on %s", errs[0], path)
			}
			if !strings.Contains(errs[0].Detail, tc.wantErr) {
				t.Errorf("detail %q, want it to contain %q", errs[0].Detail, tc.wantErr)
			}
		})
	}
}

// Until expiry runs, a lease that is set is a lease that is ignored, and the
// author is told so in terms they can follow up on.
func TestRetentionLeaseWarning(t *testing.T) {
	path := field.NewPath("spec", "maxRetentionLease")
	if w := RetentionLeaseWarning(nil, path); w != nil {
		t.Errorf("warned about an unset lease: %v", w)
	}
	w := RetentionLeaseWarning(&metav1.Duration{Duration: time.Hour}, path)
	if len(w) != 1 {
		t.Fatalf("warnings = %v, want one", w)
	}
	for _, want := range []string{"spec.maxRetentionLease", "not yet enforced", "milo-os/ipam/issues/107"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("warning %q does not mention %q", w[0], want)
		}
	}
}

func TestRetentionLeaseChanged(t *testing.T) {
	hour := &metav1.Duration{Duration: time.Hour}
	for _, tc := range []struct {
		name         string
		updated, old *metav1.Duration
		want         bool
	}{
		{"both unset", nil, nil, false},
		{"unchanged", hour, &metav1.Duration{Duration: time.Hour}, false},
		{"newly set", hour, nil, true},
		{"cleared", nil, hour, true},
		{"changed", &metav1.Duration{Duration: 2 * time.Hour}, hour, true},
	} {
		if got := RetentionLeaseChanged(tc.updated, tc.old); got != tc.want {
			t.Errorf("%s: RetentionLeaseChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}
