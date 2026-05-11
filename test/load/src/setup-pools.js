// setup-pools.js
//
// One-time provisioning for IPAM performance tests, multi-tenant aware.
//
// Layout produced:
//   Platform-level (kept for backwards compatibility with older tests):
//     - IPPrefixClass `perf-private`        (visibility: consumer)
//     - IPPrefix       `perf-prefix` (10.0.0.0/8, /20-/28)
//     - ASNPoolClass   `perf-asn`
//     - ASNPool        `perf-asn-pool` (4200000000-4200099999)
//
//   Per-project (one set per perf project, n in [0, PROJECT_COUNT)):
//     - IPPrefix       `perf-prefix-<n>`         covering 10.<n>.0.0/16  (/20-/28)
//     - IPPrefix       `perf-ipv6-prefix-<n>`    covering fd<NN>:<n>::/32 (/40-/56)
//     - ASNPool        `perf-asn-pool-<n>`       each spanning 20k ASNs
//
//   Shared cross-project pool (owned by project 0):
//     - IPPrefixClass  `perf-shared`             (visibility: shared, IPv4)
//     - IPPrefix       `perf-shared-prefix`      (172.16.0.0/12, /24-/28)
//     - IPPrefixClass  `perf-ipv6-shared-class`  (visibility: shared, IPv6)
//     - IPPrefix       `perf-ipv6-shared`        (fd00:ffff::/28, /40-/56)
//     - ClusterRole    `perf-shared-pool-user`       (use on perf-shared-prefix)
//     - ClusterRole    `perf-ipv6-shared-pool-user`  (use on perf-ipv6-shared)
//     - ClusterRoleBinding per project [1..N) granting use of each shared pool
//
//   Namespaces: `ipam-perf-<n>` for n in [0, NAMESPACE_COUNT)
//
// Run with: task -t test/load/Taskfile.yaml setup
//
// Configuration:
//   IPAM_API_URL    - Apiserver URL (default localhost:8001)
//   NAMESPACE_COUNT - How many ipam-perf-* namespaces to create (default 10)
//   PROJECT_COUNT   - How many perf projects to provision (default 5)

import { check, sleep } from 'k6';
import {
  createPrefixClass,
  createPrefix,
  createASNPoolClass,
  createASNPool,
  createNamespace,
  createClusterRole,
  createClusterRoleBinding,
  nsFor,
  projectIDFor,
} from '../lib/ipam-client.js';

const NAMESPACE_COUNT = parseInt(__ENV.NAMESPACE_COUNT || '10');
const PROJECT_COUNT = parseInt(__ENV.PROJECT_COUNT || '5');
const SETUP_VUS = parseInt(__ENV.SETUP_VUS || '1');
// IPPrefixClass.spec.visibility for the cross-project pool. The server
// accepts any string for Visibility (plain string field with no enum
// validation), so 'shared' is accepted today and matches the documented
// intent.
const SHARED_VISIBILITY = __ENV.SHARED_VISIBILITY || 'shared';

// Each per-project ASN pool spans 20k ASNs starting at this base.
const ASN_BASE = 4200000000;
const ASN_PER_PROJECT = 20000;

const SHARED_CLASS_NAME = 'perf-shared';
const SHARED_PREFIX_NAME = 'perf-shared-prefix';
const SHARED_POOL_USER_ROLE = 'perf-shared-pool-user';

// IPv6 layout. ULA prefix space (fd00::/8) provides 16M /16s for testing.
// Per-project /32 pools at fd00:<2-byte-project>::/32, each large enough to
// carve thousands of /48 customer prefixes.
//
// We use /40-/56 as the allowed claim range to mirror real-world allocations:
//   /40 ≈ regional carve
//   /48 ≈ per-customer site assignment (RFC 6177 baseline)
//   /56 ≈ home-network handoff
//
// minPrefixLength=40 corresponds to a SMALLER prefix length number (LARGER
// block), maxPrefixLength=56 a LARGER number (SMALLER block).
const SHARED_IPV6_CLASS_NAME = 'perf-ipv6-shared-class';
const SHARED_IPV6_PREFIX_NAME = 'perf-ipv6-shared';
const IPV6_POOL_USER_ROLE = 'perf-ipv6-shared-pool-user';
const IPV6_MIN_LEN = 40;
const IPV6_MAX_LEN = 56;

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    setup: {
      executor: 'shared-iterations',
      vus: SETUP_VUS,
      iterations: SETUP_VUS,
      maxDuration: '10m',
    },
  },
};

function okOrConflict(name) {
  return (res) => res.status === 201 || res.status === 409;
}

