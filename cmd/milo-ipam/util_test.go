package main

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestUtilizationPercent(t *testing.T) {
	cases := []struct {
		name string
		cap  ipamv1alpha1.PoolCapacity
		want float64
	}{
		{"empty total", ipamv1alpha1.PoolCapacity{}, 0},
		{"half", ipamv1alpha1.PoolCapacity{Total: "100", Allocated: "50"}, 50},
		{"full", ipamv1alpha1.PoolCapacity{Total: "256", Allocated: "256"}, 100},
		{"73pct", ipamv1alpha1.PoolCapacity{Total: "100", Allocated: "73"}, 73},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := utilizationPercent(tc.cap); got != tc.want {
				t.Fatalf("utilizationPercent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUtilizationLabel(t *testing.T) {
	cases := map[float64]string{
		10:  "",
		74:  "",
		75:  "MED",
		89:  "MED",
		90:  "HIGH",
		100: "HIGH",
	}
	for pct, want := range cases {
		if got := utilizationLabel(pct); got != want {
			t.Errorf("utilizationLabel(%v) = %q, want %q", pct, got, want)
		}
	}
}

func TestValidateCIDR(t *testing.T) {
	cases := []struct {
		in       string
		wantErr  bool
		wantFam  ipamv1alpha1.IPFamily
		wantBits int
	}{
		{"10.0.0.0/8", false, ipamv1alpha1.IPv4, 8},
		{"2001:db8::/32", false, ipamv1alpha1.IPv6, 32},
		{"not-a-cidr", true, "", 0},
		{"10.0.0.0", true, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p, fam, err := validateCIDR(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fam != tc.wantFam {
				t.Errorf("family = %q, want %q", fam, tc.wantFam)
			}
			if p.Bits() != tc.wantBits {
				t.Errorf("bits = %d, want %d", p.Bits(), tc.wantBits)
			}
		})
	}
}

func TestHumanDuration(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		t    metav1.Time
		want string
	}{
		{"zero", metav1.Time{}, "<unknown>"},
		{"seconds", metav1.NewTime(now.Add(-30 * time.Second)), "30s"},
		{"minutes", metav1.NewTime(now.Add(-5 * time.Minute)), "5m"},
		{"days", metav1.NewTime(now.Add(-312 * 24 * time.Hour)), "312d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanDuration(tc.t); got != tc.want {
				t.Fatalf("humanDuration = %q, want %q", got, tc.want)
			}
		})
	}
}
