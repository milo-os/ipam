// cross-project-claim-throughput.js
//
// Dedicated cross-project IPPrefixClaim throughput test. Each VU acts as a
// non-owner project (any project N != 0) claiming a /28 from project 0's
// shared pool (`perf-shared-prefix`). The claim spec carries a
// `prefixRef.projectRef` pointing at project 0, and the request itself
// carries the caller's project identity in the X-Remote-Extra parent
// headers.
//
// This is the slow path that exercises whatever cross-project authorization
// (SubjectAccessReview or similar) the server adds — thresholds are wider
// than same-project throughput.
//
// Run setup-pools.js first.
//
// Configuration:
//   NAMESPACE_COUNT - Pool of namespaces (default 10)
//   PROJECT_COUNT   - Number of perf projects (default 5)
//   VUS             - Concurrent virtual users (default 10)
//   DURATION        - Test duration (default 2m)
//   IPAM_API_URL    - Apiserver URL

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createCrossProjectPrefixClaim,
  deletePrefixClaimForProject,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');
const VUS = parseInt(__ENV.VUS || '10');
const DURATION = __ENV.DURATION || '2m';
const SHARED_PREFIX = __ENV.SHARED_PREFIX || 'perf-shared-prefix';
const SHARED_OWNER = __ENV.SHARED_OWNER || projectIDFor(0);

const crossProjectLatency = new Trend('ipam_cross_project_claim_ms', true);
const crossProjectDelete = new Trend('ipam_cross_project_delete_ms', true);
const crossProjectSuccess = new Rate('ipam_cross_project_success_rate');
const crossProjectCreated = new Counter('ipam_cross_project_created');
const crossProjectDenied = new Counter('ipam_cross_project_denied');
const crossProjectErrors = new Counter('ipam_cross_project_errors');

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    cross_project: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      tags: { scenario: 'cross_project' },
    },
  },
  thresholds: {
    'ipam_cross_project_claim_ms{phase:success}': ['p(95)<1000'],
    'ipam_cross_project_success_rate': ['rate>0.95'],
    'http_req_failed': ['rate<0.05'],
  },
};

export default function () {
  if (PROJECT_COUNT < 2) {
    throw new Error('PROJECT_COUNT must be >= 2 for cross-project throughput');
  }
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  // Pick any project except project 0 (which owns the shared pool).
  const callerIdx = 1 + Math.floor(Math.random() * (PROJECT_COUNT - 1));
  const callerProject = projectIDFor(callerIdx);
  const claimName = `xclaim-${__VU}-${__ITER}`;

  const createRes = createCrossProjectPrefixClaim(
    ns,
    claimName,
    SHARED_PREFIX,
    SHARED_OWNER,
    callerProject,
    28,
  );
  const ok = check(createRes, { 'cross-project claim created': (r) => r.status === 201 });

  if (ok) {
    crossProjectCreated.add(1);
    crossProjectLatency.add(createRes.timings.duration, { phase: 'success' });
    crossProjectSuccess.add(1);
  } else if (createRes.status === 507) {
    crossProjectDenied.add(1);
    crossProjectLatency.add(createRes.timings.duration, { phase: 'denied' });
    crossProjectSuccess.add(0);
  } else {
    crossProjectErrors.add(1);
    crossProjectLatency.add(createRes.timings.duration, { phase: 'error' });
    crossProjectSuccess.add(0);
    if (__ITER < 5) {
      console.error(`cross-project claim error ${createRes.status}: ${createRes.body}`);
    }
  }

  if (ok) {
    const delRes = deletePrefixClaimForProject(ns, claimName, callerProject);
    crossProjectDelete.add(delRes.timings.duration);
    if (delRes.status !== 200 && delRes.status !== 202 && delRes.status !== 404) {
      crossProjectErrors.add(1);
    }
  }
}
