package ipamerrors

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var ipclaims = schema.GroupResource{Group: GroupName, Resource: "ipclaims"}

// roundTrip is what a client actually classifies: the status as it arrives
// after being serialised by the apiserver and decoded by client-go. A
// classification that only works on the in-process value is worthless.
func roundTrip(t *testing.T, err *apierrors.StatusError) error {
	t.Helper()
	data, marshalErr := json.Marshal(err.ErrStatus)
	if marshalErr != nil {
		t.Fatalf("marshal status: %v", marshalErr)
	}
	var status metav1.Status
	if unmarshalErr := json.Unmarshal(data, &status); unmarshalErr != nil {
		t.Fatalf("unmarshal status: %v", unmarshalErr)
	}
	return &apierrors.StatusError{ErrStatus: status}
}

func TestReasonForEveryRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  *apierrors.StatusError
		want Reason
		code int32
	}{
		{"leaf exhaustion", NewPoolExhausted("us-east-v4", `IPPool "us-east-v4" is exhausted`), ReasonExhausted, http.StatusInsufficientStorage},
		{"cascade exhaustion", New(ReasonExhausted, "ipam: pool exhausted"), ReasonExhausted, http.StatusInsufficientStorage},
		{"class not found", New(ReasonClassNotFound, "ipam: class not found"), ReasonClassNotFound, http.StatusBadRequest},
		{"no default class", New(ReasonNoDefaultClass, "ipam: no default class for address family"), ReasonNoDefaultClass, http.StatusBadRequest},
		{"no offering pool", New(ReasonNoOfferingPool, "ipam: no pool offers this class"), ReasonNoOfferingPool, http.StatusBadRequest},
		{"prefix length", New(ReasonPrefixLengthRejected, "prefixLength 20 is outside class range"), ReasonPrefixLengthRejected, http.StatusBadRequest},
		{"scope roles", NewScopeRolesMissing([]string{"location"}, "scope is missing role \"location\""), ReasonScopeRolesMissing, http.StatusBadRequest},
		{"retained allocation", NewRetainedAllocation(ipclaims, "web", "alloc-1234", "retained by an earlier claim"), ReasonAllocationRetained, http.StatusConflict},
		{"claim exists", NewClaimExists(ipclaims, "web", "already holds an allocation"), ReasonClaimExists, http.StatusConflict},
		{"no project scope", New(ReasonNoProjectScope, "ipam: request carries no project scope"), ReasonNoProjectScope, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.ErrStatus.Code; got != tc.code {
				t.Errorf("code = %d, want %d", got, tc.code)
			}
			if got := ReasonFor(roundTrip(t, tc.err)); got != tc.want {
				t.Errorf("ReasonFor after the wire = %q, want %q", got, tc.want)
			}
		})
	}
}

// The message is what an operator reads, so nothing may swallow it on the way
// to the client.
func TestMessageSurvives(t *testing.T) {
	err := roundTrip(t, New(ReasonClassNotFound, "ipam: class not found"))
	if got := err.Error(); got != "ipam: class not found" {
		t.Errorf("Error() = %q, want the message IPAM refused with", got)
	}
}

// A claim names a class, never a pool, so a caller that cannot read the pool
// name has nothing to widen.
func TestExhaustedPoolNamesThePool(t *testing.T) {
	err := roundTrip(t, NewPoolExhausted("us-east-v4", `IPPool "us-east-v4" is exhausted`))
	if !IsExhausted(err) {
		t.Fatal("pool exhaustion is not classified as exhaustion")
	}
	pool, ok := ExhaustedPool(err)
	if !ok || pool != "us-east-v4" {
		t.Errorf("ExhaustedPool = %q, %v; want the pool that ran out", pool, ok)
	}
}

// Running out while an ancestor pool is provisioned is the same outcome, but no
// single pool accounts for it. Reporting a name there would name the wrong one.
func TestCascadeExhaustionNamesNoPool(t *testing.T) {
	err := roundTrip(t, New(ReasonExhausted, "ipam: pool exhausted"))
	if !IsExhausted(err) {
		t.Fatal("cascade exhaustion is not classified as exhaustion")
	}
	if pool, ok := ExhaustedPool(err); ok {
		t.Errorf("ExhaustedPool = %q, want no name", pool)
	}
}

