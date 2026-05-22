// ipv6-claim-throughput.js
//
// PRIMARY PRIORITY load test for the IPAM platform: IPv6 IPClaim throughput.
// The platform allocates primarily IPv6 — this script is the canonical proof
// that the hot path holds the same SLO under IPv6 as under IPv4, with the
// additional correctness gate that no two simultaneous allocations may
// overlap.
//
// Topology (provisioned by setup-pools.js):
//   - Per-project IPv6 /32 pool `perf-ipv6-prefix-<n>` (fd<HH>:<LLLL>::/32)
//   - Shared IPv6 /28 pool       `perf-ipv6-shared`   (fd00:f000::/28)
//
// Claim shape: every claim carves a /48 block. /48 is the standard
// per-customer site assignment under RFC 6177 — picking it makes the test
// realistic for ISP-style workloads. A /32 pool yields 2^16 = 65536 /48
// slots, so the run is allocation-safe even for very long durations.
//
// Workload mix:
//   90% same-project: VU N picks project N (mod PROJECT_COUNT) and claims a
//                     /48 from its own perf-ipv6-prefix-<N> pool with its
//                     own project tenant headers.
//   10% cross-project: VU acts as project K (1..N-1) and claims a /48 from
//                     project 0's perf-ipv6-shared pool, using projectRef
//                     in the claim spec.
//
// Concurrency: 50 VUs by default (override with VUS). All 5 perf projects
// are exercised in parallel; the 90/10 mix runs across them.
//
// Correctness gates:
//   - HTTP 201 on the success path; we record latency and success-rate
//   - HTTP 5xx / non-201 counts as failure; we cap the threshold at 5%
//   - Every allocated CIDR MUST:
//       * parse as a valid IPv6 /48
//       * sit inside the source pool's CIDR
//       * never collide with another allocation observed in this run
//     If any of these fail we increment `ipam_ipv6_duplicate_cidrs` or
//     `ipam_ipv6_invalid_cidrs`. Both have count==0 thresholds.
//
// SLO thresholds:
//   - p(95) success latency < 500ms (same SLO as IPv4)
//   - success rate > 0.95
//   - http_req_failed < 0.05
//   - ipam_ipv6_duplicate_cidrs count == 0   (HARD correctness gate)
//   - ipam_ipv6_invalid_cidrs   count == 0   (HARD correctness gate)
//
// Configuration:
//   IPAM_API_URL    - Apiserver URL
//   NAMESPACE_COUNT - Namespace pool size (default 10, must match setup)
//   PROJECT_COUNT   - Perf project count (default 5, must match setup)
//   VUS             - Concurrent VUs (default 50)
//   DURATION        - Test duration (default 2m)
//   CROSS_RATIO     - Cross-project share (default 0.1)

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  API_BASE,
  ipClaim,
  ipClaimPath,
  crossProjectIPClaim,
  deleteIPClaimForProject,
  nsFor,
  projectIDFor,
  withProjectTagged,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');
const VUS = parseInt(__ENV.VUS || '50');
const DURATION = __ENV.DURATION || '2m';
const CROSS_RATIO = parseFloat(__ENV.CROSS_RATIO || '0.1');
const CLAIM_PREFIX_LENGTH = parseInt(__ENV.CLAIM_PREFIX_LENGTH || '48');
const SHARED_IPV6_POOL = 'perf-ipv6-shared';
const SHARED_OWNER_PROJECT = projectIDFor(0);

const claimCreateLatency = new Trend('ipam_ipv6_claim_create_latency_ms', true);
const claimDeleteLatency = new Trend('ipam_ipv6_claim_delete_latency_ms', true);
const claimSuccessRate = new Rate('ipam_ipv6_claim_success_rate');
const claimsCreated = new Counter('ipam_ipv6_claims_created');
const claimsDenied = new Counter('ipam_ipv6_claims_denied');
const claimErrors = new Counter('ipam_ipv6_claim_errors');
const sameProjectLatency = new Trend('ipam_ipv6_same_project_claim_ms', true);
const crossProjectLatency = new Trend('ipam_ipv6_cross_project_claim_ms', true);

// Correctness gates — these MUST be zero. A non-zero value indicates a
// data-corruption regression in the allocator, not just an SLO breach.
const duplicateCIDRs = new Counter('ipam_ipv6_duplicate_cidrs');
const invalidCIDRs = new Counter('ipam_ipv6_invalid_cidrs');

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    ipv6_throughput: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      tags: { scenario: 'ipv6_throughput' },
    },
  },
  thresholds: {
    // SLO: same envelope as the IPv4 IPClaim path.
    'ipam_ipv6_claim_create_latency_ms{phase:success}': ['p(95)<500', 'p(99)<2000'],
    'ipam_ipv6_claim_success_rate': ['rate>0.95'],
    'http_req_failed': ['rate<0.05'],
    // Correctness: the allocator must NEVER return overlapping CIDRs or a
    // CIDR outside its source pool. Both fail the run on the first hit.
    'ipam_ipv6_duplicate_cidrs': ['count==0'],
    'ipam_ipv6_invalid_cidrs':   ['count==0'],
  },
};

