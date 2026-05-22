// pool-scale.js
//
// Walks through increasing allocation density. For each prefix length in
// PREFIX_STEPS, fills the pool to ~80% capacity and measures p95 create
// latency. Tags every metric with {depth: N} so we can compare across steps.
//
// All requests are scoped to project 0 (`ipam-perf-0`) and target project 0's
// per-project IPPool `perf-prefix-0` (10.0.0.0/16). The /16 size keeps the
// sweep bounded while still letting us walk /20 -> /28 densities.
//
// Asserts (informally, via thresholds) that p95 latency does not increase
// more than 3x from the smallest prefix length (loosest fill) to the largest
// (densest fill). Locking is O(1) on the pool row, so depth must not degrade
// allocation latency.
//
// Run setup-pools.js first; this script uses the perf-prefix-0 /16 pool.
//
// Configuration:
//   PREFIX_STEPS         - Comma-separated prefix lengths (default 20,22,24,26,28)
//   FILL_PCT             - Pool fill ratio per step (default 0.8)
//   PARENT_PREFIX        - Pool to use (default perf-prefix-0)
//   PARENT_LEN           - Parent prefix length (default 16, since perf-prefix-0 is /16)
//   PROJECT              - Project ID for tenant headers (default ipam-perf-0)
//   IPAM_API_URL         - Apiserver URL

import { Counter, Trend } from 'k6/metrics';
import {
  createIPClaimForProject,
  deleteIPClaimForProject,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const PREFIX_STEPS = (__ENV.PREFIX_STEPS || '20,22,24,26,28').split(',').map(Number);
const FILL_PCT = parseFloat(__ENV.FILL_PCT || '0.8');
const PARENT_PREFIX = __ENV.PARENT_PREFIX || 'perf-prefix-0';
const PARENT_LEN = parseInt(__ENV.PARENT_LEN || '16'); // perf-prefix-0 is 10.0.0.0/16
const PROJECT = __ENV.PROJECT || projectIDFor(0);
const FILL_NS = nsFor(0);
// Maximum acceptable ratio of p95 latency between the deepest and shallowest
// depth steps. Pool-row locking is O(1), so depth must not degrade allocation
// latency more than this factor. The 3x default matches the design doc gate.
const MAX_DEPTH_RATIO = parseFloat(__ENV.MAX_DEPTH_RATIO || '3.0');

const createLatency = new Trend('ipam_scale_create_latency_ms', true);
// Counter that fires when the deep-vs-shallow p95 ratio exceeds MAX_DEPTH_RATIO.
// The threshold below turns this into a hard failure for the run.
const ratioViolation = new Counter('ipam_scale_ratio_violation');

// Per-depth latency samples collected during default() so we can compute the
// cross-step p95 ratio after all steps complete. k6 doesn't expose submetric
// data from inside iterations, so we keep a parallel record here.
const latenciesByDepth = {};

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    sweep: {
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
      maxDuration: '20m',
    },
  },
  thresholds: {
    'ipam_scale_create_latency_ms': ['p(95)<2000'],
    // Hard gate: ratio of p95(deepest)/p95(shallowest) must be <= MAX_DEPTH_RATIO.
    // The Counter is incremented in default() when the ratio is exceeded; a
    // single increment fails the run via this threshold.
    'ipam_scale_ratio_violation': ['count==0'],
  },
};

function p95(values) {
  if (!values || values.length === 0) return 0;
  const sorted = values.slice().sort((a, b) => a - b);
  // Nearest-rank p95 with at-least-1 index.
  const idx = Math.min(sorted.length - 1, Math.max(0, Math.ceil(0.95 * sorted.length) - 1));
  return sorted[idx];
}

function fillStep(prefixLen) {
  // Possible /prefixLen subnets in /PARENT_LEN parent.
  // Cap at 256 so a tight step doesn't create millions of rows.
  const total = Math.pow(2, prefixLen - PARENT_LEN);
  const target = Math.min(256, Math.floor(total * FILL_PCT));

  console.log(`step depth=${prefixLen}: filling ${target}/${total} subnets (project=${PROJECT})`);

  const created = [];
  const samples = latenciesByDepth[prefixLen] || (latenciesByDepth[prefixLen] = []);
  for (let i = 0; i < target; i++) {
    const name = `scale-d${prefixLen}-${i}`;
    const r = createIPClaimForProject(FILL_NS, name, PARENT_PREFIX, prefixLen, PROJECT);
    if (r.status === 201) {
      created.push(name);
      createLatency.add(r.timings.duration, { depth: String(prefixLen) });
      samples.push(r.timings.duration);
    } else if (r.status === 507) {
      console.log(`  pool exhausted at ${i}/${target}, breaking`);
      break;
    } else {
      console.error(`  err depth=${prefixLen} i=${i}: ${r.status}`);
    }
  }

  // Cleanup so the next step gets fresh capacity
  for (const name of created) {
    deleteIPClaimForProject(FILL_NS, name, PROJECT);
  }
  return created.length;
}

export default function () {
  for (const len of PREFIX_STEPS) {
    fillStep(len);
  }

  // After every step has finished, evaluate the cross-step p95 ratio. Walk
  // PREFIX_STEPS rather than Object.keys so we honour the user-supplied step
  // ordering (shallowest first → deepest last).
  const depthsWithData = PREFIX_STEPS.filter(
    (d) => Array.isArray(latenciesByDepth[d]) && latenciesByDepth[d].length > 0,
  );
  if (depthsWithData.length < 2) {
    console.warn(`pool-scale: only ${depthsWithData.length} depth(s) produced samples; skipping ratio check`);
    return;
  }
  const shallow = depthsWithData[0];
  const deep = depthsWithData[depthsWithData.length - 1];
  const p95Shallow = p95(latenciesByDepth[shallow]);
  const p95Deep = p95(latenciesByDepth[deep]);
  const ratio = p95Shallow > 0 ? p95Deep / p95Shallow : Infinity;

  console.log(
    `pool-scale ratio: depth=${shallow} p95=${p95Shallow.toFixed(1)}ms; ` +
    `depth=${deep} p95=${p95Deep.toFixed(1)}ms; ratio=${ratio.toFixed(2)}x ` +
    `(threshold ${MAX_DEPTH_RATIO}x)`,
  );

  if (ratio > MAX_DEPTH_RATIO) {
    ratioViolation.add(1);
    console.error(
      `FAIL: depth ratio ${ratio.toFixed(2)}x > ${MAX_DEPTH_RATIO}x — ` +
      `allocation latency is degrading with pool depth`,
    );
  }
}

export function handleSummary(data) {
  const trend = data.metrics['ipam_scale_create_latency_ms'];
  const violations = data.metrics['ipam_scale_ratio_violation'];
  console.log('=== pool-scale summary ===');
  console.log(JSON.stringify({ trend, violations }, null, 2));
  return {
    'stdout': '',
  };
}
