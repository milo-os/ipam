// ipam-provider.jsonnet — Platform operator (provider) dashboard.
//
// Audience: SREs / platform operators running the IPAM service.
// Focus:    service health, allocation throughput, latency distribution,
//           pool utilization, error budget, dependency health.
//
// Naming note: the original IPAM dashboard spec called for four separately
// named dashboards (`ipam-overview`, `ipam-pool-utilization`,
// `ipam-allocation-latency`, `ipam-watch-health`). They were consolidated
// into this single provider dashboard so SREs see service health, pool
// utilization, latency, and watch health on one pane. The UID is kept as
// `ipam-provider` to preserve existing Grafana saved links and bookmarks —
// renaming the UID would silently break every URL in runbooks and on-call
// pages. The four spec sections live as rows below: "Service health",
// "Allocation throughput", "Allocation latency", "Pool utilization", and
// "Dependencies (DB + watch)".
//
// Compile with:
//   jsonnet -J vendor dashboards/jsonnet/ipam-provider.jsonnet \
//     > dashboards/generated/ipam-provider.json
//
// Metric source of truth: internal/metrics/metrics.go.
// Metrics that don't exist yet are rendered as TODO panels (clearly labeled)
// so the dashboard layout is stable as instrumentation lands.

local common = import 'lib/common.libsonnet';
local config = import 'config.libsonnet';
local g = common.g;

local dashboard = g.dashboard;
local row = g.panel.row;
local ts = g.panel.timeSeries;
local stat = g.panel.stat;
local text = g.panel.text;

local ipamSel = common.ipamSel;
local jobSel = common.jobSel;

// ─────────────────────────────────────────────────────────────────────────────
// Row: Service health
// ─────────────────────────────────────────────────────────────────────────────

local upPanel = common.statPanel(
  'Apiserver up',
  'sum(up{' + jobSel + '})',
  unit='short',
  thresholds=common.thresholds.booleanUp,
  description='Number of healthy IPAM apiserver pods scraped by Prometheus.'
);

local requestRatePanel = common.statPanel(
  'Apiserver request rate',
  'sum(rate(apiserver_request_total{' + jobSel + '}[5m]))',
  unit='reqps',
  description='Total apiserver request rate across all verbs/resources.'
);

local errorRatePanel = common.statPanel(
  'Apiserver 5xx rate',
  'sum(rate(apiserver_request_total{' + jobSel + ', code=~"5.."}[5m]))',
  unit='reqps',
  thresholds=common.thresholds.okWarnCrit,
  description='Server-side errors returned by the apiserver.'
);

