# Multi-Tenant Querying — Design Proposal

**Status:** Draft  
**Date:** 2026-05-08

---

## Problem

IPAM currently has no tenant boundary enforcement. Any authenticated caller can list all `IPPrefixClaim`, `ASNClaim`, and `IPAddress` resources regardless of which project created them. A claim in project A can reference a prefix pool belonging to project B. There is no scoping on LIST or GET responses.

The Milo platform injects tenant identity into every request before forwarding it to aggregated apiservers (including IPAM). IPAM does not yet read or act on that identity.

---

## How Milo Passes Tenant Identity

Milo's front-door aggregator runs HTTP filter middleware that stamps three `UserInfo.Extra` keys after authentication:

| Key | Example |
|-----|---------|
| `iam.miloapis.com/parent-api-group` | `resourcemanager.miloapis.com` |
| `iam.miloapis.com/parent-type` | `Project` |
| `iam.miloapis.com/parent-name` | `my-project-id` |

These are forwarded to IPAM as `X-Remote-Extra-*` headers via the standard Kubernetes requestheader auth mechanism. IPAM's requestheader CA enforces authenticity — the values can be trusted without additional verification callbacks.

When no project context is present (e.g., a platform-operator request at the root), the keys are absent. IPAM must handle both cases.

---

## Scope

This proposal covers **querying** (LIST, GET, WATCH) and **admission** (CREATE) for tenant-facing claim resources, plus the shared-pool model that enables cross-project claiming.

Out of scope:
- Milo IAM authorization — who can access a project is Milo's concern. IPAM enforces which project's data is returned and whether a caller can claim from a specific pool.

---

## Key Design Decisions

**Pools live in projects, not at the platform level.** Platform-managed pools and project-owned pools use the same `IPPrefix` / `ASNPool` types. The owner is encoded in the storage key prefix.

**Cross-project pool references are explicit, not inferred.** `spec.prefixRef` and `spec.prefixSelector` carry an optional `projectRef` field. When absent it defaults to the caller's own project. This eliminates lookup-order ambiguity and makes cross-project claiming auditable from the spec alone.

**Classes are project-scoped, seeded by the platform.** `IPPrefixClass` and `ASNPoolClass` live in each project's virtual cluster (key prefix `project/<id>/ipprefixclass/<name>`). The platform seeds a standard catalog of classes into every new project during provisioning. Projects see classes as their own resources and can define additional ones if the operator permits. The class lookup for shareability checks always uses the source pool's project scope.

**The allocation class declares shareability.** Classes gain a `visibility: shared` level indicating pools of that class are *eligible* for cross-project claiming. The actual per-caller grant is a Kubernetes RBAC RoleBinding for the `use` verb. This keeps policy (the class) separate from topology (which project owns the pool) and from access control (RBAC).

**Cross-project claiming is allowed when access is explicitly granted.** IPAM checks pool shareability and performs a SubjectAccessReview before allowing a cross-project allocation. No IPAM-specific permission system is required.

**Scoping is key-prefix based.** Project-owned resources use a `project/<id>/` key prefix in PostgreSQL. LIST and WATCH filter by this prefix. The existing `SELECT ... FOR UPDATE` pool lock already scopes to `pool_key`, so cross-tenant lock contention is structurally impossible.

---

## Proposed Changes

### 1. Tenant identity helper

Add `internal/tenant/tenant.go`:

```go
package tenant

import (
    "context"
    "k8s.io/apiserver/pkg/endpoints/request"
)

const (
    ExtraParentAPIGroup = "iam.miloapis.com/parent-api-group"
    ExtraParentType     = "iam.miloapis.com/parent-type"
    ExtraParentName     = "iam.miloapis.com/parent-name"
)

type Identity struct {
    APIGroup string // "resourcemanager.miloapis.com" or ""
    Kind     string // "Project", "Organization", or ""
    Name     string // project/org ID, or "" for platform requests
}

// IsPlatform returns true when there is no project context (operator request).
func (id Identity) IsPlatform() bool { return id.Name == "" }

// KeyPrefix returns the storage key prefix for this tenant's resources.
// Platform requests return "".
func (id Identity) KeyPrefix() string {
    if id.Name == "" {
        return ""
    }
    return "project/" + id.Name + "/"
}

// FromContext extracts tenant identity from the request user's Extra fields.
func FromContext(ctx context.Context) Identity {
    user, ok := request.UserFrom(ctx)
    if !ok {
        return Identity{}
    }
    extra := user.GetExtra()
    return Identity{
        APIGroup: first(extra[ExtraParentAPIGroup]),
        Kind:     first(extra[ExtraParentType]),
        Name:     first(extra[ExtraParentName]),
    }
}

func first(vals []string) string {
    if len(vals) > 0 {
        return vals[0]
    }
    return ""
}
```

