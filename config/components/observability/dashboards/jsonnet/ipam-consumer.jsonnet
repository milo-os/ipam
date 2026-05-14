// ipam-consumer.jsonnet — Consumer (workload owner / project admin) dashboard.
//
// Audience: developers and project admins who file claims and want to know
//           whether the IPAM service is serving *their* namespace/project well.
// Scope:    one Grafana folder, scoped by $namespace template variable.
//
// Naming note: this dashboard does not match a spec name 1:1. The original
// IPAM dashboard spec listed four provider-side dashboards (`ipam-overview`,
// `ipam-pool-utilization`, `ipam-allocation-latency`, `ipam-watch-health`)
// — see ipam-provider.jsonnet, which consolidates those four. This file
// adds the consumer-facing complement that the spec does not enumerate:
// per-namespace claim throughput, success rate, and latency for the
// project/namespace caller. The UID is kept as `ipam-consumer` to preserve
// existing Grafana saved links.
//
// Compile with:
//   jsonnet -J vendor dashboards/jsonnet/ipam-consumer.jsonnet \
//     > dashboards/generated/ipam-consumer.json
//
// Honest limits (from the explorer's metrics audit):
//   - `ipam_allocation_*` series carry a `project` label (populated from the
//     iam.miloapis.com/parent-name UserInfo.Extra at request entry). Empty
//     project values mark platform-scoped requests; the project-scoped
//     success-rate panel filters on `project!=""`.
//   - `ipam_pool_*` series do NOT yet carry a project / namespace label.
//   - `apiserver_request_total` and `apiserver_request_duration_seconds`
//     DO carry a `namespace` label, so per-namespace request rate, success
//     rate, and apiserver-side latency are derivable from those.
//   - Time-to-bound and BYOIP per-prefix verification status do not exist as
//     metrics yet — those panels remain TODO stubs that document the gap.

local common = import 'lib/common.libsonnet';
local config = import 'config.libsonnet';
local g = common.g;

local dashboard = g.dashboard;
local row = g.panel.row;
local ts = g.panel.timeSeries;
local stat = g.panel.stat;
local text = g.panel.text;
local variable = dashboard.variable;

// ─────────────────────────────────────────────────────────────────────────────
// Variables
// ─────────────────────────────────────────────────────────────────────────────

local namespaceVar =
  variable.query.new('namespace')
  + variable.query.generalOptions.withLabel('Namespace')
  + variable.query.withDatasourceFromVariable(common.datasourceVar)
  + variable.query.queryTypes.withLabelValues(
      'namespace',
      'apiserver_request_total{job="$job", resource=~".*claim.*"}'
    )
  + variable.query.withRefresh('time')
  + variable.query.selectionOptions.withIncludeAll(true, '.+')
  + variable.query.selectionOptions.withMulti(true);

local resourceVar =
  variable.query.new('resource')
  + variable.query.generalOptions.withLabel('Resource')
  + variable.query.withDatasourceFromVariable(common.datasourceVar)
  + variable.query.queryTypes.withLabelValues(
      'resource',
      'apiserver_request_total{job="$job", resource=~".*claim.*"}'
    )
  + variable.query.withRefresh('time')
  + variable.query.selectionOptions.withIncludeAll(true, '.+')
  + variable.query.selectionOptions.withMulti(true);

local consumerVars = common.defaultVars + [namespaceVar, resourceVar];

local nsSel = 'job="$job", instance=~"$instance", namespace=~"$namespace", resource=~"$resource"';
local createSel = nsSel + ', verb="create"';

// ─────────────────────────────────────────────────────────────────────────────
// Row: Claim activity for the selected namespaces
// ─────────────────────────────────────────────────────────────────────────────

local claimRatePanel = common.tsPanel(
  'Claim CREATE rate',
  [
    {
      expr: 'sum by (namespace, resource) (rate(apiserver_request_total{' + createSel + '}[5m]))',
      legend: '{{namespace}} / {{resource}}',
    },
  ],
  unit='reqps',
  description='Successful + failed claim CREATE requests per second, scoped to the selected namespaces.'
);

local claimByCodePanel = common.tsPanel(
  'Claim CREATE responses by code',
  [
    {
      expr: 'sum by (namespace, code) (rate(apiserver_request_total{' + createSel + '}[5m]))',
      legend: '{{namespace}} / HTTP {{code}}',
    },
  ],
  unit='reqps',
  description='4xx/5xx tells you which namespaces are seeing denials. 507 = pool exhausted.'
);

