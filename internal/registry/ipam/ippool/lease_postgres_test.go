package ippool

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/warning"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

type warningRecorder struct{ warnings []string }

func (r *warningRecorder) AddWarning(_, text string) { r.warnings = append(r.warnings, text) }

// isLeaseWarning reports a warning about a lease. Leases are enforced, so a
// cap that is set needs no warning, and none should come back.
func isLeaseWarning(w string) bool { return strings.Contains(strings.ToLower(w), "lease") }

func withMaxLease(p *ipam.IPPool, d time.Duration) *ipam.IPPool {
	p.Spec.MaxRetentionLease = &metav1.Duration{Duration: d}
	return p
}

// The pool's cap is held to the same bounds as a class's lease, and one within
// them is accepted without a warning.
func TestAPoolMaxRetentionLeaseIsBounded(t *testing.T) {
	s := newPostgresPoolStorage(t, testdb.Pool(t))
	rec := &warningRecorder{}
	ctx := warning.WithWarningRecorder(poolCtx("tenant-a"), rec)

	for _, tc := range []struct {
		name  string
		cidr  string
		lease time.Duration
	}{
		{"cap-zero", "10.61.0.0/24", 0},
		{"cap-negative", "10.61.1.0/24", -time.Hour},
		{"cap-too-long", "10.61.2.0/24", 8761 * time.Hour},
	} {
		err := createPool(t, s, ctx, withMaxLease(rootPoolObj(tc.name, tc.cidr, ipam.IPv4), tc.lease))
		if !apierrors.IsInvalid(err) {
			t.Errorf("%s: Create returned %v, want 422 Invalid", tc.name, err)
		}
	}

	rec.warnings = nil
	if err := createPool(t, s, ctx, withMaxLease(rootPoolObj("capped", "10.61.3.0/24", ipam.IPv4), 24*time.Hour)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, w := range rec.warnings {
		if isLeaseWarning(w) {
			t.Errorf("create warned about the cap: %q", w)
		}
	}
}

// The pool's cap ratchets as a class's lease does: a stored out-of-bounds cap
// does not block an update that leaves it alone. Changing the cap is held to
// the bounds.
func TestAPoolMaxRetentionLeaseIsRatchetedOnUpdate(t *testing.T) {
	db := testdb.Pool(t)
	s := newPostgresPoolStorage(t, db)
	rec := &warningRecorder{}
	ctx := warning.WithWarningRecorder(poolCtx("tenant-a"), rec)

	if err := createPool(t, s, ctx, withMaxLease(rootPoolObj("legacy", "10.61.4.0/24", ipam.IPv4), time.Hour)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_objects
		    SET data = convert_to(jsonb_set(ipam_data_to_jsonb(data), '{spec,maxRetentionLease}', '"-1h0m0s"')::text, 'UTF8')
		  WHERE key = $1`, poolStorageKey("tenant-a", "legacy")); err != nil {
		t.Fatalf("store a negative cap: %v", err)
	}

	get := func() *ipam.IPPool {
		t.Helper()
		obj, err := s.Get(ctx, "legacy", &metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return obj.(*ipam.IPPool).DeepCopy()
	}
	update := func(p *ipam.IPPool) error {
		t.Helper()
		_, _, err := s.Update(ctx, "legacy", rest.DefaultUpdatedObjectInfo(p),
			nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
		return err
	}

	rec.warnings = nil
	labelled := get()
	labelled.Labels = map[string]string{"team": "edge"}
	if err := update(labelled); err != nil {
		t.Fatalf("a label-only update of a pool with a stored negative cap was refused: %v", err)
	}
	for _, w := range rec.warnings {
		if isLeaseWarning(w) {
			t.Errorf("a label-only update was warned about the cap: %q", w)
		}
	}

	zero := get()
	zero.Spec.MaxRetentionLease = &metav1.Duration{}
	if err := update(zero); !apierrors.IsInvalid(err) {
		t.Errorf("changing the cap to 0s returned %v, want 422 Invalid", err)
	}

	rec.warnings = nil
	fixed := get()
	fixed.Spec.MaxRetentionLease = &metav1.Duration{Duration: 24 * time.Hour}
	if err := update(fixed); err != nil {
		t.Fatalf("changing the cap to 24h: %v", err)
	}
	for _, w := range rec.warnings {
		if isLeaseWarning(w) {
			t.Errorf("changing the cap warned about it: %q", w)
		}
	}
}
