// read-latency.js
//
// Measures read-path latency under several workload shapes:
//   - steady (10 VUs, 3m): 60% cluster-list IPPrefix, 20% ns list IPPrefixClaims, 20% single GET
//   - ramp (0->20->50->0 VUs over 3m): same workload mix
//   - spike (0->100->0 VUs over 30s): list-heavy
//
// Coverage extension scenarios (audit Task #11): assert read latency for the
// other listable resources matches the IPPrefix list envelope. Each runs in
// parallel with the original three so the operator gets a unified summary.
//   - addr_list:       constant LIST ipaddresses (namespaced)
//   - asnpool_list:    constant LIST asnpools    (cluster scope)
//   - asnclaim_list:   constant LIST asnclaims   (namespaced)
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
  listPrefixesForProject,
  listPrefixClaimsForProject,
  getPrefixForProject,
  listIPAddressesForProject,
  listASNPoolsForProject,
  listASNClaimsForProject,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');

const prefixListLatency = new Trend('ipam_prefix_list_ms', true);
const claimGetLatency = new Trend('ipam_claim_get_ms', true);
const clusterListLatency = new Trend('ipam_cluster_list_ms', true);
// New per-resource list trends for the audit-expansion scenarios. Tagged the
// same way as the existing prefix-list trend so dashboards can plot them
// side-by-side.
const ipAddressListLatency = new Trend('ipam_ipaddress_list_ms', true);
const asnPoolListLatency = new Trend('ipam_asnpool_list_ms', true);
const asnClaimListLatency = new Trend('ipam_asnclaim_list_ms', true);
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
    addr_list: {
      executor: 'constant-vus',
      vus: 5,
      duration: '3m',
      tags: { scenario: 'addr_list' },
      exec: 'ipAddressList',
    },
    asnpool_list: {
      executor: 'constant-vus',
      vus: 5,
      duration: '3m',
      tags: { scenario: 'asnpool_list' },
      exec: 'asnPoolList',
    },
    asnclaim_list: {
      executor: 'constant-vus',
      vus: 5,
      duration: '3m',
      tags: { scenario: 'asnclaim_list' },
      exec: 'asnClaimList',
    },
  },
  thresholds: {
    'ipam_prefix_list_ms': ['p(95)<200'],
    'ipam_claim_get_ms': ['p(95)<100'],
    'ipam_cluster_list_ms': ['p(95)<500'],
    // Audit gap-fill thresholds: same envelope as the IPPrefix list path.
    'ipam_ipaddress_list_ms': ['p(95)<200'],
    'ipam_asnpool_list_ms': ['p(95)<200'],
    'ipam_asnclaim_list_ms': ['p(95)<200'],
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
      res = listPrefixesForProject(projectID);
      clusterListLatency.add(res.timings.duration);
      break;
    case 'ns_list': {
      const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
      res = listPrefixClaimsForProject(ns, projectID);
      prefixListLatency.add(res.timings.duration);
      break;
    }
    case 'single_get':
      res = getPrefixForProject(`perf-prefix-${projectIdx}`, projectID);
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
    res = listPrefixesForProject(projectID);
    clusterListLatency.add(res.timings.duration);
  } else {
    const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
    res = listPrefixClaimsForProject(ns, projectID);
    prefixListLatency.add(res.timings.duration);
  }
  const ok = check(res, { 'read ok': (r) => r.status === 200 });
  readSuccessRate.add(ok ? 1 : 0);
}

// ipAddressList: namespaced LIST against a random perf namespace, scoped to
// a random project's tenant context.
export function ipAddressList() {
  const projectID = pickProject();
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const res = listIPAddressesForProject(ns, projectID);
  ipAddressListLatency.add(res.timings.duration);
  const ok = check(res, { 'ipaddress list ok': (r) => r.status === 200 });
  readSuccessRate.add(ok ? 1 : 0);
}

// asnPoolList: cluster-scoped LIST. ASNPools are global; the project headers
// are still applied so the auth path matches production traffic.
export function asnPoolList() {
  const projectID = pickProject();
  const res = listASNPoolsForProject(projectID);
  asnPoolListLatency.add(res.timings.duration);
  const ok = check(res, { 'asnpool list ok': (r) => r.status === 200 });
  readSuccessRate.add(ok ? 1 : 0);
}

// asnClaimList: namespaced LIST against a random perf namespace.
export function asnClaimList() {
  const projectID = pickProject();
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const res = listASNClaimsForProject(ns, projectID);
  asnClaimListLatency.add(res.timings.duration);
  const ok = check(res, { 'asnclaim list ok': (r) => r.status === 200 });
  readSuccessRate.add(ok ? 1 : 0);
}
