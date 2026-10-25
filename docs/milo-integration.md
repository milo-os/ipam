# Milo integration environment

A local environment that runs the IPAM apiserver wired to a **real, in-cluster
Milo control plane**, so the full IPAM↔Milo path can be exercised end-to-end:

- **Delegated authn/authz** — IPAM resolves TokenReview / SubjectAccessReview
  against Milo's IAM (not the local kube-apiserver).
- **Quota enforcement** — Milo's `ResourceQuotaEnforcement` admission plugin runs
  inside the IPAM apiserver on `IPClaim` CREATE.
- **Entitlement → grant → claim** — a project gets an allowance, an `IPClaim`
  CREATE auto-creates a `ResourceClaim`, the claim is **granted** against an
  `AllowanceBucket`, and only then does the IPClaim bind.

This is the path the **standalone e2e cannot cover**: that suite runs IPAM with
`--enable-quota=false` and no Milo, so it tests allocation math and tenant
key-prefixing but never the quota admission plugin, the per-project control-plane
routing, or delegated authz.

## What it covers vs the standalone e2e

| Capability | Standalone e2e (`test/e2e`) | Milo integration env |
|---|---|---|
| CIDR/ASN allocation math | ✅ | ✅ |
| Tenant key-prefix isolation | ✅ (impersonation extras) | ✅ |
| `--enable-quota` admission plugin | ❌ (off) | ✅ (on) |
| Delegated authn/authz to Milo IAM | ❌ (local kube fallback) | ✅ |
| `readyz` with quota + APF informers syncing from Milo | ❌ | ✅ |
| ResourceClaim auto-creation + grant | ❌ | ✅ |
| AllowanceBucket decrement on claim | ❌ | ✅ |

## Prerequisites

- A running test-infra kind cluster (`task test-infra:cluster-up`).
- A local Milo checkout. Default path `../../datum-cloud/milo`; override with
  `MILO_REPO=/path/to/milo`.
- `docker`, `kind`, `kubectl`, `kustomize`, `task` (with `TASK_X_REMOTE_TASKFILES=1`).

> **Resource note.** Milo (etcd + apiserver + controller-manager + argo-events)
> needs ~2 CPU on top of IPAM. On a 4-CPU kind node you may need to scale down
> the optional `telemetry-system` observability stack first:
> `kubectl scale --replicas=0 -n telemetry-system deploy,statefulset --all`.

## Run it

```bash
# One shot: cluster + Milo + IPAM (quota ON) + quota provisioning.
MILO_REPO=../../datum-cloud/milo task milo-integration:up
```

Or step by step:

```bash
task milo-integration:deploy-milo      # build/load/deploy Milo into test-infra
task dev:build dev:load                # build/load the IPAM image
task milo-integration:deploy-ipam      # deploy IPAM via the milo-integration overlay
task milo-integration:provision-quota  # register the IPAM quotable type + policies in Milo
```

`milo-integration:deploy-ipam` deletes and recreates the IPAM Deployment because
switching from another overlay's volume shape (e.g. test-infra's tracing volume)
cannot be strategic-merged in place.

### Verify IPAM is ready with quota ON

```bash
kubectl get pods -n ipam-system
kubectl get apiservice v1alpha1.ipam.miloapis.com \
  -o jsonpath='{.status.conditions[?(@.type=="Available")].status}'   # True
```

The IPAM apiserver log should show the quota loopback config injected, the
ClaimCreationPolicies synced from Milo, and **FlowSchema + PriorityLevelConfiguration
caches populated** — the APF readyz dependency that historically blocked staging.

## Drive a full claim (end-to-end proof)

