// Package registryerrors provides apierror helpers shared across the IPAM
// registry storages. The standard k8s.io/apimachinery api/errors package
// does not ship a constructor for HTTP 507 (Insufficient Storage), so it is
// declared here to keep the resource-specific storage files terse.
package registryerrors

import (
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
