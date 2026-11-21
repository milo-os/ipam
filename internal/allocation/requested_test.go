package allocation

import (
	"errors"
	"net"
	"testing"
)

func TestRequestedBlock(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		prefixLen int
		want      string
		wantErr   error
	}{
		{name: "bare IPv4 host", addr: "10.0.0.10", prefixLen: 32, want: "10.0.0.10/32"},
		{name: "bare IPv4 aligned block", addr: "10.0.0.16", prefixLen: 28, want: "10.0.0.16/28"},
		{name: "bare IPv4 zero-length", addr: "0.0.0.0", prefixLen: 0, want: "0.0.0.0/0"},
		{name: "IPv4 CIDR", addr: "10.0.0.64/28", prefixLen: 28, want: "10.0.0.64/28"},
		{name: "IPv4 host CIDR", addr: "10.0.0.7/32", prefixLen: 32, want: "10.0.0.7/32"},
		{name: "bare IPv6 host", addr: "fd00::53", prefixLen: 128, want: "fd00::53/128"},
		{name: "bare IPv6 non-canonical", addr: "fd00:0:0:0::53", prefixLen: 128, want: "fd00::53/128"},
		{name: "bare IPv6 aligned block", addr: "fd00:0:0:1::", prefixLen: 64, want: "fd00:0:0:1::/64"},
		{name: "IPv6 CIDR", addr: "fd00:0:0:2::/64", prefixLen: 64, want: "fd00:0:0:2::/64"},

		// Unaligned requests are refused, never rounded to the containing block.
		{name: "bare IPv4 unaligned", addr: "10.0.0.5", prefixLen: 28, wantErr: ErrUnalignedAddress},
		{name: "IPv4 CIDR unaligned", addr: "10.0.0.5/28", prefixLen: 28, wantErr: ErrUnalignedAddress},
		{name: "bare IPv6 unaligned", addr: "fd00::1", prefixLen: 64, wantErr: ErrUnalignedAddress},
		{name: "IPv6 CIDR unaligned", addr: "fd00::1/64", prefixLen: 64, wantErr: ErrUnalignedAddress},

		{name: "CIDR shorter than prefixLen", addr: "10.0.0.0/28", prefixLen: 29, wantErr: ErrPrefixLenMismatch},
		{name: "CIDR longer than prefixLen", addr: "10.0.0.0/30", prefixLen: 29, wantErr: ErrPrefixLenMismatch},
		{name: "IPv4 prefix too long", addr: "10.0.0.0", prefixLen: 33, wantErr: ErrInvalidPrefixLen},
		{name: "negative prefix", addr: "10.0.0.0", prefixLen: -1, wantErr: ErrInvalidPrefixLen},
		{name: "IPv6 prefix too long", addr: "fd00::", prefixLen: 129, wantErr: ErrInvalidPrefixLen},

		{name: "garbage", addr: "not-an-ip", prefixLen: 32, wantErr: ErrInvalidAddress},
		{name: "octet out of range", addr: "10.0.0.999", prefixLen: 32, wantErr: ErrInvalidAddress},
		{name: "empty", addr: "", prefixLen: 32, wantErr: ErrInvalidAddress},
		{name: "malformed CIDR", addr: "10.0.0.0/abc", prefixLen: 32, wantErr: ErrInvalidAddress},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RequestedBlock(tc.addr, tc.prefixLen)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("RequestedBlock(%q, %d) = %v, %v; want error %v", tc.addr, tc.prefixLen, got.String(), err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RequestedBlock(%q, %d) error = %v", tc.addr, tc.prefixLen, err)
			}
			if got.String() != tc.want {
				t.Errorf("RequestedBlock(%q, %d) = %s, want %s", tc.addr, tc.prefixLen, got.String(), tc.want)
			}
		})
	}
}

// The block must compare equal to what the rest of the package produces, so
// the containment and overlap checks treat it like any other IPv4 block.
func TestRequestedBlockIPv4IsFourBytes(t *testing.T) {
	for _, addr := range []string{"10.0.0.10", "10.0.0.16/28"} {
		got, err := RequestedBlock(addr, map[string]int{"10.0.0.10": 32, "10.0.0.16/28": 28}[addr])
		if err != nil {
			t.Fatalf("RequestedBlock(%q): %v", addr, err)
		}
		if len(got.IP) != net.IPv4len || len(got.Mask) != net.IPv4len {
			t.Errorf("RequestedBlock(%q) IP len %d mask len %d, want 4/4", addr, len(got.IP), len(got.Mask))
		}
		_, parent, _ := net.ParseCIDR("10.0.0.0/24")
		if !IsBlockAvailable([]net.IPNet{*parent}, nil, got) {
			t.Errorf("RequestedBlock(%q) = %s not reported inside %s", addr, got.String(), parent)
		}
	}
}

func TestIsSingleAddress(t *testing.T) {
	tests := map[string]bool{
		"10.0.0.1/32": true,
		"10.0.0.0/31": false,
		"fd00::1/128": true,
		"fd00::/127":  false,
		"10.0.0.0/24": false,
		"fd00::/64":   false,
	}
	for cidr, want := range tests {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("parse %q: %v", cidr, err)
		}
		if got := IsSingleAddress(*block); got != want {
			t.Errorf("IsSingleAddress(%s) = %v, want %v", cidr, got, want)
		}
	}
	if IsSingleAddress(net.IPNet{}) {
		t.Error("IsSingleAddress(zero) = true, want false")
	}
}

func TestContainingBlock(t *testing.T) {
	tests := []struct {
		name      string
		block     string
		prefixLen int
		want      string
		wantErr   error
	}{
		{name: "IPv6 subnet in its VPC", block: "fd00:0:a:10::/64", prefixLen: 48, want: "fd00:0:a::/48"},
		{name: "IPv6 endpoint in its region", block: "fd00:0:a:10:0:5::/96", prefixLen: 64, want: "fd00:0:a:10::/64"},
		{name: "same length is itself", block: "fd00:0:a::/48", prefixLen: 48, want: "fd00:0:a::/48"},
		{name: "IPv4", block: "10.20.30.64/26", prefixLen: 16, want: "10.20.0.0/16"},
		{name: "IPv4 to zero", block: "10.20.30.64/26", prefixLen: 0, want: "0.0.0.0/0"},
		{name: "longer than the block", block: "fd00:0:a::/48", prefixLen: 64, wantErr: ErrInvalidPrefixLen},
		{name: "negative", block: "10.0.0.0/8", prefixLen: -1, wantErr: ErrInvalidPrefixLen},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, block, err := net.ParseCIDR(tc.block)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.block, err)
			}
			got, err := ContainingBlock(*block, tc.prefixLen)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ContainingBlock(%s, %d) = %s, %v; want error %v", tc.block, tc.prefixLen, got.String(), err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ContainingBlock(%s, %d) error = %v", tc.block, tc.prefixLen, err)
			}
			if got.String() != tc.want {
				t.Errorf("ContainingBlock(%s, %d) = %s, want %s", tc.block, tc.prefixLen, got.String(), tc.want)
			}
		})
	}
}
