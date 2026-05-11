// ipaddress-claim-concurrent.js
//
// Stress-tests IPAddressClaim concurrency (audit Task #11 gap-fill: parallel
// to concurrent-claims.js, but exercises the IPAddressClaim path which had
// no dedicated concurrency coverage).
//
// Concurrent IPAddressClaim CREATEs from many VUs against a single pool must
// always produce non-overlapping addresses. The SELECT...FOR UPDATE pool-row
// lock guarantees this regardless of parallelism.
//
// Approach:
//   - setup() creates a dedicated pool (default /22 = 1024 addresses).
//   - Each VU iteration creates an IPAddressClaim, captures status.allocatedIP,
//     then immediately deletes it so the pool stays under capacity.
//   - A separate uniqueness scenario fills the pool sequentially and asserts
//     every status.allocatedIP is unique.
//
// Thresholds (audit spec):
//   - p95 create latency < 500ms, p99 < 2000ms (success phase)
//   - success rate > 0.95
//   - http_req_failed < 5%
//   - ipam_ipaddr_duplicate == 0      (uniqueness assertion)
//   - ipam_ipaddr_missing_status == 0 (status.allocatedIP must be populated)
//
// Configuration:
//   VUS             - Concurrent virtual users (default 50)
//   DURATION        - Test duration (default 2m)
//   NAMESPACE_COUNT - Namespace pool size (default 10, must match setup-pools.js)
//   POOL_CIDR       - Parent CIDR for the dedicated pool (default 10.250.0.0/22)
//   IPAM_API_URL    - Apiserver URL

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createPrefixClass,
  createPrefix,
  createIPAddressClaimForProject,
  deleteIPAddressClaimForProject,
  ipamDelete,
  prefixPath,
  prefixClassPath,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const VUS = parseInt(__ENV.VUS || '50');
const DURATION = __ENV.DURATION || '2m';
const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const POOL_CIDR = __ENV.POOL_CIDR || '10.250.0.0/22';

const CLASS_NAME = 'perf-ipaddr-concurrent';
const POOL_NAME = 'perf-ipaddr-concurrent-pool';
const PROJECT = projectIDFor(0);

// /22 = 1024 addresses. Bounded, but well above the per-VU iteration count
// expected in a 2m run at VUS=50 since each iteration releases its slot.
const POOL_SIZE = 1024;

const createLatency = new Trend('ipam_ipaddr_create_latency_ms', true);
const deleteLatency = new Trend('ipam_ipaddr_delete_latency_ms', true);
const successRate = new Rate('ipam_ipaddr_success_rate');
const created = new Counter('ipam_ipaddr_created');
const denied = new Counter('ipam_ipaddr_denied');
const errors = new Counter('ipam_ipaddr_errors');
const missingStatus = new Counter('ipam_ipaddr_missing_status');
const uniqueAllocated = new Counter('ipam_ipaddr_unique_allocated');
const duplicates = new Counter('ipam_ipaddr_duplicate');

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    concurrent_burst: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      tags: { scenario: 'concurrent' },
      exec: 'concurrent',
    },
    uniqueness_check: {
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
      maxDuration: '5m',
      // Run after the burst finishes so the pool is empty.
      startTime: DURATION,
      tags: { scenario: 'uniqueness' },
      exec: 'uniqueness',
    },
  },
  thresholds: {
    'ipam_ipaddr_create_latency_ms{phase:success}': ['p(95)<500', 'p(99)<2000'],
    'ipam_ipaddr_success_rate': ['rate>0.95'],
    'http_req_failed': ['rate<0.05'],
    // Hard guards from the audit spec.
    'ipam_ipaddr_missing_status': ['count==0'],
    'ipam_ipaddr_duplicate': ['count==0'],
  },
};

// setup creates the dedicated class + pool used by both scenarios. Idempotent
// — if the resources already exist (409), we proceed.
export function setup() {
  // Class with single allocation length (effectively /32 for IPAddressClaim,
  // but the IPPrefixClass.defaultAllocation must permit /32 carve-outs).
  const classRes = createPrefixClass(CLASS_NAME, {
    requiresVerification: false,
    visibility: 'consumer',
    minLen: 22,
    maxLen: 32,
    strategy: 'FirstFit',
  });
  if (classRes.status !== 201 && classRes.status !== 409) {
    throw new Error(`prefix class create failed: ${classRes.status} ${classRes.body}`);
  }

  const poolRes = createPrefix(POOL_NAME, POOL_CIDR, CLASS_NAME, {
    ipFamily: 'IPv4',
    minLen: 22,
    maxLen: 32,
    strategy: 'FirstFit',
  });
  if (poolRes.status !== 201 && poolRes.status !== 409) {
    throw new Error(`pool create failed: ${poolRes.status} ${poolRes.body}`);
  }

  console.log(`setup complete: class=${CLASS_NAME} pool=${POOL_NAME} cidr=${POOL_CIDR} (~${POOL_SIZE} addresses)`);
  return { className: CLASS_NAME, poolName: POOL_NAME };
}

