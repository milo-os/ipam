// class-chain-claim-throughput.js
//
// Claim throughput through a three-level class chain, the network-services-
// operator shape (lib/ipam-client.js CHAIN):
//
//   perf-vpc /48 per (network, project) from perf-vpc-root (fd20::/32)
//     perf-subnet /64 per (network, location, project), carved from perf-vpc
//       perf-endpoint /96, carved from perf-subnet   <- every claim names this
//
// Two cases, run one after the other so neither's numbers include the other's
// load:
//
//   steady  The pools already exist. Claims spread over STEADY_NETWORKS x
//           LOCATIONS scopes per project, each warmed in setup(), so every
//           claim resolves the chain, finds its subnet pool, locks it and
//           carves a /96.
//   first   Every claim names a network nothing has claimed before, so the
//           create provisions a vpc pool (carved from the root) and a subnet
//           pool (carved from that) before binding its /96: cascade
//           provisioning on the request path, the slowest create there is.
//
// Every claim is deleted straight after it binds. Pools outlive claims, so the
// first case leaves two pools per iteration; teardown deletes the ones this
// run made unless PRUNE_POOLS=false (cleanup.js removes any left over).
//
// This is the benchmark for issue 195's database round-trip work: compare
// ipam_chain_claim_latency_ms{case:steady} and {case:first} before and after.
//
// Thresholds (provisional; override with the variables in brackets):
//   ipam_chain_claim_latency_ms{case:steady,phase:success} p95 < 500ms  [CHAIN_STEADY_P95_MS]
//   ipam_chain_claim_latency_ms{case:first,phase:success}  p95 < 1000ms [CHAIN_FIRST_P95_MS]
//   ipam_chain_claim_success_rate{case:*}                  > 0.95
//   http_req_failed < 0.05, ipam_quota_denied == 0
//
// Run setup-pools.js first.
//
// Configuration:
//   VUS (10), DURATION (1m, per case), CASES (steady,first),
//   STEADY_NETWORKS (4), LOCATIONS (2), PRUNE_POOLS (true),
//   CHAIN_STEADY_P95_MS (500), CHAIN_FIRST_P95_MS (1000),
//   plus the transport and project variables in lib/ipam-client.js.

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  CHAIN,
  INSECURE,
  LOAD_LABEL,
  allocatedCIDR,
  boundPool,
  describe,
  dur,
  endpointScope,
  ipClaim,
  ipClaimPath,
  ipamDelete,
  ipamPost,
  namespaces,
  outcome,
  pick,
  projects,
  pruneChainPools,
  seconds,
  uniqueName,
} from '../lib/ipam-client.js';

const VUS = parseInt(__ENV.VUS || '10', 10);
const DURATION = __ENV.DURATION || '1m';
const CASES = (__ENV.CASES || 'steady,first').split(',').map((s) => s.trim()).filter(Boolean);
const STEADY_NETWORKS = parseInt(__ENV.STEADY_NETWORKS || '4', 10);
const LOCATIONS = parseInt(__ENV.LOCATIONS || '2', 10);
const PRUNE_POOLS = (__ENV.PRUNE_POOLS || 'true') !== 'false';
const STEADY_P95 = parseInt(__ENV.CHAIN_STEADY_P95_MS || '500', 10);
const FIRST_P95 = parseInt(__ENV.CHAIN_FIRST_P95_MS || '1000', 10);

for (const c of CASES) {
  if (c !== 'steady' && c !== 'first') throw new Error(`CASES: unknown case "${c}" (want steady, first)`);
}

const latency = new Trend('ipam_chain_claim_latency_ms', true);
const successRate = new Rate('ipam_chain_claim_success_rate');
const quotaDenied = new Counter('ipam_quota_denied');
const failures = new Counter('ipam_chain_claim_failures');

