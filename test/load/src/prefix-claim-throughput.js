// prefix-claim-throughput.js
//
// Claim throughput on the shortest allocation path: a flat IPv4 class
// (perf-v4) offered directly by one root pool (perf-v4-root, 10.0.0.0/8).
//
// Each iteration picks a random project and namespace, creates an IPClaim of
// class perf-v4 asking for a /CLAIM_PREFIX_LENGTH (default /28), records the
// create latency and outcome, and deletes the claim. Claims are not held, so
// the pool never fills and with quota on a project holds at most one claim per
// VU at a time.
//
// Thresholds (CLAUDE.md acceptance criteria):
//   ipam_claim_create_latency_ms{phase:success}  p95 < 500ms, p99 < 2000ms
//   ipam_claim_success_rate                      > 0.95
//   http_req_failed                              < 0.05
//   ipam_quota_denied                            == 0 (quota on and short of grants)
//
// Run setup-pools.js first.
//
// Configuration: VUS (10), DURATION (2m), CLAIM_PREFIX_LENGTH (28), plus the
// transport and project variables in lib/ipam-client.js.

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  FLAT,
  INSECURE,
  LOAD_LABEL,
  allocatedCIDR,
  describe,
  ipClaim,
  ipClaimPath,
  ipamDelete,
  ipamPost,
  namespaces,
  outcome,
  pick,
  projects,
  uniqueName,
} from '../lib/ipam-client.js';
import { limit } from '../lib/ipam-client.js';

const createLatency = new Trend('ipam_claim_create_latency_ms', true);
const deleteLatency = new Trend('ipam_claim_delete_latency_ms', true);
const successRate = new Rate('ipam_claim_success_rate');
const quotaDenied = new Counter('ipam_quota_denied');
const failures = new Counter('ipam_claim_failures');

export const options = {
  insecureSkipTLSVerify: INSECURE,
  vus: parseInt(__ENV.VUS || '10', 10),
  duration: __ENV.DURATION || '2m',
  thresholds: {
    'ipam_claim_create_latency_ms{phase:success}': [`p(95)<${limit(500, 6000)}`, `p(99)<${limit(2000, 10000)}`],
    ipam_claim_success_rate: ['rate>0.95'],
    http_req_failed: ['rate<0.05'],
    ipam_quota_denied: ['count==0'],
  },
};

const PROJECTS = projects();
const NAMESPACES = namespaces();
let reported = 0;

export default function () {
  const project = pick(PROJECTS);
  const ns = pick(NAMESPACES);
  const name = uniqueName('tp');

  const body = ipClaim(ns, name, { className: FLAT.className, prefixLength: FLAT.claimPrefixLength }, LOAD_LABEL);
  const res = ipamPost(project, ipClaimPath(ns), body, 'ipclaim_create');
  const result = outcome(res);
  const ok = result === 'ok' && allocatedCIDR(res) !== '';

  createLatency.add(res.timings.duration, { phase: ok ? 'success' : 'failure', outcome: result });
  successRate.add(ok);
  check(res, { 'claim Bound with a CIDR': () => ok });

  if (!ok) {
    failures.add(1, { outcome: result });
    if (result === 'quota_denied') quotaDenied.add(1);
    if (reported++ < 5) console.warn(`create ${project}/${ns}/${name}: ${describe(res)}`);
    return;
  }

  const del = ipamDelete(project, ipClaimPath(ns, name), 'ipclaim_delete');
  deleteLatency.add(del.timings.duration);
  check(del, { 'claim deleted': (r) => r.status === 200 || r.status === 202 });
}
