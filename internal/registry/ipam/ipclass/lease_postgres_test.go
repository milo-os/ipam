package ipclass

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/warning"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// warningRecorder collects the warnings a request would return to kubectl.
type warningRecorder struct{ warnings []string }

func (r *warningRecorder) AddWarning(_, text string) { r.warnings = append(r.warnings, text) }

func withLease(d time.Duration) func(*ipam.IPClass) {
	return func(c *ipam.IPClass) { c.Spec.RetentionLease = &metav1.Duration{Duration: d} }
}

// leaseWarnings picks out any warning about a lease. Leases are enforced, so a
// lease that is set needs no warning, and none should come back.
func leaseWarnings(ws []string) []string {
	var out []string
	for _, w := range ws {
		if strings.Contains(strings.ToLower(w), "lease") {
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

// A class that sets a lease is accepted without a warning, on create and on an
// update that changes it: the sweeper releases what a lease says it will.
func TestAClassRetentionLeaseIsAcceptedWithoutAWarning(t *testing.T) {
	s := newPostgresClassStorage(t, nil)
	rec := &warningRecorder{}
	ctx := warning.WithWarningRecorder(classCtx(callerProject), rec)

	obj, err := create(t, s, ctx, definition("leased", withLease(time.Hour)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := leaseWarnings(rec.warnings); len(got) != 0 {
		t.Errorf("create warned about the lease: %v", got)
	}

	rec.warnings = nil
	updated := obj.(*ipam.IPClass).DeepCopy()
	updated.Spec.RetentionLease = &metav1.Duration{Duration: 2 * time.Hour}
	if _, _, err := s.Update(ctx, "leased", rest.DefaultUpdatedObjectInfo(updated),
		nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := leaseWarnings(rec.warnings); len(got) != 0 {
		t.Errorf("update warned about the lease: %v", got)
	}

	rec.warnings = nil
	if _, err := create(t, s, ctx, definition("unleased")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := leaseWarnings(rec.warnings); len(got) != 0 {
		t.Errorf("a class with no lease was warned about one: %v", got)
	}
}

// A class stored with a lease outside today's bounds stays editable: an update
// that leaves the lease alone is not validated against it. Changing the lease
// is held to the bounds.
func TestAClassRetentionLeaseIsRatchetedOnUpdate(t *testing.T) {
	s, db := newPostgresClassStorageWithDB(t, nil)
	rec := &warningRecorder{}
	ctx := warning.WithWarningRecorder(classCtx(callerProject), rec)

	if _, err := create(t, s, ctx, definition("legacy", withLease(time.Hour))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// What a class written before the bounds existed looks like in storage.
	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_objects
		    SET data = convert_to(jsonb_set(ipam_data_to_jsonb(data), '{spec,retentionLease}', '"0s"')::text, 'UTF8')
		  WHERE key = $1`,
		tenant.Identity{Name: callerProject}.ResourceKey("ipclasses", "legacy")); err != nil {
		t.Fatalf("store a zero lease: %v", err)
	}

	get := func() *ipam.IPClass {
		t.Helper()
		obj, err := s.Get(ctx, "legacy", &metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return obj.(*ipam.IPClass).DeepCopy()
	}
	update := func(c *ipam.IPClass) error {
		t.Helper()
		_, _, err := s.Update(ctx, "legacy", rest.DefaultUpdatedObjectInfo(c),
			nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
		return err
	}

	rec.warnings = nil
	labelled := get()
	if labelled.Spec.RetentionLease == nil || labelled.Spec.RetentionLease.Duration != 0 {
		t.Fatalf("setup: stored lease = %v, want 0s", labelled.Spec.RetentionLease)
	}
	labelled.Labels = map[string]string{"team": "edge"}
	if err := update(labelled); err != nil {
		t.Fatalf("a label-only update of a class with a stored 0s lease was refused: %v", err)
	}
	if got := leaseWarnings(rec.warnings); len(got) != 0 {
		t.Errorf("a label-only update was warned about the lease: %v", got)
	}

	tooLong := get()
	tooLong.Spec.RetentionLease = &metav1.Duration{Duration: 8761 * time.Hour}
	if err := update(tooLong); !apierrors.IsInvalid(err) {
		t.Errorf("changing the lease to 8761h returned %v, want 422 Invalid", err)
	}

	rec.warnings = nil
	fixed := get()
	fixed.Spec.RetentionLease = &metav1.Duration{Duration: time.Hour}
	if err := update(fixed); err != nil {
		t.Fatalf("changing the lease to 1h: %v", err)
	}
	if got := leaseWarnings(rec.warnings); len(got) != 0 {
		t.Errorf("changing the lease warned about it: %v", got)
	}
}
