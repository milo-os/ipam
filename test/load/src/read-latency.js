// read-latency.js
//
// Measures read-path latency under several workload shapes:
//   - steady (10 VUs, 3m): 60% cluster-list IPPool, 20% ns list IPClaims, 20% single GET
//   - ramp (0->20->50->0 VUs over 3m): same workload mix
//   - spike (0->100->0 VUs over 30s): list-heavy
//
// Coverage extension scenarios: assert read latency for the other listable
// resources matches the IPPool list envelope. Each runs in parallel with the
// original three so the operator gets a unified summary.
//   - alloc_list:     namespaced LIST ipallocations
//   - asnpool_list:   constant LIST asnpools  (cluster scope)
//   - asnclaim_list:  namespaced LIST asnclaims
//
// Every iteration picks a random perf project and scopes all reads to that
// project's tenant context (X-Remote-Extra parent headers).
//
// Run setup-pools.js first to ensure pools and namespaces exist.
//
// Configuration:
//   NAMESPACE_COUNT - Pool of namespaces (default 10)
//   PROJECT_COUNT   - Number of perf projects (default 5)
//   IPAM_API_URL    - Apiserver URL

import { check } from 'k6';
import { Rate, Trend } from 'k6/metrics';
import {
  listIPPoolsForProject,
  listIPClaimsForProject,
  getIPPoolForProject,
  listIPAllocationsForProject,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');

const poolListLatency = new Trend('ipam_prefix_list_ms', true);
const claimGetLatency = new Trend('ipam_claim_get_ms', true);
const clusterListLatency = new Trend('ipam_cluster_list_ms', true);
// Per-resource list trends for the audit-expansion scenarios. Tagged the
// same way as the existing pool-list trend so dashboards can plot them
// side-by-side.
const ipAllocationListLatency = new Trend('ipam_ipallocation_list_ms', true);
const readSuccessRate = new Rate('ipam_read_success_rate');

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    steady: {
      executor: 'constant-vus',
      vus: 10,
      duration: '3m',
      tags: { scenario: 'steady' },
      exec: 'steady',
    },
    ramp: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '1m', target: 20 },
        { duration: '1m', target: 50 },
        { duration: '1m', target: 0 },
      ],
      tags: { scenario: 'ramp' },
      exec: 'ramp',
    },
    spike: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '15s', target: 100 },
        { duration: '15s', target: 0 },
      ],
      tags: { scenario: 'spike' },
      startTime: '3m',
      exec: 'spike',
    },
    // -- Coverage extension: dedicated list-only scenarios for the resources
    //    that previously had no read-latency coverage. Each runs against a
    //    modest VU pool for the full steady duration so we get stable p95s.
    alloc_list: {
      executor: 'constant-vus',
      vus: 5,
      duration: '3m',
      tags: { scenario: 'alloc_list' },
      exec: 'ipAllocationList',
    },
    // NOTE: asnpool_list / asnclaim_list scenarios disabled — ASNPool/ASNClaim
    // resources are not yet implemented in this branch (see commit 86aceec).
  },
  thresholds: {
    'ipam_prefix_list_ms': ['p(95)<200'],
    'ipam_claim_get_ms': ['p(95)<100'],
    'ipam_cluster_list_ms': ['p(95)<500'],
    // Audit gap-fill threshold: same envelope as the IPPool list path.
    'ipam_ipallocation_list_ms': ['p(95)<200'],
    'ipam_read_success_rate': ['rate>0.99'],
  },
};

function pickProject() {
  return projectIDFor(Math.floor(Math.random() * PROJECT_COUNT));
}

function pickProjectIdx() {
  return Math.floor(Math.random() * PROJECT_COUNT);
}

function pickWorkload() {
  const r = Math.random();
  if (r < 0.6) return 'cluster_list';
  if (r < 0.8) return 'ns_list';
  return 'single_get';
}

function doWork() {
  const projectIdx = pickProjectIdx();
  const projectID = projectIDFor(projectIdx);
  const w = pickWorkload();
  let res;
  switch (w) {
    case 'cluster_list':
      res = listIPPoolsForProject(projectID);
      clusterListLatency.add(res.timings.duration);
      break;
    case 'ns_list': {
      const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
      res = listIPClaimsForProject(ns, projectID);
      poolListLatency.add(res.timings.duration);
      break;
    }
    case 'single_get':
      res = getIPPoolForProject(`perf-prefix-${projectIdx}`, projectID);
      claimGetLatency.add(res.timings.duration);
      break;
  }
  const ok = check(res, { 'read ok': (r) => r.status === 200 });
  readSuccessRate.add(ok ? 1 : 0);
}

export function steady() { doWork(); }
export function ramp() { doWork(); }
export function spike() {
  // Spike scenario favors lists; still scopes to a random project.
  const projectID = pickProject();
  const r = Math.random();
  let res;
  if (r < 0.7) {
    res = listIPPoolsForProject(projectID);
    clusterListLatency.add(res.timings.duration);
  } else {
    const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
    res = listIPClaimsForProject(ns, projectID);
    poolListLatency.add(res.timings.duration);
  }
  const ok = check(res, { 'read ok': (r) => r.status === 200 });
  readSuccessRate.add(ok ? 1 : 0);
}

// ipAllocationList: namespaced LIST against a random perf namespace, scoped
// to a random project's tenant context.
export function ipAllocationList() {
  const projectID = pickProject();
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const res = listIPAllocationsForProject(ns, projectID);
  ipAllocationListLatency.add(res.timings.duration);
  const ok = check(res, { 'ipallocation list ok': (r) => r.status === 200 });
  readSuccessRate.add(ok ? 1 : 0);
}

// ASN list scenarios removed — ASNPool/ASNClaim resources are not implemented
// on this branch. Restore once `asnpools.ipam.miloapis.com` / `asnclaims.ipam.miloapis.com`
// are served.