Project scope reaches IPAM via the `iam.miloapis.com/parent-*` impersonation
userextras (the same mechanism Milo's front gate uses). Create an Organization
and a Project in Milo, then create a project-scoped IPPool + IPClaim through the
local kube-apiserver (where the IPAM APIService is aggregated).

```bash
MILO_KUBECONFIG=../../datum-cloud/milo/.milo/kubeconfig   # admin / test-admin-token

# 1. Organization (core control-plane)
KUBECONFIG=$MILO_KUBECONFIG kubectl apply -f - <<'EOF'
apiVersion: resourcemanager.miloapis.com/v1alpha1
kind: Organization
metadata: { name: ipam-int-org }
spec: { type: Standard }
EOF

# 2. Project (org control-plane sub-path)
cat > /tmp/org.kubeconfig <<EOF
apiVersion: v1
kind: Config
clusters: [{name: org, cluster: {insecure-skip-tls-verify: true, server: "https://localhost:30443/apis/resourcemanager.miloapis.com/v1alpha1/organizations/ipam-int-org/control-plane"}}]
contexts: [{name: org, context: {cluster: org, user: admin}}]
current-context: org
users: [{name: admin, user: {token: test-admin-token}}]
EOF
KUBECONFIG=/tmp/org.kubeconfig kubectl apply -f - <<'EOF'
apiVersion: resourcemanager.miloapis.com/v1alpha1
kind: Project
metadata: { name: ipam-int-project }
spec: { ownerRef: { kind: Organization, name: ipam-int-org } }
EOF
KUBECONFIG=/tmp/org.kubeconfig kubectl wait --for=condition=Ready project/ipam-int-project --timeout=120s

# 3. Project-scoped IPPool + IPClaim via impersonation (against the test-infra cluster).
IMP="--as=e2e-tenant-tester \
  --as-user-extra=iam.miloapis.com/parent-api-group=resourcemanager.miloapis.com \
  --as-user-extra=iam.miloapis.com/parent-type=Project \
  --as-user-extra=iam.miloapis.com/parent-name=ipam-int-project"

kubectl $IMP create -f - <<'EOF'
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPPool
metadata: { name: int-parent }
spec:
  cidr: 10.200.0.0/20
  ipFamily: IPv4
  visibility: consumer
  allocation: { minPrefixLength: 24, maxPrefixLength: 28, strategy: FirstFit }
EOF

kubectl $IMP -n default create -f - <<'EOF'
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPClaim
metadata: { name: int-claim-1, namespace: default }
spec:
  ipFamily: IPv4
  prefixLength: 24
  poolRef: { name: int-parent }
  reclaimPolicy: Delete
EOF
```

### Confirm quota was actually enforced

```bash
# IPClaim bound synchronously with an allocated CIDR
kubectl $IMP -n default get ipclaim int-claim-1 -o jsonpath='{.status}'
# -> {"allocatedCIDR":"10.200.0.0/24","boundAllocationRef":{...},"phase":"Bound"}

# A ResourceClaim was GRANTED in the project control-plane (not bypassed)
cat > /tmp/proj.kubeconfig <<EOF
apiVersion: v1
kind: Config
clusters: [{name: proj, cluster: {insecure-skip-tls-verify: true, server: "https://localhost:30443/apis/resourcemanager.miloapis.com/v1alpha1/projects/ipam-int-project/control-plane"}}]
contexts: [{name: proj, context: {cluster: proj, user: admin}}]
current-context: proj
users: [{name: admin, user: {token: test-admin-token}}]
EOF
KUBECONFIG=/tmp/proj.kubeconfig kubectl get resourceclaim -A      # GRANTED True
KUBECONFIG=/tmp/proj.kubeconfig kubectl get allowancebucket -A \
  -o jsonpath='{range .items[*]}limit={.status.limit} alloc={.status.allocated} avail={.status.available}{"\n"}{end}'
# -> limit=100 alloc=1 avail=99
```

## How it's wired

- **`config/overlays/milo-integration/`** — IPAM overlay. Flips `--enable-quota=true`,
  points all three delegation kubeconfig flags at the in-cluster `milo-apiserver`
  Service via the `milo-kubeconfig` Secret, and opens NetworkPolicy egress to
  `milo-system:6443`. No tracing component (keeps the footprint minimal).
- **`config/overlays/milo-integration/quota/`** — applied to **Milo**, not the IPAM
  cluster. Registers the quotable type and the auto-claim / auto-grant policies:
  - `ResourceRegistration` `ipclaims-per-project` (`resourceType: ipam.miloapis.com/ipclaims`)
  - `ClaimCreationPolicy` triggered by `IPClaim` → creates a `ResourceClaim`
  - `GrantCreationPolicy` triggered by `Project` → creates a `ResourceGrant` **in
    the project control-plane** (via `target.parentContext`), pre-creating an
    `AllowanceBucket`.
- **`config/overlays/milo-integration/rbac-tenant-user.yaml`** — applied to **Milo**.
  Grants the impersonated `e2e-tenant-tester` IPAM verbs (IPAM delegates authz to
  Milo, so the grant must live in Milo).

## Why this Milo build differs from staging

In this Milo, the quota grant controllers (`resource-registration`,
`resource-grant`, `resource-claim`, `allowance-bucket`, `claim-creation-policy`,
`grant-creation-policy`, `grant-creation-controller`, …) run **inside the single
`milo-controller-manager`** via its multicluster manager — there is **no separate
`services-controller-manager`** as in staging. `task dev:deploy` therefore brings
up the entire grant pipeline on its own.

Note also: this Milo build ships only the raw `quota.miloapis.com` primitives,
**not** the higher-level `services.miloapis.com` catalog API. The pre-existing
`config/components/service-catalog/` component (Service / ServiceConfiguration)
cannot be applied here — its function is reproduced directly with the
`quota.miloapis.com` ResourceRegistration + policies in
`config/overlays/milo-integration/quota/`.

## Known issues observed in this env

- **`ClaimCreationPolicy` name template must use `requestInfo.name`, not
  `trigger.metadata.name`.** The IPClaim object the IPAM aggregated apiserver
  hands Milo's quota plugin is converted from an internal Go type and does not
  expose a usable `metadata` key to the CEL template engine, so
  `trigger.metadata.name` fails to render. `requestInfo.name` is the same value
  and always set. (Related: IPAM logs `[SHOULD NOT HAPPEN] failed to update
  managedFields ... no type found matching IPClaimSpec`, a structured-merge-diff
  schema-registration gap on the IPAM side.)
- **Intermittent `fatal error: concurrent map writes` on the claim path.** The
  IPAM apiserver can crash once under the quota admission + watch-manager path —
  the same unsynchronised-map / heap-instability failure mode documented around
  the `MaxConns=10` cap in `cmd/ipam/serve.go`. The pod restarts and the retried
  claim succeeds. This is an IPAM runtime bug, not a config issue.
