// asn-claim-throughput.js
//
// Drives ASNClaim creation through both the spec.classRef path and the
// direct spec.poolRef path against the same backing ASNPool. The two paths
// land at different code in `internal/registry/ipam/asnclaim/storage.go`:
// classRef goes through `allocator.ResolveASNPool` (label-driven enumeration
// + first-match), poolRef does a direct `ResourceKey` lookup. Both must
// honour the same SLO, and concurrent traffic mixing the two paths exposes
// any regression in the FOR UPDATE / pool-scope contract that's
// path-specific.
//
// Three scenarios:
//
//   1. classref_throughput (concurrent with #2): VUS concurrent VUs for
//      DURATION, each doing classRef-driven CREATE + DELETE.
//   2. poolref_throughput (concurrent with #1): VUS concurrent VUs for
//      DURATION, each doing poolRef-driven CREATE + DELETE.
//      Both gated independently by p95 < 500ms, p99 < 2000ms, success rate
//      > 0.95, via per-path tags on shared metrics.
//   3. uniqueness_check (after both throughputs): single VU drains the pool
//      via classRef and records every status.asn.
//        - no duplicate ASN value may be returned
//      A duplicate or missing ASN fails the run.
//
// Running #1 and #2 concurrently (instead of sequentially) keeps total wall
// time the same as the prior single-scenario script and exercises the
// pool's lock under mixed-path contention.
//
// Setup creates a dedicated ASNPoolClass + ASNPool with the 16-bit private
// range 64512..65534 (1023 ASNs). Teardown removes both.
//
// Configuration:
//   NAMESPACE_COUNT - Pool of namespaces (must match setup-pools.js, default 10)
//   PROJECT_COUNT   - Number of perf projects (must match setup, default 5)
//   VUS             - Concurrent virtual users for the throughput scenario (default 10)
//   DURATION        - Throughput scenario duration (default 2m)
//   IPAM_API_URL    - Apiserver URL (default localhost:8001)

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createASNPoolClass,
  createASNPool,
  createASNClaimForProject,
  createASNClaimWithClassRefForProject,
  deleteASNClaimForProject,
  ipamDelete,
  asnPoolPath,
  asnPoolClassPath,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');
const VUS = parseInt(__ENV.VUS || '10');
const DURATION = __ENV.DURATION || '2m';

const CLASS_NAME = 'perf-asn-classref';
const POOL_NAME = 'perf-asn-classref-pool';
// 16-bit private ASN range: 1023 ASNs total. Sized so the throughput
// scenario can run for 2m with VUS=10 without ever exhausting (each iteration
// frees its slot), while the uniqueness_check scenario can drain the pool to
// completion in well under the maxDuration.
const ASN_RANGE_START = 64512;
const ASN_RANGE_END = 65534;
const POOL_SIZE = ASN_RANGE_END - ASN_RANGE_START + 1;

const asnCreateLatency = new Trend('ipam_asn_create_latency_ms', true);
const asnDeleteLatency = new Trend('ipam_asn_delete_latency_ms', true);
const asnSuccessRate = new Rate('ipam_asn_success_rate');
const asnsCreated = new Counter('ipam_asns_created');
const asnsDenied = new Counter('ipam_asns_denied');
const asnErrors = new Counter('ipam_asn_errors');
// Counts ASNs that came back without status.asn populated. Healthy run = 0.
const asnMissingStatus = new Counter('ipam_asn_missing_status');
// Uniqueness scenario counters. Asserted in handleSummary.
const uniqueAsnsAllocated = new Counter('ipam_unique_asns_allocated');
const duplicateAsns = new Counter('ipam_duplicate_asns');

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    classref_throughput: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      tags: { scenario: 'classref' },
      exec: 'throughput',
    },
    poolref_throughput: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      // Same startTime (default 0) → runs concurrently with classref_throughput
      // so the pool's FOR UPDATE lock sees both code paths under contention.
      tags: { scenario: 'poolref' },
      exec: 'poolrefThroughput',
    },
    uniqueness_check: {
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
      maxDuration: '5m',
      // Run after both throughput scenarios so the pool is empty.
      startTime: DURATION,
      tags: { scenario: 'uniqueness' },
      exec: 'uniqueness',
    },
  },
  thresholds: {
    // Each path is gated independently — a regression on one shouldn't be
    // masked by the other being healthy.
    'ipam_asn_create_latency_ms{phase:success,path:classref}': ['p(95)<500', 'p(99)<2000'],
    'ipam_asn_create_latency_ms{phase:success,path:poolref}':  ['p(95)<500', 'p(99)<2000'],
    'ipam_asn_success_rate{path:classref}': ['rate>0.95'],
    'ipam_asn_success_rate{path:poolref}':  ['rate>0.95'],
    'http_req_failed': ['rate<0.05'],
    // Hard guards from the audit spec.
    'ipam_asn_missing_status': ['count==0'],
    'ipam_duplicate_asns': ['count==0'],
  },
};

