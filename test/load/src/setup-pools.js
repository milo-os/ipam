// setup-pools.js
//
// One-time provisioning for the load tests. Run before any other script
// (`task setup`). Idempotent: anything that already exists is left alone.
//
// In every project (PROJECTS, or PROJECT_PREFIX-0..PROJECT_COUNT-1) it creates:
//   - the flat IPv4 fixture:  IPClass perf-v4 offered by IPPool perf-v4-root
//                             (10.0.0.0/8, /20-/28)
//   - the class-chain fixture: IPClasses perf-vpc -> perf-subnet ->
//                             perf-endpoint, with perf-vpc offered by IPPool
//                             perf-vpc-root (fd20::/32)
// and the claim namespaces (NAMESPACE_PREFIX-0..NAMESPACE_COUNT-1) unless
// CREATE_NAMESPACES=false.
//
// It then proves each fixture allocates: one claim of each class must be
// Bound, and the class-chain claim must land in a pool that perf-subnet
// provisioned. A fixture that does not allocate fails setup (k6 exit 107)
// rather than letting the throughput scripts measure refusals. The work runs
// in setup(), so any failure aborts the run with k6's script-error exit code.
//
// Root pools of one project may not overlap, but two projects may hold the
// same range, so every project gets identical fixtures.

import { check } from 'k6';
import {
  CHAIN,
  FLAT,
  INSECURE,
  LOAD_LABEL,
  TENANCY,
  boundPool,
  chainFixture,
  describe,
  endpointScope,
  ensure,
  ensureNamespace,
  flatFixture,
  ipClaim,
  ipClaimPath,
  ipPoolPath,
  ipamGet,
  ipamPost,
  namespaces,
  projects,
  remove,
  uniqueName,
} from '../lib/ipam-client.js';

const CREATE_NAMESPACES = (__ENV.CREATE_NAMESPACES || 'true') !== 'false';

export const options = {
  insecureSkipTLSVerify: INSECURE,
  setupTimeout: __ENV.SETUP_TIMEOUT || '10m',
  vus: 1,
  iterations: 1,
};

function probe(project, ns, className, spec, expectSubnetPool) {
  const name = uniqueName('setup-probe');
  const res = ipamPost(project, ipClaimPath(ns), ipClaim(ns, name, Object.assign({ className }, spec), LOAD_LABEL), 'setup_probe');
  const ok = check(res, { [`${className} claim binds`]: (r) => r.status === 201 });
  if (!ok) {
    throw new Error(`project ${project}: ${className} claim did not bind: ${describe(res)}`);
  }
  if (expectSubnetPool) {
    const poolName = boundPool(res);
    const pool = ipamGet(project, ipPoolPath(poolName), 'setup_probe_pool');
    let classRef = '';
    try {
      classRef = (pool.json().spec.classRef || {}).name || '';
    } catch (e) { /* reported below */ }
    const chained = check(pool, {
      'chain claim lands in a pool perf-subnet provisioned': () => classRef === CHAIN.subnetClass,
    });
    if (!chained) {
      throw new Error(`project ${project}: chain claim bound to pool "${poolName}" with classRef "${classRef}", expected ${CHAIN.subnetClass}`);
    }
  }
  remove(project, 'IPClaim', name, ns);
}

export function setup() {
  const ps = projects();
  const nss = namespaces();
  console.log(`setup: tenancy=${TENANCY} projects=${ps.join(',')} namespaces=${nss.join(',')}`);

  for (const project of ps) {
    if (CREATE_NAMESPACES) {
      for (const ns of nss) ensureNamespace(project, ns);
    }
    for (const obj of flatFixture()) ensure(project, obj);
    for (const obj of chainFixture()) ensure(project, obj);

    probe(project, nss[0], FLAT.className, { prefixLength: FLAT.claimPrefixLength }, false);
    // A scope used only by setup: its vpc and subnet pools exist afterwards,
    // which is harmless, and proves the whole chain provisions.
    probe(project, nss[0], CHAIN.endpointClass, { scope: endpointScope('setup-probe', 'loc-0') }, true);
    console.log(`setup: project ${project} ready`);
  }
}

export default function () {
  // All the work is in setup(), where a failure aborts the run.
}
