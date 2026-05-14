// common.libsonnet — Shared panel builders, variable constructors, and
// threshold presets for all IPAM Grafana dashboards.
//
// Import path (from any dashboard file in dashboards/jsonnet/):
//   local common = import 'lib/common.libsonnet';
//
// Grafonnet is vendored under dashboards/jsonnet/vendor/ and compiled with:
//   jsonnet -J vendor <dashboard>.jsonnet

local g = import 'github.com/grafana/grafonnet/gen/grafonnet-v11.4.0/main.libsonnet';

local dashboard = g.dashboard;
local variable = dashboard.variable;
local row = g.panel.row;
local ts = g.panel.timeSeries;
local stat = g.panel.stat;
local heatmap = g.panel.heatmap;
local gauge = g.panel.gauge;
local table = g.panel.table;
local prom = g.query.prometheus;
local util = g.util;

// ─────────────────────────────────────────────────────────────────────────────
// Variables
// ─────────────────────────────────────────────────────────────────────────────

local datasourceVar =
  variable.datasource.new('datasource', 'prometheus')
  + variable.datasource.generalOptions.withLabel('Data source')
  + variable.datasource.withRegex('');

local jobVar =
  variable.query.new('job')
  + variable.query.generalOptions.withLabel('Job')
  + variable.query.withDatasourceFromVariable(datasourceVar)
  + variable.query.queryTypes.withLabelValues('job', 'apiserver_request_total')
  + variable.query.withRefresh('time')
  + variable.query.withRegex('/.*ipam.*/')
  + variable.query.selectionOptions.withIncludeAll(false);

local instanceVar =
  variable.query.new('instance')
  + variable.query.generalOptions.withLabel('Instance')
  + variable.query.withDatasourceFromVariable(datasourceVar)
  + variable.query.queryTypes.withLabelValues('instance', 'apiserver_request_total{job="$job"}')
  + variable.query.withRefresh('time')
  + variable.query.selectionOptions.withIncludeAll(true, '.*')
  + variable.query.selectionOptions.withMulti(true);

// ─────────────────────────────────────────────────────────────────────────────
// Selectors
// ─────────────────────────────────────────────────────────────────────────────

local ds = '$datasource';
// Selector used in all apiserver_* metrics (scraped from the IPAM pods).
local jobSel = 'job="$job", instance=~"$instance"';
// IPAM custom metrics share the same job label as apiserver_* metrics because
// they are exposed on the same /metrics endpoint.
local ipamSel = 'job="$job", instance=~"$instance"';

// ─────────────────────────────────────────────────────────────────────────────
// Panel helpers
// ─────────────────────────────────────────────────────────────────────────────

// Build an array of prometheus targets from [{expr, legend}, ...].
local targets(items) = [
  prom.new(ds, item.expr) + prom.withLegendFormat(item.legend)
  for item in items
];

// Time-series panel with sensible defaults: table legend, shared tooltip,
// no points, light fill.
local tsPanel(title, items, unit='short', stack=false, description='') =
  ts.new(title)
  + (if description != '' then ts.panelOptions.withDescription(description) else {})
  + ts.queryOptions.withTargets(targets(items))
  + ts.standardOptions.withUnit(unit)
  + ts.options.legend.withDisplayMode('table')
  + ts.options.legend.withPlacement('bottom')
  + ts.options.legend.withCalcs(['lastNotNull', 'max', 'mean'])
  + ts.options.tooltip.withMode('multi')
  + ts.options.tooltip.withSort('desc')
  + ts.fieldConfig.defaults.custom.withFillOpacity(if stack then 30 else 10)
  + ts.fieldConfig.defaults.custom.withShowPoints('never')
  + ts.fieldConfig.defaults.custom.withLineWidth(1)
  + (if stack
     then ts.fieldConfig.defaults.custom.stacking.withMode('normal')
     else {});

// Single-value stat panel with area sparkline.
local statPanel(title, expr, unit='short', thresholds=null, description='') =
  stat.new(title)
  + (if description != '' then stat.panelOptions.withDescription(description) else {})
  + stat.queryOptions.withTargets([prom.new(ds, expr)])
  + stat.standardOptions.withUnit(unit)
  + stat.options.withColorMode('value')
  + stat.options.withGraphMode('area')
  + stat.options.withJustifyMode('auto')
  + stat.options.reduceOptions.withCalcs(['lastNotNull'])
  + (if thresholds != null
     then stat.standardOptions.thresholds.withMode('absolute')
          + stat.standardOptions.thresholds.withSteps(thresholds)
     else {});

// Gauge panel (0–1 ratio).
local gaugePanel(title, expr, unit='percentunit', thresholds=null, description='') =
  gauge.new(title)
  + (if description != '' then gauge.panelOptions.withDescription(description) else {})
  + gauge.queryOptions.withTargets([prom.new(ds, expr) + prom.withLegendFormat('{{pool_key}}')])
  + gauge.standardOptions.withUnit(unit)
  + gauge.standardOptions.withMin(0)
  + gauge.standardOptions.withMax(1)
  + (if thresholds != null
     then gauge.standardOptions.thresholds.withMode('absolute')
          + gauge.standardOptions.thresholds.withSteps(thresholds)
     else {});

// Heatmap from native Prometheus histogram buckets.
local heatPanel(title, expr, unit='s', description='') =
  heatmap.new(title)
  + (if description != '' then heatmap.panelOptions.withDescription(description) else {})
  + heatmap.queryOptions.withTargets([
      prom.new(ds, expr)
      + prom.withFormat('heatmap')
      + prom.withLegendFormat('{{le}}'),
    ])
  + heatmap.options.withCalculate(false)
  + heatmap.options.yAxis.withUnit(unit)
  + heatmap.options.color.withScheme('Spectral')
  + heatmap.options.color.withMode('scheme');

// ─────────────────────────────────────────────────────────────────────────────
// Threshold presets
// ─────────────────────────────────────────────────────────────────────────────

local okWarnCritThresholds = [
  { color: 'green', value: null },
  { color: 'orange', value: 1 },
  { color: 'red', value: 5 },
];

local latencyThresholds = [
  { color: 'green', value: null },
  { color: 'orange', value: 0.2 },
  { color: 'red', value: 0.5 },
];

local utilizationThresholds = [
  { color: 'green', value: null },
  { color: 'orange', value: 0.80 },
  { color: 'red', value: 1.0 },
];

local successRateThresholds = [
  { color: 'red', value: null },
  { color: 'orange', value: 0.90 },
  { color: 'green', value: 0.95 },
];

local booleanUpThresholds = [
  { color: 'red', value: null },
  { color: 'green', value: 1 },
];

// ─────────────────────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────────────────────

{
  // Grafonnet root
  g: g,

  // Variable constructors
  datasourceVar: datasourceVar,
  jobVar: jobVar,
  instanceVar: instanceVar,
  defaultVars: [datasourceVar, jobVar, instanceVar],

  // Selectors
  ds: ds,
  jobSel: jobSel,
  ipamSel: ipamSel,

  // Panel builders
  tsPanel: tsPanel,
  statPanel: statPanel,
  gaugePanel: gaugePanel,
  heatPanel: heatPanel,

  // Threshold presets
  thresholds: {
    okWarnCrit: okWarnCritThresholds,
    latency: latencyThresholds,
    utilization: utilizationThresholds,
    successRate: successRateThresholds,
    booleanUp: booleanUpThresholds,
  },
}
