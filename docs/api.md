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
AddressScope    = Public | Private
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
| `spec.maxRetentionLease` | duration | no | Caps how long any allocation of this pool may stay retained, whatever its class asks for; see [Retention leases](#retention-leases). Positive, at most `8760h`. Lowering it brings forward the deadlines of allocations the pool already retains; raising or removing it moves none back. A cap also lets a claim override a Delete class that sets no lease to `Retain`. |
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
| `spec.addressScope` | AddressScope | no | Copied from the claim's class on create; if set, must agree with the class. Immutable. |
| `spec.prefixLength` | int (0–128) | yes | Requested sub-prefix size; must be valid for the family and within the pool's min/max. |
| `spec.poolRef` | NamespacedRef | one of | Pin to a specific pool (optionally in another project via `projectRef`). |
| `spec.poolSelector` | PoolSelector | one of | Pick a pool by labels (optionally scoped by `projectRef`). |
| `spec.reclaimPolicy` | ReclaimPolicy | no | `Delete` (default) or `Retain` the allocation when the claim is deleted. A claim recreated under the same name, namespace and project takes a retained address back if it resolves to the same class (defining project and name), family and address scope, the same uniqueWithin address space, a pool it would be placed in now, and the same size and address; otherwise 409 `RetainedAllocationMismatch` naming the field. Scope refs match by apiGroup/kind/name, so a network recreated under the same name reclaims. A reclaim is a create and counts against claim quota (milo-os/milo#1028). `Retain` on a claim whose class defaults to `Delete` needs a finite lease, from the class's `retentionLease` or the `maxRetentionLease` of the pool the claim lands in (for a reclaim, the pool holding the address); otherwise 422 `RetentionLeaseRequired`. A class whose own default is `Retain` needs no lease. Checked at create only; see [Retention leases](#retention-leases) for the edits that can still leave an address retained with no lease. Immutable. |
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
| `spec.claimRef.name` | string | The claim holding the allocation; unset once its claim is deleted under `Retain`. |
| `status.phase` | AllocationPhase | Lifecycle phase. |
| `status.allocatedCIDR` | CIDR string | The allocated block. |
| `status.retainedAt` | timestamp | When the claim was deleted under `Retain`. Unset while a claim holds the allocation. |
| `status.expiresAt` | timestamp | When the retention lease runs out: retention plus the effective lease, rounded up to the whole second, so it is exactly when a release would happen and never before the full lease. Unset when no lease applies, and while a claim holds the allocation. Not yet enforced; see below. |
| `status.conditions[type=Retained]` | Condition | `True` while retained, with reason `LeaseActive` (a deadline is set), `NoLease`, or `LeasePending` (the lease could not be evaluated at the delete; the sweeper evaluates it). `False`/`Reclaimed` once a recreated claim takes it back. |

`IPAllocation` is namespace-scoped (`shortName: ipalloc`) and system-created; it
records the block granted to an IPClaim. Its status is written only by the
server: `ipallocations/status` serves get and nothing else. Deleting an
IPAllocation releases its address, and it is refused with 409 while a claim is bound to the allocation. Who may release a retained one depends on its deadline:

| Retained allocation | Who can release it |
|---|---|
| Has a lease deadline (`status.expiresAt` set, running or passed) | IPAM Consumer (`ipallocations.delete`) or IPAM Admin |
| No lease deadline (`expiresAt` unset: reason `NoLease`, or `LeasePending` while unevaluated) | IPAM Admin only (`ipallocations.release-unleased`, on top of delete) |

An address held with no deadline is held on purpose, typically because whatever owned it, such as a VPC or a subnet whose class retains by default with no lease, still uses it; freeing it would let the next claim take an address in use. The server checks `release-unleased` itself, against the row it is about to delete and under the locks it deletes it under, and refuses a caller without it with 403. A server with no authorizer configured refuses every such release. The same permission is needed to take such an address back under `reclaimPolicy: Delete`: recreating the claim under its own name with a `Delete` policy, its own or its class's, would free the address when that claim is deleted. Without it the create is refused with 403 and the allocation stays retained; a reclaim under `Retain`, or of an address with a lease deadline, needs nothing more than creating the claim. A dry run is answered the same way. Its table output has an `Expires`
column: the time left on the lease, `never` with no lease, `<pending>` while
unevaluated, and `<none>` while a claim holds it.

## Retention leases

An `IPClass` may set `spec.retentionLease` and an `IPPool`
`spec.maxRetentionLease`. When a claim is deleted under `Retain`, the effective
lease is the shorter of the two, and either alone applies if the other is
unset. It is fixed at that moment and shown as `status.expiresAt` on the
IPAllocation. Changing the class later does not move a deadline already set;
lowering the pool's cap does, straight after the update and, should that
attempt fail, on the retention sweeper's next pass. With neither set there is
no deadline.

A claim may retain what its class would delete only under a lease. A claim
setting `reclaimPolicy: Retain` against a class whose own default is `Delete`
is refused with 422 `RetentionLeaseRequired` when neither the class's
`retentionLease` nor the `maxRetentionLease` of the pool the claim lands in is
set, because its address would be held with no end. The check is made under
the pool's lock, against the pool the claim is actually placed in, and a
reclaim is held to it against the pool holding the retained address; a refused
reclaim leaves the address retained, and recreating the claim without the
override takes it back. A claim under a chained class whose pool for its scope
does not exist yet is refused before anything is provisioned: provisioned
pools carry no cap, so under a chained class it is the class's
`retentionLease` that admits the override. A dry run is refused the same way.
A class whose own default is `Retain` is the operator's decision and is not
checked: its allocations may be retained without a lease.

The rule is checked when the claim is created, and only then. The lease itself
is resolved from the class and pool as they stand when the claim is deleted, so
these edits made while the claim is bound can still leave its address retained
with no lease:

- the class's `retentionLease`, or the pool's `maxRetentionLease`, is removed;
- the class is deleted, and a class that no longer exists sets no lease;
- the claim stated `Retain` under a class whose default was `Retain`, and the
  class's default is then changed to `Delete`.

**Leases are not yet enforced.** The apiserver's retention sweeper runs in
observe mode: it evaluates leases, publishes `ipam_retained_allocations`, and
counts each deadline as it passes in
`ipam_retention_releases_total{mode="observe"}`, but releases nothing. A
retained allocation past its `expiresAt` is still held until its IPAllocation
is deleted or a recreated claim reclaims it. Creating or updating an object
that sets a lease returns a warning saying so. Once enforcement is turned on
(`--retention-sweep-mode=enforce`), an allocation whose lease has run out is
released shortly after `expiresAt`: its IPAllocation is deleted and the address
returns to the pool.

## Errors

| HTTP | Reason |
|------|--------|
| 400 | Validation error (invalid CIDR, length out of bounds) |
| 403 | RBAC denial, or releasing a retained IPAllocation that has no lease deadline, or reclaiming one under `reclaimPolicy: Delete`, without `ipallocations.release-unleased` |
| 404 | No such pool/prefix, or not visible in the caller's project |
| 409 | Conflict (name or block already exists) |
| 422 | Invalid (an immutable field changed; `RetentionLeaseRequired`: a `Retain` override with no lease to bound it) |
| 507 | Pool exhausted (Insufficient Storage) |

The `datumctl ipam` CLI maps these to stable exit codes — see the
[CLI guide](guides/managing-ip-address-space.md#output-formats-and-scripting).
