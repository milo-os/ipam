package metrics

import (
	"context"
	"testing"
	"time"
)

// The gauges export exactly the last set handed over: a pool left out of it
// is no longer exported, and nothing else sets a pool series.
func TestReplacePoolGaugesDropsPoolsNoLongerPresent(t *testing.T) {
	t.Cleanup(func() { ReplacePoolGauges(nil) })

	ReplacePoolGauges([]PoolGauge{
		{PoolKey: "projects/p/ippools/a", IPFamily: "IPv4", Resource: "ippools", Project: "p", Total: 256, Allocated: 64},
		{PoolKey: "projects/p/ippools/b", IPFamily: "IPv4", Resource: "ippools", Project: "p", Total: 256, Allocated: 256},
	})
	got := exportedPools(t)
	if got["projects/p/ippools/a"] != 0.25 || got["projects/p/ippools/b"] != 1 {
		t.Fatalf("utilization = %v, want a=0.25 b=1", got)
	}

	ReplacePoolGauges([]PoolGauge{
		{PoolKey: "projects/p/ippools/a", IPFamily: "IPv4", Resource: "ippools", Project: "p", Total: 256, Allocated: 128},
	})
	got = exportedPools(t)
	if _, ok := got["projects/p/ippools/b"]; ok {
		t.Errorf("deleted pool b is still exported: %v", got)
	}
	if got["projects/p/ippools/a"] != 0.5 {
		t.Errorf("pool a utilization = %v, want 0.5", got["projects/p/ippools/a"])
	}

	for _, name := range []string{"ipam_pool_capacity_total", "ipam_pool_allocated_total"} {
		for _, mf := range gather(t) {
			if mf.GetName() == name && len(mf.GetMetric()) != 1 {
				t.Errorf("%s exports %d series, want 1", name, len(mf.GetMetric()))
			}
		}
	}
}

func TestPoolUtilizationStaysInRange(t *testing.T) {
	for _, tc := range []struct {
		g    PoolGauge
		want float64
	}{
		{PoolGauge{Total: 0, Allocated: 0}, 0},
		{PoolGauge{Total: 10, Allocated: 20}, 1},
		{PoolGauge{Total: 10, Allocated: -1}, 0},
		{PoolGauge{Total: 4, Allocated: 1}, 0.25},
	} {
		if got := tc.g.Utilization(); got != tc.want {
			t.Errorf("%+v utilization = %v, want %v", tc.g, got, tc.want)
		}
	}
}

// exportedPools maps pool_key to ipam_pool_utilization_ratio.
func exportedPools(t *testing.T) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, mf := range gather(t) {
		if mf.GetName() != "ipam_pool_utilization_ratio" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "pool_key" {
					out[lp.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}

// A dry-run's cascade work is rolled back, so it records nothing.
func TestCascadeMetricsSkipADryRun(t *testing.T) {
	const class = "TestCascadeMetricsSkipADryRun"
	ctx := WithDryRun(context.Background())
	RecordCascadeLevel(ctx, class, levelProvisioned)
	ObserveCascadeResolution(ctx, "success", true, time.Now())

	for _, mf := range gather(t) {
		if mf.GetName() != "ipam_cascade_levels_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "class" && lp.GetValue() == class {
					t.Errorf("dry-run recorded a cascade level for %s", class)
				}
			}
		}
	}
}