Zero dependencies beyond `k8s.io/apiserver/pkg/endpoints/request`.

---

### 2. Classes are project-scoped

`IPPrefixClass` and `ASNPoolClass` are stored per-project under the same key prefix scheme as all other project resources:

```
project/<projectID>/ipprefixclass/<name>
project/<projectID>/asnpoolclass/<name>
```

**Standard class catalog.** The platform seeds a fixed set of classes into each new project during provisioning — the same classes every project gets. The provisioning step (Kyverno policy or FluxCD HelmRelease triggered on `ProjectControlPlane` creation) applies a standard `IPPrefixClass` manifest bundle into each project's context.

```yaml
# Seeded into every project at provisioning time
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPPrefixClass
metadata:
  name: consumer-private
spec:
  requiresVerification: false
  visibility: consumer
  defaultAllocation:
    strategy: BestFit
---
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPPrefixClass
metadata:
  name: consumer-shared
spec:
  requiresVerification: false
  visibility: shared
  defaultAllocation:
    strategy: BestFit
---
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPPrefixClass
metadata:
  name: consumer-public
spec:
  requiresVerification: true
  visibility: consumer
  defaultAllocation:
    strategy: BestFit
```

A project lists `ipprefixclasses` and sees exactly these — their own resources, scoped to their virtual cluster. No merge with platform-level globals needed.

**Class visibility values:**

```go
// visibility values: "consumer" | "shared"
// "consumer" = project-private; cross-project claiming not permitted.
// "shared"   = eligible for cross-project claiming, subject to SAR.
type IPPrefixClassSpec struct {
    Visibility        string
    DefaultAllocation AllocationSpec
}
```

A pool owner who does not want cross-project claiming uses `visibility: consumer`. No pool-level flag is needed — the RoleBinding is the access control gate, and without one the SAR blocks any cross-project claim regardless.

**Class lookup on shareability check.** When IPAM evaluates a cross-project claim, it looks up the class from the *pool's* project (not the caller's project). This means class definitions are authoritative for the project that owns the pool.

---

### 3. Explicit project reference on claim specs

`IPPrefixClaimSpec`, `IPAddressClaimSpec`, and `ASNClaimSpec` gain an optional `ProjectRef` on both `prefixRef` and `prefixSelector`:

```go
type PrefixRef struct {
    Name       string
    ProjectRef *LocalRef  // nil = same project as the caller
}

type PrefixSelector struct {
    MatchLabels      map[string]string
    MatchExpressions []metav1.LabelSelectorRequirement
    ProjectRef       *LocalRef  // nil = same project as the caller
}

type IPPrefixClaimSpec struct {
    IPFamily            IPFamily
    PrefixLength        int
    PrefixRef           *PrefixRef      // updated: carries optional ProjectRef
    PrefixSelector      *PrefixSelector // updated: carries optional ProjectRef
    CreateChildPrefix   bool
    ChildPrefixTemplate *IPPrefixTemplate
    ReclaimPolicy       ReclaimPolicy
    OwnerRef            *ObjectRef
}
```

When `ProjectRef` is nil, IPAM uses the caller's own project (from `UserInfo.Extra`). When `ProjectRef` is set, it explicitly names the project that owns the pool, triggering the cross-project access check.

**Example — same-project claim (default):**
```yaml
spec:
  prefixRef:
    name: my-pool        # resolved in caller's own project
  prefixLength: 24
```

**Example — cross-project claim:**
```yaml
spec:
  prefixRef:
    name: shared-infra-pool
    projectRef:
      name: infra-project   # explicit: look in project "infra-project"
  prefixLength: 24
```

**Example — cross-project label selector:**
```yaml
spec:
  prefixSelector:
    matchLabels:
      purpose: egress
    projectRef:
      name: network-project  # select from pools in "network-project"
  prefixLength: 28
```

---

### 4. Storage key prefix by tenant

All project-owned objects use a prefixed key:

```
Project class:      project/<projectID>/ipprefixclass/<name>
Project pool:       project/<projectID>/ipprefix/<name>
Project claim:      project/<projectID>/ipprefixclaim/<namespace>/<name>
```

The `pool_key` in `ipam_prefix_allocations` follows the same scheme so the allocation lock is scoped to the owning project:

