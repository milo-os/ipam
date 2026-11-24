// pool-scale.js
//
// Allocation latency must not grow with how full a pool is. The allocator
// locks one pool row whatever the pool holds, but it also reads the pool's
// existing allocations to find a free block, so a regression there shows up
// as latency that climbs with density.
//
// For each prefix length in PREFIX_STEPS (20,22,24,26,28), against IPPool
// perf-scale-pool (SCALE_CIDR, 172.16.0.0/16) in one project:
//   1. fill the pool to FILL_PCT (0.8) with claims of that length
//   2. time SAMPLES (50) create+delete pairs at that density, tagged depth:N
//   3. drain the pool
// At /28 the fill is 3276 claims, so this is the script that holds the most.
//
// It then compares the p95 at the deepest step with the p95 at the shallowest
// and fails (ipam_scale_ratio_violation) if the ratio exceeds MAX_DEPTH_RATIO
// (3). Everything runs in one VU and one iteration so the comparison sees
// every sample; a fill or sample that cannot allocate aborts the run.
//
// The class and pool are created by setup() and deleted by teardown(), with
// any claims left behind by an aborted run.
//
// Configuration: PREFIX_STEPS, FILL_PCT, SAMPLES, MAX_DEPTH_RATIO,
// SCALE_CIDR, SCALE_PROJECT (first project), FILL_BATCH (20), plus the
// transport and project variables in lib/ipam-client.js.
//
// With quota on, the project needs an ipclaims allowance of at least the
// largest fill (0.8 x 2^(28 - pool length)) plus one.

import exec from 'k6/execution';
import { Counter, Gauge, Trend } from 'k6/metrics';
import {
  INSECURE,
  LOAD_LABEL,
  batchDelete,
  batchPost,
  batchRun,
  describe,
  ensure,
  ipClaim,
  ipClaimPath,
  ipClass,
  ipPool,
  ipamDelete,
  ipamPost,
  listItems,
  namespaces,
  outcome,
  projects,
  remove,
  uniqueName,
} from '../lib/ipam-client.js';
import { limit } from '../lib/ipam-client.js';

const STEPS = (__ENV.PREFIX_STEPS || '20,22,24,26,28').split(',').map((s) => parseInt(s, 10));
const FILL_PCT = parseFloat(__ENV.FILL_PCT || '0.8');
const SAMPLES = parseInt(__ENV.SAMPLES || '50', 10);
const MAX_DEPTH_RATIO = parseFloat(__ENV.MAX_DEPTH_RATIO || '3');
const CIDR = __ENV.SCALE_CIDR || '172.16.0.0/16';
const POOL_LEN = parseInt(CIDR.split('/')[1], 10);
const FILL_BATCH = parseInt(__ENV.FILL_BATCH || '20', 10);
const PROJECT = __ENV.SCALE_PROJECT || projects()[0];
const NS = namespaces()[0];
const CLASS = 'perf-scale';
const POOL = 'perf-scale-pool';
const MIN_LEN = Math.min.apply(null, STEPS);
const MAX_LEN = Math.max.apply(null, STEPS);

for (const s of STEPS) {
  if (!(s > POOL_LEN && s <= 32)) throw new Error(`PREFIX_STEPS: ${s} does not fit in ${CIDR}`);
}

const createLatency = new Trend('ipam_scale_create_latency_ms', true);
const fillLatency = new Trend('ipam_scale_fill_latency_ms', true);
const depthRatio = new Gauge('ipam_scale_depth_ratio');
const ratioViolation = new Counter('ipam_scale_ratio_violation');

const thresholds = {
  ipam_scale_ratio_violation: ['count==0'],
};
// One entry per step so each depth's p95 appears in the end-of-test summary.
for (const s of STEPS) thresholds[`ipam_scale_create_latency_ms{depth:${s}}`] = [`p(95)<${limit(2000, 5000)}`];

export const options = {
  insecureSkipTLSVerify: INSECURE,
  batchPerHost: FILL_BATCH,
  setupTimeout: __ENV.SETUP_TIMEOUT || '2m',
  teardownTimeout: __ENV.TEARDOWN_TIMEOUT || '10m',
  scenarios: {
    sweep: {
      executor: 'per-vu-iterations',
      vus: 1,
      iterations: 1,
      maxDuration: __ENV.MAX_DURATION || '60m',
    },
  },
  thresholds,
};

