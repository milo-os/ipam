// Package metrics defines Prometheus metrics for the IPAM service.
//
// All metric series live under the "ipam" namespace and are exposed on the
// apiserver's standard /metrics endpoint via the legacy registry.
//
// The metric set is intentionally aligned with the alert rules in
// config/components/observability/ and the Grafana dashboard panels — when
// adding a new alert/panel that references an "ipam_*" series, register the
// matching collector here so the series exists from process start (a missing
// series is operationally indistinguishable from a healthy "0").
package metrics

import (
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

var (
	// Tenant label cardinality bound (applies to `project` and `org` labels
	// throughout this file): cardinality is bounded by the platform's project
	// and organization counts. The current Milo deployment expects on the
	// order of tens of thousands of projects (worst case), each label keeps
	// its slot in the metric vector for the process lifetime so a runaway
	// label set surfaces as memory pressure rather than silent dropouts. If
	// project count grows past the low six figures, move tenant context to
	// an `_info`-style metric joined at query time instead of an inline
	// label.
	//
	// This used to point at docs/production-readiness.md "for the cardinality
	// discussion". That report is retired, and the discussion it held was one
	// line — "no documented cardinality bound" — which the paragraph above now
	// supersedes. The bound belongs here, next to the labels it constrains,
	// rather than in a document that expires.

	// AllocationDuration tracks the latency of synchronous allocation
	// transactions for IPClaim and ASNClaim.
	//
	// METRIC NAMING NOTE: the spec (.claude/agents/observability.md) lists a
	// single `ipam_allocation_total` counter alongside the duration histogram.
	// This implementation intentionally diverges and emits two counters
	// (`ipam_allocation_attempts_total` and `ipam_allocation_failures_total`)
	// instead. The split lets dashboards compute success-ratio cleanly even
	// when transactions crash mid-flight (where the histogram count would
	// silently undercount). Do not rename these back to `ipam_allocation_total`
	// — alerts, runbooks, and dashboards all reference the split names; a
	// rename would silently break every alert that depends on them.
	AllocationDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Namespace:      "ipam",
			Name:           "allocation_duration_seconds",
			Help:           "Duration of IPAM allocation transactions in seconds",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		// resource:  "ipclaims" | "asnclaims"
		// result:    "success" | "exhausted" | "error"
		// ip_family: "IPv4" | "IPv6" | "ASN" — derived from the claim spec or
		//            the resolved CIDR for prefix/address claims, hardcoded
		//            to "ASN" for asnclaim. Bounded cardinality (3 values).
		// project:   the iam.miloapis.com/parent-name UserInfo.Extra value when
		//            Kind=="Project", else "" for platform / org-scoped requests.
		// org:       parent-name when Kind=="Organization", else "" for now
		//            (project-scoped requests do not carry the owning org id
		//            in extras yet; see internal/tenant/tenant.go).
		// Cardinality bound: see top-of-block comment for project / org.
		[]string{"resource", "result", "ip_family", "project", "org"},
	)

	// AllocationAttempts counts allocation attempts by resource type. Paired
	// with AllocationFailures so dashboards can compute a clean
	// success-ratio = 1 - (failures / attempts) without depending on the
	// AllocationDuration histogram count (which is only observed on
	// completion, not on transactions that crash mid-flight).
	AllocationAttempts = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "allocation_attempts_total",
			Help:           "Total number of allocation attempts (incremented at the top of the allocation path before any DB work)",
			StabilityLevel: metrics.ALPHA,
		},
		// resource:  "ipclaims" | "asnclaims"
		// ip_family: "IPv4" | "IPv6" | "ASN" — sourced from the same handler
		//            value used for ObserveAllocationDuration so attempts,
		//            failures, and the latency histogram split identically.
		// project, org: see AllocationDuration for label semantics + cardinality.
		[]string{"resource", "ip_family", "project", "org"},
	)

	// AllocationFailures counts allocation failures by reason.
	AllocationFailures = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "allocation_failures_total",
			Help:           "Total number of allocation failures",
			StabilityLevel: metrics.ALPHA,
		},
		// resource:  "ipclaims" | "asnclaims"
		// reason:    "pool_exhausted" | "pool_not_found" | "verification_required" | "tx_error" | "internal"
		// ip_family: "IPv4" | "IPv6" | "ASN" — mirrors AllocationAttempts so
		//            success-ratio = 1 - (failures / attempts) can be computed
		//            per address family.
		// project, org: see AllocationDuration for label semantics + cardinality.
		[]string{"resource", "reason", "ip_family", "project", "org"},
	)

	// PoolUtilization tracks per-pool utilization as a ratio in [0, 1].
	// Tenant labels (project + org) let dashboards aggregate utilization by
	// owning project / organization without re-deriving it from the
	// pool_key. See top-of-block cardinality comment.
	PoolUtilization = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Namespace:      "ipam",
			Name:           "pool_utilization_ratio",
			Help:           "Ratio of allocated to total capacity per pool",
			StabilityLevel: metrics.ALPHA,
		},
		// resource is the plural lowercase pool kind ("ippools" |
		// "asnpools"), kept here so dashboards can split prefix vs ASN
		// utilization without parsing pool_key — same shape used by
		// PoolCapacity and PoolAllocated.
		[]string{"pool_key", "ip_family", "resource", "project", "org"},
	)

	// PoolCapacity / PoolAllocated expose the absolute numerator and
	// denominator behind PoolUtilization. The ratio gauge by itself doesn't
	// distinguish "small pool half full" from "huge pool half full"; these
	// two gauges close that gap so dashboards can show capacity in absolute
	// terms (a /22 with 512 free is a different operational concern from a
	// /28 with 8 free even though both are at 50%).
	//
	// Values are addresses for IPv4 / IPv6 prefix pools and ASN counts for
	// ASN pools. resource is "ippools" | "asnpools" so a single PromQL
	// can split prefix vs ASN capacity without parsing pool_key.
	PoolCapacity = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Namespace:      "ipam",
			Name:           "pool_capacity_total",
			Help:           "Total addressable capacity of the pool (addresses for prefix pools, ASN count for ASN pools)",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"pool_key", "ip_family", "resource", "project", "org"},
	)

	PoolAllocated = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Namespace:      "ipam",
			Name:           "pool_allocated_total",
			Help:           "Currently allocated count for the pool (addresses for prefix pools, ASN count for ASN pools)",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"pool_key", "ip_family", "resource", "project", "org"},
	)

	// WatchLag measures end-to-end watch propagation latency: the elapsed
	// time between the changelog row's INSERT timestamp (created_at) and
	// the moment the PostgresWatcher reads it from the poll/notify path
	// and is about to dispatch a watch.Event to its result channel.
	//
	// This is the metric the watch-lag SLO alert rule fires on. Wide
	// buckets (default Prometheus distribution) cover the full expected
	// range from sub-millisecond NOTIFY-driven dispatches up to multi-
	// second windows when the safety poll catches missed notifications.
	WatchLag = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Namespace:      "ipam",
			Name:           "watch_lag_seconds",
			Help:           "Latency between changelog INSERT (created_at) and watch event dispatch",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
	)

	// PostgresQueryDuration records the wall-clock duration of individual
	// Postgres statements run inside the allocation transaction. A label
	// per query name lets the Grafana panel break the bar chart down by
	// the work that actually contributes to allocation latency.
	//
	// Suggested query_name values:
	//   "select_pool_for_update" — SELECT data FROM ipam_objects ... FOR UPDATE
	//   "load_existing_allocations" — SELECT existing CIDRs/ASNs for the pool
	//   "insert_allocation" — INSERT INTO ipam_cidr_allocations / ipam_asn_allocations
	//   "insert_object" — INSERT INTO ipam_objects (claim row + child prefix)
	//   "update_pool_status" — UPDATE ipam_objects ... when the pool status row is rewritten
	PostgresQueryDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Namespace:      "ipam",
			Name:           "postgres_query_duration_seconds",
			Help:           "Duration of individual Postgres statements in the allocation path",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"query_name"},
	)

	// PgxpoolTotalConnections / IdleConnections / AcquiredConnections /
	// MaxConnections expose the live state of the pgxpool used by the
	// synchronous allocators. Updated on a 15s tick from
	// (*pgxpool.Pool).Stat() — see ObservePgxpoolStat below and the
	// background sampler started in cmd/ipam/serve.go.
	//
	// MaxConnections is a configured ceiling, not a runtime value, but
	// keeping it as a gauge lets dashboards plot saturation
	// (acquired / max) over time without baking the ceiling into the
	// query.
	PgxpoolTotalConnections = metrics.NewGauge(
		&metrics.GaugeOpts{
			Namespace:      "ipam",
			Name:           "pgxpool_total_connections",
			Help:           "Current total number of pgx connections (acquired + idle + constructing)",
			StabilityLevel: metrics.ALPHA,
		},
	)
	PgxpoolIdleConnections = metrics.NewGauge(
		&metrics.GaugeOpts{
			Namespace:      "ipam",
			Name:           "pgxpool_idle_connections",
			Help:           "Current number of idle pgx connections in the pool",
			StabilityLevel: metrics.ALPHA,
		},
	)
	PgxpoolAcquiredConnections = metrics.NewGauge(
		&metrics.GaugeOpts{
			Namespace:      "ipam",
			Name:           "pgxpool_acquired_connections",
			Help:           "Current number of pgx connections checked out by callers",
			StabilityLevel: metrics.ALPHA,
		},
	)
	PgxpoolMaxConnections = metrics.NewGauge(
		&metrics.GaugeOpts{
			Namespace:      "ipam",
			Name:           "pgxpool_max_connections",
			Help:           "Configured maximum number of pgx connections",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// PgxpoolSamplerLastRunSeconds is a heartbeat gauge updated by the
	// background sampler goroutine in cmd/ipam/serve.go on every successful
	// tick. The IPAMPgxpoolMetricsStale alert fires on
	// `time() - ipam_pgxpool_sampler_last_run_seconds > 90` — i.e. the
	// sampler has missed more than ~6 ticks (15s cadence). This replaces the
	// older `time() - timestamp(ipam_pgxpool_total_connections)` expression,
	// which was broken under Prometheus' staleness semantics (timestamp()
	// returns the evaluation time, not the last-update time, so the alert
	// could never fire while scrapes continued).
	PgxpoolSamplerLastRunSeconds = metrics.NewGauge(
		&metrics.GaugeOpts{
			Namespace:      "ipam",
			Name:           "pgxpool_sampler_last_run_seconds",
			Help:           "Unix timestamp of the last successful pgxpool stats collection by the background sampler. Used to detect sampler goroutine death.",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// WatchEvents counts watch events dispatched from the LISTEN/NOTIFY
	// changelog watcher to subscribers' result channels. Bookmarks and dropped
	// (predicate-rejected) entries are NOT counted — only events the watcher
	// actually hands off downstream.
	//
	// kind:       lowercase plural resource (ippools, ipclaims, ipallocations,
	//             asnpools, asnclaims, ...).
	//             Derived from the storage key prefix; "unknown" if the key
	//             does not match the expected /ipam.miloapis.com/<resource>/...
	//             layout (which would indicate a bug, not user input).
	// event_type: ADDED | MODIFIED | DELETED.
	WatchEvents = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "watch_events_total",
			Help:           "Total number of watch events dispatched to subscribers, by resource kind and event type",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"kind", "event_type"},
	)

	// WatcherPollBatchSize tracks how many rows each pollChanges call returns,
	// labeled by resource kind. When the value consistently equals the batch
	// limit (500) the watcher is behind and drainChangelog is looping. Values
	// below the limit mean the watcher is caught up after that poll.
	//
	// Use `rate(ipam_watcher_poll_rows_total[1m]) / rate(ipam_watcher_polls_total[1m])`
	// for average batch size, or watch `ipam_watcher_poll_batch_size_bucket{le="500"}`
	// saturation to spot drain episodes.
	WatcherPollBatchSize = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Namespace:      "ipam",
			Name:           "watcher_poll_batch_size",
			Help:           "Rows returned per pollChanges call; values at the batch limit (500) indicate a backlog",
			Buckets:        []float64{0, 1, 10, 50, 100, 200, 300, 400, 500},
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"kind"},
	)

	// WatcherDrainCycles counts how many full drainChangelog loops completed,
	// labeled by resource kind. A drain cycle that consumed more than one batch
	// (i.e. looped) means the watcher fell behind and caught up.
	WatcherDrainCycles = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "watcher_drain_cycles_total",
			Help:           "Total drainChangelog invocations; label 'multi_batch=true' when more than one batch was needed",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"kind", "multi_batch"},
	)

	// Releases counts successful claim deletions. Paired with the existing
	// AllocationAttempts/AllocationFailures family so dashboards can plot the
	// full lifecycle: attempts → failures → bound (attempts − failures) →
	// releases. We do not bucket by reason here because Delete failures are
	// extremely rare (transaction-only failure mode) and surface as
	// apiserver_request_total{verb="delete", code!~"2.."} already.
	//
	// resource: "ipclaims" | "asnclaims".
	Releases = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "releases_total",
			Help:           "Total number of successfully released (deleted) bound claims",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"resource"},
	)
)

