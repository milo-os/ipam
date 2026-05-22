// concurrent-claims.js
//
// Stress-tests the IPAM service's concurrency guarantee: concurrent IPClaim
// CREATE requests must always produce non-overlapping CIDRs.
//
// Approach:
//   - burst scenario: constant-vus for DURATION. Each VU creates and deletes
//     a /28 claim inline so the pool stays available for subsequent iterations.
//     Measures p95 latency under SELECT...FOR UPDATE contention.
//   - uniqueness scenario (single VU, runs after burst):
//       Phase 1 — concurrent batch: fires VUS simultaneous creates via
//       http.batch() and asserts all returned status.allocatedCIDR values are
//       unique. http.batch() dispatches all requests in parallel; if
//       SELECT...FOR UPDATE regresses, two requests could race to the same CIDR.
//       This is the hard concurrent correctness check.
//       Phase 2 — sequential drain: fills remaining pool capacity serially,
//       asserting uniqueness of each successive allocation.
//
// SLO-aligned thresholds:
//   - p95 create latency < 500ms (same as prefix-claim-throughput)
//   - success rate > 0.95
//   - http_req_failed < 5%
//   - ipam_duplicate_cidrs == 0  (hard gate — any duplicate fails the run)
//   - ipam_concurrent_missing_status == 0
//
// Run setup-pools.js first (uses perf-prefix-0 from project 0).
//
// Configuration:
//   VUS             - Concurrent virtual users (default 50)
//   DURATION        - Burst duration (default 2m)
//   NAMESPACE_COUNT - Namespace pool size (default 10)
//   IPAM_API_URL    - Apiserver URL

import http from 'k6/http';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  buildIPClaimRequest,
  createIPClaimForProject,
  deleteIPClaimForProject,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const VUS = parseInt(__ENV.VUS || '50');
const DURATION = __ENV.DURATION || '2m';
const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
// Pool owned by project 0; perf-prefix-0 is 10.0.0.0/16 (65536 /32 slots,
// 256 /28 slots — enough for a 2m concurrent run without exhaustion).
const POOL_NAME = 'perf-prefix-0';
const PROJECT = projectIDFor(0);

const concurrentCreateLatency = new Trend('ipam_concurrent_create_latency_ms', true);
const concurrentSuccessRate = new Rate('ipam_concurrent_success_rate');
const concurrentCreated = new Counter('ipam_concurrent_claims_created');
const concurrentDenied = new Counter('ipam_concurrent_claims_denied');
const concurrentErrors = new Counter('ipam_concurrent_claim_errors');
// Track unexpected 507s (pool not exhausted — signals a concurrency bug if
// they appear in the first few hundred iterations).
const unexpectedDeny = new Counter('ipam_concurrent_unexpected_deny');
// Hard-fail counters surfaced by the uniqueness scenario.
const duplicateCIDRs = new Counter('ipam_duplicate_cidrs');
const missingStatus = new Counter('ipam_concurrent_missing_status');
const uniqueAllocated = new Counter('ipam_concurrent_unique_allocated');

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    concurrent_burst: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      tags: { scenario: 'concurrent' },
      exec: 'burst',
    },
    uniqueness_check: {
      // Runs after burst: concurrent batch (http.batch) then sequential drain,
      // both asserting strict CIDR uniqueness.
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
      maxDuration: '5m',
      startTime: DURATION,
      tags: { scenario: 'uniqueness' },
      exec: 'uniqueness',
    },
  },
  thresholds: {
    // Core SLO: concurrent claim latency must stay within the same envelope as
    // the single-project throughput test.
    'ipam_concurrent_create_latency_ms{phase:success}': ['p(95)<500', 'p(99)<2000'],
    // Success rate: pool is large enough that 507 should never appear in the
    // first iteration of a fresh run. A low success rate signals either a
    // correctness bug or stale leftover claims from a prior run.
    'ipam_concurrent_success_rate': ['rate>0.95'],
    'http_req_failed': ['rate<0.05'],
    // Correctness gates from the audit spec.
    'ipam_duplicate_cidrs': ['count==0'],
    'ipam_concurrent_missing_status': ['count==0'],
  },
};

function extractCIDR(res) {
  let body;
  try {
    body = JSON.parse(res.body);
  } catch (_e) {
    return null;
  }
  if (!body || !body.status) return null;
  const cidr = body.status.allocatedCIDR || body.status.allocatedPrefix;
  if (!cidr || cidr === '') return null;
  return cidr;
}

