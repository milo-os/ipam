// Package registryerrors provides apierror helpers shared across the IPAM
// registry storages. The standard k8s.io/apimachinery api/errors package
// does not ship a constructor for HTTP 507 (Insufficient Storage), so it is
// declared here to keep the resource-specific storage files terse.
package registryerrors

import (
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"go.miloapis.com/ipam/internal/allocator"
)

// MapWriteError translates an error from a hand-written object write into the
// error a REST verb should return, and is the required wrapper for every
// allocator.InsertObject call site in the registries.
//
// # Why it has to exist
//
// A registry verb that delegates to the embedded genericregistry.Store inherits
// the storage layer's unique-violation mapping, which is why creating a
// duplicate IPClass answers 409 AlreadyExists. The verbs that write objects
// themselves — IPPool.Create, and the two claim-transaction paths — inherit
// none of it, and nobody wrote the mapping by hand. A duplicate IPPool
// therefore answered HTTP 500 carrying `duplicate key value violates unique
// constraint "ipam_objects_pkey" (SQLSTATE 23505)`: a code that tells an
// idempotent setup script to retry something that will never succeed, and a
// message that describes our schema to a client who cannot act on it.
//
// # Why the wrapping matters
//
// The apiserver renders an error as a status by a direct type switch on the
// `Status() metav1.Status` interface — it does not unwrap. So a StatusError
// passed through fmt.Errorf("%w") silently degrades to a 500. Call sites must
// return what this returns, unwrapped; the guard test in this package's
// consumers asserts the call sites do.
//
// context is the operation prefix used for genuine failures ("persist pool"),
// matching what the call site would have written itself.
func MapWriteError(err error, gr schema.GroupResource, name, context string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, allocator.ErrObjectExists) {
		return apierrors.NewAlreadyExists(gr, name)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// NewInsufficientStorage returns a StatusError that serializes to HTTP 507
// with the supplied reason text. IPAM uses 507 to signal pool exhaustion on
// claim creation.
func NewInsufficientStorage(message string) *apierrors.StatusError {
	return &apierrors.StatusError{
		ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusInsufficientStorage,
			Reason:  metav1.StatusReason("InsufficientStorage"),
			Message: message,
		},
	}
}

// ExhaustionCause names the reason field IPAM sets on a 507's Status.Details.
// Clients read these keys; the values are the machine-readable half of an
// exhaustion report.
const (
	// CausePoolName is the pool that ran out. Under the class model nobody
	// names a pool on the way in, so nothing downstream can reconstruct which
	// one it was — a client asking the class would have to list every pool
	// offering it, which fans out and misses cascade-provisioned pools
	// entirely, since those carry classRef rather than classNames.
	CausePoolName = "poolName"
	// CauseClassName is the class the claim was made under.
	CauseClassName = "className"
	// CauseUtilizationPercent is the pool's allocated share, 0–100.
	CauseUtilizationPercent = "utilizationPercent"
	// CauseRequestedPrefix is the block size that could not be satisfied.
	CauseRequestedPrefix = "requestedPrefixLength"
)

// NewPoolExhausted returns the 507 a claim gets when its pool cannot satisfy it,
// carrying the facts a caller needs and cannot recover on its own.
//
// The details matter more than they look. Before the class model the caller
// named the pool, so an exhaustion message could assume the caller already knew
// which one it was. Now the allocator resolves the pool and the caller never
// sees it, so a bare "pool exhausted" leaves a client with no way to say what
// filled up — and every way of guessing is wrong for cascade-provisioned pools.
// All three facts are in hand at the point of failure and nowhere else.
func NewPoolExhausted(className, poolName string, requestedPrefix int32, utilizationPercent float64, message string) *apierrors.StatusError {
	causes := []metav1.StatusCause{
		{Type: metav1.CauseType(CauseClassName), Message: className, Field: CauseClassName},
		{Type: metav1.CauseType(CausePoolName), Message: poolName, Field: CausePoolName},
		{Type: metav1.CauseType(CauseRequestedPrefix), Message: fmt.Sprintf("%d", requestedPrefix), Field: CauseRequestedPrefix},
		{Type: metav1.CauseType(CauseUtilizationPercent), Message: fmt.Sprintf("%g", utilizationPercent), Field: CauseUtilizationPercent},
	}
	return &apierrors.StatusError{
		ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusInsufficientStorage,
			Reason:  metav1.StatusReason("InsufficientStorage"),
			Message: message,
			Details: &metav1.StatusDetails{
				Name:   poolName,
				Group:  "ipam.miloapis.com",
				Kind:   "IPPool",
				Causes: causes,
			},
		},
	}
}