```
Project pool_key:   project/<projectID>/ipprefix/<name>
```

The registry `Create` handler prepends `tenant.FromContext(ctx).KeyPrefix()` to the object key before writing to storage. The `List` handler passes the key prefix as the storage scan root. This applies uniformly to all resource types including `IPPrefixClass`.

---

### 5. Stamp `ownerRef` from tenant identity on CREATE

In every claim registry's `Create` handler:

```go
id := tenant.FromContext(ctx)
if !id.IsPlatform() {
    // Overwrite any client-supplied value — requestheader CA guarantees authenticity.
    claim.Spec.OwnerRef = &ipam.ObjectRef{
        APIGroup: id.APIGroup,
        Kind:     id.Kind,
        Name:     id.Name,
    }
}
// Platform requests may supply ownerRef explicitly (operator use case).
```

---

### 6. Cross-project pool access via SubjectAccessReview

When a claim references a pool owned by a different project (or a shared pool in another project), IPAM performs a SubjectAccessReview before allocating:

```
verb:     "use"
group:    "ipam.miloapis.com"
resource: "ipprefixes"   (or "asnpools")
name:     <pool name>
namespace: <pool owner project> (or "" for cluster-scoped)
user:     caller from UserInfo
groups:   caller's groups
extra:    caller's extra
```

If the SAR returns `allowed: false`, IPAM returns HTTP 403 Forbidden.

