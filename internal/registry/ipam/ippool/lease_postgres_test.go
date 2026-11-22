package ippool

import (
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/warning"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

type warningRecorder struct{ warnings []string }

func (r *warningRecorder) AddWarning(_, text string) { r.warnings = append(r.warnings, text) }

func withMaxLease(p *ipam.IPPool, d time.Duration) *ipam.IPPool {
	p.Spec.MaxRetentionLease = &metav1.Duration{Duration: d}
	return p
}

// The pool's cap is held to the same bounds as a class's lease, and setting
// one is answered with the same warning that it is not enforced yet.
func TestAPoolMaxRetentionLeaseIsBoundedAndWarned(t *testing.T) {
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
	var found bool
	for _, w := range rec.warnings {
		if strings.Contains(w, "spec.maxRetentionLease") && strings.Contains(w, "issues/107") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one saying the cap is not enforced", rec.warnings)
	}
}
