# Runbook: Trace an IPAM claim end-to-end

**Audience:** operator debugging *"why did this claim allocate where it did, or
get denied?"*

IPAM emits OpenTelemetry traces for every allocation and release. A single
trace shows the full causal chain — request → tenant resolution → cross-project
authorization decision → block selection → the individual SQL statements
(including the `SELECT … FOR UPDATE` lock wait) — turning a multi-hop
investigation into one view.

---

## 1. Is tracing on?

Tracing is **opt-in per overlay**. It is enabled when the `tracing` component
(`config/components/tracing/`) is composed into the running overlay, which:

- ships the `ipam-tracing-config` ConfigMap (collector endpoint + sampling), and
- passes `--tracing-config-file=/etc/ipam/tracing/tracing.yaml` to the apiserver.

Check the running pod:

```bash
kubectl -n ipam-system get deploy ipam-apiserver -o yaml | grep tracing-config-file
kubectl -n ipam-system get configmap ipam-tracing-config -o yaml
```

If the arg is absent, tracing is off (the apiserver runs a no-op tracer
provider and the `ipam.*` spans are zero-cost no-ops). Compose the `tracing`
component into the overlay to turn it on.

Collector endpoint (deployed default):
`opentelemetry-collector-collector.open-telemetry-system.svc.cluster.local:4317`
(OTLP gRPC → Tempo).

---

## 2. Find the trace in Grafana / Tempo

1. Open **Grafana → Explore → Tempo** data source.
2. Use the **Search** tab and filter by:
   - Service name: `apiserver` (the IPAM apiserver reports the standard
     `apiserver` service name; instance ID distinguishes pods).
   - Span name: `ipam.claim.allocate` (or `ipam.claim.release`,
     `ipam.pool.child_allocate`).
   - Tag filters (the low-cardinality identity/decision attributes below), e.g.
     `ipam.tenant.project = <projectID>`, `ipam.pool.name = <pool>`, or
     `ipam.error.reason = exhausted`.
3. To jump straight from a failing request: if the client/controller propagates
   `traceparent`, that trace is **always** sampled end-to-end (the apiserver
   sampler is parent-based), so search by the known trace ID.

> Sampling: head sampling is 5% (`samplingRatePerMillion: 50000`). Most claims
> are *not* sampled. For reproducible debugging, drive the request with an
> upstream-sampled `traceparent` so the full IPAM span tree is guaranteed to be
> recorded.

---

## 3. The span tree

A successful `IPClaim` create looks like this (DB spans are produced by
`otelpgx` and nest automatically because they run on the allocation span's
context):

```
<apiserver request span>  POST .../ipclaims
└─ ipam.claim.allocate
   ├─ ipam.tenant.resolve
   ├─ ipam.pool.authorize_cross_project        (only for cross-project claims)
   ├─ ipam.allocate.find_block
   ├─ SELECT ipam_objects … FOR UPDATE          (pool-row lock; lock wait shows here)
   ├─ SELECT ipam_cidr_allocations …            (existing allocations)
   ├─ INSERT ipam_cidr_allocations …            (reserve the block)
   ├─ UPDATE ipam_objects …                     (pool capacity)
   ├─ INSERT ipam_objects … (IPAllocation)      (+ changelog)
   ├─ INSERT ipam_objects … (IPClaim)           (+ changelog)
   └─ COMMIT
```

Release (`DELETE`) mirrors this under `ipam.claim.release` (TX1 publishes
`Releasing`, TX2 releases + deletes). Child-pool creation uses
`ipam.pool.child_allocate`, which shares the `find_block` + DB sub-tree.

The `find_block` span deliberately wraps only the *call site* of the pure
`FindFirstAvailableBlock` library; that library
(`internal/allocation/`) is a zero-dependency package and carries no
instrumentation itself.

---

## 4. What each `ipam.*` attribute means

On **`ipam.claim.allocate`** (the root domain span):