// setup creates the class + pool used by both scenarios. Runs once, before
// any VU starts. Returns context to teardown for cleanup.
export function setup() {
  const classRes = createASNPoolClass(CLASS_NAME, {
    requiresVerification: false,
    visibility: 'consumer',
  });
  if (classRes.status !== 201 && classRes.status !== 409) {
    throw new Error(`asn pool class create failed: ${classRes.status} ${classRes.body}`);
  }

  const poolRes = createASNPool(
    POOL_NAME,
    [{ start: ASN_RANGE_START, end: ASN_RANGE_END }],
    CLASS_NAME,
  );
  if (poolRes.status !== 201 && poolRes.status !== 409) {
    throw new Error(`asn pool create failed: ${poolRes.status} ${poolRes.body}`);
  }

  console.log(
    `setup complete: class=${CLASS_NAME} pool=${POOL_NAME} range=${ASN_RANGE_START}-${ASN_RANGE_END} (${POOL_SIZE} ASNs)`,
  );
  return { className: CLASS_NAME, poolName: POOL_NAME };
}

// extractASN parses the response body and returns the allocated ASN, or null
// if it's missing/malformed.
function extractASN(res) {
  let body;
  try {
    body = JSON.parse(res.body);
  } catch (_e) {
    return null;
  }
  if (!body || !body.status) return null;
  const asn = body.status.asn;
  if (asn === undefined || asn === null || asn === 0) return null;
  return asn;
}

// runThroughputIteration is the shared CREATE + DELETE hot loop body. Each
// iteration immediately frees its slot so the pool stays available for the
// full DURATION. `pathTag` ("classref" | "poolref") drives both metric tags
// and which create helper is used.
function runThroughputIteration(pathTag, claimName, ns, projectID) {
  let createRes;
  if (pathTag === 'classref') {
    createRes = createASNClaimWithClassRefForProject(ns, claimName, CLASS_NAME, projectID);
  } else {
    // poolRef path: direct named lookup. spec.poolRef is a LocalRef so the
    // pool name is the only required field; storage scopes it to the
    // caller's project key prefix.
    createRes = createASNClaimForProject(ns, claimName, { name: POOL_NAME }, projectID);
  }

  const ok = check(createRes, {
    [`asn ${pathTag} claim created`]: (r) => r.status === 201,
  });

  if (ok) {
    asnsCreated.add(1, { path: pathTag });
    asnCreateLatency.add(createRes.timings.duration, { phase: 'success', path: pathTag });
    asnSuccessRate.add(1, { path: pathTag });

    if (extractASN(createRes) === null) {
      asnMissingStatus.add(1, { path: pathTag });
      if (__ITER < 5) {
        console.error(`asn ${pathTag} claim ${claimName} created without status.asn: ${createRes.body}`);
      }
    }

    const delRes = deleteASNClaimForProject(ns, claimName, projectID);
    asnDeleteLatency.add(delRes.timings.duration, { path: pathTag });
    if (delRes.status !== 200 && delRes.status !== 202 && delRes.status !== 404) {
      asnErrors.add(1, { path: pathTag });
    }
  } else if (createRes.status === 507) {
    asnsDenied.add(1, { path: pathTag });
    asnCreateLatency.add(createRes.timings.duration, { phase: 'denied', path: pathTag });
    asnSuccessRate.add(0, { path: pathTag });
  } else {
    asnErrors.add(1, { path: pathTag });
    asnCreateLatency.add(createRes.timings.duration, { phase: 'error', path: pathTag });
    asnSuccessRate.add(0, { path: pathTag });
    if (__ITER < 5) {
      console.error(`asn ${pathTag} claim error ${createRes.status}: ${createRes.body}`);
    }
  }
}