func init() {
	legacyregistry.MustRegister(
		AllocationDuration,
		AllocationAttempts,
		AllocationFailures,
		PoolUtilization,
		PoolCapacity,
		PoolAllocated,
		WatchLag,
		PostgresQueryDuration,
		PgxpoolTotalConnections,
		PgxpoolIdleConnections,
		PgxpoolAcquiredConnections,
		PgxpoolMaxConnections,
		PgxpoolSamplerLastRunSeconds,
		WatchEvents,
		WatcherPollBatchSize,
		WatcherDrainCycles,
		Releases,
	)
}

// RecordPollBatch records the number of rows returned by one pollChanges call.
func RecordPollBatch(kind string, n int) {
	WatcherPollBatchSize.WithLabelValues(kind).Observe(float64(n))
}

// RecordDrainCycle records one drainChangelog completion.
// multiBatch is true when more than one pollChanges call was needed (backlog > one batch).
func RecordDrainCycle(kind string, multiBatch bool) {
	v := "false"
	if multiBatch {
		v = "true"
	}
	WatcherDrainCycles.WithLabelValues(kind, v).Inc()
}

// RecordWatchEvent increments the watch_events_total counter for the given
// resource kind (lowercase plural, e.g. "ipclaims") and event type
// ("ADDED" | "MODIFIED" | "DELETED"). Called from the watcher's dispatch
// path, immediately after an event is handed off to the subscriber channel.
func RecordWatchEvent(kind, eventType string) {
	WatchEvents.WithLabelValues(kind, eventType).Inc()
}

