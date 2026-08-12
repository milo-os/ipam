# IPAM API Reference (`ipam.miloapis.com/v1alpha1`)

The IPAM service exposes three resources in the `ipam.miloapis.com` API group.
Claim CREATEs are synchronous — the allocated CIDR is returned in the response
body, no polling required.

New to IPAM? Start with the [CLI guide](guides/managing-ip-address-space.md);
this page is the field-level reference.

## Resources

| Kind | Scope | Purpose |
|------|-------|---------|
| `IPPool` | Cluster | An allocatable block of address space. A *root* pool declares a CIDR; a *child* pool carves a sub-prefix from a parent pool. |
| `IPClaim` | Namespace | A request for a sub-prefix of a given size from a pool. Bound synchronously on CREATE. |
| `IPAllocation` | Namespace | System-created record of the CIDR carved out of a pool by an IPClaim. Not created directly. |

A pool and everything allocated from it share a single address family; IPv4 and
IPv6 are never mixed.

## Shared enums

```go
IPFamily        = IPv4 | IPv6
Strategy        = FirstFit | BestFit | LeastUtilized
ReclaimPolicy   = Delete | Retain
ClaimPhase      = Pending | Bound | Releasing | Error
PoolPhase       = Pending | Ready | Exhausted | Error
AllocationPhase = Pending | Ready | Exhausted | Error
```

## Allocation flow

An IPClaim CREATE runs synchronously inside one PostgreSQL transaction:

1. `SELECT ... FOR UPDATE` on the parent pool row (O(1) lock)
2. Load existing allocations for the pool
3. `FindFirstAvailableBlock` (Go) — returns HTTP 507 if the pool is full
4. `INSERT` the allocation row (and, when configured, a child IPPool in the same transaction)
5. `UPDATE` the claim with `status.allocatedCIDR` and `status.phase = Bound`
6. `INSERT` changelog rows for watchers
7. `COMMIT`

The claim's CREATE response body contains the allocated CIDR.

## IPPool

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `spec.cidr` | CIDR string | root pools | The block a root pool owns (e.g. `10.0.0.0/8`). |
| `spec.ipFamily` | IPFamily | no | Inferred from `cidr` when omitted. |
| `spec.parentPoolRef.name` | string | child pools | Parent pool to carve from (same namespace/cluster scope). |
| `spec.prefixLength` | int (0–128) | child pools | Size to carve from the parent. |
| `spec.allocation.minPrefixLength` | int | no | Smallest prefix a claim may request from this pool. |
| `spec.allocation.maxPrefixLength` | int | no | Largest prefix a claim may request from this pool. |
| `spec.allocation.strategy` | Strategy | no | How free blocks are chosen. |
| `spec.visibility` | `platform` \| `consumer` \| `shared` | no | Who may allocate from the pool. |
| `status.phase` | PoolPhase | — | Lifecycle phase. |
| `status.allocatedCIDR` | CIDR string | — | Effective CIDR (declared for root pools, carved for child pools). |
| `status.ipFamily` | IPFamily | — | Effective family. |
| `status.capacity` | `{total, allocated, available}` | — | Address counts; exact for IPv4, saturated for very large IPv6 spaces. |
| `status.largestFreePrefix` | int (0–128) | — | Prefix length of the largest free aligned block; `0` when exhausted. |
| `status.utilizationPercent` | int (0–100) | — | Allocated share, accurate for IPv4 and IPv6. |

`IPPool` is cluster-scoped (`shortName: ippool`).

## IPClaim

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `spec.ipFamily` | IPFamily | yes | Address family to allocate. |
| `spec.prefixLength` | int (0–128) | yes | Requested sub-prefix size; must be valid for the family and within the pool's min/max. |
| `spec.poolRef` | NamespacedRef | one of | Pin to a specific pool (optionally in another project via `projectRef`). |
| `spec.poolSelector` | PoolSelector | one of | Pick a pool by labels (optionally scoped by `projectRef`). |
| `spec.reclaimPolicy` | ReclaimPolicy | no | `Delete` (default) or `Retain` the allocation when the claim is deleted. |
| `spec.ownerRef` | ObjectRef | no | Opaque `{apiGroup, kind, namespace, name}` consumer reference. |
| `status.phase` | ClaimPhase | — | `Bound` after synchronous allocation. |
| `status.allocatedCIDR` | CIDR string | — | Set on `Bound`. |
| `status.boundAllocationRef.name` | string | — | The IPAllocation this claim is bound to. |

`IPClaim` is namespace-scoped (`shortName: ipclaim`). Allocation is **not**
idempotent unless the claim has a stable `metadata.name`: re-creating a claim
with the same name returns the existing allocation rather than consuming another
block.

## IPAllocation

| Field | Type | Notes |
|-------|------|-------|
| `spec.ipFamily` | IPFamily | Family of the allocated block. |
| `spec.poolRef.name` | string | Pool the block was carved from. |
| `status.phase` | AllocationPhase | Lifecycle phase. |
| `status.allocatedCIDR` | CIDR string | The allocated block. |

`IPAllocation` is namespace-scoped (`shortName: ipalloc`) and system-created; it
records the block granted to an IPClaim.

## Errors

| HTTP | Reason |
|------|--------|
| 400 | Validation error (invalid CIDR, length out of bounds) |
| 403 | RBAC denial |
| 404 | No such pool/prefix, or not visible in the caller's project |
| 409 | Conflict (name or block already exists) |
| 507 | Pool exhausted (Insufficient Storage) |

The `datumctl ipam` CLI maps these to stable exit codes — see the
[CLI guide](guides/managing-ip-address-space.md#output-formats-and-scripting).
