package ipclaim

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// Every refusal a claim create can return, labelled by what the caller is
// told. A refusal reaching reasonInternal or reasonTxError would page someone
// for a request the caller has to fix.
func TestFailureReasonLabelsRefusalsByWhatTheCallerIsTold(t *testing.T) {
	gr := v1alpha1.Resource("ipclaims")
	plain := errors.New("connection reset by peer")

	tests := []struct {
		name  string
		err   error
		label string
		want  string
	}{
		// Validation and admission, which once landed on internal.
		{"strategy validation (422)", apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupName, Kind: "IPClaim"}, "c",
			field.ErrorList{field.Invalid(field.NewPath("spec", "address"), "not-an-ip", "must be an IP address")}),
			reasonInternal, reasonInvalid},
		{"bad request", apierrors.NewBadRequest("spec.className or spec.ipFamily is required"), reasonInternal, reasonInvalid},
		{"admission forbidden (quota)", apierrors.NewForbidden(gr, "c", errors.New("quota exceeded")), reasonInternal, reasonForbidden},
		{"admission conflict", apierrors.NewConflict(gr, "c", errors.New("conflict")), reasonInternal, reasonConflict},
		{"already exists", apierrors.NewAlreadyExists(gr, "c"), reasonInternal, reasonConflict},
		{"not found", apierrors.NewNotFound(gr, "c"), reasonInternal, reasonInvalid},

		// Every IPAM refusal reason, whatever label the step passed.
		{"Exhausted", registryerrors.NewInsufficientStorage("full"), reasonTxError, reasonPoolExhausted},
		{"Exhausted naming the pool", registryerrors.NewPoolExhausted("p"), reasonTxError, reasonPoolExhausted},
		{"ClassNotFound", ipamerrors.New(ipamerrors.ReasonClassNotFound, "x"), reasonTxError, reasonClassNotFound},
		{"NoDefaultClass", ipamerrors.New(ipamerrors.ReasonNoDefaultClass, "x"), reasonTxError, reasonClassNotFound},
		{"NoOfferingPool", ipamerrors.New(ipamerrors.ReasonNoOfferingPool, "x"), reasonTxError, reasonPoolNotFound},
		{"PrefixLengthRejected", ipamerrors.New(ipamerrors.ReasonPrefixLengthRejected, "x"), reasonInternal, reasonInvalid},
		{"FamilyMismatch", ipamerrors.New(ipamerrors.ReasonFamilyMismatch, "x"), reasonInvalid, reasonInvalid},
		{"AddressScopeMismatch", ipamerrors.New(ipamerrors.ReasonAddressScopeMismatch, "x"), reasonInvalid, reasonInvalid},
		{"ScopeRolesMissing", ipamerrors.NewScopeRolesMissing([]string{"network"}, "x"), reasonPoolNotFound, reasonInvalid},
		{"NoProjectScope", ipamerrors.New(ipamerrors.ReasonNoProjectScope, "x"), reasonClassNotFound, reasonInvalid},
		{"ClaimExists", duplicateClaimConflict("c"), reasonTxError, reasonConflict},
		{"AllocationRetained", retainedAllocationConflict("c", "a"), reasonTxError, reasonConflict},
		{"RetainedAllocationMismatch", retainedMismatchConflict("c", "a", &reclaimMismatch{field: "spec.className", message: "x"}), reasonTxError, reasonConflict},
		{"RangeUnplaced", ipamerrors.New(ipamerrors.ReasonRangeUnplaced, "x"), reasonTxError, reasonInvalid},
		// Found inside the allocation transaction, whose step label is a fault.
		{"RetentionLeaseRequired", ipamerrors.NewRetentionLeaseRequired(gr, "c", "x"), reasonTxError, reasonInvalid},
		{"SharedPoolUnplaced", ipamerrors.New(ipamerrors.ReasonSharedPoolUnplaced, "x"), reasonTxError, reasonInvalid},
		{"AddressUnaligned", ipamerrors.New(ipamerrors.ReasonAddressUnaligned, "x"), reasonTxError, reasonInvalid},
		{"AddressOutsidePool", ipamerrors.New(ipamerrors.ReasonAddressOutsidePool, "x"), reasonTxError, reasonInvalid},
		{"AddressInUse", ipamerrors.NewAddressInUse(gr, "c", ipamerrors.Holder{Name: "a"}, "x"), reasonTxError, reasonConflict},
		{"AddressReserved", ipamerrors.NewAddressReserved(gr, "c", "x"), reasonTxError, reasonConflict},

		// A specific label stands for an error with no IPAM reason.
		{"namespace not live", apierrors.NewForbidden(gr, "", errors.New("terminating")), reasonNamespaceNotLive, reasonNamespaceNotLive},
		{"class holds no range", apierrors.NewBadRequest("no poolPer"), reasonClassHoldsNoRange, reasonClassHoldsNoRange},

		// Server faults keep the label the step gave them.
		{"plain error from postgres", plain, reasonTxError, reasonTxError},
		{"wrapped plain error", fmt.Errorf("commit: %w", plain), reasonTxError, reasonTxError},
		{"encode failure", errors.New("encode"), reasonInternal, reasonInternal},
		{"500 status", apierrors.NewInternalError(plain), reasonTxError, reasonTxError},
		{"503 status", apierrors.NewServiceUnavailable("later"), reasonInternal, reasonInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureReason(tc.err, tc.label); got != tc.want {
				t.Errorf("failureReason(%v, %q) = %q, want %q", tc.err, tc.label, got, tc.want)
			}
		})
	}
}

