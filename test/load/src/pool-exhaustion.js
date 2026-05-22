// pool-exhaustion.js
//
// Verifies the deny path is fast: claims against a full pool must return
// HTTP 507 (Insufficient Storage) under 200ms p95. Exercises both the
// same-project deny path (project 0 claiming from its own exhausted pool)
// and the cross-project deny path (project 1 claiming from project 0's
// shared pool, which is also exhausted).
//
// Setup phase:
//   - Create perf-exhaust-pool (192.168.100.0/28, /30 only, visibility=shared)
//     owned by project 0
//   - Bind perf-exhaust-pool-user role to all other perf projects
//   - Fill the pool with 4 /30 claims (project 0 identity)
// Main phase: hammer additional claim requests from both same-project and
//             cross-project callers.
// Teardown:   delete the 4 fill claims, then the pool.
//
// Configuration:
//   VUS           - Concurrent virtual users (default 20)
//   DURATION      - Main phase duration (default 1m)
//   PROJECT_COUNT - Number of perf projects (default 5)
//   IPAM_API_URL  - Apiserver URL

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createIPPool,
  deleteIPPool,
  createClusterRole,
  createClusterRoleBinding,
  createIPClaimForProject,
  deleteIPClaimForProject,
  createCrossProjectIPClaim,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');
const VUS = parseInt(__ENV.VUS || '20');
const DURATION = __ENV.DURATION || '1m';
const POOL_NAME = 'perf-exhaust-pool';
const EXHAUST_USER_ROLE = 'perf-exhaust-pool-user';
// Visibility for the cross-project pool. The apiserver enum is
// platform|consumer|shared; 'shared' enables cross-project claiming.
const SHARED_VISIBILITY = __ENV.SHARED_VISIBILITY || 'shared';
const FILL_NAMESPACE = nsFor(0);
const OWNER_PROJECT = projectIDFor(0);

const denyLatency = new Trend('ipam_deny_latency_ms', true);
const successLatency = new Trend('ipam_success_latency_ms', true);
const denyRate = new Rate('ipam_deny_rate');
const denials = new Counter('ipam_denials');
const successes = new Counter('ipam_successes');
const errors = new Counter('ipam_errors');

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    deny_path: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      startTime: '0s',
    },
  },
  thresholds: {
    'ipam_deny_latency_ms{mode:same}':  ['p(95)<200'],
    'ipam_deny_latency_ms{mode:cross}': ['p(95)<200'],
    // Same-project deny path is purely local; cross-project has SAR overhead
    // on the success path so we give it a wider success-latency budget.
    'ipam_success_latency_ms{mode:same}':  ['p(95)<800'],
    'ipam_success_latency_ms{mode:cross}': ['p(95)<1200'],
    // Pool must actually be full: at least 90% of probes should be denied.
    // If this drops, fill claims got reclaimed and the deny-latency numbers
    // measure success-path latency instead.
    'ipam_deny_rate': ['rate>0.90'],
  },
};

export function setup() {
  const p = createIPPool(POOL_NAME, '192.168.100.0/28', {
    ipFamily: 'IPv4',
    visibility: SHARED_VISIBILITY,
    minLen: 30,
    maxLen: 30,
    strategy: 'FirstFit',
  });
  if (p.status !== 201 && p.status !== 409) {
    throw new Error(`pool create failed: ${p.status} ${p.body}`);
  }

  // ClusterRole + bindings so cross-project callers can issue use claims.
  // CanUsePool targets the ippools resource.
  const role = createClusterRole(EXHAUST_USER_ROLE, [
    {
      apiGroups: ['ipam.miloapis.com'],
      resources: ['ippools'],
      resourceNames: [POOL_NAME],
      verbs: ['use'],
    },
  ]);
  if (role.status !== 201 && role.status !== 409) {
    console.error(`exhaust user role create: ${role.status} ${role.body}`);
  }
  for (let n = 1; n < PROJECT_COUNT; n++) {
    const projectID = projectIDFor(n);
    const bRes = createClusterRoleBinding(
      `perf-exhaust-pool-user-${projectID}`,
      EXHAUST_USER_ROLE,
      [{ kind: 'Group', apiGroup: 'rbac.authorization.k8s.io', name: `system:project:${projectID}` }],
    );
    if (bRes.status !== 201 && bRes.status !== 409) {
      console.error(`exhaust binding ${projectID}: ${bRes.status} ${bRes.body}`);
    }
  }

  // Fill the pool with 4 /30 claims as project 0.
  const fillNames = [];
  for (let i = 0; i < 4; i++) {
    const name = `exhaust-fill-${i}`;
    const r = createIPClaimForProject(FILL_NAMESPACE, name, POOL_NAME, 30, OWNER_PROJECT);
    if (r.status === 201) {
      fillNames.push(name);
    } else {
      console.error(`fill ${i} status=${r.status} body=${r.body}`);
    }
  }
  console.log(`setup complete: filled pool with ${fillNames.length}/4 claims`);
  return { fillNames };
}

function record(res, mode, ns, name, callerProject) {
  if (res.status === 507) {
    denials.add(1, { mode });
    denyLatency.add(res.timings.duration, { mode });
    denyRate.add(1);
  } else if (res.status === 201) {
    // Pool not actually full (e.g., a fill claim got deleted); record but
    // don't fail the test.
    successes.add(1, { mode });
    successLatency.add(res.timings.duration, { mode });
    denyRate.add(0);
    deleteIPClaimForProject(ns, name, callerProject);
  } else {
    errors.add(1, { mode });
    denyRate.add(0);
    if (__ITER < 5) {
      console.error(`${mode} unexpected ${res.status}: ${res.body}`);
    }
  }
}

export default function () {
  const ns = nsFor(Math.floor(Math.random() * NAMESPACE_COUNT));
  const name = `exhaust-probe-${__VU}-${__ITER}`;

  // Alternate same-project (project 0) and cross-project (project 1) probes.
  if (__ITER % 2 === 0) {
    const r = createIPClaimForProject(ns, name, POOL_NAME, 30, OWNER_PROJECT);
    record(r, 'same', ns, name, OWNER_PROJECT);
  } else {
    const callerIdx = 1 + (__VU % Math.max(1, PROJECT_COUNT - 1));
    const callerProject = projectIDFor(callerIdx);
    const r = createCrossProjectIPClaim(ns, name, POOL_NAME, OWNER_PROJECT, callerProject, 30);
    record(r, 'cross', ns, name, callerProject);
  }
}

export function teardown(data) {
  for (const name of data.fillNames || []) {
    deleteIPClaimForProject(FILL_NAMESPACE, name, OWNER_PROJECT);
  }
  deleteIPPool(POOL_NAME);
  console.log('teardown complete');
}
