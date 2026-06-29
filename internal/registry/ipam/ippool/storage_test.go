package ippool

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

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
