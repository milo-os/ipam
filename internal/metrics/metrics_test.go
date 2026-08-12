package metrics

import (
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Labels that must never reach a metric. A series keeps its label slot for the
// process lifetime, so an unbounded label is a memory leak that surfaces only
// under the load it was added to explain. Each of these takes a new value per
// pool, per scope, or per claim.
var unboundedLabels = []string{
	"pool_key", "pool_name", "scope_digest", "scope",
	"cidr", "allocated_cidr", "allocation_key", "claim", "claim_key",
}

// The cascade metrics describe work done per claim, so an unbounded label on
// one of them multiplies by request volume.
func TestCascadeMetricsCarryOnlyBoundedLabels(t *testing.T) {
	RecordCascadeLevel("example", levelReused)
	ObserveCascadeResolution("success", false, time.Now())

	want := map[string][]string{
		"ipam_cascade_levels_total":                {"class", "outcome"},
		"ipam_cascade_resolution_duration_seconds": {"provisioned", "result"},
	}

	seen := map[string]bool{}
	for name, labels := range gatherLabelNames(t) {
		if !strings.HasPrefix(name, "ipam_cascade_") {
			continue
		}
		expected, ok := want[name]
		if !ok {
			t.Errorf("unreviewed cascade metric %q with labels %v", name, labels)
			continue
		}
		seen[name] = true
		if !equal(labels, expected) {
			t.Errorf("%s labels = %v, want %v", name, labels, expected)
		}
		for _, label := range labels {
			for _, banned := range unboundedLabels {
				if label == banned {
					t.Errorf("%s carries unbounded label %q", name, label)
				}
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("%s was not registered", name)
		}
	}
}

// A lost race is an outcome on the same counter as a successful provision, not
// a separate metric and not an error. Recorded anywhere else, a normal
// first-claim herd would read as a fault, and the steady state, where every
// level is reused, would have no series at all.
func TestLostRacesAndReusesShareTheProvisioningCounter(t *testing.T) {
	const class = "TestLostRacesAndReusesShareTheProvisioningCounter"

	for _, outcome := range []string{levelReused, levelProvisioned, levelLost, levelError} {
		RecordCascadeLevel(class, outcome)
	}

	got := map[string]bool{}
	for _, mf := range gather(t) {
		if mf.GetName() != "ipam_cascade_levels_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["class"] == class {
				got[labels["outcome"]] = true
			}
		}
	}

	for _, outcome := range []string{levelReused, levelProvisioned, levelLost, levelError} {
		if !got[outcome] {
			t.Errorf("outcome %q produced no series on ipam_cascade_levels_total", outcome)
		}
	}
}

// The provisioned label must stay a two-valued flag; anything derived from the
// chain itself would be unbounded.
func TestResolutionDurationProvisionedLabelIsABoolean(t *testing.T) {
	ObserveCascadeResolution("success", true, time.Now())
	ObserveCascadeResolution("success", false, time.Now())

	for _, mf := range gather(t) {
		if mf.GetName() != "ipam_cascade_resolution_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() != "provisioned" {
					continue
				}
				if v := lp.GetValue(); v != "true" && v != "false" {
					t.Errorf("provisioned = %q, want true or false", v)
				}
			}
		}
	}
}

// Outcome constants mirror internal/allocator's; duplicated rather than
// imported because allocator imports this package.
const (
	levelReused      = "reused"
	levelProvisioned = "provisioned"
	levelLost        = "lost"
	levelError       = "error"
)

func gather(t *testing.T) []*dto.MetricFamily {
	t.Helper()
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return families
}

func gatherLabelNames(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, mf := range gather(t) {
		for _, m := range mf.GetMetric() {
			names := make([]string, 0, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				names = append(names, lp.GetName())
			}
			sort.Strings(names)
			out[mf.GetName()] = names
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