// RecordRelease increments the releases_total counter for the given claim
// resource ("ipclaims" | "asnclaims"). Called from
// the claim Delete handler immediately after the deletion transaction
// commits successfully.
func RecordRelease(resource string) {
	Releases.WithLabelValues(resource).Inc()
}

// ObserveQuery records a Postgres query duration. Intended for use as
//
//	defer metrics.ObserveQuery("select_pool_for_update", time.Now())
//
// where the deferred call captures the start instant and reports
// time.Since(start) when the surrounding statement returns.
func ObserveQuery(queryName string, start time.Time) {
	PostgresQueryDuration.WithLabelValues(queryName).Observe(time.Since(start).Seconds())
}

// ObserveWatchLag records the elapsed time between a changelog row's
// created_at timestamp and the moment the watcher is about to dispatch the
// resulting watch.Event. Negative values (clock skew) are clamped to 0 so
// they do not pollute the histogram tail.
func ObserveWatchLag(createdAt time.Time) {
	if createdAt.IsZero() {
		return
	}
	lag := time.Since(createdAt).Seconds()
	if lag < 0 {
		lag = 0
	}
	WatchLag.Observe(lag)
}

// PgxpoolStatLike is the subset of (*pgxpool.Stat) that the metrics package
// reads. Defined here so this package does not pull pgxpool into its own
// import set; the caller in cmd/ipam/serve.go bridges the concrete
// *pgxpool.Stat to this interface.
type PgxpoolStatLike interface {
	TotalConns() int32
	IdleConns() int32
	AcquiredConns() int32
	MaxConns() int32
}