// Every IPAM reason, not only the ones listed above, has an explicit label, and
// none that answers below 500 can land on a server-fault label whatever the
// failing step passed. A reason added to ipamerrors without a label here fails
// this test instead of paging the server-fault alert.
func TestEveryIPAMReasonHasANonFaultLabel(t *testing.T) {
	reasons := ipamerrors.KnownReasons()
	if len(reasons) == 0 {
		t.Fatal("ipamerrors.KnownReasons is empty")
	}
	for _, reason := range reasons {
		t.Run(string(reason), func(t *testing.T) {
			err := ipamerrors.New(reason, "refused")
			if _, ok := ipamReasonLabel(err); !ok {
				t.Errorf("reason %q has no label in ipamReasonLabel", reason)
			}
			code := err.Status().Code
			for _, fault := range []string{reasonInternal, reasonTxError} {
				got := failureReason(err, fault)
				if code < 500 && (got == reasonInternal || got == reasonTxError) {
					t.Errorf("reason %q (HTTP %d) with step label %q is counted as server fault %q", reason, code, fault, got)
				}
			}
		})
	}
}

// The allocator's address refusals, through the statuses the registry maps
// them to, for both a Block claim (mapAllocationError) and a ScopeRange claim
// (scopeRangeError).
func TestAddressRefusalsAreNeverServerFaults(t *testing.T) {
	refusals := map[string]struct {
		err  error
		want string
	}{
		"AddressInUse":       {&allocator.AddressInUseError{AllocationName: "a"}, reasonConflict},
		"AddressReserved":    {allocator.ErrAddressReserved, reasonConflict},
		"AddressOutsidePool": {allocator.ErrAddressOutsidePool, reasonInvalid},
		"PoolExhausted":      {allocator.ErrPoolExhausted, reasonPoolExhausted},
	}
	for name, tc := range refusals {
		t.Run("block/"+name, func(t *testing.T) {
			if got := failureReason(mapAllocationError(tc.err, "p", "c"), allocationFailureReason(tc.err)); got != tc.want {
				t.Errorf("block %s = %q, want %q", name, got, tc.want)
			}
		})
		t.Run("scoperange/"+name, func(t *testing.T) {
			label, apiErr := scopeRangeError(tc.err, "c")
			if got := failureReason(apiErr, label); got != tc.want {
				t.Errorf("scope range %s = %q, want %q", name, got, tc.want)
			}
		})
	}

	scopeRange := map[string]struct {
		err  error
		want string
	}{
		"SharedPoolUnplaced": {fmt.Errorf("%w: level", allocator.ErrSharedPoolUnplaced), reasonInvalid},
		"ClassHoldsNoRange":  {allocator.ErrClassHoldsNoRange, reasonClassHoldsNoRange},
		"NoOfferingPool":     {allocator.ErrNoOfferingPool, reasonPoolNotFound},
		"database":           {errors.New("conn closed"), reasonTxError},
	}
	for name, tc := range scopeRange {
		t.Run("scoperange/"+name, func(t *testing.T) {
			label, apiErr := scopeRangeError(tc.err, "c")
			if got := failureReason(apiErr, label); got != tc.want {
				t.Errorf("%s = %q, want %q", name, got, tc.want)
			}
		})
	}

	resolve := map[string]struct {
		err  error
		want string
	}{
		"RangeUnplaced":      {fmt.Errorf("%w: level", allocator.ErrRangeUnplaced), reasonInvalid},
		"AddressOutsidePool": {allocator.ErrAddressOutsidePool, reasonInvalid},
		"NoOfferingPool":     {allocator.ErrNoOfferingPool, reasonPoolNotFound},
		// Was counted as pool_not_found.
		"PoolExhausted": {allocator.ErrPoolExhausted, reasonPoolExhausted},
		"database":      {errors.New("conn closed"), reasonTxError},
	}
	for name, tc := range resolve {
		t.Run("resolve/"+name, func(t *testing.T) {
			if got := failureReason(resolvePoolError(tc.err), reasonTxError); got != tc.want {
				t.Errorf("%s = %q, want %q", name, got, tc.want)
			}
		})
	}
}
