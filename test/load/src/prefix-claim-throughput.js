// prefix-claim-throughput.js
//
// Measures the hot path of the IPAM service: IPPrefixClaim creation throughput
// and latency under sustained load, with a multi-tenant traffic mix.
//
// 90% of iterations: same-project claim — VU picks a random project N, sends
//                    a claim against perf-prefix-N with the project N tenant
//                    headers (no projectRef in spec).
// 10% of iterations: cross-project claim — VU picks a random project N != 0
//                    and claims from project 0's shared pool (perf-shared-prefix)
//                    using its own project identity in headers and projectRef
//                    in the claim spec pointing at project 0.
//                    Reflects real-world usage: cross-project claiming is only
//                    used for public IP address provisioning.
//
// Run setup-pools.js first to provision per-project + shared pools.
//
// Configuration:
//   NAMESPACE_COUNT - Pool of namespaces (must match setup, default 10)
//   PROJECT_COUNT   - Number of perf projects (must match setup, default 5)
//   VUS             - Concurrent virtual users (default 10)
//   DURATION        - Test duration (default 2m)
//   IPAM_API_URL    - Apiserver URL (default localhost:8001)
//   CROSS_RATIO     - Fraction of iterations that are cross-project (default 0.1)

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createPrefixClaimForProject,
  deletePrefixClaimForProject,
  createCrossProjectPrefixClaim,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');
const VUS = parseInt(__ENV.VUS || '10');
const DURATION = __ENV.DURATION || '2m';
const CROSS_RATIO = parseFloat(__ENV.CROSS_RATIO || '0.1');
const SHARED_PREFIX = 'perf-shared-prefix';
const SHARED_OWNER_PROJECT = projectIDFor(0);

const claimCreateLatency = new Trend('ipam_claim_create_latency_ms', true);
const claimDeleteLatency = new Trend('ipam_claim_delete_latency_ms', true);
const claimSuccessRate = new Rate('ipam_claim_success_rate');
const claimsCreated = new Counter('ipam_claims_created');
const claimsDenied = new Counter('ipam_claims_denied');
const claimErrors = new Counter('ipam_claim_errors');

const sameProjectLatency = new Trend('ipam_same_project_claim_ms', true);
const crossProjectLatency = new Trend('ipam_cross_project_claim_ms', true);

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    steady_throughput: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      tags: { scenario: 'steady' },
    },
  },
  thresholds: {
    'ipam_claim_create_latency_ms{phase:success}': ['p(95)<500', 'p(99)<2000'],
    'ipam_claim_success_rate': ['rate>0.95'],
    'http_req_failed': ['rate<0.05'],
  },
};

function recordCreate(res, mode) {
  const ok = check(res, { [`${mode} claim created`]: (r) => r.status === 201 });
  if (ok) {
    claimsCreated.add(1);
    claimCreateLatency.add(res.timings.duration, { phase: 'success', mode });
    claimSuccessRate.add(1);
    if (mode === 'same') sameProjectLatency.add(res.timings.duration);
    else crossProjectLatency.add(res.timings.duration);
  } else if (res.status === 507) {
    claimsDenied.add(1);
    claimCreateLatency.add(res.timings.duration, { phase: 'denied', mode });
    claimSuccessRate.add(0);
  } else {
    claimErrors.add(1);
    claimCreateLatency.add(res.timings.duration, { phase: 'error', mode });
    claimSuccessRate.add(0);
    if (__ITER < 5) {
      console.error(`${mode} claim error ${res.status}: ${res.body}`);
    }
  }
  return ok;
}

export default function () {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const claimName = `claim-${__VU}-${__ITER}`;
  const isCross = Math.random() < CROSS_RATIO;

  let createRes;
  let mode;
  let callerProject;

  if (isCross) {
    mode = 'cross';
    // Pick any project except project 0 (which owns the shared pool).
    const callerIdx = 1 + Math.floor(Math.random() * Math.max(1, PROJECT_COUNT - 1));
    callerProject = projectIDFor(callerIdx);
    createRes = createCrossProjectPrefixClaim(
      ns,
      claimName,
      SHARED_PREFIX,
      SHARED_OWNER_PROJECT,
      callerProject,
      28,
    );
  } else {
    mode = 'same';
    const projectIdx = Math.floor(Math.random() * PROJECT_COUNT);
    callerProject = projectIDFor(projectIdx);
    const poolName = `perf-prefix-${projectIdx}`;
    createRes = createPrefixClaimForProject(ns, claimName, poolName, 28, callerProject);
  }

  const ok = recordCreate(createRes, mode);

  if (ok) {
    const delRes = deletePrefixClaimForProject(ns, claimName, callerProject);
    claimDeleteLatency.add(delRes.timings.duration);
    if (delRes.status !== 200 && delRes.status !== 202 && delRes.status !== 404) {
      claimErrors.add(1);
    }
  }
}
