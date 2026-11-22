package ipallocation

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestBoundConflictNamesTheClaimOrSaysThereIsNone(t *testing.T) {
	err := boundConflict("alloc-1", "web")
	if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), `IPClaim "web"`) {
		t.Errorf("bound to a claim: %v", err)
	}
	err = boundConflict("alloc-1", "")
	if !apierrors.IsConflict(err) {
		t.Errorf("held by no claim: %v, want 409", err)
	}
	if strings.Contains(err.Error(), "IPClaim") {
		t.Errorf("refusal %q names a claim that does not exist", err)
	}
}
