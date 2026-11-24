// cleanup.js
//
// Removes everything the load tests create, in every project, through the
// same transport the tests use (so it works for both IPAM_TENANCY modes; a
// kubectl loop cannot carry the project).
//
// Order matters, because a pool cannot go while something is carved from it:
//   1. IPClaims carrying the load-test label, in every namespace
//   2. pools the class chain provisioned (subnet pools, then vpc pools)
//   3. the operator-authored root pools
//   4. the classes, leaf first
//   5. the claim namespaces, unless DELETE_NAMESPACES=false
//
// Anything already gone is skipped. Run via `task cleanup`.

import { check } from 'k6';
import {
  CHAIN,
  FLAT,
  INSECURE,
  TENANCY,
  batchDelete,
  batchRun,
  deleteNamespace,
  ipClaimPath,
  listItems,
  namespaces,
  projects,
  pruneChainPools,
  remove,
} from '../lib/ipam-client.js';

const DELETE_NAMESPACES = (__ENV.DELETE_NAMESPACES || 'true') !== 'false';

export const options = {
  insecureSkipTLSVerify: INSECURE,
  setupTimeout: __ENV.SETUP_TIMEOUT || '20m',
  vus: 1,
  iterations: 1,
};

const ROOT_POOLS = [FLAT.poolName, CHAIN.rootPool, 'perf-exhaust-pool', 'perf-scale-pool'];
const CLASSES = [CHAIN.endpointClass, CHAIN.subnetClass, CHAIN.vpcClass, FLAT.className, 'perf-exhaust', 'perf-scale'];

function gone(r) {
  return r.status === 200 || r.status === 202 || r.status === 404;
}

export function setup() {
  console.log(`cleanup: tenancy=${TENANCY} projects=${projects().join(',')}`);
  for (const project of projects()) {
    const claims = listItems(project, ipClaimPath())
      .filter((c) => ((c.metadata || {}).labels || {})['ipam.miloapis.com/load-test'] === 'true');
    const res = batchRun(claims.map((c) => batchDelete(project, ipClaimPath(c.metadata.namespace, c.metadata.name), 'cleanup_claim')));
    check(res, { 'claims deleted': (rs) => rs.every(gone) });

    const pools = pruneChainPools(project, () => true);

    for (const p of ROOT_POOLS) {
      const r = remove(project, 'IPPool', p);
      check(r, { [`pool ${p} deleted`]: gone });
      if (!gone(r)) console.warn(`${project}: pool ${p}: HTTP ${r.status} ${r.body}`);
    }
    for (const c of CLASSES) {
      const r = remove(project, 'IPClass', c);
      check(r, { [`class ${c} deleted`]: gone });
      if (!gone(r)) console.warn(`${project}: class ${c}: HTTP ${r.status} ${r.body}`);
    }
    if (DELETE_NAMESPACES) {
      for (const ns of namespaces()) deleteNamespace(project, ns);
    }
    console.log(`cleanup: ${project}: ${claims.length} claims, ${pools} provisioned pools removed`);
  }
}

export default function () {}
