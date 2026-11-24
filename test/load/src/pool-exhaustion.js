// pool-exhaustion.js
//
// The deny path must be fast: a claim against a full pool is refused with
// HTTP 507 Insufficient Storage, and that refusal must come back in under
// 200ms p95 under load.
//
// Setup, in every project: IPClass perf-exhaust (IPv4, /30 only) offered by
// IPPool perf-exhaust-pool (192.168.100.0/28), which holds exactly four /30s,
// then four claims that fill it. Their create latency is the success path.
// Main phase: VUS hammer further claims, every one of which must be a 507.
// Teardown: delete the fill claims, the pool and the class.
//
// The fixture is created here rather than in setup-pools.js so a run cannot
// inherit a pool a previous run left half full.
//
// Thresholds:
//   ipam_deny_latency_ms     p95 < 200ms (CLAUDE.md acceptance criterion)
//   ipam_success_latency_ms  p95 < 800ms
//   ipam_deny_rate           > 0.90 (the pool really is full; otherwise the
//                            deny numbers are measuring something else)
//   ipam_quota_denied        == 0 (a quota 403 is not a 507 and must not pass
//                            for one)
//
// Configuration: VUS (20), DURATION (1m), EXHAUST_CIDR (192.168.100.0/28),
// plus the transport and project variables in lib/ipam-client.js.

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  INSECURE,
  LOAD_LABEL,
  describe,
  ensure,
  ipClaim,
  ipClaimPath,
  ipClass,
  ipPool,
  ipamDelete,
  ipamPost,
  namespaces,
  outcome,
  pick,
  projects,
  remove,
  uniqueName,
} from '../lib/ipam-client.js';
import { limit } from '../lib/ipam-client.js';

const CLASS = 'perf-exhaust';
const POOL = 'perf-exhaust-pool';
const CIDR = __ENV.EXHAUST_CIDR || '192.168.100.0/28';
const CLAIM_LEN = 30;
const SLOTS = 1 << (CLAIM_LEN - parseInt(CIDR.split('/')[1], 10));

const denyLatency = new Trend('ipam_deny_latency_ms', true);
const successLatency = new Trend('ipam_success_latency_ms', true);
const denyRate = new Rate('ipam_deny_rate');
const quotaDenied = new Counter('ipam_quota_denied');
const unexpected = new Counter('ipam_unexpected_responses');

// 507 is the answer this script is asking for, so it is not a failed request.
http.setResponseCallback(http.expectedStatuses(200, 201, 202, 404, 409, 507));

export const options = {
  insecureSkipTLSVerify: INSECURE,
  setupTimeout: __ENV.SETUP_TIMEOUT || '2m',
  teardownTimeout: __ENV.TEARDOWN_TIMEOUT || '2m',
  scenarios: {
    deny_path: {
      executor: 'constant-vus',
      vus: parseInt(__ENV.VUS || '20', 10),
      duration: __ENV.DURATION || '1m',
    },
  },
  thresholds: {
    ipam_deny_latency_ms: [`p(95)<${limit(200, 6000)}`],
    ipam_success_latency_ms: [`p(95)<${limit(800, 2000)}`],
    ipam_deny_rate: ['rate>0.90'],
    ipam_quota_denied: ['count==0'],
  },
};

const PROJECTS = projects();
const NS = namespaces()[0];

function fixture() {
  return [
    ipClass(CLASS, {
      ipFamily: 'IPv4',
      uniqueWithin: [],
      allowedPrefixLengths: { min: CLAIM_LEN, max: CLAIM_LEN },
      defaultPrefixLength: CLAIM_LEN,
      strategy: 'FirstFit',
      reclaimPolicy: 'Delete',
    }),
    ipPool(POOL, {
      cidr: CIDR,
      ipFamily: 'IPv4',
      visibility: 'consumer',
      classNames: [CLASS],
      allocation: { minPrefixLength: CLAIM_LEN, maxPrefixLength: CLAIM_LEN, strategy: 'FirstFit' },
    }),
  ];
}

export function setup() {
  const fills = {};
  for (const project of PROJECTS) {
    for (const obj of fixture()) ensure(project, obj);
    fills[project] = [];
    for (let i = 0; i < SLOTS; i++) {
      const name = `exhaust-fill-${i}`;
      const res = ipamPost(project, ipClaimPath(NS), ipClaim(NS, name, { className: CLASS }, LOAD_LABEL), 'fill_create');
      if (res.status === 409) {
        // Left by an earlier run that did not tear down; it still holds a slot.
        fills[project].push(name);
        continue;
      }
      if (outcome(res) !== 'ok') {
        throw new Error(`fill ${project}/${name}: ${describe(res)}`);
      }
      successLatency.add(res.timings.duration);
      fills[project].push(name);
    }
  }
  return { fills };
}

let reported = 0;

export default function () {
  const project = pick(PROJECTS);
  const name = uniqueName('exhaust');
  const res = ipamPost(project, ipClaimPath(NS), ipClaim(NS, name, { className: CLASS }, LOAD_LABEL), 'overflow_create');
  const result = outcome(res);

  denyRate.add(result === 'exhausted');
  check(res, { 'full pool answers 507': () => result === 'exhausted' });

  switch (result) {
    case 'exhausted':
      denyLatency.add(res.timings.duration);
      break;
    case 'ok':
      // A slot was free after all. Record it as the success path and give the
      // slot back so the pool stays full.
      successLatency.add(res.timings.duration);
      ipamDelete(project, ipClaimPath(NS, name), 'overflow_delete');
      break;
    default:
      if (result === 'quota_denied') quotaDenied.add(1);
      unexpected.add(1, { outcome: result });
      if (reported++ < 5) console.warn(`overflow ${project}/${name}: ${describe(res)}`);
  }
}

export function teardown(data) {
  for (const project of PROJECTS) {
    for (const name of (data.fills[project] || [])) remove(project, 'IPClaim', name, NS);
    remove(project, 'IPPool', POOL);
    remove(project, 'IPClass', CLASS);
  }
}