The pool owner grants access by creating a standard Kubernetes `ClusterRoleBinding` or `RoleBinding`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ipam-pool-consumer
rules:
- apiGroups: ["ipam.miloapis.com"]
  resources: ["ipprefixes"]
  verbs: ["use"]
  resourceNames: ["shared-consumer-pool"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: project-b-can-use-project-a-pool
subjects:
- kind: User
  name: system:serviceaccount:project-b:default
roleRef:
  kind: ClusterRole
  name: ipam-pool-consumer
  apiGroup: rbac.authorization.k8s.io
```

No IPAM-specific permission system is required. The SAR delegates to Kubernetes RBAC, which Milo's IAM layer already manages.

#### SAR implementation

```go
// internal/access/sar.go
type PoolAccessChecker interface {
    CanUsePool(ctx context.Context, userInfo user.Info, poolKey string) (bool, error)
}

type sarChecker struct {
    authz authorizer.Authorizer
}

func (c *sarChecker) CanUsePool(ctx context.Context, userInfo user.Info, poolKey string) (bool, error) {
    attrs := authorizer.AttributesRecord{
        User:            userInfo,
        Verb:            "use",
        APIGroup:        "ipam.miloapis.com",
        Resource:        resourceFromPoolKey(poolKey), // "ipprefixes" or "asnpools"
        Name:            nameFromPoolKey(poolKey),
        ResourceRequest: true,
    }
    decision, _, err := c.authz.Authorize(ctx, attrs)
    return decision == authorizer.DecisionAllow, err
}
```

The `authorizer.Authorizer` is wired in from `internal/apiserver/apiserver.go` using the existing `GenericAPIServer.Authorizer` — no new dependencies.

---

### 7. Cross-project claim flow

A caller in project B creates an `IPPrefixClaim` with an explicit `projectRef`:

```yaml
spec:
  prefixRef:
    name: shared-consumer-pool
    projectRef:
      name: project-a     # explicit: resolve pool in project-a
  prefixLength: 24
```

Registry `Create` flow for this case:

```
1. tenant.FromContext(ctx) → {Kind: "Project", Name: "project-b"}
2. Resolve prefixRef.projectRef: target key = "project/project-a/ipprefix/shared-consumer-pool"
3. Assert pool.spec.class.visibility == "shared"  → else HTTP 403
4. SAR: can caller "use" ipprefixes/shared-consumer-pool (in project-a context)? → else HTTP 403
5. Allocate from pool (SELECT ... FOR UPDATE on pool_key="project/project-a/ipprefix/shared-consumer-pool")
6. Write claim under "project/project-b/ipprefixclaim/..."
7. Write allocation row: pool_key="project/project-a/...", owner_project="project-b"
```

The allocation record links the claim's owning project to the pool's owning project, enabling correct capacity accounting on both sides.

**Same-project claim (no projectRef)** skips steps 3–5 entirely — no shareability check, no SAR. The pool key is derived from the caller's own tenant identity.

---

### 8. Schema migration

```sql
-- migrations/002_multi_tenant.sql

-- Track owning project on allocation rows for per-project capacity queries.
ALTER TABLE ipam_prefix_allocations ADD COLUMN owner_project TEXT NOT NULL DEFAULT '';
ALTER TABLE ipam_asn_allocations     ADD COLUMN owner_project TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_ipam_prefix_alloc_project ON ipam_prefix_allocations (owner_project);
CREATE INDEX idx_ipam_asn_alloc_project    ON ipam_asn_allocations (owner_project);
```

Existing rows default to `''` (platform-owned). No backfill needed for the dev system.

---

## Data Flow Summary

```
Milo front-door
  └─ injects X-Remote-Extra-* (parent-type=Project, parent-name=proj-b)
       └─ IPAM requestheader auth → UserInfo.Extra
            └─ tenant.FromContext(ctx) → {Name: "proj-b"}
                 │
                 ├─ LIST/GET/WATCH
                 │    └─ scan key prefix "project/proj-b/" only
                 │
                 └─ CREATE claim
                      ├─ stamp ownerRef from tenant identity
                      ├─ resolve pool (own project or explicit projectRef)
                      ├─ if cross-project: assert pool's class.visibility == "shared"
                      ├─ if cross-project: SubjectAccessReview (verb=use)
                      ├─ AllocatePrefix (SELECT ... FOR UPDATE on pool_key)
                      └─ write claim under "project/proj-b/ipprefixclaim/..."

Platform operator request (no Extra keys)
  └─ sees all resources, no SAR, no key-prefix filtering
```

---

## What Does Not Change

- CIDR arithmetic in `internal/allocation/` — pure math, no tenant concept.
- `SELECT ... FOR UPDATE` pool locking — structurally cross-tenant safe (scoped to `pool_key`).
- `spec.ownerRef` field — already exists, only population mechanism changes.
- `IPPrefixClass` and `ASNPoolClass` type definitions — only their storage scoping changes.

---

## Implementation Order

1. `internal/tenant/tenant.go` — standalone, unit-testable.
2. `migrations/002_multi_tenant.sql` — schema additions (safe to apply immediately).
3. Type changes + deepcopy regen:
   - `visibility: shared` on `IPPrefixClass` / `ASNPoolClass`
   - `PrefixRef.ProjectRef` and `PrefixSelector.ProjectRef` on claim specs
4. Key-prefix stamping in `internal/storage/postgres/store.go` Create/List/Watch — applies to all resource types uniformly, including `IPPrefixClass`.
5. `internal/access/sar.go` — SAR checker wired from `GenericAPIServer.Authorizer`.
6. `ownerRef` stamping + cross-project flow in claim registries.
7. Platform class provisioning — Kyverno/FluxCD manifest bundle that seeds the standard `IPPrefixClass` catalog into new projects on `ProjectControlPlane` creation.
8. e2e and load test updates — see sections below.

---

## End-to-End Test Updates

### Existing suites — no changes required

The 7 existing chainsaw suites run without project context and exercise the platform-operator path. Platform requests bypass tenant filtering and key-prefix scoping, so they continue to pass unchanged. They serve as a regression baseline confirming that the non-tenant path is unaffected.

### New suite: `test/e2e/multi-tenant/`

6 steps covering the full tenant lifecycle:

**Step 1 — Seed classes into two test projects.**
Apply the standard class catalog into `project-alpha` and `project-beta` contexts (simulating the provisioning step). Assert `LIST ipprefixclasses` from each project returns only that project's classes and not the other's.

**Step 2 — Project isolation on claims.**
Create an `IPPrefix` and `IPPrefixClaim` in `project-alpha`. `LIST ipprefixclaims` from `project-beta` returns empty — the claim is invisible across the tenant boundary.

**Step 3 — Same-project claim (no projectRef).**
`project-alpha` creates an `IPPrefixClaim` with no `projectRef`. Assert it resolves against `project-alpha`'s own pool and returns `status.allocatedCIDR` synchronously.

**Step 4 — Cross-project claim rejected without RoleBinding.**
`project-beta` creates an `IPPrefixClaim` with `prefixRef.projectRef.name: project-alpha` pointing at a `consumer-private` class pool. Expect HTTP 403 (`class visibility: consumer`).

Retry with a `consumer-shared` class pool but no RoleBinding. Expect HTTP 403 (`use` permission denied).

**Step 5 — Cross-project claim succeeds after RoleBinding.**
Create `ClusterRole` + `ClusterRoleBinding` granting `project-beta`'s caller `use` on the shared pool in `project-alpha`. Retry the claim. Assert `status.phase: Bound` and `status.allocatedCIDR` falls within `project-alpha`'s pool CIDR. Assert the allocation row records `owner_project=project-beta`.

**Step 6 — Capacity accounting is correct on both sides.**
After step 5, `GET` the pool from `project-alpha`'s context. Assert `status.capacity.allocated` reflects the cross-project claim. `DELETE` the claim from `project-beta`. Assert capacity returns to previous value.

---

## Performance Test Updates

### `test/load/lib/ipam-client.js` — add tenant header injection

Add a `withProject(projectID)` helper that injects the three `X-Remote-Extra-*` headers Milo's front-door would normally add:

```javascript
export function withProject(projectID) {
  return {
    headers: {
      'X-Remote-Extra-Iam.Miloapis.Com.Parent-Api-Group': 'resourcemanager.miloapis.com',
      'X-Remote-Extra-Iam.Miloapis.Com.Parent-Type':      'Project',
      'X-Remote-Extra-Iam.Miloapis.Com.Parent-Name':      projectID,
    },
  };
}
```

All existing scripts pass these headers when running against a multi-tenant-enabled build. Platform-path tests (no headers) remain valid for non-tenant benchmarking.

### `test/load/src/setup-pools.js` — project-scoped setup

Update the one-time setup to create classes and pools within a project context rather than as platform resources:

1. Create `N` perf test projects (e.g., `ipam-perf-0` … `ipam-perf-9`).
2. For each project, POST the standard class catalog (`consumer-private`, `consumer-shared`) using `withProject(projectID)` headers.
3. Create one large `IPPrefix` per project (`10.<n>.0.0/16`, allow /20–/28) using `withProject(projectID)` headers.
4. Create one shared pool in `ipam-perf-0` using `consumer-shared` class. Create `ClusterRoleBinding` granting all other perf projects `use` on it.
5. Create `ASNPool` per project as before.

### `test/load/src/prefix-claim-throughput.js` — tenant path

Each VU picks a random perf project and injects `withProject(projectID)` headers on every POST and DELETE. No other changes — thresholds and VU count unchanged. This measures same-project allocation throughput with tenant context overhead.

```javascript
const projectID = nsFor(Math.floor(Math.random() * NUM_PROJECTS));
const params = withProject(projectID);
const res = ipamPost(prefixClaimPath(projectID, claimName), body, params);
```

Expected: p95 stays under 500ms. Tenant key-prefix logic is O(1) string prepend; no throughput regression expected.

### `test/load/src/asn-claim-throughput.js` — tenant path

Same pattern as prefix-claim-throughput. Each VU uses `withProject` headers.

### `test/load/src/cross-project-claim-throughput.js` — new script

Measures the cost of cross-project claiming with SAR. Isolates the SAR round-trip latency from the allocation latency:

```javascript
export const options = {
  vus: __ENV.VUS || 10,
  duration: __ENV.DURATION || '2m',
  thresholds: {
    'ipam_cross_project_claim_ms{phase:success}': ['p(95)<1000', 'p(99)<3000'],
    'ipam_cross_project_success_rate':             ['rate>0.95'],
  },
};
```

Setup: all VUs claim from `ipam-perf-0`'s shared pool using their own project identity. The RoleBinding is pre-created in setup. Each VU loop: POST `IPPrefixClaim` with `prefixRef.projectRef.name: ipam-perf-0`, record latency tagged `{phase:success}` or `{phase:denied}`, DELETE on success.

The p95 threshold is deliberately wider (1000ms vs 500ms for same-project) to account for the SAR round-trip. If the SAR proves cheap (authorizer is in-process), the threshold can be tightened after the first run.

### `test/load/src/read-latency.js` — tenant-scoped reads

Update the three scenarios to inject `withProject` headers so LIST and GET operations scan the tenant key prefix rather than the full table. This validates that index performance holds when reads are naturally filtered to a project's key space.

Thresholds unchanged: prefix list p95 < 200ms, claim GET p95 < 100ms.

### `test/load/src/pool-exhaustion.js` and `pool-scale.js`

Inject `withProject` headers pointing at the setup project that owns the exhaustion/scale pool. Logic and thresholds unchanged.

### Taskfile additions

```yaml
test/load:cross-project-throughput:
  desc: Cross-project claim throughput with SAR
  cmds:
    - k6 run test/load/src/cross-project-claim-throughput.js

test/load:tenant-setup:
  desc: Create perf projects, seed classes, create per-project pools and shared pool
  cmds:
    - k6 run --vus 1 --iterations 1 test/load/src/setup-pools.js
```

`task test/load:setup` now calls `test/load:tenant-setup` as the first step before the existing pool setup.
