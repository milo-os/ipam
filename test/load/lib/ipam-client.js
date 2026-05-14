// Shared HTTP client for the IPAM apiserver. Provides typed helpers for the
// nine IPAM resources and standardized request configuration.
//
// Configuration via environment variables:
//   IPAM_API_URL    - Base URL of the apiserver (default: kubectl proxy localhost:8001)
//   IPAM_TOKEN      - Explicit bearer token (overrides in-cluster SA token)
//   IPAM_TOKEN_FILE - Path to a file containing a bearer token (default: SA token path)
//   K6_INSECURE_SKIP_TLS_VERIFY - Skip TLS verification (default: true)
//
// When running inside the k6 operator, the test pod's ServiceAccount token
// is mounted at /var/run/secrets/kubernetes.io/serviceaccount/token. The
// client reads it automatically at init time if IPAM_TOKEN isn't set.

import http from 'k6/http';

export const BASE_URL = __ENV.IPAM_API_URL || 'http://localhost:8001';
export const API_GROUP = 'ipam.miloapis.com';
export const API_VERSION = 'v1alpha1';
export const API_BASE = `${BASE_URL}/apis/${API_GROUP}/${API_VERSION}`;

const DEFAULT_TOKEN_PATH = '/var/run/secrets/kubernetes.io/serviceaccount/token';

function loadToken() {
  if (__ENV.IPAM_TOKEN) {
    return __ENV.IPAM_TOKEN;
  }
  const path = __ENV.IPAM_TOKEN_FILE || DEFAULT_TOKEN_PATH;
  try {
    return open(path).trim();
  } catch (e) {
    return '';
  }
}

const TOKEN = loadToken();

export function defaultHeaders() {
  const h = {
    'Content-Type': 'application/json',
    'Accept': 'application/json',
  };
  if (TOKEN) {
    h['Authorization'] = `Bearer ${TOKEN}`;
  }
  return h;
}

function defaultParams(tag) {
  return {
    headers: defaultHeaders(),
    tags: { operation: tag },
  };
}

// Returns k6 params with Milo tenant headers identifying the calling project.
// Merges with defaultHeaders() so auth + content-type are preserved.
export function withProject(projectID) {
  return {
    headers: {
      ...defaultHeaders(),
      'X-Remote-Extra-Iam.Miloapis.Com.Parent-Api-Group': 'resourcemanager.miloapis.com',
      'X-Remote-Extra-Iam.Miloapis.Com.Parent-Type':      'Project',
      'X-Remote-Extra-Iam.Miloapis.Com.Parent-Name':      projectID,
    },
    tags: {},
  };
}

// withProjectTagged is like withProject but also sets an operation tag.
export function withProjectTagged(projectID, tag) {
  const p = withProject(projectID);
  p.tags = { operation: tag };
  return p;
}

// --- Generic helpers ---

export function ipamGet(path, tag) {
  return http.get(`${API_BASE}${path}`, defaultParams(tag || 'get'));
}

export function ipamPost(path, body, tag) {
  return http.post(`${API_BASE}${path}`, JSON.stringify(body), defaultParams(tag || 'post'));
}

export function ipamDelete(path, tag) {
  return http.del(`${API_BASE}${path}`, null, defaultParams(tag || 'delete'));
}

export function ipamList(path, tag) {
  return http.get(`${API_BASE}${path}`, defaultParams(tag || 'list'));
}

// --- Path helpers ---

export function nsFor(n) {
  return `ipam-perf-${n}`;
}

export function prefixClaimPath(ns, name) {
  return name
    ? `/namespaces/${ns}/ipprefixclaims/${name}`
    : `/namespaces/${ns}/ipprefixclaims`;
}

export function ipAddressClaimPath(ns, name) {
  return name
    ? `/namespaces/${ns}/ipaddressclaims/${name}`
    : `/namespaces/${ns}/ipaddressclaims`;
}

export function asnClaimPath(ns, name) {
  return name
    ? `/namespaces/${ns}/asnclaims/${name}`
    : `/namespaces/${ns}/asnclaims`;
}

export function prefixPath(name) {
  return name ? `/ipprefixes/${name}` : '/ipprefixes';
}

export function prefixClassPath(name) {
  return name ? `/ipprefixclasses/${name}` : '/ipprefixclasses';
}