local insufficientStoragePanel = common.statPanel(
  '507 Insufficient Storage rate',
  'sum(rate(apiserver_request_total{' + jobSel + ', code="507"}[5m]))',
  unit='reqps',
  description='Pool-exhausted denials (synchronous allocation rejects with 507).'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Allocation throughput
// ─────────────────────────────────────────────────────────────────────────────

local allocThroughputPanel = common.tsPanel(
  'Allocation rate by resource (req/s)',
  [
    {
      expr: 'sum by (resource, result) (rate(ipam_allocation_duration_seconds_count{' + ipamSel + '}[5m]))',
      legend: '{{resource}} / {{result}}',
    },
  ],
  unit='reqps',
  description='Allocation throughput. result=success|exhausted|error from internal/metrics/metrics.go.'
);

local allocFailureRatePanel = common.tsPanel(
  'Allocation failures by reason',
  [
    {
      expr: 'sum by (resource, reason) (rate(ipam_allocation_failures_total{' + ipamSel + '}[5m]))',
      legend: '{{resource}} / {{reason}}',
    },
  ],
  unit='reqps',
  description='Failure breakdown: pool_exhausted|pool_not_found|verification_required|tx_error|internal.'
);

local allocFailureRatioPanel = common.tsPanel(
  'Allocation failure ratio (errors / total)',
  [
    {
      expr: |||
        sum by (resource) (rate(ipam_allocation_failures_total{%(sel)s}[5m]))
        /
        clamp_min(sum by (resource) (rate(ipam_allocation_duration_seconds_count{%(sel)s}[5m])), 1e-9)
      ||| % { sel: ipamSel },
      legend: '{{resource}}',
    },
  ],
  unit='percentunit',
  description='Failures divided by total attempts. Alert at 5% sustained for 2m.'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Allocation latency (the SLO panel)
// ─────────────────────────────────────────────────────────────────────────────

local latencyQuantilesPanel = common.tsPanel(
  'Allocation latency quantiles (success only)',
  [
    {
      expr: |||
        histogram_quantile(0.50,
          sum by (le, resource) (rate(ipam_allocation_duration_seconds_bucket{%(sel)s, result="success"}[5m]))
        )
      ||| % { sel: ipamSel },
      legend: 'p50 {{resource}}',
    },
    {
      expr: |||
        histogram_quantile(0.95,
          sum by (le, resource) (rate(ipam_allocation_duration_seconds_bucket{%(sel)s, result="success"}[5m]))
        )
      ||| % { sel: ipamSel },
      legend: 'p95 {{resource}}',
    },
    {
      expr: |||
        histogram_quantile(0.99,
          sum by (le, resource) (rate(ipam_allocation_duration_seconds_bucket{%(sel)s, result="success"}[5m]))
        )
      ||| % { sel: ipamSel },
      legend: 'p99 {{resource}}',
    },
  ],
  unit='s',
  description='SLO panel: p95 < 500ms target. Filtered to result="success" so failures do not skew tail.'
);

local latencyHeatmapPanel = common.heatPanel(
  'Allocation latency heatmap (all results)',
  'sum by (le) (rate(ipam_allocation_duration_seconds_bucket{' + ipamSel + '}[$__rate_interval]))',
  unit='s',
  description='Full latency distribution across all resources/results.'
);

local p95StatPanel = common.statPanel(
  'p95 allocation latency',
  |||
    histogram_quantile(0.95,
      sum by (le) (rate(ipam_allocation_duration_seconds_bucket{%(sel)s, result="success"}[5m]))
    )
  ||| % { sel: ipamSel },
  unit='s',
  thresholds=common.thresholds.latency,
  description='Headline p95 latency across all resources. Alert at 500ms.'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Pool utilization
// ─────────────────────────────────────────────────────────────────────────────

local poolUtilTimeseriesPanel = common.tsPanel(
  'Pool utilization over time',
  [
    {
      expr: 'ipam_pool_utilization_ratio{' + ipamSel + '}',
      legend: '{{pool_key}} ({{ip_family}})',
    },
  ],
  unit='percentunit',
  description='Per-pool utilization ratio. Alert thresholds: warning at 80%, critical at 90%.'
);

local poolUtilGaugePanel =
  g.panel.gauge.new('Pool utilization (current)')
  + g.panel.gauge.queryOptions.withTargets([
    g.query.prometheus.new(common.ds, 'ipam_pool_utilization_ratio{' + ipamSel + '}')
    + g.query.prometheus.withLegendFormat('{{pool_key}} ({{ip_family}})')
    + g.query.prometheus.withInstant(true),
  ])
  + g.panel.gauge.standardOptions.withUnit('percentunit')
  + g.panel.gauge.standardOptions.withMin(0)
  + g.panel.gauge.standardOptions.withMax(1)
  + g.panel.gauge.standardOptions.thresholds.withMode('absolute')
  + g.panel.gauge.standardOptions.thresholds.withSteps([
    { color: 'green', value: null },
    { color: 'orange', value: 0.80 },
    { color: 'red', value: 0.90 },
  ])
  + g.panel.gauge.options.withShowThresholdMarkers(true)
  + g.panel.gauge.panelOptions.withDescription('Per-pool utilization. Yellow at 80%, red at 90%.');

local topExhaustedPoolsPanel =
  g.panel.table.new('Top 10 most utilized pools')
  + g.panel.table.queryOptions.withTargets([
    g.query.prometheus.new(common.ds, 'topk(10, ipam_pool_utilization_ratio{' + ipamSel + '})')
    + g.query.prometheus.withInstant(true)
    + g.query.prometheus.withFormat('table'),
  ])
  + g.panel.table.standardOptions.withUnit('percentunit')
  + g.panel.table.panelOptions.withDescription('Highest-utilization pools right now.');

// ─────────────────────────────────────────────────────────────────────────────
// Row: Apiserver request breakdown (proxy for read-path latency)
// ─────────────────────────────────────────────────────────────────────────────

local apiserverByVerbPanel = common.tsPanel(
  'Apiserver request rate by verb',
  [
    {
      expr: 'sum by (verb) (rate(apiserver_request_total{' + jobSel + ', resource=~".*claim.*|ippool|ipprefix|ipaddress|asnpool"}[5m]))',
      legend: '{{verb}}',
    },
  ],
  unit='reqps',
  description='Verb breakdown across IPAM resources. Useful for spotting list/watch hotspots.'
);

local apiserverLatencyByVerbPanel = common.tsPanel(
  'Apiserver request p95 by verb',
  [
    {
      expr: |||
        histogram_quantile(0.95,
          sum by (le, verb) (rate(apiserver_request_duration_seconds_bucket{%(sel)s, resource=~".*claim.*|ippool|ipprefix|ipaddress|asnpool"}[5m]))
        )
      ||| % { sel: jobSel },
      legend: '{{verb}}',
    },
  ],
  unit='s',
  description=|||
    Built-in apiserver latency by verb. Also the substitute for read-path metrics
    until dedicated ones are added.

    Known steady-state regression (2026-05-09 perf-tester baseline):
      - prefix `list` p95 = 544ms (target < 200ms, 2.7× over)
      - claim   `get` p95 = 311ms (target < 100ms, 3.1× over)

    The `get` and `list` p95 values converge despite very different result-set
    sizes, which points at per-request fixed cost (apiserver auth/admission
    middleware or serialization) rather than DB scan time. Write path is fine
    (CREATE p95 = 42ms with 12× headroom) — this is read-side only.
    See docs/runbooks/ipam-read-latency-high.md.
  |||
);

local apiserverErrorByCodePanel = common.tsPanel(
  'Apiserver responses by code',
  [
    {
      expr: 'sum by (code) (rate(apiserver_request_total{' + jobSel + ', code=~"4..|5.."}[5m]))',
      legend: 'HTTP {{code}}',
    },
  ],
  unit='reqps',
  description='4xx/5xx breakdown — 507 = pool exhausted, 409 = conflict, 5xx = server error.'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Resource pressure on the pod
// ─────────────────────────────────────────────────────────────────────────────

local goroutinePanel = common.tsPanel(
  'Goroutines',
  [{ expr: 'go_goroutines{' + jobSel + '}', legend: '{{instance}}' }],
  unit='short'
);

local cpuPanel = common.tsPanel(
  'CPU seconds (rate)',
  [{ expr: 'rate(process_cpu_seconds_total{' + jobSel + '}[5m])', legend: '{{instance}}' }],
  unit='percentunit'
);

local rssPanel = common.tsPanel(
  'Resident memory',
  [{ expr: 'process_resident_memory_bytes{' + jobSel + '}', legend: '{{instance}}' }],
  unit='bytes'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Dependencies — DB query latency, pgxpool saturation, watch lag
// ─────────────────────────────────────────────────────────────────────────────
//
// These three signals close the dependency-health loop for the IPAM
// apiserver. All series are emitted today (audited 2026-05-09):
//   - ipam_postgres_query_duration_seconds → cmd/ipam/serve.go (per-query)
//   - ipam_pgxpool_*                       → background sampler in serve.go
//   - ipam_watch_lag_seconds               → internal/watch/postgres.go

local dbDurationP95Panel = common.tsPanel(
  'DB query p95 by query_name',
  [
    {
      expr: |||
        histogram_quantile(0.95,
          sum by (le, query_name) (rate(ipam_postgres_query_duration_seconds_bucket{%(sel)s}[5m]))
        )
      ||| % { sel: jobSel },
      legend: '{{query_name}}',
    },
  ],
  unit='s',
  description='p95 wall-clock duration of named SQL statements inside the allocation transaction. Spikes in select_pool_for_update typically point to lock contention; spikes in load_existing_allocations point to large pools without an index hit.'
);

local pgxPoolSaturationPanel = common.tsPanel(
  'pgxpool saturation (acquired / max)',
  [
    {
      // clamp_min keeps the ratio defined when the sampler has not yet
      // published a max — the IPAMDBConnectionPoolSaturated alert uses the
      // same expression so dashboard and alert agree.
      expr: |||
        ipam_pgxpool_acquired_connections{%(sel)s}
        /
        clamp_min(ipam_pgxpool_max_connections{%(sel)s}, 1)
      ||| % { sel: jobSel },
      legend: 'acquired/max ({{instance}})',
    },
    {
      expr: 'ipam_pgxpool_idle_connections{%(sel)s} / clamp_min(ipam_pgxpool_max_connections{%(sel)s}, 1)' % { sel: jobSel },
      legend: 'idle/max ({{instance}})',
    },
  ],
  unit='percentunit',
  description='Postgres connection-pool utilization. acquired/max above 0.90 means new allocation transactions are about to queue on Acquire(). The IPAMDBConnectionPoolSaturated alert fires at idle/max < 0.10.'
);

local watchLagP99Panel = common.tsPanel(
  'Watch lag p99 (changelog INSERT → dispatch)',
  [
    {
      expr: |||
        histogram_quantile(0.99,
          sum by (le) (rate(ipam_watch_lag_seconds_bucket{%(sel)s}[5m]))
        )
      ||| % { sel: jobSel },
      legend: 'p99',
    },
    {
      expr: |||
        histogram_quantile(0.50,
          sum by (le) (rate(ipam_watch_lag_seconds_bucket{%(sel)s}[5m]))
        )
      ||| % { sel: jobSel },
      legend: 'p50',
    },
  ],
  unit='s',
  description='End-to-end lag between an ipam_changelog INSERT (commit_xid stamp) and the watcher handing the resulting watch.Event to its subscriber. p99 above 30s fires IPAMWatchLagHigh.'
);

local watchEventRatePanel = common.tsPanel(
  'Watch events dispatched (by kind)',
  [
    {
      expr: 'sum by (kind, event_type) (rate(ipam_watch_events_total{%(sel)s}[5m]))' % { sel: jobSel },
      legend: '{{kind}} / {{event_type}}',
    },
  ],
  unit='reqps',
  description='Per-resource watch event dispatch rate. A drop to zero with non-zero apiserver write traffic indicates the watcher is stuck or all subscribers have disconnected.'
);

// Watcher poll batch size — how full each pollChanges call comes back. The
// batch limit is 500 (see internal/watch/postgres.go); average values
// approaching 500 mean drainChangelog is looping to catch up. Plot the
// average alongside p95 and a threshold line at 500 so saturation jumps out.
local watchPollBatchPanel = common.tsPanel(
  'Watcher poll batch size (avg + p95)',
  [
    {
      expr: |||
        sum(rate(ipam_watcher_poll_batch_size_sum{%(sel)s}[5m]))
        /
        clamp_min(sum(rate(ipam_watcher_poll_batch_size_count{%(sel)s}[5m])), 1e-9)
      ||| % { sel: jobSel },
      legend: 'avg batch size',
    },
    {
      expr: |||
        histogram_quantile(0.95,
          sum by (le) (rate(ipam_watcher_poll_batch_size_bucket{%(sel)s}[5m]))
        )
      ||| % { sel: jobSel },
      legend: 'p95 batch size',
    },
    {
      // Static reference line at the 500-row batch limit. Constant value is
      // emitted via vector(500) so it renders as a flat threshold across the
      // selected time range without needing Grafana threshold configuration.
      expr: 'vector(500)',
      legend: 'batch limit (500)',
    },
  ],
  unit='short',
  description='Rows returned per pollChanges call. Average or p95 hugging the 500-row limit means the watcher is consistently saturating its batch budget — drainChangelog is looping and watch lag will trend up. IPAMWatcherBacklogSaturated fires at avg > 400 over 5m.'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Automatic pool provisioning
// ─────────────────────────────────────────────────────────────────────────────

// Where each claim's class chain came from. Every level of every claim is
// counted, so "reused" carries the traffic and the other outcomes are the
// exceptions standing out against it. Stacked, because the split is the point.
local cascadeOutcomePanel = common.tsPanel(
  'Class-chain levels by outcome',
  [
    {
      expr: 'sum by (outcome) (rate(ipam_cascade_levels_total{%(sel)s}[5m]))' % { sel: ipamSel },
      legend: '{{outcome}}',
    },
  ],
  unit='reqps',
  stack=true,
  description='What each claim had to do at each level of its class chain: reuse an existing pool, create one, or lose the race to create it. Losses come in a burst around each creation — that is one claim building the pool and the rest of the herd waiting for it, and it is healthy. Sustained losses with no creations mean claims are queueing without anything being built (IPAMCascadeProvisioningThrashing).'
);

// Pools nobody created by hand. Split by class so a chain that keeps rebuilding
// itself is attributable.
local cascadeProvisionedPanel = common.tsPanel(
  'Pools created automatically',
  [
    {
      expr: 'sum by (class) (increase(ipam_cascade_levels_total{%(sel)s, outcome="provisioned"}[1h]))' % { sel: ipamSel },
      legend: '{{class}}',
    },
  ],
  unit='short',
  description='Pools IPAM created on its own to satisfy a claim, per hour, by the class that caused them. This is the answer to "where did this pool come from" — a pool with no human author appears here, attributed to a class. Steady state is zero: a chain is built once per scope and reused thereafter.'
);

// Resolution runs before the allocation transaction, so this latency is charged
// to the claim without appearing in any query timing.
local cascadeLatencyPanel = common.tsPanel(
  'Pool resolution p95 (before allocation)',
  [
    {
      expr: |||
        histogram_quantile(0.95,
          sum by (le, provisioned) (
            rate(ipam_cascade_resolution_duration_seconds_bucket{%(sel)s, result="success"}[5m])
          )
        )
      ||| % { sel: ipamSel },
      legend: 'provisioned={{provisioned}}',
    },
  ],
  unit='s',
  description='Time each claim spends finding its pool, before the allocation transaction opens. provisioned=false is the common path and should be a few milliseconds; provisioned=true is the first claim into a scope paying to build the chain, and is legitimately slower. This cost is invisible in the Postgres query panels — if end-to-end claim latency is high and the query breakdown does not explain it, look here.'
);

// ─────────────────────────────────────────────────────────────────────────────
// Layout helper: place a panel on the grid.
// ─────────────────────────────────────────────────────────────────────────────

// In grafonnet v11.4.0 the panel `gridPos` is a plain object on the panel,
// not a builder. Set it directly via field merge.
local at(panel, x, y, w, h) =
  panel + { gridPos: { x: x, y: y, w: w, h: h } };

local rowAt(title, y) =
  row.new(title)
  + { gridPos: { x: 0, y: y, w: 24, h: 1 } };

// ─────────────────────────────────────────────────────────────────────────────
// Dashboard
// ─────────────────────────────────────────────────────────────────────────────

dashboard.new('IPAM — Provider')
+ dashboard.withUid('ipam-provider')
+ dashboard.withDescription('Provider-facing view covering overview, pool utilization, allocation latency, and watch health. Consolidates the four dashboards named in the spec (ipam-overview, ipam-pool-utilization, ipam-allocation-latency, ipam-watch-health) into a single SRE pane. UID stays `ipam-provider` to preserve existing saved links.')
+ dashboard.withTags(config.dashboards.tags + ['provider', 'sre'])
+ dashboard.withTimezone(config.dashboards.timezone)
+ dashboard.withRefresh(config.dashboards.refresh)
+ dashboard.time.withFrom(config.dashboards.timeRange.from)
+ dashboard.time.withTo(config.dashboards.timeRange.to)
+ dashboard.withVariables(common.defaultVars)
+ dashboard.withPanels([
  rowAt('Service health', 0),
  at(upPanel, 0, 1, 6, 4),
  at(requestRatePanel, 6, 1, 6, 4),
  at(errorRatePanel, 12, 1, 6, 4),
  at(insufficientStoragePanel, 18, 1, 6, 4),

  rowAt('Allocation throughput', 5),
  at(allocThroughputPanel, 0, 6, 12, 8),
  at(allocFailureRatePanel, 12, 6, 12, 8),
  at(allocFailureRatioPanel, 0, 14, 24, 6),

  rowAt('Allocation latency', 20),
  at(p95StatPanel, 0, 21, 6, 6),
  at(latencyQuantilesPanel, 6, 21, 18, 8),
  at(latencyHeatmapPanel, 0, 29, 24, 8),

  rowAt('Pool utilization', 37),
  at(poolUtilGaugePanel, 0, 38, 12, 8),
  at(topExhaustedPoolsPanel, 12, 38, 12, 8),
  at(poolUtilTimeseriesPanel, 0, 46, 24, 8),

  rowAt('Apiserver request mix', 54),
  at(apiserverByVerbPanel, 0, 55, 8, 8),
  at(apiserverLatencyByVerbPanel, 8, 55, 8, 8),
  at(apiserverErrorByCodePanel, 16, 55, 8, 8),

  rowAt('Pod resources', 63),
  at(goroutinePanel, 0, 64, 8, 6),
  at(cpuPanel, 8, 64, 8, 6),
  at(rssPanel, 16, 64, 8, 6),

  rowAt('Dependencies (DB + watch)', 70),
  at(dbDurationP95Panel, 0, 71, 12, 7),
  at(pgxPoolSaturationPanel, 12, 71, 12, 7),
  at(watchLagP99Panel, 0, 78, 12, 7),
  at(watchEventRatePanel, 12, 78, 12, 7),
  at(watchPollBatchPanel, 0, 85, 24, 7),

  rowAt('Automatic pool provisioning', 92),
  at(cascadeOutcomePanel, 0, 93, 12, 7),
  at(cascadeProvisionedPanel, 12, 93, 12, 7),
  at(cascadeLatencyPanel, 0, 100, 24, 7),
])