// ObservePgxpoolStat publishes the supplied pool stat to the four pgxpool
// gauges. Safe to call from any goroutine — gauges are atomic.
func ObservePgxpoolStat(stat PgxpoolStatLike) {
	PgxpoolTotalConnections.Set(float64(stat.TotalConns()))
	PgxpoolIdleConnections.Set(float64(stat.IdleConns()))
	PgxpoolAcquiredConnections.Set(float64(stat.AcquiredConns()))
	PgxpoolMaxConnections.Set(float64(stat.MaxConns()))
}

// ObserveAllocationDuration records an end-to-end allocation latency sample
// against (resource, result, ipFamily, project, org). Intended for use as
//
//	start := time.Now()
//	defer func() {
//	    metrics.ObserveAllocationDuration("ipclaims", result, ipFamily, project, org, start)
//	}()
//
// where the surrounding code mutates `result` ("success" | "exhausted" |
// "error") and `ipFamily` ("IPv4" | "IPv6" | "ASN") just before each return so
// the observation lands in the right bucket. ipFamily defaults to "" until
// the address-family-aware code path is reached; that yields a brief window
// where validation/permission failures land in the empty-string label, which
// is fine — those failures are also already counted in
// AllocationFailures{reason="internal"|...} and are visually distinct from
// the family-tagged successes. `project` and `org` come from the tenant
// identity helpers (Identity.Project() / Identity.Org()); both are "" for
// platform-scoped requests.
func ObserveAllocationDuration(resource, result, ipFamily, project, org string, start time.Time) {
	AllocationDuration.WithLabelValues(resource, result, ipFamily, project, org).Observe(time.Since(start).Seconds())
}

