// Package ipamerrors classifies the failures IPAM returns to a client.
//
// A client that asks for an address gets back an API status, and what it
// should do next depends on why the request was refused: an exhausted pool
// needs more space and is worth retrying later, an unconfigured class needs an
// operator, and a name held by a retained allocation needs the allocation
// deleted. None of that is legible from the HTTP status code — IPAM answers
// several distinct refusals with 400, and it signals exhaustion with 507, which
// client-go has no helper for. Without this package every consumer rediscovers
// the mapping and hard-codes the numbers.
//
// The classification travels in the status itself, as a cause on
// Status.Details, so it survives the wire and does not depend on message text.
// ReasonFor reads it; the accessors read the values IPAM already puts in
// Details, such as the name of the pool that ran out.
//
// The package imports nothing but apimachinery. Classifying an error must not
// pull an apiserver into a consumer's binary.
package ipamerrors

import (
	"errors"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Reason names why IPAM refused a request. It is the value carried in
// Status.Details.Causes[].Type, and it is part of IPAM's API: values are added
// over time, so a client should treat an unrecognised reason like
// ReasonUnknown rather than assuming the set is closed.
type Reason string

const (
	// ReasonUnknown is returned for anything this package cannot classify,
	// including errors that never came from IPAM.
	ReasonUnknown Reason = ""

	// ReasonExhausted reports that there was no space left. The pool that ran
	// out is named by ExhaustedPool when the request reached a pool; when an
	// ancestor pool ran out while the class's chain was being provisioned there
	// is no pool the caller named, and only the class appears in the message.
	ReasonExhausted Reason = "Exhausted"

	// ReasonClassNotFound reports that the named IPClass does not exist in the
	// project the claim resolves in.
	ReasonClassNotFound Reason = "ClassNotFound"

	// ReasonNoDefaultClass reports that a claim named an address family rather
	// than a class, and the project marks no default class for that family.
	ReasonNoDefaultClass Reason = "NoDefaultClass"

	// ReasonNoOfferingPool reports that the class exists but no pool offers it,
	// so there is nothing to allocate from.
	ReasonNoOfferingPool Reason = "NoOfferingPool"

	// ReasonPrefixLengthRejected reports that the requested prefix length is
	// outside what the class allows, or that neither the claim nor the class
	// stated one.
	ReasonPrefixLengthRejected Reason = "PrefixLengthRejected"

	// ReasonScopeRolesMissing reports that the claim's scope did not carry
	// roles the class requires. MissingScopeRoles names them.
	ReasonScopeRolesMissing Reason = "ScopeRolesMissing"

	// ReasonAllocationRetained reports that an earlier claim of the same name
	// retained its address, so the identity the new claim would take is
	// occupied. RetainedAllocation names the IPAllocation holding it.
	ReasonAllocationRetained Reason = "AllocationRetained"

	// ReasonClaimExists reports that a live claim of this name already holds an
	// allocation.
	ReasonClaimExists Reason = "ClaimExists"

	// ReasonNoProjectScope reports that the request did not arrive with a
	// project. Every object IPAM stores belongs to one, so there is nowhere to
	// allocate from.
	ReasonNoProjectScope Reason = "NoProjectScope"
)

// scopeFieldPrefix is the field path under which a missing scope role is
// reported, so the cause points at the claim field the caller must fill in.
const scopeFieldPrefix = "spec.scope."

// ReasonFor classifies err. It returns ReasonUnknown for an error that is not
// an API status, and for a status carrying no IPAM cause.
//
// A 507 with no cause is still read as exhaustion: that is what an IPAM older
// than this package returns, and exhaustion is the one refusal consumers were
// already handling.
func ReasonFor(err error) Reason {
	status, ok := statusOf(err)
	if !ok {
		return ReasonUnknown
	}
	for _, cause := range causes(status) {
		if reason := Reason(cause.Type); knownReasons[reason] {
			return reason
		}
	}
	if status.Code == http.StatusInsufficientStorage {
		return ReasonExhausted
	}
	return ReasonUnknown
}

// IsExhausted reports whether err says IPAM ran out of space. It covers both
// the pool the claim reached and an ancestor pool provisioned on its way there:
// the caller's options are the same either way.
func IsExhausted(err error) bool {
	return ReasonFor(err) == ReasonExhausted
}

// ExhaustedPool returns the name of the IPPool that ran out, and whether err
// named one. A claim names a class rather than a pool, so this is the only way
// to learn which pool to widen.
//
// It reports false for an exhaustion that no single pool accounts for, which is
// what a caller sees when a level of the class's chain ran out while it was
// being provisioned.
func ExhaustedPool(err error) (string, bool) {
	if !IsExhausted(err) {
		return "", false
	}
	status, _ := statusOf(err)
	if status.Details == nil || status.Details.Name == "" {
		return "", false
	}
	return status.Details.Name, true
}

// MissingScopeRoles returns the scope roles the claim did not carry, in the
// order the class asked for them, or nil when err is not a missing-role
// refusal.
func MissingScopeRoles(err error) []string {
	status, ok := statusOf(err)
	if !ok {
		return nil
	}
	var roles []string
	for _, cause := range causes(status) {
		if Reason(cause.Type) != ReasonScopeRolesMissing {
			continue
		}
		if role := strings.TrimPrefix(cause.Field, scopeFieldPrefix); role != "" {
			roles = append(roles, role)
		}
	}
	return roles
}

// RetainedAllocation returns the name of the IPAllocation holding the identity
// a claim tried to take, and whether err is that refusal. Deleting the named
// allocation frees the name.
func RetainedAllocation(err error) (string, bool) {
	status, ok := statusOf(err)
	if !ok {
		return "", false
	}
	for _, cause := range causes(status) {
		if Reason(cause.Type) == ReasonAllocationRetained && cause.Message != "" {
			return cause.Message, true
		}
	}
	return "", false
}

// New returns the status IPAM answers a refusal with, carrying reason as a
// cause so a client can classify it without reading the message. The HTTP code
// follows from the reason.
//
// Reasons that carry structure beyond the message have their own constructor
// below; New is for the rest.
func New(reason Reason, message string) *apierrors.StatusError {
	return newStatus(reason, message, nil, nil)
}

// NewPoolExhausted returns the refusal for a pool with no room left, naming the
// pool in Details so a client can read it without parsing the message.
func NewPoolExhausted(poolName, message string) *apierrors.StatusError {
	return newStatus(ReasonExhausted, message, &metav1.StatusDetails{
		Name:  poolName,
		Group: GroupName,
		Kind:  "ippools",
	}, nil)
}

// NewScopeRolesMissing returns the refusal for a scope short of roles its class
// requires, naming every missing role at once. A claim short two roles is one
// refusal naming both, not two round trips.
func NewScopeRolesMissing(roles []string, message string) *apierrors.StatusError {
	causes := make([]metav1.StatusCause, 0, len(roles))
	for _, role := range roles {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseType(ReasonScopeRolesMissing),
			Message: message,
			Field:   scopeFieldPrefix + role,
		})
	}
	return newStatus(ReasonScopeRolesMissing, message, nil, causes)
}

