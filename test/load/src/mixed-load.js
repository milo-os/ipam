// mixed-load.js
//
// Simulates real-world IPAM traffic: concurrent reads and writes, including
// provisioning bursts and read spikes, all running simultaneously.
//
// Scenarios (all concurrent):
//   write_steady  - 5 VUs × 3m constant writer (create + delete claims)
//   read_steady   - 10 VUs × 3m constant reader (list/get mix)
//   write_burst   - 0→20→0 VUs ramping writer, starts at t=1m (provisioning spike)
//   read_spike    - 0→50→0 VUs ramping reader, starts at t=2m (stresses cacher)
//
// Assumes setup-pools.js has already been run (`task test/load:setup`).
//
// Configuration:
//   IPAM_API_URL      - Apiserver URL (default: http://localhost:8001)
//   NAMESPACE_COUNT   - Pool of namespaces (must match setup, default 10)
//   PROJECT_COUNT     - Number of perf projects (must match setup, default 5)
//   K6_INSECURE_SKIP_TLS_VERIFY - Skip TLS (default: true)

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createIPClaimForProject,
  deleteIPClaimForProject,
  listIPPoolsForProject,
  listIPClaimsForProject,
  getIPPoolForProject,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT   = parseInt(__ENV.PROJECT_COUNT   || '5');

// --- Custom metrics ---

const claimCreateLatency = new Trend('ipam_claim_create_latency_ms', true);
const claimDeleteLatency = new Trend('ipam_claim_delete_latency_ms', true);
const claimSuccessRate   = new Rate('ipam_claim_success_rate');
const claimsCreated      = new Counter('ipam_claims_created');
const claimsDenied       = new Counter('ipam_claims_denied');
const claimErrors        = new Counter('ipam_claim_errors');

const poolListLatency    = new Trend('ipam_prefix_list_ms', true);
const claimGetLatency    = new Trend('ipam_claim_get_ms', true);
const clusterListLatency = new Trend('ipam_cluster_list_ms', true);
const readSuccessRate    = new Rate('ipam_read_success_rate');

// --- Options ---

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    // Constant baseline write load for the full test duration.
    write_steady: {
      executor: 'constant-vus',
      vus: 5,
      duration: '3m',
      tags: { scenario: 'write_steady' },
      exec: 'writeScenario',
    },
    // Constant baseline read load for the full test duration.
    read_steady: {
      executor: 'constant-vus',
      vus: 10,
      duration: '3m',
      tags: { scenario: 'read_steady' },
      exec: 'readScenario',
    },
    // Provisioning burst: ramps up mid-test to stress the allocator while
    // the steady read load is already running.
    write_burst: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '20s', target: 20 },
        { duration: '20s', target: 20 },
        { duration: '20s', target: 0 },
      ],
      startTime: '1m',
      tags: { scenario: 'write_burst' },
      exec: 'writeScenario',
    },
    // Read spike: hammers the cacher/watcher while both steady writers and
    // the tail of write_burst are still active.
    read_spike: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '15s', target: 50 },
        { duration: '15s', target: 0 },
      ],
      startTime: '2m',
      tags: { scenario: 'read_spike' },
      exec: 'readScenario',
    },
  },
  thresholds: {
    'ipam_claim_create_latency_ms{phase:success}': ['p(95)<500'],
    'ipam_prefix_list_ms':    ['p(95)<200'],
    'ipam_claim_get_ms':      ['p(95)<100'],
    'ipam_claim_success_rate':['rate>0.95'],
    'ipam_read_success_rate': ['rate>0.99'],
    'http_req_failed':        ['rate<0.05'],
  },
};

// --- Helpers ---

function pickProjectIdx() {
  return Math.floor(Math.random() * PROJECT_COUNT);
}

function pickNs() {
  return nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
}

// recordCreate records latency and success/failure for a claim creation
// response. Returns true on HTTP 201.
function recordCreate(res) {
  const ok = check(res, { 'claim created': (r) => r.status === 201 });
  if (ok) {
    claimsCreated.add(1);
    claimCreateLatency.add(res.timings.duration, { phase: 'success' });
    claimSuccessRate.add(1);
  } else if (res.status === 507) {
    claimsDenied.add(1);
    claimCreateLatency.add(res.timings.duration, { phase: 'denied' });
    claimSuccessRate.add(0);
  } else {
    claimErrors.add(1);
    claimCreateLatency.add(res.timings.duration, { phase: 'error' });
    claimSuccessRate.add(0);
    if (__ITER < 5) {
      console.error(`claim create error ${res.status}: ${res.body}`);
    }
  }
  return ok;
}

// --- Exported scenario functions ---

// writeScenario: create a /28 IPClaim then delete it. Used by both
// write_steady (baseline) and write_burst (spike) scenarios.
export function writeScenario() {
  const projectIdx = pickProjectIdx();
  const projectID  = projectIDFor(projectIdx);
  const ns         = pickNs();
  const poolName   = `perf-prefix-${projectIdx}`;
  const claimName  = `mixed-${__VU}-${__ITER}`;

  const createRes = createIPClaimForProject(ns, claimName, poolName, 28, projectID);
  const ok = recordCreate(createRes);

  if (ok) {
    const delRes = deleteIPClaimForProject(ns, claimName, projectID);
    claimDeleteLatency.add(delRes.timings.duration);
    if (delRes.status !== 200 && delRes.status !== 202 && delRes.status !== 404) {
      claimErrors.add(1);
    }
  }
}

// readScenario: randomly picks one of three read operations weighted to match
// real operator traffic patterns. Used by both read_steady and read_spike.
//   60% — cluster-scoped IPPool list (pool utilisation check)
//   20% — namespace-scoped IPClaim list (operator reconcile)
//   20% — single IPPool GET (read pool state for a specific pool)
export function readScenario() {
  const projectIdx = pickProjectIdx();
  const projectID  = projectIDFor(projectIdx);
  const r          = Math.random();
  let res;

  if (r < 0.6) {
    res = listIPPoolsForProject(projectID);
    clusterListLatency.add(res.timings.duration);
  } else if (r < 0.8) {
    const ns = pickNs();
    res = listIPClaimsForProject(ns, projectID);
    poolListLatency.add(res.timings.duration);
  } else {
    res = getIPPoolForProject(`perf-prefix-${projectIdx}`, projectID);
    claimGetLatency.add(res.timings.duration);
  }

  const ok = check(res, { 'read ok': (r) => r.status === 200 });
  readSuccessRate.add(ok ? 1 : 0);
}