export function asnPoolPath(name) {
  return name ? `/asnpools/${name}` : '/asnpools';
}

export function asnPoolClassPath(name) {
  return name ? `/asnpoolclasses/${name}` : '/asnpoolclasses';
}

// IPAddress is namespaced (the cluster-allocated address resource — distinct
// from IPAddressClaim).
export function ipAddressPath(ns, name) {
  return name
    ? `/namespaces/${ns}/ipaddresses/${name}`
    : `/namespaces/${ns}/ipaddresses`;
}

// --- Resource builders ---

export function ipPrefixClass(name, { visibility = 'consumer', minLen = 20, maxLen = 28, strategy = 'FirstFit' } = {}) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'IPPrefixClass',
    metadata: { name },
    spec: {
      visibility,
      defaultAllocation: { minPrefixLength: minLen, maxPrefixLength: maxLen, strategy },
    },
  };
}

export function ipPrefix(name, cidr, classRef, { ipFamily = 'IPv4', minLen = 20, maxLen = 28, strategy = 'FirstFit' } = {}) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'IPPrefix',
    metadata: { name },
    spec: {
      cidr,
      ipFamily,
      classRef: { name: classRef },
      allocation: { minPrefixLength: minLen, maxPrefixLength: maxLen, strategy },
    },
  };
}

export function ipPrefixClaim(ns, name, prefixRef, prefixLength, { ipFamily = 'IPv4', reclaimPolicy = 'Delete' } = {}) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'IPPrefixClaim',
    metadata: { name, namespace: ns },
    spec: {
      ipFamily,
      prefixLength,
      prefixRef: { name: prefixRef },
      reclaimPolicy,
    },
  };
}

export function ipAddressClaim(ns, name, prefixRef, { ipFamily = 'IPv4', reclaimPolicy = 'Delete' } = {}) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'IPAddressClaim',
    metadata: { name, namespace: ns },
    spec: {
      ipFamily,
      prefixRef: { name: prefixRef },
      reclaimPolicy,
    },
  };
}

export function asnPoolClass(name, { visibility = 'consumer' } = {}) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'ASNPoolClass',
    metadata: { name },
    spec: { visibility },
  };
}

export function asnPool(name, ranges, classRef) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'ASNPool',
    metadata: { name },
    spec: { ranges, classRef: { name: classRef } },
  };
}

export function asnClaim(ns, name, poolRef) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'ASNClaim',
    metadata: { name, namespace: ns },
    spec: { poolRef: { name: poolRef } },
  };
}

// asnClaimWithClassRef builds an ASNClaim driven by spec.classRef rather than
// spec.poolRef. The apiserver picks a pool that matches the class. Mutually
// exclusive with poolRef in the resource model.
export function asnClaimWithClassRef(ns, name, classRefName) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'ASNClaim',
    metadata: { name, namespace: ns },
    spec: { classRef: { name: classRefName } },
  };
}

// --- Typed helper functions ---

export function createPrefixClaim(ns, name, prefixRef, prefixLength, opts) {
  return ipamPost(prefixClaimPath(ns), ipPrefixClaim(ns, name, prefixRef, prefixLength, opts), 'prefix_claim_create');
}

export function deletePrefixClaim(ns, name) {
  return ipamDelete(prefixClaimPath(ns, name), 'prefix_claim_delete');
}

export function getPrefixClaim(ns, name) {
  return ipamGet(prefixClaimPath(ns, name), 'prefix_claim_get');
}

export function listPrefixClaims(ns) {
  return ipamList(prefixClaimPath(ns), 'prefix_claim_list');
}

export function createIPAddressClaim(ns, name, prefixRef, opts) {
  return ipamPost(ipAddressClaimPath(ns), ipAddressClaim(ns, name, prefixRef, opts), 'ip_addr_claim_create');
}

export function deleteIPAddressClaim(ns, name) {
  return ipamDelete(ipAddressClaimPath(ns, name), 'ip_addr_claim_delete');
}

export function createASNClaim(ns, name, poolRef) {
  return ipamPost(asnClaimPath(ns), asnClaim(ns, name, poolRef), 'asn_claim_create');
}

export function deleteASNClaim(ns, name) {
  return ipamDelete(asnClaimPath(ns, name), 'asn_claim_delete');
}