// NewClaimExists returns the conflict for a name a live claim already holds.
func NewClaimExists(gr schema.GroupResource, claimName, message string) *apierrors.StatusError {
	return newConflict(ReasonClaimExists, gr, claimName, message, "")
}

// NewRetainedAllocation returns the conflict for a name whose address an
// earlier claim retained, naming the IPAllocation that must go for the name to
// be reusable.
func NewRetainedAllocation(gr schema.GroupResource, claimName, allocationName, message string) *apierrors.StatusError {
	return newConflict(ReasonAllocationRetained, gr, claimName, message, allocationName)
}

// GroupName is the API group every IPAM resource belongs to. It is repeated
// here rather than imported so this package stays free of the API types.
const GroupName = "ipam.miloapis.com"

var knownReasons = map[Reason]bool{
	ReasonExhausted:            true,
	ReasonClassNotFound:        true,
	ReasonNoDefaultClass:       true,
	ReasonNoOfferingPool:       true,
	ReasonPrefixLengthRejected: true,
	ReasonScopeRolesMissing:    true,
	ReasonAllocationRetained:   true,
	ReasonClaimExists:          true,
	ReasonNoProjectScope:       true,
}

// codeFor is the HTTP code each reason answers with. Exhaustion is 507:
// the request was well formed and the server simply has no space for it. The
// conflicts are 409 because a name is taken. Everything else is a request the
// server will keep refusing until the caller or an operator changes something.
func codeFor(reason Reason) int32 {
	switch reason {
	case ReasonExhausted:
		return http.StatusInsufficientStorage
	case ReasonAllocationRetained, ReasonClaimExists:
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// statusReasonFor keeps the wire compatible with what a generic client-go
// consumer expects: apierrors.IsConflict and apierrors.IsBadRequest keep
// working on these errors, and the IPAM reason rides in the causes.
func statusReasonFor(reason Reason) metav1.StatusReason {
	switch reason {
	case ReasonExhausted:
		return metav1.StatusReason("InsufficientStorage")
	case ReasonAllocationRetained, ReasonClaimExists:
		return metav1.StatusReasonConflict
	default:
		return metav1.StatusReasonBadRequest
	}
}

func newStatus(reason Reason, message string, details *metav1.StatusDetails, extraCauses []metav1.StatusCause) *apierrors.StatusError {
	if details == nil {
		details = &metav1.StatusDetails{}
	}
	if len(extraCauses) > 0 {
		details.Causes = extraCauses
	} else {
		details.Causes = []metav1.StatusCause{{
			Type:    metav1.CauseType(reason),
			Message: message,
		}}
	}
	return &apierrors.StatusError{
		ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    codeFor(reason),
			Reason:  statusReasonFor(reason),
			Message: message,
			Details: details,
		},
	}
}

// newConflict builds the conflict client-go would build — same message shape,
// same details, so a consumer reading it as a plain conflict sees no change —
// and stamps the IPAM reason onto its causes.
func newConflict(reason Reason, gr schema.GroupResource, name, message, causeMessage string) *apierrors.StatusError {
	if causeMessage == "" {
		causeMessage = message
	}
	err := apierrors.NewConflict(gr, name, errors.New(message))
	if err.ErrStatus.Details == nil {
		err.ErrStatus.Details = &metav1.StatusDetails{}
	}
	err.ErrStatus.Details.Causes = []metav1.StatusCause{{
		Type:    metav1.CauseType(reason),
		Message: causeMessage,
	}}
	return err
}

func statusOf(err error) (metav1.Status, bool) {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return metav1.Status{}, false
	}
	return status.Status(), true
}

func causes(status metav1.Status) []metav1.StatusCause {
	if status.Details == nil {
		return nil
	}
	return status.Details.Causes
}
