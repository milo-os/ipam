// Package ipamvalidation holds validation rules shared by more than one IPAM
// registry.
//
// It exists because a reservation is stated in two places — on an IPPool by its
// author, and on an IPClass for the pools it provisions — and the two must agree
// on what a valid one is. A rule duplicated across the two strategies would
// drift, and the drift would show up as a reservation accepted on a class that
// the pool it built would have rejected.
package ipamvalidation

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// MaxReservedPositions caps how many positions one pool may withhold at its
// edges.
//
// The cap exists because a reserved position is not a policy note — it is a real
// allocation row and, once inventory lands, a real API object. The capacity
// check cannot substitute for it: a `/48` of `/96` units holds 2^48 positions,
// so `leading: 1000000000` passes every arithmetic test in the allocation
// library and then tries to write a billion rows inside one request
// transaction. The allocation library deliberately leaves this to admission,
// because it is policy rather than arithmetic.
//
// 1024 is chosen as the point where the intent has clearly changed. Reserving a
// gateway takes one; reserving a management range at each end takes a handful.
// Withholding a thousand positions is no longer an edge reservation, it is a
// sub-pool, and the model has a way to say that — a child pool.
const MaxReservedPositions = 1024

// Reservations validates a reservation spec wherever one appears.
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

	total := int64(spec.Leading) + int64(spec.Trailing)
	if total > MaxReservedPositions {
		allErrs = append(allErrs, field.Invalid(path, total, fmt.Sprintf(
			"leading + trailing must not exceed %d: each reserved position becomes a real allocation, so a large count writes that many rows in one transaction; withholding more than this is a child pool rather than a reservation",
			MaxReservedPositions)))
	}

	// Zero reservations are a no-op and validate nothing further — that is the
	// common case, and a class stating `reservations: {}` should not be forced
	// to name a unit size for positions it does not reserve.
	if spec.Leading == 0 && spec.Trailing == 0 {
		return allErrs
	}

	maxLen := int32(32)
	if family == ipam.IPv6 {
		maxLen = 128
	}
	switch {
	case spec.UnitPrefixLength == 0:
		// The pool cannot infer it. Pools serve classes of differing allocation
		// sizes, so "one position" has no meaning until someone says how big a
		// position is.
		allErrs = append(allErrs, field.Required(path.Child("unitPrefixLength"),
			"unitPrefixLength is required whenever leading or trailing is non-zero: a position has no size until it is stated, and a pool cannot take it from the class carving it because a pool may serve several"))
	case spec.UnitPrefixLength < 0 || spec.UnitPrefixLength > maxLen:
		allErrs = append(allErrs, field.Invalid(path.Child("unitPrefixLength"), spec.UnitPrefixLength,
			fmt.Sprintf("must be in [1, %d] for %s", maxLen, family)))
	}

	return allErrs
}

// Lease validates a retention lease field wherever one appears — IPClass's
// RetentionLease and IPPool's MaxRetentionLease.
//
// Shared for the same reason Reservations is: the two fields express the same
// quantity from opposite ends, and a rule enforced on one but not the other
// would let a class state something the pool capping it would have refused.
//
// A zero duration is rejected rather than read as "expire immediately". An
// operator writing `retentionLease: 0s` has almost certainly left a placeholder,
// and treating it as instant release turns Retain into Delete while the API
// still says Retain. Unset is how no-expiry is expressed; reclaimPolicy Delete
// already means release immediately, so there is nothing left for zero to say.
func Lease(lease *metav1.Duration, path *field.Path) field.ErrorList {
	if lease == nil {
		return nil
	}
	if lease.Duration <= 0 {
		return field.ErrorList{field.Invalid(path, lease.Duration.String(),
			"must be a positive duration; leave it unset for no expiry, and use reclaimPolicy Delete to release immediately")}
	}
	return nil
}
