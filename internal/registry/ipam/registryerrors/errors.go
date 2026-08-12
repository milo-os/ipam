// Package registryerrors provides apierror helpers shared across the IPAM
// registry storages. The standard k8s.io/apimachinery api/errors package
// does not ship a constructor for HTTP 507 (Insufficient Storage), so it is
// declared here to keep the resource-specific storage files terse.
package registryerrors

import (
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// NewPoolExhausted returns the 507 for a pool that has no room left, naming the
// pool in Status.Details.
//
// A claim names a class, not a pool, so the caller cannot know which pool ran
// out. Without the name they get "IPPool exhausted" and no way to tell which of
// the class's pools to widen. The name goes in Details rather than only in the
// message so a client can read it without parsing prose.
func NewPoolExhausted(poolName string) *apierrors.StatusError {
	err := NewInsufficientStorage(fmt.Sprintf("IPPool %q is exhausted", poolName))
	err.ErrStatus.Details = &metav1.StatusDetails{
		Name:  poolName,
		Group: "ipam.miloapis.com",
		Kind:  "ippools",
	}
	return err
}
