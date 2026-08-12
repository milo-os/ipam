package ipclaim

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// stubNamespaceChecker answers with a fixed state, or fails the lookup.
type stubNamespaceChecker struct {
	state access.NamespaceState
	err   error
}

func (c stubNamespaceChecker) State(context.Context, string, string) (access.NamespaceState, error) {
	return c.state, c.err
}

// A live namespace can collect what is bound into it, so the claim binds.
func TestCreateBindsAClaimInALiveNamespace(t *testing.T) {
	r, _ := newPostgresREST(t)
	r.nsChecker = stubNamespaceChecker{state: access.NamespaceLive}

	obj, err := r.Create(claimCtx(testProject), newClassClaim(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != "10.0.0.0/24" {
		t.Errorf("allocatedCIDR = %q, want the first /24 of the pool", got)
	}
}

// An address bound into a namespace with no collector is never released, so the
// claim is refused, and nothing is written or reserved on the way out.
func TestCreateRefusesANamespaceThatCannotCollectTheClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state access.NamespaceState
		want  string
	}{
		{"missing", access.NamespaceMissing, "does not exist"},
		{"terminating", access.NamespaceTerminating, "being terminated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db := newPostgresREST(t)
			r.nsChecker = stubNamespaceChecker{state: tc.state}

			_, err := r.Create(claimCtx(testProject), newClassClaim(), nil, &metav1.CreateOptions{})
			if err == nil {
				t.Fatal("Create succeeded")
			}
			if !apierrors.IsForbidden(err) {
				t.Errorf("error = %v, want Forbidden", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}

			if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`); n != 0 {
				t.Errorf("a refused claim reserved %d allocations, want 0", n)
			}
			if n := countRows(t, db,
				`SELECT count(*) FROM ipam_objects WHERE kind IN ('IPClaim','IPAllocation')`); n != 0 {
				t.Errorf("a refused claim wrote %d objects, want 0", n)
			}
		})
	}
}

// An undetermined namespace is not a missing one. A control plane IPAM cannot
// reach must not stop it handing out addresses.
func TestCreateAdmitsWhenTheNamespaceLookupFails(t *testing.T) {
	r, _ := newPostgresREST(t)
	r.nsChecker = stubNamespaceChecker{
		state: access.NamespaceUnknown,
		err:   errors.New("control plane unreachable"),
	}

	obj, err := r.Create(claimCtx(testProject), newClassClaim(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create refused a claim because the lookup failed: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != "10.0.0.0/24" {
		t.Errorf("allocatedCIDR = %q, want the first /24 of the pool", got)
	}
}