| Attribute | Meaning / how to read it |
|---|---|
| `ipam.tenant.scope` | `platform` or `project` — which scope the request ran under. |
| `ipam.tenant.project` | Project ID (empty for platform). |
| `ipam.tenant.org` | Org ID when forwarded by Milo, else empty. |
| `ipam.pool.name` | The IPPool the claim was resolved to (after poolRef/selector). |
| `ipam.claim.prefix_length` | Requested prefix length. |
| `ipam.claim.ip_family` | `IPv4` / `IPv6`. |
| `ipam.dry_run` | `true` if server dry-run (no rows persisted, allocation rolled back). |
| `ipam.error.reason` | Set on failure: `pool_not_found`, `exhausted`, `cross_project_denied`, `tx_error`. Span status is `Error`. |

On **`ipam.tenant.resolve`**:

| Attribute | Meaning |
|---|---|
| `scope` | `platform` / `project`. |
| `project` | Resolved project ID. |
| `has_parent_extras` | **Key diagnostic.** `true` when the request carried the `iam.miloapis.com/parent-*` extras. A project request showing `has_parent_extras=false` (resolving to platform scope) is the fingerprint of a lost/stripped identity — the exact signal behind the recent impersonation/scope bug. |

On **`ipam.pool.authorize_cross_project`** (cross-project claims only):

| Attribute | Meaning |
|---|---|
| `cross_project` | `true` (this span only runs for cross-project claims). |
| `decision` | `allowed` / `denied`. |
| `reason` | `allowed`, `not_shared` (pool missing or `visibility != shared`), `sar_denied` (SubjectAccessReview denied), `no_checker` (no authorizer wired — fail-closed). |

> Note: the API response intentionally collapses `not_shared` and the
> pool-doesn't-exist case to avoid leaking another project's pool labels. The
> trace keeps the precise `reason` so an operator can tell them apart.

On **`ipam.allocate.find_block`**:

| Attribute | Meaning |
|---|---|
| `strategy` | Allocation strategy from the pool spec (e.g. first-fit). |
| `existing_count` | Number of allocations already in the pool — high counts explain slower block search and growing utilization. |
| `result_cidr` | The CIDR chosen — *this is the answer to "why this block?"* |
| `exhausted` | `true` when no free block of the requested size exists (→ HTTP 507). |

On the **DB spans** (`otelpgx`): `db.statement` carries the static SQL (safe —
no user data in the statement text; bound parameters are deliberately **not**
recorded). Span duration on `SELECT … FOR UPDATE` is the pool-row lock-wait —
watch this under concurrent load against the same pool.

---

## 5. Common diagnoses

- **Claim denied, caller surprised:** open `ipam.pool.authorize_cross_project`,
  read `reason`. `sar_denied` → IAM/RBAC; `not_shared` → pool not marked
  `visibility: shared` (or doesn't exist); `no_checker` → apiserver started
  without an authorizer.
- **Claim got 507 (exhausted):** `ipam.allocate.find_block` shows
  `exhausted=true` and `existing_count`; cross-check with
  `ipam_pool_utilization_ratio`.
- **Claim landed in the "wrong" project / platform scope:**
  `ipam.tenant.resolve` `has_parent_extras=false` → identity extras were not
  forwarded; check Milo's front-gate impersonation headers.
- **Allocation slow:** look at the `SELECT … FOR UPDATE` span duration
  (lock contention) and `find_block` (large `existing_count`).

---

## 6. Overhead check (pending live cluster)

Enabling tracing adds the per-request span plus ~6 DB spans per claim at the
configured 5% sample rate. Before raising the sample rate or enabling tracing in
a throughput-sensitive environment, confirm the k6 gate still holds:

```bash
task test/load:throughput   # gate: p95 < 500ms, success rate > 0.95
```

Run this once with the `tracing` component composed into the target overlay and
compare p95 against a tracing-off baseline. **Status: not yet run against a live
cluster with a collector** — the local kind dev cluster has no collector, so
spans do not export there. Record the result here when run.