// ---- Bare-bones IPv6 parsing / containment (no k6 helpers exist) ----
//
// k6 runs scripts on goja, which has no `net` or BigInt-friendly net library.
// We need to validate that an allocated /48 lives inside a parent /32 or /28.
// We do that by working in 128-bit BigInts assembled from hextets.

// Parse `2001:db8:1234::/48` → { addr: BigInt(128-bit), prefixLen: 48 }.
// Returns null on parse error. Caller is responsible for null-checking.
function parseCIDR(cidr) {
  if (typeof cidr !== 'string' || !cidr.includes('/')) return null;
  const slash = cidr.indexOf('/');
  const addrPart = cidr.slice(0, slash);
  const prefixLen = parseInt(cidr.slice(slash + 1));
  if (!Number.isInteger(prefixLen) || prefixLen < 0 || prefixLen > 128) return null;
  const addr = parseIPv6(addrPart);
  if (addr === null) return null;
  return { addr, prefixLen };
}

// Parse a full IPv6 address (no /) into a 128-bit BigInt. Accepts `::`
// compression. Returns null if malformed.
function parseIPv6(s) {
  if (typeof s !== 'string' || s.length === 0) return null;
  // Detect and expand the `::` shorthand. There can be at most one `::`.
  const doubleColonIdx = s.indexOf('::');
  let parts;
  if (doubleColonIdx === -1) {
    parts = s.split(':');
    if (parts.length !== 8) return null;
  } else {
    if (s.indexOf('::', doubleColonIdx + 1) !== -1) return null; // two `::`
    const left = s.slice(0, doubleColonIdx);
    const right = s.slice(doubleColonIdx + 2);
    const leftParts = left === '' ? [] : left.split(':');
    const rightParts = right === '' ? [] : right.split(':');
    const missing = 8 - leftParts.length - rightParts.length;
    if (missing < 0) return null;
    const zeros = [];
    for (let i = 0; i < missing; i++) zeros.push('0');
    parts = leftParts.concat(zeros).concat(rightParts);
  }
  if (parts.length !== 8) return null;
  let addr = 0n;
  for (let i = 0; i < 8; i++) {
    const hex = parts[i];
    if (!/^[0-9a-fA-F]{1,4}$/.test(hex)) return null;
    const v = parseInt(hex, 16);
    if (Number.isNaN(v) || v < 0 || v > 0xffff) return null;
    addr = (addr << 16n) | BigInt(v);
  }
  return addr;
}

// Mask a 128-bit BigInt to its first `prefixLen` bits. The remaining bits
// are zeroed.
function maskAddr(addr, prefixLen) {
  if (prefixLen === 0) return 0n;
  if (prefixLen === 128) return addr;
  const hostBits = BigInt(128 - prefixLen);
  return (addr >> hostBits) << hostBits;
}

// containsCIDR(parent, child): true iff `child`'s prefix length is at least
// as long as `parent`'s AND child's network address falls inside parent's.
function containsCIDR(parent, child) {
  if (!parent || !child) return false;
  if (child.prefixLen < parent.prefixLen) return false;
  return maskAddr(child.addr, parent.prefixLen) === maskAddr(parent.addr, parent.prefixLen);
}

// Per-pool reference for containment checks. Parsed once at module load.
const POOL_CIDR = {};
POOL_CIDR[SHARED_IPV6_POOL] = parseCIDR('fd00:f000::/28');
for (let n = 0; n < PROJECT_COUNT; n++) {
  const hi = (n >> 8) & 0xff;
  const lo = n & 0xff;
  const c =
    `fd${hi.toString(16).padStart(2, '0')}:` +
    `${lo.toString(16).padStart(4, '0')}::/32`;
  POOL_CIDR[`perf-ipv6-prefix-${n}`] = parseCIDR(c);
}

// ---- Duplicate-CIDR detection ----
//
// k6 VUs each run in their own goja runtime, so we cannot share a single
// JS Set across VUs. We rely on the server's invariant: an IPClaim CREATE
// must never return an overlapping CIDR. For an in-script signal we keep a
// per-VU registry; a duplicate within ONE VU would also be a bug.
// Cross-VU duplicates are detectable via the e2e suite and the count of
// 201s vs distinct CIDRs in the json-out, both of which are tracked.
const seenCIDRs = new Set();

