package main

import (
	"fmt"
	"math"
	"math/big"
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

// utilizationPercent computes allocated/total as a percentage from the exact
// counts. A pool with no reported total (status not yet populated) is 0%.
//
// big.Rat rather than float64 division: the counts are exact decimal strings
// precisely because they exceed float64's integer range for IPv6, so converting
// them to float64 first would discard the precision the strings exist to carry.
// This path is only the fallback for a server that did not report
// utilizationPercent; the server's own figure is preferred.
func utilizationPercent(c ipamv1alpha1.PoolCapacity) float64 {
	total, ok := new(big.Int).SetString(c.Total, 10)
	if !ok || total.Sign() <= 0 {
		return 0
	}
	allocated, ok := new(big.Int).SetString(c.Allocated, 10)
	if !ok {
		return 0
	}
	pct, _ := new(big.Rat).SetFrac(new(big.Int).Mul(allocated, big.NewInt(100)), total).Float64()
	return math.Round(pct*10000) / 10000
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
func utilizationCell(pct float64, width int, useColor bool) string {
	bar := utilizationBar(pct, width, useColor)
	label := utilizationLabel(pct)
	if label != "" {
		return fmt.Sprintf("%s %s (%s)", bar, formatPercent(pct), label)
	}
	return fmt.Sprintf("%s %s", bar, formatPercent(pct))
}

// formatPercent renders a utilization figure without rounding a real value away
// to nothing.
//
// %3.0f printed 0% for every pool sized the way these are: 256 addresses in a
// /12 is 0.024%, and a pool holding sixteen claims read as empty. Below 1% the
// figure is shown to two decimals — enough to distinguish "nothing" from "a
// little", which is the distinction that was lost. At or above 1% it stays a
// whole number, because 47.0022% is noise where 47% is the answer.
//
// Exactly zero prints as 0%, not 0.00%, so an empty pool still reads as empty
// at a glance.
func formatPercent(pct float64) string {
	switch {
	case pct == 0:
		return "  0%"
	case pct < 1:
		return fmt.Sprintf("%5.2f%%", pct)
	default:
		return fmt.Sprintf("%3.0f%%", pct)
	}
}

// poolHasServerStatus reports whether the server populated the family-agnostic
// status fields (ipFamily, utilizationPercent). The status
// family is the reliable signal: the server sets it for both root and child
// pools, so its presence means the accurate fields can be trusted over the
// int64 capacity counts, which saturate for IPv6.
func poolHasServerStatus(p *ipamv1alpha1.IPPool) bool {
	return p.Status.IPFamily != ""
}

// poolFamily returns the pool's effective address family. It prefers the
// server-reported status family — set on child pools too, which inherit rather
// than declare spec.ipFamily — and falls back to the spec family.
func poolFamily(p *ipamv1alpha1.IPPool) ipamv1alpha1.IPFamily {
	if p.Status.IPFamily != "" {
		return p.Status.IPFamily
	}
	return p.Spec.IPFamily
}

// poolUtilization returns the pool's utilization as a percentage. The server
// computes this with arbitrary-precision arithmetic (accurate for IPv6); the
// int64 capacity ratio is used only for older servers that don't report it.
func poolUtilization(p *ipamv1alpha1.IPPool) float64 {
	if poolHasServerStatus(p) {
		return p.Status.UtilizationPercent
	}
	return utilizationPercent(p.Status.Capacity)
}

// poolLargestFreeCell formats the largest-free-block column.
//
// The server no longer reports an exact largest free prefix — status.
// largestFreePrefix was removed because computing it meant reading every
// allocation in the pool on every write. What is left is the estimate derived
// from the integer capacity counts, which is approximate for IPv6 by
// construction and is labelled as such by largestFreeCell.
func poolLargestFreeCell(p *ipamv1alpha1.IPPool) string {
	return largestFreeCell(p.Spec.IPFamily, p.Status.Capacity)
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
	avail, ok := new(big.Int).SetString(c.Available, 10)
	if !ok || !avail.IsInt64() {
		// Either unreported, or a count too large for the estimator's int64
		// arithmetic — which for IPv6 is the normal case, not an error.
		return "—"
	}
	l := largestFreePrefix(family, avail.Int64())
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
// a usage error. Used by `pool create --cidr`.
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
