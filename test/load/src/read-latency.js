// read-latency.js
//
// Read-path latency under three simultaneous workload shapes:
//   steady  READ_VUS (10) for DURATION (3m)
//   ramp    0 -> 20 -> 50 -> 0 VUs, a third of DURATION per stage
//   spike   0 -> 100 -> 0 VUs over SPIKE_DURATION (30s), starting halfway
//           through, list-heavy
//
// Every request picks a random project and is scoped to it. The steady and
// ramp mix:
//   50%  LIST ippools                      ipam_prefix_list_ms  (the prefix list)
//   20%  LIST ipclaims in one namespace    ipam_claim_list_ms
//   20%  GET one ipclaim                   ipam_claim_get_ms
//   10%  LIST ipclaims across namespaces   ipam_cluster_list_ms
// The spike mix is 70% prefix list, 30% cross-namespace claim list.
//
// setup() creates READ_CLAIMS bound claims per project (flat IPv4 class) so the
// lists have content and the GETs have targets; teardown() deletes them.
//
// Thresholds:
//   ipam_prefix_list_ms     p95 < 200ms (CLAUDE.md acceptance criterion)
//   ipam_claim_list_ms      p95 < 200ms
//   ipam_claim_get_ms       p95 < 100ms
//   ipam_cluster_list_ms    p95 < 2000ms
//   ipam_read_success_rate  > 0.99
//
// Run setup-pools.js first.
//
// Configuration: DURATION (3m), READ_VUS (10), SPIKE_DURATION (30s),
// READ_CLAIMS (10), plus the transport and project variables in
// lib/ipam-client.js.

import { check } from 'k6';
import { Rate, Trend } from 'k6/metrics';
import {
  FLAT,
  INSECURE,
  LOAD_LABEL,
  describe,
  dur,
  ipClaim,
  ipClaimPath,
  ipPoolPath,
  ipamGet,
  ipamPost,
  namespaces,
  outcome,
  pick,
  projects,
  remove,
  seconds,
} from '../lib/ipam-client.js';
import { limit } from '../lib/ipam-client.js';

const D = seconds(__ENV.DURATION || '3m');
const SPIKE = seconds(__ENV.SPIKE_DURATION || '30s');
const READ_VUS = parseInt(__ENV.READ_VUS || '10', 10);
const READ_CLAIMS = Math.max(1, parseInt(__ENV.READ_CLAIMS || '10', 10));

const prefixList = new Trend('ipam_prefix_list_ms', true);
const claimList = new Trend('ipam_claim_list_ms', true);
const claimGet = new Trend('ipam_claim_get_ms', true);
const clusterList = new Trend('ipam_cluster_list_ms', true);
const readSuccess = new Rate('ipam_read_success_rate');

export const options = {
  insecureSkipTLSVerify: INSECURE,
  setupTimeout: __ENV.SETUP_TIMEOUT || '2m',
  teardownTimeout: __ENV.TEARDOWN_TIMEOUT || '2m',
  scenarios: {
    steady: {
      executor: 'constant-vus',
      vus: READ_VUS,
      duration: dur(D),
      exec: 'mixed',
    },
    ramp: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: dur(D / 3), target: 20 },
        { duration: dur(D / 3), target: 50 },
        { duration: dur(D / 3), target: 0 },
      ],
      exec: 'mixed',
    },
    spike: {
      executor: 'ramping-vus',
      startVUs: 0,
      startTime: dur(D / 2),
      stages: [
        { duration: dur(SPIKE / 2), target: 100 },
        { duration: dur(SPIKE / 2), target: 0 },
      ],
      exec: 'spike',
    },
  },
  thresholds: {
    ipam_prefix_list_ms: [`p(95)<${limit(200, 500)}`],
    ipam_claim_list_ms: [`p(95)<${limit(200, 500)}`],
    ipam_claim_get_ms: [`p(95)<${limit(100, 300)}`],
    ipam_cluster_list_ms: [`p(95)<${limit(2000, 2000)}`],
    ipam_read_success_rate: ['rate>0.99'],
  },
};

const PROJECTS = projects();
const NAMESPACES = namespaces();

export function setup() {
  const held = {};
  for (const project of PROJECTS) {
    held[project] = [];
    for (let i = 0; i < READ_CLAIMS; i++) {
      const ns = NAMESPACES[i % NAMESPACES.length];
      const name = `read-target-${i}`;
      const res = ipamPost(project, ipClaimPath(ns),
        ipClaim(ns, name, { className: FLAT.className, prefixLength: FLAT.claimPrefixLength }, LOAD_LABEL),
        'read_target_create');
      if (outcome(res) !== 'ok' && res.status !== 409) {
        throw new Error(`read target ${project}/${ns}/${name}: ${describe(res)}`);
      }
      held[project].push([ns, name]);
    }
  }
  return { held };
}

function record(trend, res) {
  trend.add(res.timings.duration);
  const ok = check(res, { 'read 200': (r) => r.status === 200 });
  readSuccess.add(ok);
}

export function mixed(data) {
  const project = pick(PROJECTS);
  const r = Math.random();
  if (r < 0.5) {
    record(prefixList, ipamGet(project, ipPoolPath(), 'ippool_list'));
  } else if (r < 0.7) {
    record(claimList, ipamGet(project, ipClaimPath(pick(NAMESPACES)), 'ipclaim_list'));
  } else if (r < 0.9) {
    const [ns, name] = pick(data.held[project]);
    record(claimGet, ipamGet(project, ipClaimPath(ns, name), 'ipclaim_get'));
  } else {
    record(clusterList, ipamGet(project, ipClaimPath(), 'ipclaim_list_all'));
  }
}

export function spike() {
  const project = pick(PROJECTS);
  if (Math.random() < 0.7) {
    record(prefixList, ipamGet(project, ipPoolPath(), 'ippool_list'));
  } else {
    record(clusterList, ipamGet(project, ipClaimPath(), 'ipclaim_list_all'));
  }
}

export function teardown(data) {
  for (const project of PROJECTS) {
    for (const [ns, name] of data.held[project] || []) remove(project, 'IPClaim', name, ns);
  }
}
