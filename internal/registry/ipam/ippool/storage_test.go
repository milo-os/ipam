package ippool

import (
	"math"
	"net"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func mustCIDR(t *testing.T, s string) net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return *n
}

// TestSetPoolStatusCapacity verifies the utilization fields are sane for both
// address families. The IPv6 case guards against the int64 capacity overflow:
// a wide IPv6 pool must not report negative or out-of-range values, and the
// meaningful where the integer counts saturate.
func TestSetPoolStatusCapacity(t *testing.T) {
	tests := []struct {
		name            string
		pool            *ipam.IPPool
		parents         []net.IPNet
		allocations     []net.IPNet
		wantFamily      ipam.IPFamily
		wantUtilization int32
		wantLargestFree int32
		wantTotalAtMost int64 // 0 means "exact match against wantTotal"
		wantTotal       int64
	}{
		{
			name:            "IPv4 child pool reports exact counts",
			pool:            &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "10.0.0.0/16"}},
			parents:         []net.IPNet{mustCIDR(t, "10.0.0.0/16")},
			allocations:     []net.IPNet{mustCIDR(t, "10.0.0.0/24"), mustCIDR(t, "10.0.1.0/25")},
			wantFamily:      ipam.IPv4,
			wantUtilization: 0, // 384 / 65536 rounds down to 0%
			wantLargestFree: 17,
			wantTotal:       65536,
		},
		{
			name:            "IPv4 half allocated",
			pool:            &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "10.0.0.0/24"}},
			parents:         []net.IPNet{mustCIDR(t, "10.0.0.0/24")},
			allocations:     []net.IPNet{mustCIDR(t, "10.0.0.0/25")},
			wantFamily:      ipam.IPv4,
			wantUtilization: 50,
			wantLargestFree: 25,
			wantTotal:       256,
		},
		{
			name:            "IPv6 wide pool does not overflow",
			pool:            &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "2001:db8::/44"}},
			parents:         []net.IPNet{mustCIDR(t, "2001:db8::/44")},
			allocations:     []net.IPNet{mustCIDR(t, "2001:db8::/48")},
			wantFamily:      ipam.IPv6,
			wantUtilization: 6, // one /48 of sixteen /48s = 6.25% -> 6
			wantLargestFree: 45,
			wantTotalAtMost: math.MaxInt64,
		},
		{
			name:            "IPv6 empty pool",
			pool:            &ipam.IPPool{Spec: ipam.IPPoolSpec{IPFamily: ipam.IPv6, CIDR: "2001:db8::/40"}},
			parents:         []net.IPNet{mustCIDR(t, "2001:db8::/40")},
			allocations:     nil,
			wantFamily:      ipam.IPv6,
			wantUtilization: 0,
			wantLargestFree: 40,
			wantTotalAtMost: math.MaxInt64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setPoolStatusCapacity(tt.pool, tt.parents, tt.allocations)
			s := tt.pool.Status

			if s.IPFamily != tt.wantFamily {
				t.Errorf("ipFamily: got %q, want %q", s.IPFamily, tt.wantFamily)
			}
			if s.UtilizationPercent != tt.wantUtilization {
				t.Errorf("utilizationPercent: got %d, want %d", s.UtilizationPercent, tt.wantUtilization)
			}
			if s.UtilizationPercent < 0 || s.UtilizationPercent > 100 {
				t.Errorf("utilizationPercent %d out of [0,100]", s.UtilizationPercent)
			}

			// Capacity counts must never be negative regardless of family.
			if s.Capacity.Total < 0 || s.Capacity.Allocated < 0 || s.Capacity.Available < 0 {
				t.Errorf("capacity has negative field: %+v", s.Capacity)
			}
			if s.Capacity.Allocated > s.Capacity.Total {
				t.Errorf("allocated %d exceeds total %d", s.Capacity.Allocated, s.Capacity.Total)
			}
			if tt.wantTotalAtMost != 0 {
				if s.Capacity.Total > tt.wantTotalAtMost {
					t.Errorf("total %d exceeds saturation cap %d", s.Capacity.Total, tt.wantTotalAtMost)
				}
			} else if s.Capacity.Total != tt.wantTotal {
				t.Errorf("total: got %d, want %d", s.Capacity.Total, tt.wantTotal)
			}
		})
	}
}

// TestEffectiveIPFamily covers family resolution when carving a child pool —
// notably the child-of-child case, where the family comes from the parent's
// status.allocatedCIDR rather than spec.ipFamily.
func TestEffectiveIPFamily(t *testing.T) {
	tests := []struct {
		name    string
		pool    *ipam.IPPool
		want    string
		wantErr bool
	}{
		{
			name: "root pool uses explicit spec.ipFamily IPv4",
			pool: &ipam.IPPool{Spec: ipam.IPPoolSpec{IPFamily: ipam.IPv4}},
			want: "IPv4",
		},
		{
			name: "root pool uses explicit spec.ipFamily IPv6",
			pool: &ipam.IPPool{Spec: ipam.IPPoolSpec{IPFamily: ipam.IPv6}},
			want: "IPv6",
		},
		{
			name: "child parent resolves IPv4 from status.allocatedCIDR",
			pool: &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "10.1.0.0/16"}},
			want: "IPv4",
		},
		{
			name: "child parent resolves IPv6 from status.allocatedCIDR",
			pool: &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "2001:db8::/40"}},
			want: "IPv6",
		},
		{
			name:    "unprovisioned child parent: no family, no CIDR",
			pool:    &ipam.IPPool{ObjectMeta: metav1.ObjectMeta{Name: "pending"}},
			wantErr: true,
		},
		{
			name:    "malformed allocated CIDR",
			pool:    &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "not-a-cidr"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveIPFamily(tt.pool)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got family %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
