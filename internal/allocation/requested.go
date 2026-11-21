package allocation

// A claim may name the exact block it wants instead of a size. These functions
// turn that request into the block itself. They decide nothing about whether
// the block is free; IsBlockAvailable and the caller's overlap check do that.

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

var (
	// ErrInvalidAddress indicates a requested address that does not parse as an
	// IP address or a CIDR.
	ErrInvalidAddress = errors.New("ipam: invalid address")

	// ErrUnalignedAddress indicates a requested address that is not the first
	// address of a block of the requested size. It is never rounded to the
	// block it falls in: a caller naming 10.0.0.5 for a /28 asked for an
	// address it would not get, and binding 10.0.0.0/28 instead would hand it
	// space it did not name.
	ErrUnalignedAddress = errors.New("ipam: address is not aligned to the requested prefix length")

	// ErrPrefixLenMismatch indicates a requested CIDR whose length is not the
	// prefix length the claim allocates at.
	ErrPrefixLenMismatch = errors.New("ipam: requested CIDR length does not match the prefix length")
)

// RequestedBlock returns the block a claim naming addr at prefixLen bits asks
// for.
//
// A bare address ("10.0.0.16") names the block of prefixLen bits starting at
// it. A CIDR ("10.0.0.16/28") names itself, and its length must equal
// prefixLen. Either way the address must be the first of its block, otherwise
// ErrUnalignedAddress: the block is never rounded down.
//
// IPv4 blocks are returned in their 4-byte form so they compare equal to the
// blocks the rest of this package produces.
func RequestedBlock(addr string, prefixLen int) (net.IPNet, error) {
	if strings.Contains(addr, "/") {
		ip, block, err := net.ParseCIDR(addr)
		if err != nil {
			return net.IPNet{}, fmt.Errorf("%w: %q is not a CIDR", ErrInvalidAddress, addr)
		}
		if ones, _ := block.Mask.Size(); ones != prefixLen {
			return net.IPNet{}, fmt.Errorf("%w: %q is a /%d, the claim allocates a /%d", ErrPrefixLenMismatch, addr, ones, prefixLen)
		}
		if !ip.Equal(block.IP) {
			return net.IPNet{}, fmt.Errorf("%w: %q is not the start of a /%d (that block starts at %s)", ErrUnalignedAddress, addr, prefixLen, block.IP)
		}
		return normalise(*block), nil
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		return net.IPNet{}, fmt.Errorf("%w: %q is not an IP address", ErrInvalidAddress, addr)
	}
	bits := 8 * net.IPv6len
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		bits = 8 * net.IPv4len
	}
	if prefixLen < 0 || prefixLen > bits {
		return net.IPNet{}, fmt.Errorf("%w: /%d for a %d-bit address", ErrInvalidPrefixLen, prefixLen, bits)
	}
	mask := net.CIDRMask(prefixLen, bits)
	if start := ip.Mask(mask); !start.Equal(ip) {
		return net.IPNet{}, fmt.Errorf("%w: %s is not the start of a /%d (that block starts at %s)", ErrUnalignedAddress, addr, prefixLen, start)
	}
	return net.IPNet{IP: ip, Mask: mask}, nil
}

// IsSingleAddress reports whether block holds exactly one address: a /32 for
// IPv4, a /128 for IPv6.
func IsSingleAddress(block net.IPNet) bool {
	ones, bits := block.Mask.Size()
	return bits != 0 && ones == bits
}

// normalise returns block with a 4-byte IP when it is IPv4.
func normalise(block net.IPNet) net.IPNet {
	if v4 := block.IP.To4(); v4 != nil && len(block.Mask) == net.IPv4len {
		return net.IPNet{IP: v4, Mask: block.Mask}
	}
	return block
}
