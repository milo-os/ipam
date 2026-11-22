package metrics

import (
	"k8s.io/component-base/metrics"
)

// Retention-lease series. None carries a pool_key: the retained set is a
// platform-wide quantity, and a per-pool label would grow with every pool.

// RetainedAllocations is the retained set by family and by what is known of
// its lease, read from the database once per sweep pass on every replica.
//
// lease:
//
//	finite       a deadline is fixed (status.expiresAt).
//	none         nothing will release it on a schedule: neither its class
//	             nor its pool sets a lease, or it was retained before
//	             retention was timestamped. This is the leak meter.
//	unevaluated  the lease could not be resolved yet; the sweeper resolves
//	             it. Anything but a transient non-zero value means it cannot.
var RetainedAllocations = metrics.NewGaugeVec(
	&metrics.GaugeOpts{
		Namespace:      "ipam",
		Name:           "retained_allocations",
		Help:           "Claim allocations retained with no claim bound, by address family and lease state",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"ip_family", "lease"},
)

// RetainedOldestAge is how long the longest-retained allocation of each
// family has been held without a claim. Zero when none is retained.
var RetainedOldestAge = metrics.NewGaugeVec(
	&metrics.GaugeOpts{
		Namespace:      "ipam",
		Name:           "retained_oldest_age_seconds",
		Help:           "Age of the oldest retained claim allocation, by address family",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"ip_family"},
)

// RetentionReleases counts retained allocations released because their lease
// ran out.
//
// mode:
//
//	enforce  released: the row and the IPAllocation are gone.
//	observe  would have been released. Counted once per deadline as it
//	         passes, not once per pass, so its rate is what enforce's will be.
var RetentionReleases = metrics.NewCounterVec(
	&metrics.CounterOpts{
		Namespace:      "ipam",
		Name:           "retention_releases_total",
		Help:           "Retained allocations released, or in observe mode due for release, because their lease ran out",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"mode"},
)

// RetentionSweepDuration is the duration of sweep passes this replica ran.
// A pass another replica held the lock for is not observed.
var RetentionSweepDuration = metrics.NewHistogram(
	&metrics.HistogramOpts{
		Namespace:      "ipam",
		Name:           "retention_sweep_duration_seconds",
		Help:           "Duration of retention sweep passes",
		Buckets:        []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		StabilityLevel: metrics.ALPHA,
	},
)

// RetentionSweepErrors counts failures inside sweep passes: a query, a pool's
// transaction, or a lease that could not be evaluated. A pass with any error
// does not advance RetentionSweepLastSuccess.
var RetentionSweepErrors = metrics.NewCounter(
	&metrics.CounterOpts{
		Namespace:      "ipam",
		Name:           "retention_sweep_errors_total",
		Help:           "Failures during retention sweep passes",
		StabilityLevel: metrics.ALPHA,
	},
)

// RetentionSweepPoolsSkipped counts pools a pass left for the next one because
// a live claim held the pool's lock. The sweeper yields rather than queueing
// claims behind itself; a sustained rate means a pool is never free.
var RetentionSweepPoolsSkipped = metrics.NewCounter(
	&metrics.CounterOpts{
		Namespace:      "ipam",
		Name:           "retention_sweep_pools_skipped_total",
		Help:           "Pools a retention sweep pass skipped because their lock was held",
		StabilityLevel: metrics.ALPHA,
	},
)

// RetentionSweepLastSuccess is the Unix time this replica last completed a
// pass with no error. Only the replica holding the advisory lock runs a pass,
// so alert on the max across replicas.
var RetentionSweepLastSuccess = metrics.NewGauge(
	&metrics.GaugeOpts{
		Namespace:      "ipam",
		Name:           "retention_sweep_last_success_timestamp_seconds",
		Help:           "Unix timestamp of the last retention sweep pass this replica completed without error",
		StabilityLevel: metrics.ALPHA,
	},
)

// RetentionSweepInterval is the configured interval between passes; zero when
// the sweeper is disabled. The stall alert scales by it, so it stays right
// whatever interval a deployment chooses.
var RetentionSweepInterval = metrics.NewGauge(
	&metrics.GaugeOpts{
		Namespace:      "ipam",
		Name:           "retention_sweep_interval_seconds",
		Help:           "Configured interval between retention sweep passes; 0 when the sweeper is disabled",
		StabilityLevel: metrics.ALPHA,
	},
)

// RetainedFamily is one address family's share of the retained set.
type RetainedFamily struct {
	IPFamily                  string
	Finite, None, Unevaluated int
	OldestAgeSeconds          float64
}

// retainedFamilies are always published, so a family with nothing retained
// reads as zero rather than as no data.
var retainedFamilies = []string{"IPv4", "IPv6"}

// PublishRetained replaces the retained-set gauges with families. A family
// absent from it is published as zero.
func PublishRetained(families []RetainedFamily) {
	byFamily := map[string]RetainedFamily{}
	for _, f := range families {
		byFamily[f.IPFamily] = f
	}
	for _, family := range retainedFamilies {
		f := byFamily[family]
		RetainedAllocations.WithLabelValues(family, "finite").Set(float64(f.Finite))
		RetainedAllocations.WithLabelValues(family, "none").Set(float64(f.None))
		RetainedAllocations.WithLabelValues(family, "unevaluated").Set(float64(f.Unevaluated))
		RetainedOldestAge.WithLabelValues(family).Set(f.OldestAgeSeconds)
	}
}

// RecordRetentionReleases adds n to retention_releases_total{mode}.
func RecordRetentionReleases(mode string, n int) {
	if n > 0 {
		RetentionReleases.WithLabelValues(mode).Add(float64(n))
	}
}

// initRetentionSeries creates the series that must exist from start, so a rate
// over either mode, or a family with nothing retained, reads as zero rather
// than as no data. It runs after registration: an unregistered vector hands
// out no-op children.
func initRetentionSeries() {
	RetentionReleases.WithLabelValues("observe")
	RetentionReleases.WithLabelValues("enforce")
	PublishRetained(nil)
}
