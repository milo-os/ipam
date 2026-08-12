// Package ipamvalidation holds validation rules shared by more than one IPAM
// registry.
//
// A reservation appears on an IPPool, and on an IPClass for the pools that
// class provisions. Both must accept the same reservations. A rule copied into
// each registry drifts, and the drift lets a class hold a reservation that its
// own pools reject.
package ipamvalidation

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// MaxReservedPositions is the largest number of positions one pool may withhold
// at its edges.
//
// Each reserved position becomes an allocation row, so the limit tracks what one
// transaction can write rather than what the address math allows. A /48 holds
// 2^48 /96 units, so an unbounded count passes every capacity check and then
// tries to write billions of rows.
//
// Above 1024, the intent has changed: a gateway takes one position, management
// ranges at both ends take a handful, and a thousand is a child pool written as
// a reservation.
const MaxReservedPositions = 1024

// Reservations validates a reservation spec wherever it appears.
//
// An empty family means the pool has no carve yet, so the wider of the two
// families bounds the unit size. The allocation library rejects a unit that does
// not fit once the carve is known.
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
			"leading + trailing must not exceed %d: each reserved position becomes an allocation, "+
				"so a large count writes that many rows in one transaction. To withhold more, "+
				"carve a child pool instead",
			MaxReservedPositions)))
	}

	// A spec of `reservations: {}` withholds nothing, so it needs no unit size.
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
			"required when leading or trailing is non-zero: a reserved position has no size until you "+
				"state one, and a pool cannot inherit it from a class, because a pool may serve several"))
	case spec.UnitPrefixLength < 0 || spec.UnitPrefixLength > maxLen:
		allErrs = append(allErrs, field.Invalid(path.Child("unitPrefixLength"), spec.UnitPrefixLength,
			fmt.Sprintf("must be in [1, %d]", maxLen)))
	}

	return allErrs
}
