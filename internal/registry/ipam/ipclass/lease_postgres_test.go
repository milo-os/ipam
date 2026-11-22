package ipclass

import (
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/warning"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// warningRecorder collects the warnings a request would return to kubectl.
type warningRecorder struct{ warnings []string }

func (r *warningRecorder) AddWarning(_, text string) { r.warnings = append(r.warnings, text) }

func withLease(d time.Duration) func(*ipam.IPClass) {
	return func(c *ipam.IPClass) { c.Spec.RetentionLease = &metav1.Duration{Duration: d} }
}

func leaseWarnings(ws []string) []string {
	var out []string
	for _, w := range ws {
		if strings.Contains(w, "retentionLease") && strings.Contains(w, "issues/107") {
			out = append(out, w)
		}
	}
	return out
}

// A lease outside (0, 8760h] is refused through the real create path, not only
// by the strategy in isolation.
func TestAClassRetentionLeaseIsBounded(t *testing.T) {
	s := newPostgresClassStorage(t, nil)
	ctx := classCtx(callerProject)
	for _, tc := range []struct {
		name  string
		lease time.Duration
	}{
		{"lease-zero", 0},
		{"lease-negative", -time.Minute},
		{"lease-too-long", 8761 * time.Hour},
	} {
		_, err := create(t, s, ctx, definition(tc.name, withLease(tc.lease)))
		if !apierrors.IsInvalid(err) {
			t.Errorf("%s: Create returned %v, want 422 Invalid", tc.name, err)
		}
	}
}

// A class that sets a lease is accepted, and the response says the lease does
// nothing yet, on create and on every update that still sets one.
func TestAClassRetentionLeaseWarnsThatItIsNotEnforced(t *testing.T) {
	s := newPostgresClassStorage(t, nil)
	rec := &warningRecorder{}
	ctx := warning.WithWarningRecorder(classCtx(callerProject), rec)

	obj, err := create(t, s, ctx, definition("leased", withLease(time.Hour)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := leaseWarnings(rec.warnings); len(got) != 1 {
		t.Fatalf("create warnings = %v, want one about the unenforced lease", rec.warnings)
	}

	rec.warnings = nil
	updated := obj.(*ipam.IPClass).DeepCopy()
	updated.Spec.RetentionLease = &metav1.Duration{Duration: 2 * time.Hour}
	if _, _, err := s.Update(ctx, "leased", rest.DefaultUpdatedObjectInfo(updated),
		nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := leaseWarnings(rec.warnings); len(got) != 1 {
		t.Errorf("update warnings = %v, want one about the unenforced lease", rec.warnings)
	}

	rec.warnings = nil
	if _, err := create(t, s, ctx, definition("unleased")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := leaseWarnings(rec.warnings); len(got) != 0 {
		t.Errorf("a class with no lease was warned about one: %v", got)
	}
}

