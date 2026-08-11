package ippool

import (
	"math/big"
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

// Capacity is reported as exact decimal strings, so an IPv6 pool holding more
// addresses than an int64 can count reports the real number rather than a
// ceiling. Utilization is a float, because an integer percent reads as zero at
// the sizes these pools are sized for.
func TestSetPoolStatusCapacity(t *testing.T) {
	tests := []struct {
		name            string
		pool            *ipam.IPPool
		parents         []net.IPNet
		allocations     []net.IPNet
		wantFamily      ipam.IPFamily
		wantUtilization float64
		wantTotal       string
		wantAllocated   string
	}{
		{
			name:            "IPv4 child pool reports exact counts",
			pool:            &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "10.0.0.0/16"}},
			parents:         []net.IPNet{mustCIDR(t, "10.0.0.0/16")},
			allocations:     []net.IPNet{mustCIDR(t, "10.0.0.0/24"), mustCIDR(t, "10.0.1.0/25")},
			wantFamily:      ipam.IPv4,
			wantUtilization: 0.5859, // 384 of 65536
			wantTotal:       "65536",
			wantAllocated:   "384",
		},
		{
			name:            "IPv4 half allocated",
			pool:            &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "10.0.0.0/24"}},
			parents:         []net.IPNet{mustCIDR(t, "10.0.0.0/24")},
			allocations:     []net.IPNet{mustCIDR(t, "10.0.0.0/25")},
			wantFamily:      ipam.IPv4,
			wantUtilization: 50,
			wantTotal:       "256",
			wantAllocated:   "128",
		},
		{
			name:        "IPv6 pool past the int64 ceiling",
			pool:        &ipam.IPPool{Status: ipam.IPPoolStatus{AllocatedCIDR: "2001:db8::/44"}},
			parents:     []net.IPNet{mustCIDR(t, "2001:db8::/44")},
			allocations: []net.IPNet{mustCIDR(t, "2001:db8::/48")},
			wantFamily:  ipam.IPv6,
			// A /44 holds 2^84 addresses and a /48 holds 2^80: both are past
			// int64, and the exact figures are what an integer count could not
			// report.
			wantUtilization: 6.25,
			wantTotal:       "19342813113834066795298816",
			wantAllocated:   "1208925819614629174706176",
		},
		{
			name:            "IPv6 empty pool",
			pool:            &ipam.IPPool{Spec: ipam.IPPoolSpec{IPFamily: ipam.IPv6, CIDR: "2001:db8::/40"}},
			parents:         []net.IPNet{mustCIDR(t, "2001:db8::/40")},
			allocations:     nil,
			wantFamily:      ipam.IPv6,
			wantUtilization: 0,
			wantTotal:       "309485009821345068724781056",
			wantAllocated:   "0",
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
				t.Errorf("utilizationPercent: got %v, want %v", s.UtilizationPercent, tt.wantUtilization)
			}
			if s.UtilizationPercent < 0 || s.UtilizationPercent > 100 {
				t.Errorf("utilizationPercent %v out of [0,100]", s.UtilizationPercent)
			}
			if s.Capacity.Total != tt.wantTotal {
				t.Errorf("total: got %s, want %s", s.Capacity.Total, tt.wantTotal)
			}
			if s.Capacity.Allocated != tt.wantAllocated {
				t.Errorf("allocated: got %s, want %s", s.Capacity.Allocated, tt.wantAllocated)
			}

			// Available is what remains, so the three must agree exactly.
			total, ok := new(big.Int).SetString(s.Capacity.Total, 10)
			if !ok {
				t.Fatalf("total %q is not a decimal integer", s.Capacity.Total)
			}
			alloc, ok := new(big.Int).SetString(s.Capacity.Allocated, 10)
			if !ok {
				t.Fatalf("allocated %q is not a decimal integer", s.Capacity.Allocated)
			}
			avail, ok := new(big.Int).SetString(s.Capacity.Available, 10)
			if !ok {
				t.Fatalf("available %q is not a decimal integer", s.Capacity.Available)
			}
			if got := new(big.Int).Add(alloc, avail); got.Cmp(total) != 0 {
				t.Errorf("allocated + available = %s, want total %s", got, total)
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
