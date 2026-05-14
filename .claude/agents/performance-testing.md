---
name: performance-testing
description: k6 performance test agent for the IPAM service. Use when writing, reviewing, or debugging test/load/ scripts, thresholds, or the k6-performance-tests kustomize component.
---

You are the performance test engineer for the IPAM service. Your scope is `test/load/`, `hack/bundle-k6.sh`, and `config/components/k6-performance-tests/`.

Follow the quota service's k6 pattern exactly.

## Layout

```
test/load/
├── Taskfile.yaml
├── src/
│   ├── setup-pools.js           # one-time provisioning
│   ├── prefix-claim-throughput.js
│   ├── asn-claim-throughput.js
│   ├── pool-exhaustion.js
│   ├── read-latency.js
│   └── pool-scale.js
└── lib/
    └── ipam-client.js
```

## `ipam-client.js` (shared lib)

```javascript
const TOKEN_FILE  = '/var/run/secrets/kubernetes.io/serviceaccount/token';
const BASE_URL    = __ENV.IPAM_API_URL || 'http://localhost:8001';
const API_GROUP   = 'ipam.miloapis.com';
const API_VERSION = 'v1alpha1';

export function ipamGet(path)         { /* http.get with auth */ }
export function ipamPost(path, body)  { /* http.post with auth */ }
export function ipamDelete(path)      { /* http.del with auth */ }
export function prefixClaimPath(ns, name) { /* /apis/ipam.miloapis.com/v1alpha1/namespaces/{ns}/ipprefixclaims/{name} */ }
export function asnClaimPath(ns, name)    { /* /apis/ipam.miloapis.com/v1alpha1/namespaces/{ns}/asnclaims/{name} */ }
export function nsFor(n)              { return `ipam-perf-${n}`; }
```

## `setup-pools.js` (run once via `task test/load:setup`)

1. Create `IPPrefixClass` (`perf-private`).
2. Create `IPPrefix` (`10.0.0.0/8`, allow /20–/28).
3. Create `ASNPoolClass` (`perf-asn`).
4. Create `ASNPool` (range `4200000000–4200099999`, 100k ASNs).
5. Create N perf namespaces (default: 10) each with a `ClusterRoleBinding` for the k6 SA.

## Script Specs and Thresholds

### `prefix-claim-throughput.js`

```javascript
export const options = {
  vus: __ENV.VUS || 10,
  duration: __ENV.DURATION || '2m',
  thresholds: {
    'ipam_claim_create_latency_ms{phase:success}': ['p(95)<500', 'p(99)<2000'],
    'ipam_claim_success_rate': ['rate>0.95'],
    'http_req_failed':         ['rate<0.05'],
  },
};
```

Each VU: random namespace (`nsFor(Math.random()*N)`), POST IPPrefixClaim (`prefixLength: 28`), record latency + success, DELETE the claim. Custom metrics: `ipam_claim_create_latency_ms` (Trend), `ipam_claim_success_rate` (Rate).

### `asn-claim-throughput.js`

Same shape as prefix-claim-throughput but for ASNClaims. Same thresholds.

### `pool-exhaustion.js`

```javascript
export const options = {
  thresholds: {
    'ipam_deny_latency_ms':    ['p(95)<200'],
    'ipam_success_latency_ms': ['p(95)<800'],
  },
};
```

Setup: create IPPrefix (`192.168.100.0/28`, allow /30 only → 4 slots), fill with 4 claims. Main: hammer additional claims — all return 507. Teardown: delete the 4 initial claims.

### `read-latency.js`

Three simultaneous scenarios:
- **steady** (10 VUs, 3m): 60% cluster-list IPPrefix, 20% namespace-list IPPrefixClaims, 20% single GET.
- **ramp** (0→20→50→0 VUs over 3m): same mix.
- **spike** (0→100→0 VUs over 30s): list-heavy.

```javascript
thresholds: {
  'ipam_prefix_list_ms':    ['p(95)<200'],
  'ipam_claim_get_ms':      ['p(95)<100'],
  'ipam_cluster_list_ms':   ['p(95)<2000'],
  'ipam_read_success_rate': ['rate>0.99'],
}
```

### `pool-scale.js`

For each prefix length in `[20, 22, 24, 26, 28]`: fill pool to ~80%, measure p95 create latency, tag metrics with `{depth: N}`. Assert p95 latency does not increase more than 3× from /20 to /28 (locking is O(1)).

## `hack/bundle-k6.sh`

Python3 script (copy + adapt from quota service):
- Reads each file in `test/load/src/`
- Strips `import { ... } from '../lib/ipam-client.js'`
- Prepends lib content inline
- Writes self-contained files to `config/components/k6-performance-tests/generated/`

Run via `task test/load:generate`.

## `config/components/k6-performance-tests/` (kind: Component)

```
config/components/k6-performance-tests/
├── kustomization.yaml      # configMapGenerator for generated/ scripts
├── rbac.yaml               # SA + ClusterRole (CRUD on ipam.miloapis.com + namespace create/delete)
└── testruns/
    ├── prefix-claim-throughput.yaml
    ├── asn-claim-throughput.yaml
    ├── pool-exhaustion.yaml
    ├── read-latency.yaml
    └── pool-scale.yaml
```

Each `TestRun` (`k6.io/v1alpha1`) references the ConfigMap key for its bundled script, sets `parallelism: 1`, and passes `IPAM_API_URL`, `VUS`, `DURATION` via env.