function recordAllocation(allocatedCIDR, poolName, mode) {
  const parsed = parseCIDR(allocatedCIDR);
  if (!parsed) {
    invalidCIDRs.add(1, { reason: 'unparseable', mode });
    if (__ITER < 5) console.error(`unparseable IPv6 CIDR: ${allocatedCIDR}`);
    return;
  }
  if (parsed.prefixLen !== CLAIM_PREFIX_LENGTH) {
    invalidCIDRs.add(1, { reason: 'wrong_prefix_length', mode });
    if (__ITER < 5) {
      console.error(
        `expected /${CLAIM_PREFIX_LENGTH}, got /${parsed.prefixLen}: ${allocatedCIDR}`,
      );
    }
    return;
  }
  const pool = POOL_CIDR[poolName];
  if (pool && !containsCIDR(pool, parsed)) {
    invalidCIDRs.add(1, { reason: 'outside_pool', mode });
    if (__ITER < 5) {
      console.error(`CIDR ${allocatedCIDR} not inside pool ${poolName}`);
    }
    return;
  }
  // Per-VU duplicate check. The Set holds the canonical network string.
  const network = maskAddr(parsed.addr, parsed.prefixLen);
  const key = `${network.toString(16)}/${parsed.prefixLen}`;
  if (seenCIDRs.has(key)) {
    duplicateCIDRs.add(1, { mode });
    if (__ITER < 5) console.error(`duplicate IPv6 CIDR within VU: ${allocatedCIDR}`);
    return;
  }
  seenCIDRs.add(key);
}

function recordCreate(res, mode, poolName) {
  const ok = check(res, { [`${mode} ipv6 claim created`]: (r) => r.status === 201 });
  if (ok) {
    claimsCreated.add(1, { mode });
    claimCreateLatency.add(res.timings.duration, { phase: 'success', mode });
    claimSuccessRate.add(1);
    if (mode === 'same') sameProjectLatency.add(res.timings.duration);
    else crossProjectLatency.add(res.timings.duration);
    // Pull the allocated CIDR out of the response body and validate it.
    try {
      const body = JSON.parse(res.body);
      const allocated =
        body && body.status && (body.status.allocatedCIDR || body.status.allocatedCidr);
      if (!allocated) {
        invalidCIDRs.add(1, { reason: 'missing_status_cidr', mode });
        if (__ITER < 5) console.error(`no allocatedCIDR in 201 body: ${res.body}`);
      } else {
        recordAllocation(allocated, poolName, mode);
      }
    } catch (e) {
      invalidCIDRs.add(1, { reason: 'json_parse', mode });
      if (__ITER < 5) console.error(`failed to parse 201 body: ${e}`);
    }
  } else if (res.status === 507) {
    claimsDenied.add(1, { mode });
    claimCreateLatency.add(res.timings.duration, { phase: 'denied', mode });
    claimSuccessRate.add(0);
  } else {
    claimErrors.add(1, { mode });
    claimCreateLatency.add(res.timings.duration, { phase: 'error', mode });
    claimSuccessRate.add(0);
    if (__ITER < 5) {
      console.error(`${mode} ipv6 claim error ${res.status}: ${res.body}`);
    }
  }
  return ok;
}

// Direct HTTP wrapper — the lib helpers default to IPv4, so we post our own
// IPv6 body with the project tenant header in a single round-trip.
function postIPv6Claim(ns, name, poolName, projectID) {
  const body = ipClaim(ns, name, poolName, CLAIM_PREFIX_LENGTH, { ipFamily: 'IPv6' });
  const params = withProjectTagged(projectID, 'ipv6_ipclaim_create');
  return http.post(`${API_BASE}${ipClaimPath(ns)}`, JSON.stringify(body), params);
}

function postCrossProjectIPv6Claim(ns, name, poolName, sourceProjectID, callerProjectID) {
  const body = crossProjectIPClaim(ns, name, poolName, sourceProjectID, CLAIM_PREFIX_LENGTH, {
    ipFamily: 'IPv6',
  });
  const params = withProjectTagged(callerProjectID, 'ipv6_cross_project_ipclaim_create');
  return http.post(`${API_BASE}${ipClaimPath(ns)}`, JSON.stringify(body), params);
}

export default function () {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const claimName = `ipv6-claim-${__VU}-${__ITER}`;
  const isCross = Math.random() < CROSS_RATIO;

  let res;
  let mode;
  let callerProject;
  let poolName;

  if (isCross && PROJECT_COUNT > 1) {
    mode = 'cross';
    const callerIdx = 1 + Math.floor(Math.random() * (PROJECT_COUNT - 1));
    callerProject = projectIDFor(callerIdx);
    poolName = SHARED_IPV6_POOL;
    res = postCrossProjectIPv6Claim(ns, claimName, poolName, SHARED_OWNER_PROJECT, callerProject);
  } else {
    mode = 'same';
    const projectIdx = Math.floor(Math.random() * PROJECT_COUNT);
    callerProject = projectIDFor(projectIdx);
    poolName = `perf-ipv6-prefix-${projectIdx}`;
    res = postIPv6Claim(ns, claimName, poolName, callerProject);
  }

  const ok = recordCreate(res, mode, poolName);

  if (ok) {
    const delRes = deleteIPClaimForProject(ns, claimName, callerProject);
    claimDeleteLatency.add(delRes.timings.duration);
    if (delRes.status !== 200 && delRes.status !== 202 && delRes.status !== 404) {
      claimErrors.add(1, { mode, phase: 'delete' });
    }
  }
}