local successRatePanel = common.tsPanel(
  'Claim CREATE success rate',
  [
    {
      expr: |||
        sum by (namespace, resource) (rate(apiserver_request_total{%(sel)s, code=~"2.."}[5m]))
        /
        clamp_min(sum by (namespace, resource) (rate(apiserver_request_total{%(sel)s}[5m])), 1e-9)
      ||| % { sel: createSel },
      legend: '{{namespace}} / {{resource}}',
    },
  ],
  unit='percentunit',
  description='Per-namespace claim success ratio (2xx / total). Below 0.95 means the namespace is degraded.'
);

local successRateStat = common.statPanel(
  'Aggregate claim success rate',
  |||
    sum(rate(apiserver_request_total{%(sel)s, code=~"2.."}[5m]))
    /
    clamp_min(sum(rate(apiserver_request_total{%(sel)s}[5m])), 1e-9)
  ||| % { sel: createSel },
  unit='percentunit',
  thresholds=common.thresholds.successRate,
  description='Aggregate success ratio across the selected namespaces over the last 5m.'
);

// Project-scoped success rate from the IPAM-owned metrics.
// `project` is propagated from UserInfo.Extra (iam.miloapis.com/parent-name)
// inside the claim Create handlers, so the ratio reflects the synchronous
// allocation outcome the caller actually saw — independent of any apiserver
// middleware accounting. We filter `project!=""` to drop platform-scoped
// traffic, which would otherwise dominate the time series and obscure
// per-project signal.
local projectSuccessRatePanel = common.tsPanel(
  'Project-scoped claim success rate',
  [
    {
      expr: |||
        1 -
        (
          sum by (project, resource) (rate(ipam_allocation_failures_total{job="$job", instance=~"$instance", project!=""}[5m]))
          /
          clamp_min(sum by (project, resource) (rate(ipam_allocation_attempts_total{job="$job", instance=~"$instance", project!=""}[5m])), 1e-9)
        )
      |||,
      legend: '{{project}} / {{resource}}',
    },
  ],
  unit='percentunit',
  description='Per-project claim success ratio (1 − failures/attempts) from the IPAM-owned counters. Source of truth for SLO conversations with project owners; below 0.95 means the project is degraded.'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Latency from the consumer's perspective
// ─────────────────────────────────────────────────────────────────────────────

local p95Panel = common.tsPanel(
  'Claim CREATE p95 latency by namespace (apiserver-side)',
  [
    {
      expr: |||
        histogram_quantile(0.95,
          sum by (le, namespace, resource) (rate(apiserver_request_duration_seconds_bucket{%(sel)s}[5m]))
        )
      ||| % { sel: createSel },
      legend: '{{namespace}} / {{resource}}',
    },
  ],
  unit='s',
  description='p95 end-to-end CREATE latency observed at the apiserver. Closest available proxy for what the consumer sees.'
);

local p99Panel = common.tsPanel(
  'Claim CREATE p99 latency by namespace',
  [
    {
      expr: |||
        histogram_quantile(0.99,
          sum by (le, namespace, resource) (rate(apiserver_request_duration_seconds_bucket{%(sel)s}[5m]))
        )
      ||| % { sel: createSel },
      legend: '{{namespace}} / {{resource}}',
    },
  ],
  unit='s'
);

local p95HeadlineStat = common.statPanel(
  'p95 claim CREATE latency',
  |||
    histogram_quantile(0.95,
      sum by (le) (rate(apiserver_request_duration_seconds_bucket{%(sel)s}[5m]))
    )
  ||| % { sel: createSel },
  unit='s',
  thresholds=common.thresholds.latency,
  description='Aggregate p95 across selected namespaces. Target: < 500ms.'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Read path (list/get) — what the consumer sees while polling
// ─────────────────────────────────────────────────────────────────────────────

local listRatePanel = common.tsPanel(
  'List/Get rate by namespace',
  [
    {
      expr: 'sum by (namespace, verb) (rate(apiserver_request_total{' + nsSel + ', verb=~"list|get|watch"}[5m]))',
      legend: '{{namespace}} / {{verb}}',
    },
  ],
  unit='reqps',
  description='Read-path traffic. High list rates suggest consumers are polling instead of using watch.'
);

local listLatencyPanel = common.tsPanel(
  'List/Get p95 latency by namespace',
  [
    {
      expr: |||
        histogram_quantile(0.95,
          sum by (le, namespace, verb) (rate(apiserver_request_duration_seconds_bucket{%(sel)s, verb=~"list|get"}[5m]))
        )
      ||| % { sel: nsSel },
      legend: '{{namespace}} / {{verb}}',
    },
  ],
  unit='s'
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Address space pressure (global, not namespace-scoped)
// ─────────────────────────────────────────────────────────────────────────────

local poolUtilPanel = common.tsPanel(
  'Pool utilization (all pools)',
  [
    {
      expr: 'ipam_pool_utilization_ratio{job="$job", instance=~"$instance"}',
      legend: '{{pool_key}} ({{ip_family}})',
    },
  ],
  unit='percentunit',
  description='Per-pool utilization ratio. Pools above 80% are at risk of denying claims with 507. NOTE: pool→namespace mapping is not exported; consumers must consult the platform team to learn which pools serve their workloads.'
);

local poolUtilWarningStat = common.statPanel(
  'Pools above 80% utilized',
  'count(ipam_pool_utilization_ratio{job="$job", instance=~"$instance"} > 0.80)',
  unit='short',
  thresholds=[
    { color: 'green', value: null },
    { color: 'orange', value: 1 },
    { color: 'red', value: 5 },
  ],
  description='Heads-up indicator: how many pools are nearing exhaustion right now.'
);

local poolUtilCritStat = common.statPanel(
  'Pools above 90% utilized',
  'count(ipam_pool_utilization_ratio{job="$job", instance=~"$instance"} > 0.90)',
  unit='short',
  thresholds=common.thresholds.okWarnCrit
);

// ─────────────────────────────────────────────────────────────────────────────
// Row: Documented gaps (TODO)
// ─────────────────────────────────────────────────────────────────────────────

local gapsText =
  text.new('Instrumentation roadmap')
  + text.options.withMode('markdown')
  + text.options.withContent(|||
    Panels not yet present in this dashboard, with the metric work each
    requires. See `.claude/agents/observability.md` for the canonical
    metric spec.

    | Spec panel | Metric needed | Note |
    |---|---|---|
    | Address space utilization in scope | Per-pool→namespace mapping | Either add a `namespace` / `consumer` label to `ipam_pool_utilization_ratio`, or emit a separate `ipam_pool_consumers` metric. Beware cardinality. |

    Project-level claim success rate is now wired up via the `project` label
    on `ipam_allocation_attempts_total` / `ipam_allocation_failures_total`
    (populated from `iam.miloapis.com/parent-name` in UserInfo.Extra). See
    the "Project-scoped claim success rate" panel above. The namespace-level
    breakdowns elsewhere on this dashboard are derived from the built-in
    `apiserver_request_total{namespace=...}` label.
  |||);

// ─────────────────────────────────────────────────────────────────────────────
// Layout
// ─────────────────────────────────────────────────────────────────────────────

// In grafonnet v11.4.0 the panel `gridPos` is a plain object on the panel,
// not a builder. Set it directly via field merge.
local at(panel, x, y, w, h) =
  panel + { gridPos: { x: x, y: y, w: w, h: h } };

local rowAt(title, y) =
  row.new(title)
  + { gridPos: { x: 0, y: y, w: 24, h: 1 } };

dashboard.new('IPAM — Consumer')
+ dashboard.withUid('ipam-consumer')
+ dashboard.withDescription('Consumer-facing IPAM dashboard, complement to ipam-provider. Scope by namespace and resource. Shows claim throughput, success rate, latency, and address-space pressure for the selected namespaces. The IPAM dashboard spec names four provider-facing dashboards (ipam-overview, ipam-pool-utilization, ipam-allocation-latency, ipam-watch-health) — those live in ipam-provider; this file is the per-consumer complement.')
+ dashboard.withTags(config.dashboards.tags + ['consumer'])
+ dashboard.withTimezone(config.dashboards.timezone)
+ dashboard.withRefresh(config.dashboards.refresh)
+ dashboard.time.withFrom(config.dashboards.timeRange.from)
+ dashboard.time.withTo(config.dashboards.timeRange.to)
+ dashboard.withVariables(consumerVars)
+ dashboard.withPanels([
  rowAt('Claim activity', 0),
  at(successRateStat, 0, 1, 6, 6),
  at(claimRatePanel, 6, 1, 18, 6),
  at(claimByCodePanel, 0, 7, 12, 7),
  at(successRatePanel, 12, 7, 12, 7),
  at(projectSuccessRatePanel, 0, 14, 24, 7),

  rowAt('Claim latency (consumer-observed)', 21),
  at(p95HeadlineStat, 0, 22, 6, 7),
  at(p95Panel, 6, 22, 18, 7),
  at(p99Panel, 0, 29, 24, 6),

  rowAt('Read path (list / get / watch)', 35),
  at(listRatePanel, 0, 36, 12, 7),
  at(listLatencyPanel, 12, 36, 12, 7),

  rowAt('Address space pressure', 43),
  at(poolUtilWarningStat, 0, 44, 6, 5),
  at(poolUtilCritStat, 6, 44, 6, 5),
  at(poolUtilPanel, 12, 44, 12, 7),

  rowAt('Instrumentation roadmap', 51),
  at(gapsText, 0, 52, 24, 7),
])
