package main

import (
	"fmt"
	"math/big"
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
//
// The counts are decimal strings, and an IPv6 pool's total exceeds any int64, so
// the division is done in big.Rat and only the result becomes a float.
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
	return pct
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

// utilizationCell renders the at-a-glance utilization for a table row: a bar (in
// color when enabled) plus the always-present numeric percentage and a textual
// severity label so the cell is meaningful without color.
func utilizationCell(pct float64, width int, useColor bool) string {
	bar := utilizationBar(pct, width, useColor)
	label := utilizationLabel(pct)
	if label != "" {
		return fmt.Sprintf("%s %3.0f%% (%s)", bar, pct, label)
	}
	return fmt.Sprintf("%s %3.0f%%", bar, pct)
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
		return float64(p.Status.UtilizationPercent)
	}
	return utilizationPercent(p.Status.Capacity)
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
	total, ok := new(big.Int).SetString(c.Total, 10)
	if !ok || total.Sign() <= 0 {
		return before, before
	}
	allocated, ok := new(big.Int).SetString(c.Allocated, 10)
	if !ok {
		return before, before
	}
	hostBits := familyBits(family) - claimLen
	if hostBits < 0 {
		return before, 100
	}
	allocated.Add(allocated, new(big.Int).Lsh(big.NewInt(1), uint(hostBits)))
	if allocated.Cmp(total) > 0 {
		allocated.Set(total)
	}
	after, _ = new(big.Rat).SetFrac(new(big.Int).Mul(allocated, big.NewInt(100)), total).Float64()
	return before, after
}
