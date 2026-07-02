package ipclaim

import (
	"context"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/pkg/apis/ipam"
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

// TestCreate_DerivesFamilyWhenOmitted verifies that a claim submitted without
// spec.ipFamily is accepted and the server defaults the field from the family
// the allocator resolved (the pool's CIDR is authoritative). Consumers no
// longer have to specify a family the pool already determines.
func TestCreate_DerivesFamilyWhenOmitted(t *testing.T) {
	r, _, _ := newTestREST()

	claim := newClaim()
	claim.Spec.IPFamily = "" // omitted — server must derive it

	obj, err := r.Create(genericapirequest.WithNamespace(context.Background(), "default"),
		claim, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create with omitted ipFamily failed: %v", err)
	}
	got, ok := obj.(*ipam.IPClaim)
	if !ok {
		t.Fatalf("expected *ipam.IPClaim, got %T", obj)
	}
	if got.Spec.IPFamily == "" {
		t.Fatal("server did not default spec.ipFamily from the resolved pool")
	}
	// The fake allocator resolves IPv4 for an omitted family; the point is the
	// claim comes back with a concrete, persisted family.
	if got.Spec.IPFamily != ipam.IPv4 {
		t.Errorf("defaulted family = %q, want IPv4", got.Spec.IPFamily)
	}
	if got.Status.Phase != ipam.ClaimBound {
		t.Errorf("phase = %q, want Bound", got.Status.Phase)
	}
}
