package ipclaim

import (
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/allocator"
)

// TestCreate_FamilyMismatchIsBadRequest verifies that a claim whose address
// family disagrees with the pool's family surfaces as HTTP 400 (Bad Request) —
// not 500, and not a misleading 507 "pool exhausted". The allocator derives the
// pool's family from its CIDR and returns ErrFamilyMismatch; the registry must
// map that to a client error explaining the mismatch.
func TestCreate_FamilyMismatchIsBadRequest(t *testing.T) {
	mismatch := fmt.Errorf("claim address family %q does not match pool %q family %q: %w",
		"IPv4", "region-v6", "IPv6", allocator.ErrFamilyMismatch)
	r, _ := newTracingREST(mismatch, nil)

	_, err := r.Create(projectContext("proj-a"), newClaim(), nil, &metav1.CreateOptions{})
	if err == nil {
		t.Fatal("Create succeeded, want a family-mismatch error")
	}
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest (400), got %#v", err)
	}
	if !strings.Contains(err.Error(), "does not match pool") {
		t.Errorf("error should explain the family mismatch, got: %v", err)
	}
}
