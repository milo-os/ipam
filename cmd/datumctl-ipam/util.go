package main

import (
	"fmt"
	"math/bits"
	"net/netip"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// familyBits returns the address width for an IP family (32 for IPv4, 128 for
// IPv6). Unknown families default to IPv4 width.
func familyBits(family ipamv1alpha1.IPFamily) int {
	if family == ipamv1alpha1.IPv6 {
		return 128
	}
	return 32
}

// utilizationPercent computes allocated/total as a percentage. A pool with no
// reported total (status not yet populated) is treated as 0%.
func utilizationPercent(c ipamv1alpha1.PoolCapacity) float64 {
	if c.Total <= 0 {
		return 0
	}
	return float64(c.Allocated) / float64(c.Total) * 100
}

// utilizationLabel returns a non-color textual severity so meaning survives
// monochrome terminals, screen readers, and color-blind users.
func utilizationLabel(pct float64) string {
	switch {
	case pct >= 90:
		return "HIGH"
	case pct >= 75:
		return "MED"
	default:
		return ""
	}
}

// largestFreePrefix derives the prefix length of the largest power-of-two block
// that fits in the pool's available address count. It is an approximation
// computed entirely from status.capacity (the API does not report a true
// largest-contiguous-free block); a value of 0 means "no free space" and the
// caller should treat -1 / 0 as "unknown / none".
//
// Example: an IPv4 pool with 1024 available addresses can fit at most a /22
// (1024 = 2^10, hostBits=10, 32-10=22).
func largestFreePrefix(family ipamv1alpha1.IPFamily, available int64) int {
	if available <= 0 {
		return 0
	}
	width := familyBits(family)
	hostBits := bits.Len64(uint64(available)) - 1 // floor(log2(available))
	if hostBits > width {
		hostBits = width
	}
	return width - hostBits
}

// utilizationCell renders the at-a-glance utilization for a table row: a bar (in
// color when enabled) plus the always-present numeric percentage and a textual
// severity label so the cell is meaningful without color.
func utilizationCell(c ipamv1alpha1.PoolCapacity, width int, useColor bool) string {
	pct := utilizationPercent(c)
	bar := utilizationBar(pct, width, useColor)
	label := utilizationLabel(pct)
	if label != "" {
		return fmt.Sprintf("%s %3.0f%% (%s)", bar, pct, label)
	}
	return fmt.Sprintf("%s %3.0f%%", bar, pct)
}

// utilizationBar renders a fixed-width bar of filled/empty cells. When color is
// enabled the filled portion is tinted by severity; the glyphs themselves still
// distinguish filled from empty so the bar reads correctly in monochrome.
func utilizationBar(pct float64, width int, useColor bool) string {
	if width <= 0 {
		width = 10
	}
	filled := int(pct/100*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	bar := ""
	for i := 0; i < filled; i++ {
		bar += "█"
	}
	for i := filled; i < width; i++ {
		bar += "░"
	}
	if !useColor {
		return bar
	}
	switch {
	case pct >= 90:
		return colorize(bar, colorRed)
	case pct >= 75:
		return colorize(bar, colorYellow)
	default:
		return colorize(bar, colorGreen)
	}
}

// largestFreeCell formats the largest-free-block column.
func largestFreeCell(family ipamv1alpha1.IPFamily, c ipamv1alpha1.PoolCapacity) string {
	l := largestFreePrefix(family, c.Available)
	if l <= 0 {
		return "—"
	}
	return fmt.Sprintf("/%d", l)
}

// humanDuration formats an age the way kubectl does: the two most significant
// units, collapsing to a single coarse unit for long ages (e.g. 312d, 44d, 3h2m).
func humanDuration(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	d := time.Since(t.Time)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d.Hours()) / 24
		if days < 8 {
			h := int(d.Hours()) % 24
			if h == 0 {
				return fmt.Sprintf("%dd", days)
			}
			return fmt.Sprintf("%dd%dh", days, h)
		}
		if days < 365 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd", days)
	}
}

// validateCIDR parses a CIDR and reports the parsed prefix plus its family, or
// a usage error. Used by `pool create --cidr` and `prefix claim --cidr`.
func validateCIDR(cidr string) (netip.Prefix, ipamv1alpha1.IPFamily, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}, "", usageErrorf("invalid CIDR %q: %v", cidr, err)
	}
	fam := ipamv1alpha1.IPv4
	if p.Addr().Is6() {
		fam = ipamv1alpha1.IPv6
	}
	return p, fam, nil
}

// projectedUtilization estimates the pool utilization after a claim of the given
// prefix length, for --dry-run previews. It assumes the claim's addresses are
// added to the allocated count. Returns before% and after%.
func projectedUtilization(c ipamv1alpha1.PoolCapacity, family ipamv1alpha1.IPFamily, claimLen int) (before, after float64) {
	before = utilizationPercent(c)
	if c.Total <= 0 {
		return before, before
	}
	width := familyBits(family)
	hostBits := width - claimLen
	if hostBits < 0 || hostBits >= 63 {
		// Degenerate or astronomically large claim: clamp to full.
		return before, 100
	}
	claimAddrs := int64(1) << uint(hostBits)
	allocated := c.Allocated + claimAddrs
	if allocated > c.Total {
		allocated = c.Total
	}
	after = float64(allocated) / float64(c.Total) * 100
	return before, after
}