export function getASNClaim(ns, name) {
  return ipamGet(asnClaimPath(ns, name), 'asn_claim_get');
}

export function listASNClaims(ns) {
  return ipamList(asnClaimPath(ns), 'asn_claim_list');
}

export function createPrefixClass(name, opts) {
  return ipamPost(prefixClassPath(), ipPrefixClass(name, opts), 'prefix_class_create');
}

export function createPrefix(name, cidr, classRef, opts) {
  return ipamPost(prefixPath(), ipPrefix(name, cidr, classRef, opts), 'prefix_create');
}

export function listPrefixes() {
  return ipamList(prefixPath(), 'prefix_list');
}

export function getPrefix(name) {
  return ipamGet(prefixPath(name), 'prefix_get');
}

export function deletePrefix(name) {
  return ipamDelete(prefixPath(name), 'prefix_delete');
}

export function createASNPoolClass(name, opts) {
  return ipamPost(asnPoolClassPath(), asnPoolClass(name, opts), 'asn_pool_class_create');
}

export function createASNPool(name, ranges, classRef) {
  return ipamPost(asnPoolPath(), asnPool(name, ranges, classRef), 'asn_pool_create');
}

// --- Namespace helpers (core API) ---

export function createNamespace(name) {
  const body = {
    apiVersion: 'v1',
    kind: 'Namespace',
    metadata: { name },
  };
  return http.post(`${BASE_URL}/api/v1/namespaces`, JSON.stringify(body), defaultParams('ns_create'));
}

export function deleteNamespace(name) {
  return http.del(`${BASE_URL}/api/v1/namespaces/${name}`, null, defaultParams('ns_delete'));
}

// --- RBAC helpers (core API, used by setup) ---

export function createClusterRole(name, rules) {
  const body = {
    apiVersion: 'rbac.authorization.k8s.io/v1',
    kind: 'ClusterRole',
    metadata: { name },
    rules,
  };
  return http.post(
    `${BASE_URL}/apis/rbac.authorization.k8s.io/v1/clusterroles`,
    JSON.stringify(body),
    defaultParams('cluster_role_create'),
  );
}

export function createClusterRoleBinding(name, roleName, subjects) {
  const body = {
    apiVersion: 'rbac.authorization.k8s.io/v1',
    kind: 'ClusterRoleBinding',
    metadata: { name },
    roleRef: {
      apiGroup: 'rbac.authorization.k8s.io',
      kind: 'ClusterRole',
      name: roleName,
    },
    subjects,
  };
  return http.post(
    `${BASE_URL}/apis/rbac.authorization.k8s.io/v1/clusterrolebindings`,
    JSON.stringify(body),
    defaultParams('cluster_role_binding_create'),
  );
}

// --- Multi-tenant helpers ---

// projectIDFor returns the perf project ID for index n.
export function projectIDFor(n) {
  return `ipam-perf-${n}`;
}

// Cross-project prefix claim body — includes projectRef pointing at sourceProjectID.
export function crossProjectPrefixClaim(ns, name, poolName, sourceProjectID, prefixLength, opts = {}) {
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'IPPrefixClaim',
    metadata: { name, namespace: ns },
    spec: {
      ipFamily: opts.ipFamily || 'IPv4',
      prefixLength,
      prefixRef: {
        name: poolName,
        projectRef: { name: sourceProjectID },
      },
      reclaimPolicy: opts.reclaimPolicy || 'Delete',
    },
  };
}

// createCrossProjectPrefixClaim posts a cross-project claim with tenant headers
// for callerProjectID, targeting a pool owned by sourceProjectID.
export function createCrossProjectPrefixClaim(ns, name, poolName, sourceProjectID, callerProjectID, prefixLength, opts = {}) {
  const body = crossProjectPrefixClaim(ns, name, poolName, sourceProjectID, prefixLength, opts);
  const params = withProjectTagged(callerProjectID, 'cross_project_prefix_claim_create');
  return http.post(`${API_BASE}${prefixClaimPath(ns)}`, JSON.stringify(body), params);
}

export function createPrefixClaimForProject(ns, name, prefixRef, prefixLength, projectID, opts = {}) {
  const body = ipPrefixClaim(ns, name, prefixRef, prefixLength, opts);
  const params = withProjectTagged(projectID, 'prefix_claim_create');
  return http.post(`${API_BASE}${prefixClaimPath(ns)}`, JSON.stringify(body), params);
}

