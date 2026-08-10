package allocation

import (
	"errors"
	"net"
	"testing"
)

func mustCIDR(t *testing.T, s string) net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return *n
}

func cidrStr(n net.IPNet) string {
	ones, _ := n.Mask.Size()
	return n.IP.String() + "/" + itoa(ones)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func TestFindFirstAvailable_FirstFit_EmptyPool(t *testing.T) {
	parent := mustCIDR(t, "10.0.0.0/16")
	got, err := FindFirstAvailableBlock([]net.IPNet{parent}, nil, 24, FirstFit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "10.0.0.0/24" {
		t.Fatalf("expected 10.0.0.0/24, got %s", cidrStr(*got))
	}
}

func TestFindFirstAvailable_FirstFit_SkipExisting(t *testing.T) {
	parent := mustCIDR(t, "10.0.0.0/16")
	existing := []net.IPNet{
		mustCIDR(t, "10.0.0.0/24"),
		mustCIDR(t, "10.0.1.0/24"),
	}
	got, err := FindFirstAvailableBlock([]net.IPNet{parent}, existing, 24, FirstFit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "10.0.2.0/24" {
		t.Fatalf("expected 10.0.2.0/24, got %s", cidrStr(*got))
	}
}

func TestFindFirstAvailable_FirstFit_FillsHole(t *testing.T) {
	parent := mustCIDR(t, "10.0.0.0/16")
	// Fragmented: hole between .0/24 and .2/24
	existing := []net.IPNet{
		mustCIDR(t, "10.0.0.0/24"),
		mustCIDR(t, "10.0.2.0/24"),
	}
	got, err := FindFirstAvailableBlock([]net.IPNet{parent}, existing, 24, FirstFit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "10.0.1.0/24" {
		t.Fatalf("expected 10.0.1.0/24, got %s", cidrStr(*got))
	}
}

func TestFindFirstAvailable_BestFit_PicksSmallestHole(t *testing.T) {
	parent := mustCIDR(t, "10.0.0.0/16")
	// Two holes:
	//   small: 10.0.1.0/24 (1 /24)
	//   large: 10.0.4.0/22 (4 /24s)
	// allocations frame the holes
	existing := []net.IPNet{
		mustCIDR(t, "10.0.0.0/24"),
		mustCIDR(t, "10.0.2.0/23"),
		mustCIDR(t, "10.0.8.0/21"),
	}
	got, err := FindFirstAvailableBlock([]net.IPNet{parent}, existing, 24, BestFit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "10.0.1.0/24" {
		t.Fatalf("BestFit should pick the tightest hole, got %s", cidrStr(*got))
	}
}

func TestFindFirstAvailable_LeastUtilized_PicksEmptiestParent(t *testing.T) {
	parents := []net.IPNet{
		mustCIDR(t, "10.0.0.0/16"),
		mustCIDR(t, "10.1.0.0/16"),
	}
	existing := []net.IPNet{
		mustCIDR(t, "10.0.0.0/24"),
		mustCIDR(t, "10.0.1.0/24"),
		mustCIDR(t, "10.0.2.0/24"),
	}
	got, err := FindFirstAvailableBlock(parents, existing, 24, LeastUtilized)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	parent2 := mustCIDR(t, "10.1.0.0/16")
	if !parent2.Contains(got.IP) {
		t.Fatalf("LeastUtilized should pick second parent, got %s", cidrStr(*got))
	}
}

func TestFindFirstAvailable_FullPool(t *testing.T) {
	// /29 pool, three /30s consume the whole space.
	parent := mustCIDR(t, "192.168.0.0/29") // 8 addresses, two /30 blocks
	existing := []net.IPNet{
		mustCIDR(t, "192.168.0.0/30"),
		mustCIDR(t, "192.168.0.4/30"),
	}
	_, err := FindFirstAvailableBlock([]net.IPNet{parent}, existing, 30, FirstFit)
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got %v", err)
	}
}

func TestFindFirstAvailable_NoParents(t *testing.T) {
	_, err := FindFirstAvailableBlock(nil, nil, 24, FirstFit)
	if !errors.Is(err, ErrNoParent) {
		t.Fatalf("expected ErrNoParent, got %v", err)
	}
}

func TestFindFirstAvailable_PrefixSmallerThanParent(t *testing.T) {
	parent := mustCIDR(t, "10.0.0.0/24")
	_, err := FindFirstAvailableBlock([]net.IPNet{parent}, nil, 16, FirstFit)
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got %v", err)
	}
}

func TestFindFirstAvailable_SingleHostV4(t *testing.T) {
	parent := mustCIDR(t, "10.0.0.0/30")
	existing := []net.IPNet{mustCIDR(t, "10.0.0.0/32")}
	got, err := FindFirstAvailableBlock([]net.IPNet{parent}, existing, 32, FirstFit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "10.0.0.1/32" {
		t.Fatalf("expected 10.0.0.1/32, got %s", cidrStr(*got))
	}
}

func TestFindFirstAvailable_IPv6(t *testing.T) {
	parent := mustCIDR(t, "2001:db8::/32")
	existing := []net.IPNet{mustCIDR(t, "2001:db8::/48")}
	got, err := FindFirstAvailableBlock([]net.IPNet{parent}, existing, 48, FirstFit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "2001:db8:1::/48" {
		t.Fatalf("expected 2001:db8:1::/48, got %s", cidrStr(*got))
	}
}

func TestFindFirstAvailable_IPv6_Single128(t *testing.T) {
	parent := mustCIDR(t, "2001:db8::/126")
	existing := []net.IPNet{
		mustCIDR(t, "2001:db8::/128"),
		mustCIDR(t, "2001:db8::1/128"),
	}
	got, err := FindFirstAvailableBlock([]net.IPNet{parent}, existing, 128, FirstFit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cidrStr(*got) != "2001:db8::2/128" {
		t.Fatalf("expected 2001:db8::2/128, got %s", cidrStr(*got))
	}
}

func TestCIDRsOverlap(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"10.0.0.0/24", "10.0.0.0/25", true},
		{"10.0.0.0/25", "10.0.0.128/25", false},
		{"10.0.0.0/24", "10.0.1.0/24", false},
		{"10.0.0.0/16", "10.0.5.0/24", true},
		{"10.0.0.0/24", "2001:db8::/32", false}, // family mismatch
	}
	for _, tc := range tests {
		got := CIDRsOverlap(mustCIDR(t, tc.a), mustCIDR(t, tc.b))
		if got != tc.want {
			t.Errorf("CIDRsOverlap(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCountAddresses(t *testing.T) {
	tests := []struct {
		cidr string
		want int64
	}{
		{"10.0.0.0/24", 256},
		{"10.0.0.0/30", 4},
		{"10.0.0.0/32", 1},
		{"2001:db8::/126", 4},
	}
	for _, tc := range tests {
		got := CountAddresses(mustCIDR(t, tc.cidr))
		if got != tc.want {
			t.Errorf("CountAddresses(%s) = %d, want %d", tc.cidr, got, tc.want)
		}
	}
}

func TestCIDRPool_Release(t *testing.T) {
	p := &CIDRPool{
		Ranges: []net.IPNet{mustCIDR(t, "10.0.0.0/16")},
		Existing: []net.IPNet{
			mustCIDR(t, "10.0.0.0/24"),
			mustCIDR(t, "10.0.1.0/24"),
		},
	}
	got, err := p.Release(mustCIDR(t, "10.0.0.0/24"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 || cidrStr(got[0]) != "10.0.1.0/24" {
		t.Fatalf("after release expected only 10.0.1.0/24, got %v", got)
	}

	if _, err := p.Release(mustCIDR(t, "10.0.99.0/24")); !errors.Is(err, ErrNotInPool) {
		t.Fatalf("expected ErrNotInPool, got %v", err)
	}
}

func TestUtilizationPercent(t *testing.T) {
	tests := []struct {
		name     string
		parents  []net.IPNet
		existing []net.IPNet
		want     int
	}{
		{name: "empty", parents: []net.IPNet{mustCIDR(t, "10.0.0.0/24")}, want: 0},
		{
			name:     "half full IPv4",
			parents:  []net.IPNet{mustCIDR(t, "10.0.0.0/24")},
			existing: []net.IPNet{mustCIDR(t, "10.0.0.0/25")},
			want:     50,
		},
		{
			name:     "full IPv4",
			parents:  []net.IPNet{mustCIDR(t, "10.0.0.0/24")},
			existing: []net.IPNet{mustCIDR(t, "10.0.0.0/24")},
			want:     100,
		},
		{
			name:     "wide IPv6 stays in range (one /48 of a /44)",
			parents:  []net.IPNet{mustCIDR(t, "2001:db8::/44")},
			existing: []net.IPNet{mustCIDR(t, "2001:db8::/48")},
			want:     6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UtilizationPercent(tt.parents, tt.existing)
			if got != tt.want {
				t.Fatalf("UtilizationPercent = %d, want %d", got, tt.want)
			}
			if got < 0 || got > 100 {
				t.Fatalf("UtilizationPercent %d out of [0,100]", got)
			}
		})
	}
}