function claimBody(name, len) {
  return ipClaim(NS, name, { className: CLASS, prefixLength: len }, Object.assign({ 'ipam.miloapis.com/scale-test': 'true' }, LOAD_LABEL));
}

function p95(values) {
  const v = values.slice().sort((a, b) => a - b);
  return v[Math.min(v.length - 1, Math.floor(v.length * 0.95))];
}

export function setup() {
  ensure(PROJECT, ipClass(CLASS, {
    ipFamily: 'IPv4',
    uniqueWithin: [],
    allowedPrefixLengths: { min: MIN_LEN, max: MAX_LEN },
    defaultPrefixLength: MAX_LEN,
    strategy: 'FirstFit',
    reclaimPolicy: 'Delete',
  }));
  ensure(PROJECT, ipPool(POOL, {
    cidr: CIDR,
    ipFamily: 'IPv4',
    visibility: 'consumer',
    classNames: [CLASS],
    allocation: { minPrefixLength: MIN_LEN, maxPrefixLength: MAX_LEN, strategy: 'FirstFit' },
  }));
}

function step(len) {
  const capacity = Math.pow(2, len - POOL_LEN);
  const fill = Math.floor(capacity * FILL_PCT);
  console.log(`depth /${len}: filling ${fill} of ${capacity}`);

  const names = [];
  for (let i = 0; i < fill; i++) names.push(`scale-${len}-${i}`);
  const created = [];
  for (let i = 0; i < names.length; i += FILL_BATCH) {
    const chunk = names.slice(i, i + FILL_BATCH);
    const res = batchRun(chunk.map((n) => batchPost(PROJECT, ipClaimPath(NS), claimBody(n, len), 'scale_fill')), FILL_BATCH);
    res.forEach((r, j) => {
      if (outcome(r) === 'ok') {
        created.push(chunk[j]);
        fillLatency.add(r.timings.duration, { depth: String(len) });
      } else {
        exec.test.abort(`depth /${len}: fill claim ${chunk[j]}: ${describe(r)}`);
      }
    });
  }

  const samples = [];
  for (let i = 0; i < SAMPLES; i++) {
    const name = uniqueName(`scale-${len}-s`);
    const r = ipamPost(PROJECT, ipClaimPath(NS), claimBody(name, len), 'scale_sample', { depth: String(len) });
    if (outcome(r) !== 'ok') exec.test.abort(`depth /${len}: sample claim: ${describe(r)}`);
    createLatency.add(r.timings.duration, { depth: String(len) });
    samples.push(r.timings.duration);
    ipamDelete(PROJECT, ipClaimPath(NS, name), 'scale_sample_delete');
  }

  batchRun(created.map((n) => batchDelete(PROJECT, ipClaimPath(NS, n), 'scale_drain')), FILL_BATCH);
  const v = p95(samples);
  console.log(`depth /${len}: p95 ${v.toFixed(1)}ms over ${samples.length} samples`);
  return v;
}

export default function () {
  const byDepth = {};
  for (const len of STEPS) byDepth[len] = step(len);
  const shallow = byDepth[MIN_LEN];
  const deep = byDepth[MAX_LEN];
  const ratio = deep / Math.max(shallow, 1);
  depthRatio.add(ratio);
  console.log(`p95 /${MAX_LEN} vs /${MIN_LEN}: ${deep.toFixed(1)}ms / ${shallow.toFixed(1)}ms = ${ratio.toFixed(2)}x (limit ${MAX_DEPTH_RATIO}x)`);
  if (ratio > MAX_DEPTH_RATIO) ratioViolation.add(1);
}

export function teardown() {
  const left = listItems(PROJECT, ipClaimPath(NS)).filter((c) => c.metadata.name.indexOf('scale-') === 0);
  batchRun(left.map((c) => batchDelete(PROJECT, ipClaimPath(NS, c.metadata.name), 'scale_cleanup')), FILL_BATCH);
  remove(PROJECT, 'IPPool', POOL);
  remove(PROJECT, 'IPClass', CLASS);
}