function extractIP(res) {
  let body;
  try {
    body = JSON.parse(res.body);
  } catch (_e) {
    return null;
  }
  if (!body || !body.status) return null;
  const ip = body.status.allocatedIP;
  if (!ip || ip === '') return null;
  return ip;
}

// concurrent is the burst loop: many VUs CREATE + DELETE in parallel. Each
// iteration releases its slot inline so the pool stays unsaturated.
export function concurrent() {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const claimName = `ipaddr-concurrent-${__VU}-${__ITER}`;

  const createRes = createIPAddressClaimForProject(ns, claimName, POOL_NAME, PROJECT);

  if (createRes.status === 201) {
    created.add(1);
    createLatency.add(createRes.timings.duration, { phase: 'success' });
    successRate.add(1);

    if (extractIP(createRes) === null) {
      missingStatus.add(1);
      if (__ITER < 5) {
        console.error(`ipaddr claim ${claimName} created without status.allocatedIP: ${createRes.body}`);
      }
    }

    const delRes = deleteIPAddressClaimForProject(ns, claimName, PROJECT);
    deleteLatency.add(delRes.timings.duration);
    if (delRes.status !== 200 && delRes.status !== 202 && delRes.status !== 404) {
      errors.add(1);
    }
  } else if (createRes.status === 507) {
    denied.add(1);
    createLatency.add(createRes.timings.duration, { phase: 'denied' });
    successRate.add(0);
  } else {
    errors.add(1);
    createLatency.add(createRes.timings.duration, { phase: 'error' });
    successRate.add(0);
    if (__ITER < 5) {
      console.error(`VU ${__VU} iter ${__ITER}: unexpected ${createRes.status}: ${createRes.body}`);
    }
  }
}

// uniqueness drains the pool sequentially with a single VU. Records every
// allocated IP and reports duplicates. Cleans up after itself.
export function uniqueness() {
  const ns = nsFor(0);
  const seen = {};
  const claims = [];
  let dupCount = 0;

  for (let i = 0; i < POOL_SIZE + 16; i++) {
    const claimName = `ipaddr-unique-${i}`;
    const res = createIPAddressClaimForProject(ns, claimName, POOL_NAME, PROJECT);
    if (res.status === 507) break;
    if (res.status !== 201) {
      console.error(`uniqueness create ${i}: status=${res.status} body=${res.body}`);
      continue;
    }
    const ip = extractIP(res);
    if (ip === null) {
      missingStatus.add(1);
      continue;
    }
    if (seen[ip]) {
      dupCount++;
      console.error(`DUPLICATE ip ${ip} returned for both ${seen[ip]} and ${claimName}`);
    } else {
      seen[ip] = claimName;
      uniqueAllocated.add(1);
    }
    claims.push(claimName);
  }

  if (dupCount > 0) {
    duplicates.add(dupCount);
  }
  console.log(
    `uniqueness scenario: ${claims.length} claims, ${Object.keys(seen).length} unique IPs, ${dupCount} duplicates`,
  );

  // Drain so the pool delete in teardown succeeds.
  for (const name of claims) {
    deleteIPAddressClaimForProject(ns, name, PROJECT);
  }
}

// teardown removes the pool and class. The throughput claims free themselves
// inline; the uniqueness scenario drains its own. A leftover claim will block
// the pool delete and surface the leak in the logs.
export function teardown(data) {
  if (!data) return;
  const poolRes = ipamDelete(prefixPath(data.poolName), 'prefix_delete');
  if (poolRes.status !== 200 && poolRes.status !== 202 && poolRes.status !== 404) {
    console.error(`teardown: pool delete ${data.poolName} status=${poolRes.status} body=${poolRes.body}`);
  }
  const classRes = ipamDelete(prefixClassPath(data.className), 'prefix_class_delete');
  if (classRes.status !== 200 && classRes.status !== 202 && classRes.status !== 404) {
    console.error(`teardown: class delete ${data.className} status=${classRes.status} body=${classRes.body}`);
  }
  console.log('ipaddress-claim-concurrent teardown complete');
}
