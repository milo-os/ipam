package ipallocation

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/registry/rest"
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

// An allocation's status is written only by the server, in the transactions
// that change the row it mirrors, so the subresource offers no write: a
// client-set expiresAt would show a deadline the sweeper does not act on.
func TestTheStatusSubresourceIsReadOnly(t *testing.T) {
	var status rest.Storage = &IPAllocationStatusStorage{}
	if _, ok := status.(rest.Updater); ok {
		t.Error("ipallocations/status accepts updates")
	}
	if _, ok := status.(rest.Patcher); ok {
		t.Error("ipallocations/status accepts patches")
	}
	if _, ok := status.(rest.Getter); !ok {
		t.Error("ipallocations/status cannot be read")
	}
}
