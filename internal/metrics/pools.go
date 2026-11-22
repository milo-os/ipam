package metrics

import (
	"sync"
	"time"

	"k8s.io/component-base/metrics"
)

// The per-pool gauges are not set by the code that changes a pool. They are
// read back from what the database holds, by a sampler that replaces the whole
// set each time it runs (see StartPoolGaugeSampler in cmd/ipam).
//
// Setting them from the write path exported pools that never existed and kept
// pools that no longer do. A dry-run computes its allocation, and can place a
// whole class chain, inside a transaction it then rolls back, so a gauge set
// along the way describes nothing. A gauge vector also keeps a series until
// something deletes it, and a pool is deleted by several paths, on whichever
// replica served the request: the other replica would export the pool until
// it restarted. Reading committed state answers both, on every replica.
var poolLabels = []string{"pool_key", "ip_family", "resource", "project", "org"}

var (
	poolUtilizationDesc = metrics.NewDesc("ipam_pool_utilization_ratio",
		"Ratio of allocated to total capacity per pool",
		poolLabels, nil, metrics.ALPHA, "")
	poolCapacityDesc = metrics.NewDesc("ipam_pool_capacity_total",
		"Total addressable capacity of the pool (addresses for prefix pools, ASN count for ASN pools)",
		poolLabels, nil, metrics.ALPHA, "")
	poolAllocatedDesc = metrics.NewDesc("ipam_pool_allocated_total",
		"Currently allocated count for the pool (addresses for prefix pools, ASN count for ASN pools)",
		poolLabels, nil, metrics.ALPHA, "")
)

// PoolGauge is one pool's figures as committed to the database.
//
// Total and Allocated are float64 because IPv6 pool sizes overflow int64 (a
// /48 holds 2^80 addresses).
type PoolGauge struct {
	PoolKey   string
	IPFamily  string
	Resource  string // "ippools"
	Project   string
	Org       string
	Total     float64
	Allocated float64
}

// Utilization is Allocated/Total clamped to [0, 1]. A zero-capacity pool
// reports 0 rather than NaN: alerts and dashboards assume a finite ratio.
func (g PoolGauge) Utilization() float64 {
	if g.Total <= 0 {
		return 0
	}
	ratio := g.Allocated / g.Total
	switch {
	case ratio < 0:
		return 0
	case ratio > 1:
		return 1
	}
	return ratio
}

// poolGaugeCollector exports the last snapshot handed to ReplacePoolGauges.
// Swapping the whole slice means a scrape sees one snapshot or the next, never
// a set half cleared.
type poolGaugeCollector struct {
	metrics.BaseStableCollector

	mu    sync.RWMutex
	pools []PoolGauge
}

var poolGauges = &poolGaugeCollector{}

func (c *poolGaugeCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- poolUtilizationDesc
	ch <- poolCapacityDesc
	ch <- poolAllocatedDesc
}

func (c *poolGaugeCollector) CollectWithStability(ch chan<- metrics.Metric) {
	c.mu.RLock()
	pools := c.pools
	c.mu.RUnlock()
	for _, p := range pools {
		labels := []string{p.PoolKey, p.IPFamily, p.Resource, p.Project, p.Org}
		ch <- metrics.NewLazyConstMetric(poolCapacityDesc, metrics.GaugeValue, p.Total, labels...)
		ch <- metrics.NewLazyConstMetric(poolAllocatedDesc, metrics.GaugeValue, p.Allocated, labels...)
		ch <- metrics.NewLazyConstMetric(poolUtilizationDesc, metrics.GaugeValue, p.Utilization(), labels...)
	}
}

// PoolGaugesLastSuccess is the Unix time of the last successful refresh of the
// per-pool gauges. A failed refresh keeps the previous values, so without this
// the gauges, and every pool alert on them, would go quiet without saying so.
// IPAMPoolGaugesStale fires on its age, per replica.
var PoolGaugesLastSuccess = metrics.NewGauge(
	&metrics.GaugeOpts{
		Namespace:      "ipam",
		Name:           "pool_gauges_last_success_timestamp_seconds",
		Help:           "Unix timestamp of the last successful refresh of the per-pool gauges from the database.",
		StabilityLevel: metrics.ALPHA,
	},
)

// ReplacePoolGauges makes pools the complete set of pools exported, and
// stamps PoolGaugesLastSuccess with now. A pool missing from pools stops
// being exported. Call it only with the result of a successful read.
func ReplacePoolGauges(pools []PoolGauge, now time.Time) {
	snapshot := append([]PoolGauge(nil), pools...)
	poolGauges.mu.Lock()
	poolGauges.pools = snapshot
	poolGauges.mu.Unlock()
	PoolGaugesLastSuccess.Set(float64(now.Unix()))
}
