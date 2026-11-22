package ipclaim

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/component-base/metrics/legacyregistry"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// failuresByReason sums ipam_allocation_failures_total per reason.
func failuresByReason(t *testing.T) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, m := range series(t, "ipam_allocation_failures_total") {
		out[label(m, "reason")] += m.GetCounter().GetValue()
	}
	return out
}

// cascadeSamples is the total count recorded across both cascade metrics.
func cascadeSamples(t *testing.T) float64 {
	t.Helper()
	var n float64
	for _, m := range series(t, "ipam_cascade_levels_total") {
		n += m.GetCounter().GetValue()
	}
	for _, m := range series(t, "ipam_cascade_resolution_duration_seconds") {
		n += float64(m.GetHistogram().GetSampleCount())
	}
	return n
}

// poolSeries returns the pool keys each per-pool gauge exports.
func poolSeries(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, name := range []string{"ipam_pool_allocated_total", "ipam_pool_capacity_total", "ipam_pool_utilization_ratio"} {
		for _, m := range series(t, name) {
			out[name] = append(out[name], label(m, "pool_key"))
		}
	}
	return out
}

func series(t *testing.T, name string) []*dto.Metric {
	t.Helper()
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == name {
			return mf.GetMetric()
		}
	}
	return nil
}

func label(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// refreshPoolGauges does what the sampler in cmd/ipam does on each tick.
func refreshPoolGauges(t *testing.T, db allocator.Querier) {
	t.Helper()
	gauges, err := allocator.LoadPoolGauges(context.Background(), db)
	if err != nil {
		t.Fatalf("load pool gauges: %v", err)
	}
	metrics.ReplacePoolGauges(gauges, time.Now())
}

// A refusal is labelled by what the caller is told. A malformed spec.address
// is refused by validation with a 422, and was counted as internal: the label
// an alert on server faults pages on.
func TestCreateRefusalsAreLabelledByWhatTheCallerIsTold(t *testing.T) {
	r, _ := newPostgresREST(t)
	ctx := claimCtx(testProject)

	// The name is taken by the time the conflict case runs.
	if _, err := r.Create(ctx, newClassClaim(), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	withSpec := func(name string, edit func(*ipam.IPClaimSpec)) *ipam.IPClaim {
		c := newClassClaim()
		c.Name = name
		edit(&c.Spec)
		return c
	}
	denyAll := func(err error) runtimeValidate {
		return func(context.Context, runtime.Object) error { return err }
	}

	tests := []struct {
		name       string
		claim      *ipam.IPClaim
		validate   runtimeValidate
		wantReason string
	}{
		{"malformed address (422)", withSpec("bad-address", func(s *ipam.IPClaimSpec) { s.Address = "not-an-ip" }), nil, reasonInvalid},
		{"address of the other family", withSpec("v6-address", func(s *ipam.IPClaimSpec) { s.Address = "fd00::1" }), nil, reasonInvalid},
		{"unaligned address", withSpec("unaligned", func(s *ipam.IPClaimSpec) { s.Address = "10.0.0.16" }), nil, reasonInvalid},
		{"address outside every pool", withSpec("outside", func(s *ipam.IPClaimSpec) { s.Address = "192.168.0.0/24" }), nil, reasonInvalid},
		{"address in use", withSpec("in-use", func(s *ipam.IPClaimSpec) { s.Address = "10.0.0.0/24" }), nil, reasonConflict},
		{"class that does not exist", withSpec("no-class", func(s *ipam.IPClaimSpec) { s.ClassName = "missing" }), nil, reasonClassNotFound},
		{"name taken", newClassClaim(), nil, reasonConflict},
		{"admission forbids it (quota)", withSpec("quota", func(*ipam.IPClaimSpec) {}),
			denyAll(apierrors.NewForbidden(v1alpha1.Resource("ipclaims"), "quota", errors.New("quota exceeded"))), reasonForbidden},
		{"admission finds it invalid", withSpec("policy", func(*ipam.IPClaimSpec) {}),
			denyAll(apierrors.NewBadRequest("denied by policy")), reasonInvalid},
		{"admission fails", withSpec("webhook-down", func(*ipam.IPClaimSpec) {}),
			denyAll(errors.New("webhook unreachable")), reasonInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := failuresByReason(t)
			if _, err := r.Create(ctx, tc.claim, tc.validate, &metav1.CreateOptions{}); err == nil {
				t.Fatal("Create succeeded")
			}
			after := failuresByReason(t)
			for reason := range unionKeys(before, after) {
				delta := after[reason] - before[reason]
				want := 0.0
				if reason == tc.wantReason {
					want = 1
				}
				if delta != want {
					t.Errorf("reason %q moved by %v, want %v", reason, delta, want)
				}
			}
			if after[tc.wantReason]-before[tc.wantReason] != 1 {
				t.Errorf("reason %q was not recorded", tc.wantReason)
			}
		})
	}
}

type runtimeValidate = func(context.Context, runtime.Object) error

func unionKeys(a, b map[string]float64) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// The ScopeRange address refusals, each labelled as a refusal.
func TestScopeRangeAddressRefusalsAreNotServerFaults(t *testing.T) {
	rest, _ := newTenantREST(t)
	vpc := rangeClaim("vpc-a", "net-a")
	vpc.Spec.Address = "fd20:1000:a::/48"
	create(t, rest, vpc)

	withAddress := func(c *ipam.IPClaim, address string) *ipam.IPClaim {
		c.Spec.Address = address
		return c
	}
	unplaced := endpointClaim("ep-g", "net-g", "loc-1")
	unplaced.Spec.Address = "fd20:1000:9:10:0:5::/96"

	tests := []struct {
		name  string
		claim *ipam.IPClaim
		want  string
	}{
		{"AddressOutsidePool", subnetRangeClaim("s-out", "net-a", "loc-2", "fd20:1000:b:10::/64"), reasonInvalid},
		{"AddressInUse", withAddress(rangeClaim("vpc-c", "net-c"), "fd20:1000:a::/48"), reasonConflict},
		{"AddressUnaligned", withAddress(rangeClaim("vpc-u", "net-u"), "fd20:1000:e:1::/48"), reasonInvalid},
		{"PrefixLengthRejected", withAddress(rangeClaim("vpc-l", "net-l"), "fd20:1000:e::/56"), reasonInvalid},
		{"FamilyMismatch", withAddress(rangeClaim("vpc-f", "net-f"), "10.0.0.0/16"), reasonInvalid},
		{"RangeUnplaced", unplaced, reasonInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := failuresByReason(t)
			if _, err := rest.Create(claimCtx(testProject), tc.claim, nil, &metav1.CreateOptions{}); err == nil {
				t.Fatal("Create succeeded")
			}
			after := failuresByReason(t)
			if d := after[tc.want] - before[tc.want]; d != 1 {
				t.Errorf("reason %q moved by %v, want 1", tc.want, d)
			}
			for _, fault := range []string{reasonInternal, reasonTxError} {
				if d := after[fault] - before[fault]; d != 0 {
					t.Errorf("refusal counted %v times as %q", d, fault)
				}
			}
		})
	}
}