// buildPrefixClaimRequest returns an http.batch()-compatible descriptor instead
// of firing the request. Use when multiple claims must be sent concurrently from
// a single VU to test SELECT...FOR UPDATE contention.
export function buildPrefixClaimRequest(ns, name, prefixRef, prefixLength, projectID, opts = {}) {
  return {
    method: 'POST',
    url: `${API_BASE}${prefixClaimPath(ns)}`,
    body: JSON.stringify(ipPrefixClaim(ns, name, prefixRef, prefixLength, opts)),
    params: withProjectTagged(projectID, 'prefix_claim_create'),
  };
}

export function deletePrefixClaimForProject(ns, name, projectID) {
  const params = withProjectTagged(projectID, 'prefix_claim_delete');
  return http.del(`${API_BASE}${prefixClaimPath(ns, name)}`, null, params);
}

export function getPrefixClaimForProject(ns, name, projectID) {
  const params = withProjectTagged(projectID, 'prefix_claim_get');
  return http.get(`${API_BASE}${prefixClaimPath(ns, name)}`, params);
}

export function listPrefixClaimsForProject(ns, projectID) {
  const params = withProjectTagged(projectID, 'prefix_claim_list');
  return http.get(`${API_BASE}${prefixClaimPath(ns)}`, params);
}

export function listPrefixesForProject(projectID) {
  const params = withProjectTagged(projectID, 'prefix_list');
  return http.get(`${API_BASE}${prefixPath()}`, params);
}

export function getPrefixForProject(name, projectID) {
  const params = withProjectTagged(projectID, 'prefix_get');
  return http.get(`${API_BASE}${prefixPath(name)}`, params);
}

export function createASNClaimForProject(ns, name, poolRef, projectID) {
  const body = asnClaim(ns, name, poolRef);
  const params = withProjectTagged(projectID, 'asn_claim_create');
  return http.post(`${API_BASE}${asnClaimPath(ns)}`, JSON.stringify(body), params);
}

export function deleteASNClaimForProject(ns, name, projectID) {
  const params = withProjectTagged(projectID, 'asn_claim_delete');
  return http.del(`${API_BASE}${asnClaimPath(ns, name)}`, null, params);
}

// createASNClaimWithClassRefForProject posts an ASNClaim that references a
// class (not a pool). Used by asn-claim-throughput.js to validate that the
// classRef-driven claim path is healthy under load.
export function createASNClaimWithClassRefForProject(ns, name, classRefName, projectID) {
  const body = asnClaimWithClassRef(ns, name, classRefName);
  const params = withProjectTagged(projectID, 'asn_claim_create');
  return http.post(`${API_BASE}${asnClaimPath(ns)}`, JSON.stringify(body), params);
}

// IPAddressClaim helpers scoped by project tenant headers — used by the
// concurrent IPAddressClaim test.
export function createIPAddressClaimForProject(ns, name, prefixRef, projectID, opts = {}) {
  const body = ipAddressClaim(ns, name, prefixRef, opts);
  const params = withProjectTagged(projectID, 'ip_addr_claim_create');
  return http.post(`${API_BASE}${ipAddressClaimPath(ns)}`, JSON.stringify(body), params);
}

export function deleteIPAddressClaimForProject(ns, name, projectID) {
  const params = withProjectTagged(projectID, 'ip_addr_claim_delete');
  return http.del(`${API_BASE}${ipAddressClaimPath(ns, name)}`, null, params);
}

// LIST helpers used by the read-latency scenarios. All accept the project
// tenant headers so reads stay scoped to the requesting tenant.
export function listIPAddressesForProject(ns, projectID) {
  const params = withProjectTagged(projectID, 'ip_addr_list');
  return http.get(`${API_BASE}${ipAddressPath(ns)}`, params);
}

export function listASNPoolsForProject(projectID) {
  const params = withProjectTagged(projectID, 'asn_pool_list');
  return http.get(`${API_BASE}${asnPoolPath()}`, params);
}

export function listASNClaimsForProject(ns, projectID) {
  const params = withProjectTagged(projectID, 'asn_claim_list');
  return http.get(`${API_BASE}${asnClaimPath(ns)}`, params);
}
