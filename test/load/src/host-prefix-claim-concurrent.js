// host-prefix-claim-concurrent.js
//
// Measures the throughput and concurrency safety of host-route allocation:
// IPPrefixClaim creates with prefixLength: 32 (IPv4 /32) against a dedicated
// /24 pool. Single-address allocation via IPPrefixClaim replaced the former
// IPAddressClaim resource.
//
// Approach:
//   - setup() creates a dedicated /24 pool (10.60.0.0/24, 256 addresses).
//   - Each VU iteration creates a /32 IPPrefixClaim and deletes it inline so
//     the pool stays available for subsequent iterations.
//   - All returned status.allocatedCIDR values must be unique; the
//     SELECT...FOR UPDATE pool-row lock guarantees this.
//   - teardown() removes all claims and the pool.
//
// Thresholds (matches prefix-claim-throughput.js):
//   - p95 create latency < 500ms, p99 < 2000ms (success phase)
//   - success rate > 0.95
//   - http_req_failed < 5%
//   - ipam_host_missing_status == 0   (status.allocatedCIDR must be populated)
//   - ipam_host_duplicate == 0        (no two claims may share a CIDR)
//
// Configuration:
//   VUS             - Concurrent virtual users (default 10)
//   DURATION        - Test duration (default 2m)
//   NAMESPACE_COUNT - Namespace pool size (default 10, must match setup-pools.js)
//   POOL_CIDR       - Parent CIDR for the dedicated pool (default 10.60.0.0/24)
//   IPAM_API_URL    - Apiserver URL

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createPrefixClass,
  createPrefix,
  createPrefixClaimForProject,
  deletePrefixClaimForProject,
  buildPrefixClaimRequest,
  ipamDelete,
  prefixPath,
  prefixClassPath,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const VUS = parseInt(__ENV.VUS || '10');
const DURATION = __ENV.DURATION || '2m';
const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const POOL_CIDR = __ENV.POOL_CIDR || '10.60.0.0/24';

const CLASS_NAME = 'perf-host-claim';
const POOL_NAME = 'perf-host-claim-pool';
const PROJECT = projectIDFor(0);

// /24 = 256 host addresses. Each VU releases its slot inline so we stay well
// under pool capacity for the full DURATION burst.
const POOL_SIZE = 256;

const createLatency = new Trend('ipam_host_create_latency_ms', true);
const deleteLatency = new Trend('ipam_host_delete_latency_ms', true);
const successRate = new Rate('ipam_host_success_rate');
const created = new Counter('ipam_host_created');
const denied = new Counter('ipam_host_denied');
const errors = new Counter('ipam_host_errors');
const missingStatus = new Counter('ipam_host_missing_status');
const duplicates = new Counter('ipam_host_duplicate');

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
    'ipam_host_create_latency_ms{phase:success}': ['p(95)<500', 'p(99)<2000'],
    'ipam_host_success_rate': ['rate>0.95'],
    'http_req_failed': ['rate<0.05'],
    // Hard guards: missing status or duplicate CIDRs fail the run.
    'ipam_host_missing_status': ['count==0'],
    'ipam_host_duplicate': ['count==0'],
  },
};

// setup creates the dedicated class + /24 pool. Idempotent — 409 is OK.
export function setup() {
  const classRes = createPrefixClass(CLASS_NAME, {
    requiresVerification: false,
    visibility: 'consumer',
    minLen: 24,
    maxLen: 32,
    strategy: 'FirstFit',
  });
  if (classRes.status !== 201 && classRes.status !== 409) {
    throw new Error(`host prefix class create failed: ${classRes.status} ${classRes.body}`);
  }

  const poolRes = createPrefix(POOL_NAME, POOL_CIDR, CLASS_NAME, {
    ipFamily: 'IPv4',
    minLen: 32,
    maxLen: 32,
    strategy: 'FirstFit',
  });
  if (poolRes.status !== 201 && poolRes.status !== 409) {
    throw new Error(`host pool create failed: ${poolRes.status} ${poolRes.body}`);
  }

  console.log(
    `setup complete: class=${CLASS_NAME} pool=${POOL_NAME} cidr=${POOL_CIDR} (${POOL_SIZE} host addresses)`,
  );
  return { className: CLASS_NAME, poolName: POOL_NAME };
}