// classRef-driven throughput scenario. Mirrors poolrefThroughput; both run
// concurrently and share the underlying pool so the FOR UPDATE lock sees
// mixed-path traffic.
export function throughput() {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const projectIdx = Math.floor(Math.random() * PROJECT_COUNT);
  const projectID = projectIDFor(projectIdx);
  const claimName = `asn-classref-${__VU}-${__ITER}`;
  runThroughputIteration('classref', claimName, ns, projectID);
}

// poolRef-driven throughput scenario. Direct named pool lookup — exercises
// the code path that classRef bypasses. Distinct claim-name prefix avoids
// any collision with the classref scenario when both run concurrently.
export function poolrefThroughput() {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const projectIdx = Math.floor(Math.random() * PROJECT_COUNT);
  const projectID = projectIDFor(projectIdx);
  const claimName = `asn-poolref-${__VU}-${__ITER}`;
  runThroughputIteration('poolref', claimName, ns, projectID);
}

// uniqueness drains the pool to completion using a single VU, recording every
// allocated ASN. After draining, it deletes each claim. handleSummary then
// reads the seen-set and verifies no duplicates were observed.
export function uniqueness() {
  const ns = nsFor(0);
  const projectID = projectIDFor(0);
  const seen = {};
  const created = [];
  let dupCount = 0;

  for (let i = 0; i < POOL_SIZE + 5; i++) {
    const claimName = `asn-unique-${i}`;
    const res = createASNClaimWithClassRefForProject(ns, claimName, CLASS_NAME, projectID);
    if (res.status === 507) {
      // Expected once the pool is fully drained.
      break;
    }
    if (res.status !== 201) {
      console.error(`uniqueness create ${i}: status=${res.status} body=${res.body}`);
      continue;
    }
    const asn = extractASN(res);
    if (asn === null) {
      asnMissingStatus.add(1);
      continue;
    }
    if (seen[asn]) {
      dupCount++;
      console.error(`DUPLICATE asn ${asn} returned for both ${seen[asn]} and ${claimName}`);
    } else {
      seen[asn] = claimName;
      uniqueAsnsAllocated.add(1);
    }
    created.push(claimName);
  }

  if (dupCount > 0) {
    duplicateAsns.add(dupCount);
  }
  console.log(
    `uniqueness scenario: drained ${created.length} claims, ${Object.keys(seen).length} unique ASNs, ${dupCount} duplicates`,
  );

  // Free the pool so teardown's pool-delete succeeds.
  for (const name of created) {
    deleteASNClaimForProject(ns, name, projectID);
  }
}

// teardown drops the pool and class. The uniqueness scenario clears its own
// claims; throughput claims are freed inline. If any claim leaks the pool
// delete will 4xx and surface the leak.
export function teardown(data) {
  if (!data) return;
  const poolRes = ipamDelete(asnPoolPath(data.poolName), 'asn_pool_delete');
  if (poolRes.status !== 200 && poolRes.status !== 202 && poolRes.status !== 404) {
    console.error(`teardown: asn pool delete ${data.poolName} status=${poolRes.status} body=${poolRes.body}`);
  }
  const classRes = ipamDelete(asnPoolClassPath(data.className), 'asn_pool_class_delete');
  if (classRes.status !== 200 && classRes.status !== 202 && classRes.status !== 404) {
    console.error(`teardown: asn pool class delete ${data.className} status=${classRes.status} body=${classRes.body}`);
  }
  console.log('asn-claim-throughput teardown complete');
}