// RecordAllocationFailure increments the failures counter for (resource,
// reason, ipFamily, project, org). Pair this with the existing
// AllocationAttempts counter so dashboard queries can compute
// success-ratio = 1 - (failures / attempts) without depending on the
// AllocationDuration histogram count.
//
// Allowed reasons: "pool_exhausted" | "pool_not_found" |
// "verification_required" | "tx_error" | "internal".
// ipFamily mirrors the value passed to ObserveAllocationDuration ("IPv4" |
// "IPv6" | "ASN", or "" for failures that fire before the claim spec is
// readable).
// `project` and `org` are the tenant labels (or "" for platform requests).
func RecordAllocationFailure(resource, reason, ipFamily, project, org string) {
	AllocationFailures.WithLabelValues(resource, reason, ipFamily, project, org).Inc()
}

// SetPoolUtilization publishes the current allocated/total ratio for a pool.
// poolKey is the storage-layer key (the same key used as the FOR UPDATE
// target in the allocation transaction); ipFamily is "IPv4", "IPv6", or
// "ASN" for ASN pools. resource is the plural lowercase pool kind
// ("ippools" | "asnpools") and matches the labels used by SetPoolCapacity
// so all three pool gauges split identically. project / org carry the owning
// tenant for org-level dashboards. Ratios outside [0, 1] are clamped — a
// buggy capacity computation should not poison the dashboard.
func SetPoolUtilization(poolKey, ipFamily, resource, project, org string, ratio float64) {
	if ratio < 0 {
		ratio = 0
	} else if ratio > 1 {
		ratio = 1
	}
	PoolUtilization.WithLabelValues(poolKey, ipFamily, resource, project, org).Set(ratio)
}

// SetPoolCapacity publishes the absolute total / allocated counts for a pool
// alongside the existing utilization ratio. Callers should invoke this in
// the same place they invoke SetPoolUtilization so all three gauges advance
// together. resource is the plural lowercase resource name ("ippools" |
// "asnpools") so dashboards can split prefix vs ASN capacity without parsing
// pool_key.
//
// total / allocated are float64 because IPv6 pool sizes overflow int64
// (a /48 has 2^80 addresses); the gauge stores them as the same float
// representation big.Float produces from the prefix arithmetic upstream.
func SetPoolCapacity(poolKey, ipFamily, resource, project, org string, total, allocated float64) {
	PoolCapacity.WithLabelValues(poolKey, ipFamily, resource, project, org).Set(total)
	PoolAllocated.WithLabelValues(poolKey, ipFamily, resource, project, org).Set(allocated)
}