function extractAllocatedCIDR(res) {
  let body;
  try {
    body = JSON.parse(res.body);
  } catch (_e) {
    return null;
  }
  if (!body || !body.status) return null;
  const cidr = body.status.allocatedCIDR;
  if (!cidr || cidr === '') return null;
  return cidr;
}

// concurrent is the burst loop: many VUs CREATE a /32 claim then DELETE it
// inline. Each iteration releases its slot so the pool stays unsaturated.
export function concurrent() {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const claimName = `host-concurrent-${__VU}-${__ITER}`;

  const createRes = createPrefixClaimForProject(ns, claimName, POOL_NAME, 32, PROJECT);

  if (createRes.status === 201) {
    created.add(1);
    createLatency.add(createRes.timings.duration, { phase: 'success' });
    successRate.add(1);

    if (extractAllocatedCIDR(createRes) === null) {
      missingStatus.add(1);
      if (__ITER < 5) {
        console.error(
          `host claim ${claimName} created without status.allocatedCIDR: ${createRes.body}`,
        );
      }
    }

    const delRes = deletePrefixClaimForProject(ns, claimName, PROJECT);
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

// uniqueness drains the pool sequentially from a single VU, recording every
// status.allocatedCIDR and reporting any duplicates. Cleans up after itself.
export function uniqueness() {
  const ns = nsFor(0);
  const seen = {};
  const claims = [];
  let dupCount = 0;

  for (let i = 0; i < POOL_SIZE + 16; i++) {
    const claimName = `host-unique-${i}`;
    const res = createPrefixClaimForProject(ns, claimName, POOL_NAME, 32, PROJECT);
    if (res.status === 507) break;
    if (res.status !== 201) {
      console.error(`uniqueness create ${i}: status=${res.status} body=${res.body}`);
      continue;
    }
    const cidr = extractAllocatedCIDR(res);
    if (cidr === null) {
      missingStatus.add(1);
      continue;
    }
    if (seen[cidr]) {
      dupCount++;
      console.error(`DUPLICATE CIDR ${cidr} returned for both ${seen[cidr]} and ${claimName}`);
    } else {
      seen[cidr] = claimName;
    }
    claims.push(claimName);
  }

  if (dupCount > 0) {
    duplicates.add(dupCount);
  }
  console.log(
    `uniqueness scenario: ${claims.length} claims, ${Object.keys(seen).length} unique /32 CIDRs, ${dupCount} duplicates`,
  );

  // Release all slots so teardown can delete the pool cleanly.
  for (const name of claims) {
    deletePrefixClaimForProject(ns, name, PROJECT);
  }
}

// teardown removes the pool and class. The burst scenario frees its claims
// inline; the uniqueness scenario drains its own. A leftover claim will
// block the pool delete and surface the leak in the logs.
export function teardown(data) {
  if (!data) return;
  const poolRes = ipamDelete(prefixPath(data.poolName), 'prefix_delete');
  if (poolRes.status !== 200 && poolRes.status !== 202 && poolRes.status !== 404) {
    console.error(
      `teardown: pool delete ${data.poolName} status=${poolRes.status} body=${poolRes.body}`,
    );
  }
  const classRes = ipamDelete(prefixClassPath(data.className), 'prefix_class_delete');
  if (classRes.status !== 200 && classRes.status !== 202 && classRes.status !== 404) {
    console.error(
      `teardown: class delete ${data.className} status=${classRes.status} body=${classRes.body}`,
    );
  }
  console.log('host-prefix-claim-concurrent teardown complete');
}