export function burst() {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const claimName = `concurrent-claim-${__VU}-${__ITER}`;

  const createRes = createIPClaimForProject(ns, claimName, POOL_NAME, 28, PROJECT);

  if (createRes.status === 201) {
    concurrentCreated.add(1);
    concurrentCreateLatency.add(createRes.timings.duration, { phase: 'success' });
    concurrentSuccessRate.add(1);

    if (extractCIDR(createRes) === null) {
      missingStatus.add(1);
      if (__ITER < 5) {
        console.error(`IPClaim ${claimName} created without status.allocatedCIDR: ${createRes.body}`);
      }
    }

    // Immediately delete so the pool stays available for subsequent iterations.
    const delRes = deleteIPClaimForProject(ns, claimName, PROJECT);
    if (delRes.status !== 200 && delRes.status !== 202 && delRes.status !== 404) {
      concurrentErrors.add(1);
    }
  } else if (createRes.status === 507) {
    // 507 during a non-exhausted run is expected only if a prior run left
    // leftover claims. Count separately so operators can distinguish pool
    // exhaustion from concurrency bugs.
    concurrentDenied.add(1);
    concurrentCreateLatency.add(createRes.timings.duration, { phase: 'denied' });
    concurrentSuccessRate.add(0);
    if (__ITER < 10) {
      // Early 507 is suspicious — log for diagnosis.
      unexpectedDeny.add(1);
      console.warn(`VU ${__VU} iter ${__ITER}: unexpected 507 — pool may have leftover claims from prior run`);
    }
  } else {
    concurrentErrors.add(1);
    concurrentCreateLatency.add(createRes.timings.duration, { phase: 'error' });
    concurrentSuccessRate.add(0);
    if (__ITER < 5) {
      console.error(`VU ${__VU} iter ${__ITER}: unexpected ${createRes.status}: ${createRes.body}`);
    }
  }
}

// uniqueness runs two phases after the burst completes:
//
// Phase 1 — concurrent batch: fires VUS simultaneous creates via http.batch()
// so requests contend for the SELECT...FOR UPDATE pool lock at the same time.
// All VUS responses are collected and their status.allocatedCIDR values are
// checked for duplicates. This is the hard concurrent correctness assertion:
// any concurrency regression that allows two requests to allocate the same CIDR
// will produce a duplicate here and fail the ipam_duplicate_cidrs threshold.
//
// Phase 2 — sequential drain: fills remaining pool capacity one-by-one,
// asserting every successive CIDR is unique. Confirms correctness under
// non-contended conditions as well.
export function uniqueness() {
  const ns = nsFor(0);
  let totalDups = 0;

  // --- Phase 1: concurrent batch ---
  const batchRequests = [];
  for (let i = 0; i < VUS; i++) {
    batchRequests.push(buildIPClaimRequest(ns, `concurrent-batch-${i}`, POOL_NAME, 28, PROJECT));
  }
  const batchResponses = http.batch(batchRequests);

  const batchSeen = {};
  const batchClaims = [];
  for (let i = 0; i < batchResponses.length; i++) {
    const res = batchResponses[i];
    if (res.status === 507) {
      // Pool unexpectedly exhausted from burst leftovers — log and skip.
      console.warn(`batch slot ${i}: 507 — leftover claims from burst may be blocking`);
      continue;
    }
    if (res.status !== 201) {
      console.error(`batch slot ${i}: status=${res.status} body=${res.body}`);
      continue;
    }
    const cidr = extractCIDR(res);
    if (cidr === null) {
      missingStatus.add(1);
      batchClaims.push(`concurrent-batch-${i}`);
      continue;
    }
    if (batchSeen[cidr]) {
      totalDups++;
      console.error(
        `DUPLICATE CIDR ${cidr}: concurrent-batch-${batchSeen[cidr]} and concurrent-batch-${i}`,
      );
    } else {
      batchSeen[cidr] = i;
      uniqueAllocated.add(1);
    }
    batchClaims.push(`concurrent-batch-${i}`);
  }
  console.log(
    `concurrent batch: ${batchClaims.length}/${VUS} claims, ${Object.keys(batchSeen).length} unique CIDRs, ${totalDups} duplicates`,
  );

  // Clean up batch claims before sequential drain.
  for (const name of batchClaims) {
    deleteIPClaimForProject(ns, name, PROJECT);
  }

  // --- Phase 2: sequential drain ---
  const seenSeq = {};
  const seqClaims = [];
  let seqDups = 0;
  const maxIters = 256 + 16; // /16 with /28 children = 256 slots

  for (let i = 0; i < maxIters; i++) {
    const claimName = `concurrent-unique-${i}`;
    const res = createIPClaimForProject(ns, claimName, POOL_NAME, 28, PROJECT);
    if (res.status === 507) break;
    if (res.status !== 201) {
      console.error(`sequential drain ${i}: status=${res.status} body=${res.body}`);
      continue;
    }
    const cidr = extractCIDR(res);
    if (cidr === null) {
      missingStatus.add(1);
      seqClaims.push(claimName);
      continue;
    }
    if (seenSeq[cidr]) {
      seqDups++;
      console.error(`DUPLICATE CIDR ${cidr} returned for both ${seenSeq[cidr]} and ${claimName}`);
    } else {
      seenSeq[cidr] = claimName;
      uniqueAllocated.add(1);
    }
    seqClaims.push(claimName);
  }

  totalDups += seqDups;
  if (totalDups > 0) {
    duplicateCIDRs.add(totalDups);
  }
  console.log(
    `sequential drain: ${seqClaims.length} claims, ${Object.keys(seenSeq).length} unique CIDRs, ${seqDups} duplicates`,
  );

  for (const name of seqClaims) {
    deleteIPClaimForProject(ns, name, PROJECT);
  }
}
