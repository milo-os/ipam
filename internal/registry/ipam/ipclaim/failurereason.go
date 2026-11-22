package ipclaim

import (
	"errors"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// The reason label on ipam_allocation_failures_total.
//
// Only reasonInternal and reasonTxError describe a server fault. Every other
// reason is the server refusing the request it was sent, which the caller
// fixes, so an alert on server faults must never see one.
const (
	reasonInternal          = "internal"             // a bug: encode failures, impossible states
	reasonTxError           = "tx_error"             // postgres: begin, query, commit
	reasonInvalid           = "invalid"              // 400/422: the request is malformed or names something it may not have
	reasonConflict          = "conflict"             // 409: the name or the address is taken
	reasonForbidden         = "forbidden"            // 403: admission refused it (quota, policy)
	reasonPoolExhausted     = "pool_exhausted"       // 507: no space left
	reasonPoolNotFound      = "pool_not_found"       // no pool offers the class
	reasonClassNotFound     = "class_not_found"      // the class, or a default class, does not exist
	reasonClassHoldsNoRange = "class_holds_no_range" // a ScopeRange claim under a class with no poolPer
	reasonNamespaceNotLive  = "namespace_not_live"   // the namespace is missing or terminating
)

// failureReason returns the reason label for err, the error a claim create
// returns to its caller. label is what the failing step knows about itself:
// a specific reason (reasonNamespaceNotLive), or reasonInternal / reasonTxError
// for a step whose failure would be the server's.
//
// The order is what keeps refusals out of the fault labels, whichever path
// produced them:
//
//  1. An IPAM refusal is labelled by its IPAM reason. That reason is what the
//     caller is told, so it is what the metric says too.
//  2. A specific label stands.
//  3. A fault label is kept only for an error that is not a refusal: any other
//     API status is labelled by its code, so a 422 from validation or a 403
//     from admission is never counted as a server fault.
func failureReason(err error, label string) string {
	if reason, ok := ipamReasonLabel(err); ok {
		return reason
	}
	if label != reasonInternal && label != reasonTxError {
		return label
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return label
	}
	switch code := status.Status().Code; {
	case code == http.StatusConflict:
		return reasonConflict
	case code == http.StatusForbidden:
		return reasonForbidden
	case code == http.StatusInsufficientStorage:
		return reasonPoolExhausted
	case code >= 400 && code < 500:
		// 400, 404, 422 and the rest: the request, not the server.
		return reasonInvalid
	}
	return label
}

// ipamReasonLabel maps every IPAM refusal reason to its label.
func ipamReasonLabel(err error) (string, bool) {
	switch ipamerrors.ReasonFor(err) {
	case ipamerrors.ReasonExhausted:
		return reasonPoolExhausted, true
	case ipamerrors.ReasonClassNotFound, ipamerrors.ReasonNoDefaultClass:
		return reasonClassNotFound, true
	case ipamerrors.ReasonNoOfferingPool:
		return reasonPoolNotFound, true
	case ipamerrors.ReasonClaimExists, ipamerrors.ReasonAllocationRetained,
		ipamerrors.ReasonRetainedAllocationMismatch,
		ipamerrors.ReasonAddressInUse, ipamerrors.ReasonAddressReserved:
		return reasonConflict, true
	case ipamerrors.ReasonPrefixLengthRejected, ipamerrors.ReasonFamilyMismatch,
		ipamerrors.ReasonAddressScopeMismatch, ipamerrors.ReasonScopeRolesMissing,
		ipamerrors.ReasonNoProjectScope, ipamerrors.ReasonAddressOutsidePool,
		ipamerrors.ReasonAddressUnaligned, ipamerrors.ReasonSharedPoolUnplaced,
		ipamerrors.ReasonRangeUnplaced, ipamerrors.ReasonRetentionLeaseRequired:
		return reasonInvalid, true
	}
	return "", false
}
