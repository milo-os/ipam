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
	"context"
	"strconv"
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
	// label. See docs/production-readiness.md for the cardinality discussion.

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
		// reason:    server faults "internal" | "tx_error"; refusals
		//            "invalid" | "conflict" | "forbidden" | "pool_exhausted" |
		//            "pool_not_found" | "class_not_found" |
		//            "class_holds_no_range" | "namespace_not_live". The
		//            mapping lives in internal/registry/ipam/ipclaim/failurereason.go.
		// ip_family: "IPv4" | "IPv6" | "ASN" — mirrors AllocationAttempts so
		//            success-ratio = 1 - (failures / attempts) can be computed
		//            per address family.
		// project, org: see AllocationDuration for label semantics + cardinality.
		[]string{"resource", "reason", "ip_family", "project", "org"},
	)

	// The per-pool gauges (ipam_pool_utilization_ratio,
	// ipam_pool_capacity_total, ipam_pool_allocated_total) are exported by a
	// collector over committed state; see pools.go.

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

	// Retentions counts the subset of releases that freed no address because
	// the allocation's reclaim policy was Retain. Retained addresses stay
	// consumed with no claim naming them, so they are invisible in every
	// claim-shaped view of the service; this is the counter that shows them
	// accumulating.
	//
	// resource: "ipclaim".
	Retentions = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "retentions_total",
			Help:           "Total number of released claims whose allocation was retained",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"resource"},
	)

	// CascadeLevels counts the levels of a class chain a claim walked, by what
	// the claim had to do at each one.
	//
	// This counts every level of every claim, including the common "reused" case
	// where the pool already exists. A counter that advanced only on creation
	// would hide the steady state: "nothing needed provisioning" and "no claims
	// are resolving at all" would both read as no data.
	//
	// outcome:
	//   reused      — the pool exists; the claim does one indexed read.
	//   provisioned — this claim creates the pool.
	//   lost        — this claim races another into the same scope and loses.
	//                 Healthy and expected: one winner per scope, and every
	//                 other member of a first-claim herd records a loss and
	//                 then uses the winner's pool. Sustained losses with no
	//                 matching provisions mean claims are contending on an
	//                 identity row whose winner keeps aborting, which from
	//                 outside looks only like slow claims.
	//   error       — the level did not resolve; the claim failed.
	//
	// class is an IPClass name, bounded by the platform's class definitions.
	// The projected scope, the scope digest, and the pool key stay off the
	// metric, because each grows without bound.
	CascadeLevels = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "cascade_levels_total",
			Help:           "Class-chain levels walked during pool resolution, by outcome",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"class", "outcome"},
	)

	// CascadeResolutionDuration tracks the time a claim spends resolving its
	// pool, before the allocation transaction opens.
	//
	// Resolution runs outside the allocation transaction, and each level commits
	// on its own, so the Postgres query histogram never shows this latency. It
	// appears only as an unexplained gap in end-to-end claim latency.
	//
	// The provisioned label separates the first claim into a scope, which builds
	// a chain, from every claim after it, which reads one row. Without the
	// label, the two average together and neither is legible.
	//
	// result: "success" | "error". provisioned: "true" when this claim created
	// at least one pool.
	CascadeResolutionDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Namespace:      "ipam",
			Name:           "cascade_resolution_duration_seconds",
			Help:           "Duration of pool resolution, including any pools provisioned on the way",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"result", "provisioned"},
	)
)

func init() {
	legacyregistry.MustRegister(
		AllocationDuration,
		AllocationAttempts,
		AllocationFailures,
		WatchLag,
		PostgresQueryDuration,
		PgxpoolTotalConnections,
		PgxpoolIdleConnections,
		PgxpoolAcquiredConnections,
		PgxpoolMaxConnections,
		PgxpoolSamplerLastRunSeconds,
		PoolGaugesLastSuccess,
		WatchEvents,
		WatcherPollBatchSize,
		WatcherDrainCycles,
		Releases,
		Retentions,
		CascadeLevels,
		CascadeResolutionDuration,
	)
	legacyregistry.CustomMustRegister(poolGauges)
}

type dryRunKey struct{}

// WithDryRun marks ctx as serving a server-side dry-run. Work done under it is
// rolled back, so metrics that count what the work changed (a pool provisioned,
// a level reused) are not recorded: they would describe changes that never
// happened. Request-level metrics (attempts, failures, latency) and query
// timings still record; the request and its queries are real.
func WithDryRun(ctx context.Context) context.Context {
	return context.WithValue(ctx, dryRunKey{}, true)
}

// IsDryRun reports whether ctx was marked by WithDryRun.
func IsDryRun(ctx context.Context) bool {
	v, _ := ctx.Value(dryRunKey{}).(bool)
	return v
}

// RecordCascadeLevel records one class-chain level a claim walked. outcome is
// "reused", "provisioned", "lost", or "error". Nothing is recorded under a
// dry-run (see WithDryRun).
func RecordCascadeLevel(ctx context.Context, className, outcome string) {
	if IsDryRun(ctx) {
		return
	}
	CascadeLevels.WithLabelValues(className, outcome).Inc()
}

// ObserveCascadeResolution records one pool resolution. result is "success" or
// "error"; provisioned reports whether the claim created any pool. Intended as
//
//	start := time.Now()
//	result, provisioned := "error", false
//	defer func() {
//	    metrics.ObserveCascadeResolution(ctx, result, provisioned, start)
//	}()
//
// where the surrounding code sets result before each successful return.
// Nothing is recorded under a dry-run (see WithDryRun).
func ObserveCascadeResolution(ctx context.Context, result string, provisioned bool, start time.Time) {
	if IsDryRun(ctx) {
		return
	}
	CascadeResolutionDuration.
		WithLabelValues(result, strconv.FormatBool(provisioned)).
		Observe(time.Since(start).Seconds())
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

// RecordRetention increments the retentions_total counter for a release that
// left its allocation in place under reclaimPolicy Retain. Recorded alongside
// RecordRelease, not instead of it.
func RecordRetention(resource string) {
	Retentions.WithLabelValues(resource).Inc()
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
// AllocationFailures{reason="invalid"|...} and are visually distinct from
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
// reason is "internal" or "tx_error" only for a server fault; a request the
// server refused (422, 409, 403, 507, ...) carries a refusal reason. See the
// AllocationFailures label comment for the set.
// ipFamily mirrors the value passed to ObserveAllocationDuration ("IPv4" |
// "IPv6" | "ASN", or "" for failures that fire before the claim spec is
// readable).
// `project` and `org` are the tenant labels (or "" for platform requests).
func RecordAllocationFailure(resource, reason, ipFamily, project, org string) {
	AllocationFailures.WithLabelValues(resource, reason, ipFamily, project, org).Inc()
}
