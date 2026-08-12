// Package ipamvalidation holds validation rules shared by more than one IPAM
// registry.
//
// A reservation is stated in two places — on an IPPool by its author, and on an
// IPClass for the pools it provisions — and the two must agree on what a valid
// one is. A rule duplicated across the two strategies drifts, and the drift
// surfaces as a reservation accepted on a class that the pool it builds would
// have rejected.
package ipamvalidation

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// MaxReservedPositions caps how many positions one pool may withhold at its
// edges.
//
// A reserved position is a real allocation row, not a policy note, so the count
// is bounded by what one request transaction can write rather than by the
// arithmetic. A /48 of /96 units holds 2^48 positions, so an unbounded count
// passes every capacity check and then tries to write billions of rows.
//
// 1024 is the point where the intent has changed: reserving a gateway takes
// one, reserving management ranges at both ends takes a handful, and
// withholding a thousand is a child pool expressed the wrong way.
const MaxReservedPositions = 1024

// Reservations validates a reservation spec wherever one appears.
//
// An empty family means the pool has no carve yet, so the unit size is bounded
// by the wider of the two families; the allocation library rejects a unit that
// does not fit once the carve is known.
func Reservations(spec *ipam.ReservationSpec, family ipam.IPFamily, path *field.Path) field.ErrorList {
	if spec == nil {
		return nil
	}
	var allErrs field.ErrorList

	if spec.Leading < 0 {
		allErrs = append(allErrs, field.Invalid(path.Child("leading"), spec.Leading,
			"must not be negative"))
	}
	if spec.Trailing < 0 {
		allErrs = append(allErrs, field.Invalid(path.Child("trailing"), spec.Trailing,
			"must not be negative"))
	}

	if total := int64(spec.Leading) + int64(spec.Trailing); total > MaxReservedPositions {
		allErrs = append(allErrs, field.Invalid(path, total, fmt.Sprintf(
			"leading + trailing must not exceed %d: each reserved position becomes a real allocation, "+
				"so a large count writes that many rows in one transaction; withholding more than this "+
				"is a child pool rather than a reservation",
			MaxReservedPositions)))
	}

	// A class stating `reservations: {}` reserves nothing and should not be made
	// to name a unit size for positions it does not take.
	if spec.Leading == 0 && spec.Trailing == 0 {
		return allErrs
	}

	maxLen := int32(128)
	if family == ipam.IPv4 {
		maxLen = 32
	}
	switch {
	case spec.UnitPrefixLength == 0:
		allErrs = append(allErrs, field.Required(path.Child("unitPrefixLength"),
			"required whenever leading or trailing is non-zero: a position has no size until it is "+
				"stated, and a pool cannot take it from the class carving it because a pool may serve several"))
	case spec.UnitPrefixLength < 0 || spec.UnitPrefixLength > maxLen:
		allErrs = append(allErrs, field.Invalid(path.Child("unitPrefixLength"), spec.UnitPrefixLength,
			fmt.Sprintf("must be in [1, %d]", maxLen)))
	}

	return allErrs
}