// An IPAM that predates this package answers exhaustion with a bare 507. A
// consumer that upgrades its client before the server must not stop seeing it.
func TestBareInsufficientStorageIsExhaustion(t *testing.T) {
	legacy := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    http.StatusInsufficientStorage,
		Reason:  metav1.StatusReason("InsufficientStorage"),
		Message: "IPPool is exhausted",
	}}
	if !IsExhausted(roundTrip(t, legacy)) {
		t.Error("a bare 507 is no longer read as exhaustion")
	}
}

// A claim short two roles is one refusal naming both, not two round trips.
func TestMissingScopeRoles(t *testing.T) {
	err := roundTrip(t, NewScopeRolesMissing([]string{"network", "location"}, "scope is missing roles"))
	got := MissingScopeRoles(err)
	if want := []string{"network", "location"}; !slices.Equal(got, want) {
		t.Errorf("MissingScopeRoles = %v, want %v in the order the class asked", got, want)
	}
	if roles := MissingScopeRoles(roundTrip(t, New(ReasonClassNotFound, "nope"))); roles != nil {
		t.Errorf("MissingScopeRoles on an unrelated refusal = %v, want none", roles)
	}
}

// The two 409s call for opposite actions: delete the allocation, or stop
// creating a claim that already exists. Telling them apart is the point.
func TestConflictsAreDistinguishable(t *testing.T) {
	retained := roundTrip(t, NewRetainedAllocation(ipclaims, "web", "alloc-1234", "retained"))
	name, ok := RetainedAllocation(retained)
	if !ok || name != "alloc-1234" {
		t.Errorf("RetainedAllocation = %q, %v; want the IPAllocation holding the name", name, ok)
	}
	if !apierrors.IsConflict(retained) {
		t.Error("a retained-allocation refusal is no longer a conflict to client-go")
	}

	exists := roundTrip(t, NewClaimExists(ipclaims, "web", "already exists"))
	if ReasonFor(exists) != ReasonClaimExists {
		t.Error("a duplicate claim is classified as a retained allocation")
	}
	if _, ok := RetainedAllocation(exists); ok {
		t.Error("a duplicate claim reports a retained allocation")
	}
}

// Consumers that already branch on client-go predicates must keep working:
// the IPAM reason rides alongside, it does not replace the status reason.
func TestGenericPredicatesStillHold(t *testing.T) {
	if !apierrors.IsBadRequest(roundTrip(t, New(ReasonClassNotFound, "nope"))) {
		t.Error("a class-not-found refusal is no longer a bad request")
	}
	details := apierrors.NewConflict(ipclaims, "web", errors.New("boom"))
	if ReasonFor(details) != ReasonUnknown {
		t.Error("a plain conflict from client-go picked up an IPAM reason")
	}
}

func TestUnclassifiableErrors(t *testing.T) {
	if got := ReasonFor(errors.New("connection refused")); got != ReasonUnknown {
		t.Errorf("ReasonFor(non-status) = %q, want unknown", got)
	}
	if got := ReasonFor(nil); got != ReasonUnknown {
		t.Errorf("ReasonFor(nil) = %q, want unknown", got)
	}
	if IsExhausted(apierrors.NewInternalError(errors.New("boom"))) {
		t.Error("a 500 reads as exhaustion")
	}
}

// A reason this build does not know must not be mistaken for one it does; a
// consumer pinned to an older IPAM sees it as unknown and falls back.
func TestUnknownReasonIsNotGuessed(t *testing.T) {
	future := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure,
		Code:   http.StatusBadRequest,
		Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{
			Type: metav1.CauseType("SomethingAddedLater"),
		}}},
	}}
	if got := ReasonFor(roundTrip(t, future)); got != ReasonUnknown {
		t.Errorf("ReasonFor(future reason) = %q, want unknown", got)
	}
}