export default function () {
  // ---- Platform-level pool (legacy / compatibility) ----
  let r = createPrefixClass('perf-private', {
    requiresVerification: false,
    visibility: 'consumer',
    minLen: 20,
    maxLen: 28,
    strategy: 'FirstFit',
  });
  check(r, { 'perf-private class created or exists': okOrConflict() });

  r = createPrefix('perf-prefix', '10.0.0.0/8', 'perf-private', {
    ipFamily: 'IPv4',
    minLen: 20,
    maxLen: 28,
    strategy: 'FirstFit',
  });
  check(r, { 'perf-prefix created or exists': okOrConflict() });

  r = createASNPoolClass('perf-asn', { requiresVerification: false, visibility: 'consumer' });
  check(r, { 'perf-asn class created or exists': okOrConflict() });

  r = createASNPool(
    'perf-asn-pool',
    [{ start: 4200000000, end: 4200099999 }],
    'perf-asn',
  );
  check(r, { 'perf-asn-pool created or exists': okOrConflict() });

  // ---- Per-project private pools ----
  // Each VU handles its own slice of [0, PROJECT_COUNT) so setup parallelises
  // across SETUP_VUS workers. __VU is 1-based; slice boundaries are computed
  // so every project is covered with no gaps or overlaps.
  const vuIndex = __VU - 1; // 0-based
  const sliceSize = Math.ceil(PROJECT_COUNT / SETUP_VUS);
  const sliceStart = vuIndex * sliceSize;
  const sliceEnd = Math.min(sliceStart + sliceSize, PROJECT_COUNT);

  let projectPrefixes = 0;
  let projectASNPools = 0;
  let projectIPv6Prefixes = 0;
  for (let n = sliceStart; n < sliceEnd; n++) {
    const prefixName = `perf-prefix-${n}`;
    // CIDR: projects 0-255 → 10.0.x.x/16, 256-511 → 10.1.x.x/16, etc.
    // Uses octets 10-13 (covering 0-1023 projects within RFC1918 space).
    const cidr = `${10 + Math.floor(n / 256)}.${n % 256}.0.0/16`;
    const pres = createPrefix(prefixName, cidr, 'perf-private', {
      ipFamily: 'IPv4',
      minLen: 20,
      maxLen: 28,
      strategy: 'FirstFit',
    });
    if (pres.status === 201 || pres.status === 409) {
      projectPrefixes++;
    } else {
      console.error(`per-project prefix ${prefixName} create failed: ${pres.status} ${pres.body}`);
    }

    // Per-project IPv6 pool. fd<HH>:<LLLL>::/32 with HH = n>>8, LLLL = n&0xff.
    // Project 0 → fd00:0000::/32, project 1 → fd00:0001::/32, ...
    // Up to 65536 perf projects fit in fd00::/16 without collisions.
    const v6Prefix = `perf-ipv6-prefix-${n}`;
    const hi = (n >> 8) & 0xff;
    const lo = n & 0xff;
    const v6Cidr =
      `fd${hi.toString(16).padStart(2, '0')}:` +
      `${lo.toString(16).padStart(4, '0')}::/32`;
    const v6Res = createPrefix(v6Prefix, v6Cidr, 'perf-private', {
      ipFamily: 'IPv6',
      minLen: IPV6_MIN_LEN,
      maxLen: IPV6_MAX_LEN,
      strategy: 'FirstFit',
    });
    if (v6Res.status === 201 || v6Res.status === 409) {
      projectIPv6Prefixes++;
    } else {
      console.error(`per-project IPv6 prefix ${v6Prefix} create failed: ${v6Res.status} ${v6Res.body}`);
    }

    const asnPoolName = `perf-asn-pool-${n}`;
    const asnStart = ASN_BASE + n * ASN_PER_PROJECT;
    const asnEnd = asnStart + ASN_PER_PROJECT - 1;
    const ares = createASNPool(asnPoolName, [{ start: asnStart, end: asnEnd }], 'perf-asn');
    if (ares.status === 201 || ares.status === 409) {
      projectASNPools++;
    } else {
      console.error(`per-project ASN pool ${asnPoolName} create failed: ${ares.status} ${ares.body}`);
    }
  }
  check(projectPrefixes, { 'per-vu prefixes created': (n) => n === sliceEnd - sliceStart });
  check(projectIPv6Prefixes, { 'per-vu IPv6 prefixes created': (n) => n === sliceEnd - sliceStart });
  check(projectASNPools, { 'per-vu ASN pools created': (n) => n === sliceEnd - sliceStart });

  // ---- Shared cross-project pool (owned by project 0) ----
  r = createPrefixClass(SHARED_CLASS_NAME, {
    requiresVerification: false,
    visibility: SHARED_VISIBILITY,
    minLen: 24,
    maxLen: 28,
    strategy: 'FirstFit',
  });
  check(r, { 'perf-shared class created or exists': okOrConflict() });

  r = createPrefix(SHARED_PREFIX_NAME, '172.16.0.0/12', SHARED_CLASS_NAME, {
    ipFamily: 'IPv4',
    minLen: 24,
    maxLen: 28,
    strategy: 'FirstFit',
  });
  check(r, { 'perf-shared-prefix created or exists': okOrConflict() });

  // ClusterRole granting the `use` verb on the shared pool
  r = createClusterRole(SHARED_POOL_USER_ROLE, [
    {
      apiGroups: ['ipam.miloapis.com'],
      resources: ['ipprefixes'],
      resourceNames: [SHARED_PREFIX_NAME],
      verbs: ['use'],
    },
  ]);
  check(r, { 'perf-shared-pool-user role created or exists': okOrConflict() });

  // ClusterRoleBinding per other project (1..N-1). Project 0 owns the pool.
  // Subjects use Group with a name shaped like the project ID — once Milo's
  // multi-tenant authorizer is implemented, it will resolve these against
  // the parent-project extras injected by the front-door.
  let bindings = 0;
  for (let n = 1; n < PROJECT_COUNT; n++) {
    const projectID = projectIDFor(n);
    const bindingName = `perf-shared-pool-user-${projectID}`;
    const subj = [
      {
        kind: 'Group',
        apiGroup: 'rbac.authorization.k8s.io',
        name: `system:project:${projectID}`,
      },
    ];
    const bres = createClusterRoleBinding(bindingName, SHARED_POOL_USER_ROLE, subj);
    if (bres.status === 201 || bres.status === 409) {
      bindings++;
    } else {
      console.error(`binding ${bindingName} create failed: ${bres.status} ${bres.body}`);
    }
  }
  check(bindings, { 'all shared-pool bindings': (n) => n === PROJECT_COUNT - 1 });

  // ---- Shared IPv6 cross-project pool (owned by project 0) ----
  // fd00:f000::/28 sits above the per-project /32s (which use lo bytes 0..ff
  // in the second 16-bit group), so it can never overlap with a per-project
  // pool no matter how PROJECT_COUNT grows.
  r = createPrefixClass(SHARED_IPV6_CLASS_NAME, {
    requiresVerification: false,
    visibility: SHARED_VISIBILITY,
    minLen: IPV6_MIN_LEN,
    maxLen: IPV6_MAX_LEN,
    strategy: 'FirstFit',
  });
  check(r, { 'perf-ipv6-shared-class created or exists': okOrConflict() });

  r = createPrefix(SHARED_IPV6_PREFIX_NAME, 'fd00:f000::/28', SHARED_IPV6_CLASS_NAME, {
    ipFamily: 'IPv6',
    minLen: IPV6_MIN_LEN,
    maxLen: IPV6_MAX_LEN,
    strategy: 'FirstFit',
  });
  check(r, { 'perf-ipv6-shared created or exists': okOrConflict() });

  r = createClusterRole(IPV6_POOL_USER_ROLE, [
    {
      apiGroups: ['ipam.miloapis.com'],
      resources: ['ipprefixes'],
      resourceNames: [SHARED_IPV6_PREFIX_NAME],
      verbs: ['use'],
    },
  ]);
  check(r, { 'perf-ipv6-shared-pool-user role created or exists': okOrConflict() });

  let v6Bindings = 0;
  for (let n = 1; n < PROJECT_COUNT; n++) {
    const projectID = projectIDFor(n);
    const bindingName = `${IPV6_POOL_USER_ROLE}-${projectID}`;
    const subj = [
      {
        kind: 'Group',
        apiGroup: 'rbac.authorization.k8s.io',
        name: `system:project:${projectID}`,
      },
    ];
    const bres = createClusterRoleBinding(bindingName, IPV6_POOL_USER_ROLE, subj);
    if (bres.status === 201 || bres.status === 409) {
      v6Bindings++;
    } else {
      console.error(`ipv6 binding ${bindingName} create failed: ${bres.status} ${bres.body}`);
    }
  }
  check(v6Bindings, { 'all ipv6 shared-pool bindings': (n) => n === PROJECT_COUNT - 1 });

  // ---- Namespaces ----
  let nsCreated = 0;
  for (let i = 0; i < NAMESPACE_COUNT; i++) {
    const ns = nsFor(i);
    const nsRes = createNamespace(ns);
    if (nsRes.status === 201 || nsRes.status === 409) nsCreated++;
    else console.error(`ns ${ns} create failed: ${nsRes.status}`);
  }
  check(nsCreated, { 'all namespaces created': (n) => n === NAMESPACE_COUNT });

  // Allow a moment for resources to reconcile
  sleep(2);

  console.log(
    `setup complete: platform pool perf-prefix(/8), ${projectPrefixes}/${PROJECT_COUNT} per-project /16 prefixes, ` +
      `${projectIPv6Prefixes}/${PROJECT_COUNT} per-project IPv6 /32 prefixes, ` +
      `${projectASNPools}/${PROJECT_COUNT} per-project ASN pools, shared pool perf-shared-prefix(/12), ` +
      `shared IPv6 pool ${SHARED_IPV6_PREFIX_NAME}(/28), ` +
      `${bindings}/${PROJECT_COUNT - 1} v4 bindings, ${v6Bindings}/${PROJECT_COUNT - 1} v6 bindings, ` +
      `${nsCreated}/${NAMESPACE_COUNT} namespaces`,
  );
}
