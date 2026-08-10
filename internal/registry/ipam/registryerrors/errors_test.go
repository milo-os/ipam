package registryerrors

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"go.miloapis.com/ipam/internal/allocator"
)

var testGR = schema.GroupResource{Group: "ipam.miloapis.com", Resource: "ippools"}

// statusError mirrors the interface the apiserver's ErrorToAPIStatus type-switches
// on. It is asserted here rather than imported so this test fails if our error
// stops satisfying the contract, whatever the vendored apiserver does.
type statusError interface {
	Status() metav1.Status
}

func TestMapWriteError_ReturnsUsableStatusError(t *testing.T) {
	// The wrapping the allocator actually produces, not a bare sentinel: the
	// mapping has to survive it.
	wrapped := fmt.Errorf("%w: %s %q", allocator.ErrObjectExists, "IPPool", "p1")

	got := MapWriteError(wrapped, testGR, "p1", "persist pool")

	if !apierrors.IsAlreadyExists(got) {
		t.Fatalf("expected AlreadyExists, got %#v", got)
	}

	// The apiserver renders a status by a direct type switch — it does not
	// unwrap. An error that is "AlreadyExists" by IsAlreadyExists but does not
	// satisfy this interface serialises as a 500, which is the whole defect.
	se, ok := got.(statusError)
	if !ok {
		t.Fatalf("mapped error does not satisfy the apiserver's statusError interface: %T", got)
	}
	if code := se.Status().Code; code != 409 {
		t.Errorf("status code = %d, want 409", code)
	}

	// The driver's SQLSTATE and constraint name must not reach the client.
	msg := got.Error()
	for _, leak := range []string{"SQLSTATE", "23505", "ipam_objects_pkey", "duplicate key"} {
		if strings.Contains(msg, leak) {
			t.Errorf("mapped error leaks driver internals (%q): %s", leak, msg)
		}
	}
	if !strings.Contains(msg, "p1") {
		t.Errorf("mapped error does not name the object: %s", msg)
	}
}

func TestMapWriteError_PassesThroughOtherErrors(t *testing.T) {
	underlying := errors.New("connection reset")

	got := MapWriteError(underlying, testGR, "p1", "persist pool")

	if apierrors.IsAlreadyExists(got) {
		t.Fatalf("unrelated error mapped to AlreadyExists: %v", got)
	}
	if !errors.Is(got, underlying) {
		t.Errorf("underlying error not preserved: %v", got)
	}
	if !strings.HasPrefix(got.Error(), "persist pool: ") {
		t.Errorf("context prefix missing: %v", got)
	}
}

func TestMapWriteError_NilStaysNil(t *testing.T) {
	if got := MapWriteError(nil, testGR, "p1", "persist pool"); got != nil {
		t.Errorf("MapWriteError(nil) = %v, want nil", got)
	}
}
