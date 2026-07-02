// class-claim-throughput.js
//
// Measures the IPClass hot path: IPClaim creation throughput and latency when
// claims select a class by name (spec.className) rather than naming a pool.
// This is the standard claim path introduced by the IPClass enhancement
// (docs/enhancements/ip-class.md) — the consumer names a *kind* of address
// space and IPAM picks the backing pool server-side.
//
// Each VU: pick a random project N, POST an IPClaim with className=perf-class
//          under project N's tenant headers (no poolRef), record latency +
//          success, then DELETE the claim.
//
// This script is self-contained: setup() provisions the class and one backing
// pool per project; teardown() removes them. Namespaces (ipam-perf-<n>) are
// expected to already exist from setup-pools.js (task test/load:setup).
//
// Thresholds mirror prefix-claim-throughput.js (p95 < 500ms, success > 0.95)
// so the class path is held to the same bar as direct pool claims — the
// enhancement's scalability note is that class resolution adds only a lookup
// and a scoped pool search, not a change to the atomic allocation guarantee.
//
// Configuration:
//   NAMESPACE_COUNT - Pool of namespaces (must match setup, default 10)
//   PROJECT_COUNT   - Number of perf projects (default 5)
//   VUS             - Concurrent virtual users (default 10)
//   DURATION        - Test duration (default 2m)
//   PREFIX_LENGTH   - Requested prefix size (default 28)
//   IPAM_API_URL    - Apiserver URL (default localhost:8001)

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createIPClass,
  deleteIPClass,
  createIPPool,
  deleteIPPool,
  createIPClaimWithClassForProject,
  deleteIPClaimForProject,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');
const VUS = parseInt(__ENV.VUS || '10');
const DURATION = __ENV.DURATION || '2m';
const PREFIX_LENGTH = parseInt(__ENV.PREFIX_LENGTH || '28');

const CLASS_NAME = 'perf-class';
// Allowed prefix lengths for the class; PREFIX_LENGTH must fall inside this.
const CLASS_MIN_LEN = 20;
const CLASS_MAX_LEN = 28;

// backingPoolName / backingPoolCIDR give each project its own pool that offers
// capacity to perf-class. The 100.64.0.0/10 (CGNAT) block keeps these clear of
// the 10.x per-project pools that setup-pools.js provisions.
function backingPoolName(n) {
  return `perf-class-pool-${n}`;
}
function backingPoolCIDR(n) {
  return `100.${64 + (n % 64)}.0.0/16`;
}

const claimCreateLatency = new Trend('ipam_claim_create_latency_ms', true);
const claimDeleteLatency = new Trend('ipam_claim_delete_latency_ms', true);
const claimSuccessRate = new Rate('ipam_claim_success_rate');
const claimsCreated = new Counter('ipam_claims_created');
const claimsDenied = new Counter('ipam_claims_denied');
const claimErrors = new Counter('ipam_claim_errors');

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

export function setup() {
  // Platform-owned policy object. visibility=consumer keeps it per-project,
  // matching how the per-project backing pools below are scoped.
  const c = createIPClass(CLASS_NAME, {
    ipFamily: 'IPv4',
    strategy: 'FirstFit',
    minLen: CLASS_MIN_LEN,
    maxLen: CLASS_MAX_LEN,
    defaultPrefixLength: 28,
    reclaimPolicy: 'Delete',
    visibility: 'consumer',
  });
  if (c.status !== 201 && c.status !== 409) {
    throw new Error(`IPClass create failed: ${c.status} ${c.body}`);
  }

  // One backing pool per project, each offering its capacity to the class.
  let pools = 0;
  for (let n = 0; n < PROJECT_COUNT; n++) {
    const name = backingPoolName(n);
    const r = createIPPool(name, backingPoolCIDR(n), {
      ipFamily: 'IPv4',
      visibility: 'consumer',
      minLen: CLASS_MIN_LEN,
      maxLen: CLASS_MAX_LEN,
      strategy: 'FirstFit',
      classNames: [CLASS_NAME],
    });
    if (r.status === 201 || r.status === 409) {
      pools++;
    } else {
      console.error(`backing pool ${name} create failed: ${r.status} ${r.body}`);
    }
  }
  console.log(`setup complete: class ${CLASS_NAME}, ${pools}/${PROJECT_COUNT} backing pools`);
  return { pools };
}

function recordCreate(res) {
  const ok = check(res, { 'class claim created': (r) => r.status === 201 });
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
      console.error(`class claim error ${res.status}: ${res.body}`);
    }
  }
  return ok;
}

export default function () {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const claimName = `class-claim-${__VU}-${__ITER}`;
  const projectIdx = Math.floor(Math.random() * PROJECT_COUNT);
  const callerProject = projectIDFor(projectIdx);

  const createRes = createIPClaimWithClassForProject(
    ns,
    claimName,
    CLASS_NAME,
    PREFIX_LENGTH,
    callerProject,
  );

  if (recordCreate(createRes)) {
    const delRes = deleteIPClaimForProject(ns, claimName, callerProject);
    claimDeleteLatency.add(delRes.timings.duration);
    if (delRes.status !== 200 && delRes.status !== 202 && delRes.status !== 404) {
      claimErrors.add(1);
    }
  }
}

export function teardown() {
  for (let n = 0; n < PROJECT_COUNT; n++) {
    deleteIPPool(backingPoolName(n));
  }
  deleteIPClass(CLASS_NAME);
  console.log('teardown complete');
}