// A dry-run places pools and allocates inside a transaction it rolls back.
// None of it may reach a metric that describes what changed: no pool series
// for a pool that was never created, no allocation counted against a pool that
// kept its capacity, and no cascade provisioning that did not happen.
func TestADryRunRecordsNoPoolSeriesAndNoCascadeMetrics(t *testing.T) {
	rest, db := newTenantREST(t)
	metrics.ReplacePoolGauges(nil, time.Now())
	dry := &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}

	// The seeded root carries no computed capacity; give it the figures a
	// created pool has, so the read-back below has something to export.
	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_objects
		    SET data = convert_to(jsonb_set(ipam_data_to_jsonb(data), '{status,capacity}',
		               '{"total":"1267650600228229401496703205376","allocated":"0","available":"1267650600228229401496703205376"}')::text, 'UTF8')
		  WHERE kind = 'IPPool' AND name = 'tenant-root'`); err != nil {
		t.Fatalf("seed root capacity: %v", err)
	}

	cascadeBefore := cascadeSamples(t)
	for _, c := range []*ipam.IPClaim{
		// Block: provisions the whole chain under the endpoint class.
		endpointClaim("dry-endpoint", "net-d", "loc-1"),
		// Addressed ScopeRange: places the chain at the address.
		subnetRangeClaim("dry-addressed", "net-e", "loc-1", "fd20:1000:f:10::/64"),
		// Unaddressed ScopeRange.
		rangeClaim("dry-range", "net-f"),
	} {
		if _, err := rest.Create(claimCtx(testProject), c, nil, dry); err != nil {
			t.Fatalf("dry-run %s: %v", c.Name, err)
		}
		if got := poolSeries(t); len(got) != 0 {
			t.Errorf("after dry-run %s, pool gauges export %v, want none", c.Name, got)
		}
	}
	if d := cascadeSamples(t) - cascadeBefore; d != 0 {
		t.Errorf("dry-runs recorded %v cascade samples, want 0", d)
	}

	// Read back, the gauges hold only the pool that exists, at the occupancy
	// it still has.
	refreshPoolGauges(t, db)
	got := poolSeries(t)["ipam_pool_allocated_total"]
	if len(got) != 1 || !strings.HasSuffix(got[0], "/tenant-root") {
		t.Errorf("after the dry-runs the gauges export %v, want only tenant-root", got)
	}
	for _, m := range series(t, "ipam_pool_allocated_total") {
		if v := m.GetGauge().GetValue(); v != 0 {
			t.Errorf("pool %s reports %v allocated after dry-runs only, want 0", label(m, "pool_key"), v)
		}
	}

	// The same claims for real do record: the suppression is the dry-run's.
	create(t, rest, endpointClaim("real-endpoint", "net-d", "loc-1"))
	if d := cascadeSamples(t) - cascadeBefore; d == 0 {
		t.Error("a real create recorded no cascade samples")
	}
}

// A deleted pool stops being exported. Releasing a ScopeRange claim deletes the
// pool it held, on whichever replica served the delete; reading the gauges
// back from the database is what drops its series everywhere.
func TestAReleasedRangesPoolStopsBeingExported(t *testing.T) {
	rest, db := newTenantREST(t)
	ctx := claimCtx(testProject)

	held := create(t, rest, rangeClaim("vpc-a", "net-a"))
	refreshPoolGauges(t, db)
	if !exportsPool(t, held.Status.PoolRef.Name) {
		t.Fatalf("pool %s is not exported while it exists: %v", held.Status.PoolRef.Name, poolSeries(t))
	}

	if _, _, err := rest.Delete(ctx, "vpc-a", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete range claim: %v", err)
	}
	refreshPoolGauges(t, db)
	if exportsPool(t, held.Status.PoolRef.Name) {
		t.Errorf("pool %s is still exported after its release: %v", held.Status.PoolRef.Name, poolSeries(t))
	}
	if !exportsPool(t, "tenant-root") {
		t.Errorf("the pool that still exists stopped being exported: %v", poolSeries(t))
	}
}

// exportsPool reports whether any per-pool gauge carries a series for the
// pool named name.
func exportsPool(t *testing.T, name string) bool {
	t.Helper()
	for _, keys := range poolSeries(t) {
		for _, key := range keys {
			if strings.HasSuffix(key, "/"+name) {
				return true
			}
		}
	}
	return false
}