function buildScenarios() {
  const scenarios = {};
  const thresholds = {
    http_req_failed: ['rate<0.05'],
    ipam_quota_denied: ['count==0'],
  };
  let start = 0;
  for (const c of CASES) {
    scenarios[c] = {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      startTime: dur(start),
      exec: c === 'steady' ? 'steady' : 'first',
      tags: { case: c },
      gracefulStop: '30s',
    };
    // 10s gap so the previous case's in-flight requests finish first.
    start += seconds(DURATION) + 10;
    const p95 = c === 'steady' ? STEADY_P95 : FIRST_P95;
    thresholds[`ipam_chain_claim_latency_ms{case:${c},phase:success}`] = [`p(95)<${p95}`];
    thresholds[`ipam_chain_claim_success_rate{case:${c}}`] = ['rate>0.95'];
  }
  return { scenarios, thresholds };
}

const built = buildScenarios();

export const options = {
  insecureSkipTLSVerify: INSECURE,
  scenarios: built.scenarios,
  thresholds: built.thresholds,
  setupTimeout: __ENV.SETUP_TIMEOUT || '5m',
  teardownTimeout: __ENV.TEARDOWN_TIMEOUT || '10m',
};

const PROJECTS = projects();
const NAMESPACES = namespaces();
let reported = 0;

function steadyScopes() {
  const out = [];
  for (let n = 0; n < STEADY_NETWORKS; n++) {
    for (let l = 0; l < LOCATIONS; l++) out.push([`steady-net-${n}`, `loc-${l}`]);
  }
  return out;
}

// setup warms every steady scope in every project: the first claim into each
// provisions its vpc and subnet pools, which then exist for the steady case.
export function setup() {
  const runId = Date.now().toString(36);
  if (CASES.indexOf('steady') >= 0) {
    for (const project of PROJECTS) {
      for (const [network, location] of steadyScopes()) {
        const ns = NAMESPACES[0];
        const name = uniqueName('chain-warm');
        const res = ipamPost(project, ipClaimPath(ns),
          ipClaim(ns, name, { className: CHAIN.endpointClass, scope: endpointScope(network, location) }, LOAD_LABEL),
          'chain_warm');
        if (outcome(res) !== 'ok') {
          throw new Error(`warm ${project} ${network}/${location}: ${describe(res)}`);
        }
        ipamDelete(project, ipClaimPath(ns, name), 'chain_warm_delete');
      }
    }
  }
  return { runId };
}

function claimOnce(caseName, network, location) {
  const project = pick(PROJECTS);
  const ns = pick(NAMESPACES);
  const name = uniqueName(caseName === 'steady' ? 'cs' : 'cf');
  const body = ipClaim(ns, name, { className: CHAIN.endpointClass, scope: endpointScope(network, location) }, LOAD_LABEL);

  const res = ipamPost(project, ipClaimPath(ns), body, 'ipclaim_create');
  const result = outcome(res);
  const ok = result === 'ok' && allocatedCIDR(res) !== '' && boundPool(res) !== '';

  latency.add(res.timings.duration, { phase: ok ? 'success' : 'failure', outcome: result });
  successRate.add(ok);
  check(res, { [`${caseName}: claim Bound with a CIDR`]: () => ok });

  if (!ok) {
    failures.add(1, { outcome: result });
    if (result === 'quota_denied') quotaDenied.add(1);
    if (reported++ < 5) console.warn(`${caseName} create ${project}/${ns}/${name}: ${describe(res)}`);
    return;
  }
  const del = ipamDelete(project, ipClaimPath(ns, name), 'ipclaim_delete');
  check(del, { [`${caseName}: claim deleted`]: (r) => r.status === 200 || r.status === 202 });
}

const STEADY = steadyScopes();

export function steady() {
  const [network, location] = pick(STEADY);
  claimOnce('steady', network, location);
}

export function first(data) {
  // Unique across runs (runId), VUs and iterations: no claim before this one
  // named this network, so the vpc and subnet pools do not exist yet.
  const network = `first-${data.runId}-${__VU}-${__ITER}`;
  claimOnce('first', network, 'loc-0');
}

export function teardown(data) {
  if (!PRUNE_POOLS || CASES.indexOf('first') < 0) return;
  const prefix = `first-${data.runId}-`;
  for (const project of PROJECTS) {
    const n = pruneChainPools(project, (net) => net.indexOf(prefix) === 0);
    console.log(`teardown: removed ${n} first-claim pools in ${project}`);
  }
}
