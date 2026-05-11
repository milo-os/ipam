# IPAM API Reference (`ipam.miloapis.com/v1alpha1`)

The IPAM service exposes eight resources in the `ipam.miloapis.com` API group.
All synchronous-allocation calls (claims) return the allocated identifier in
the response body — no polling required.

## Resource scope

| Kind                    | Scope     | Purpose                                    |
|-------------------------|-----------|--------------------------------------------|
| `IPPrefixClass`         | Cluster   | Class-of-service for IP prefixes           |
| `IPPrefix`              | Cluster or namespace | A CIDR pool, leaf or hierarchical |
| `IPPrefixClaim`         | Namespace | Claim a sub-prefix from a parent           |
| `IPAddress`             | Namespace | A single allocated IP                      |
| `IPAddressClaim`        | Namespace | Claim a single IP from a prefix            |
| `ASNPoolClass`          | Cluster   | Class-of-service for ASN pools             |
| `ASNPool`               | Cluster   | A range of ASNs                            |
| `ASNClaim`              | Namespace | Claim an ASN from a pool                   |
## Shared enums

```go
IPFamily      = IPv4 | IPv6
Strategy      = FirstFit | BestFit | LeastUtilized
ReclaimPolicy = Delete | Retain
ClaimPhase    = Pending | Bound | Releasing | Error
PrefixPhase   = Pending | Ready | Exhausted | Error
```

## Allocation flow

A claim CREATE runs synchronously inside one PostgreSQL transaction:

1. `SELECT ... FOR UPDATE` on the parent pool row (O(1) lock)
2. Load existing allocations from `ipam_prefix_allocations`
3. `FindFirstAvailableBlock` (Go) — returns 507 if pool is full
4. `INSERT` allocation row + (optional) child IPPrefix
5. `UPDATE` claim with `status.allocatedCIDR`, `status.phase = Bound`
6. `INSERT` changelog rows for watchers
7. `COMMIT`

The claim's CREATE response body contains the allocated CIDR or ASN.

## IPPrefixClaim

| Field                        | Type            | Required | Notes                              |
|------------------------------|-----------------|----------|------------------------------------|
| `spec.ipFamily`              | IPFamily        | yes      | Must match `prefixRef`             |
| `spec.prefixLength`          | int             | yes      | Within parent's min/max            |
| `spec.prefixRef.name`        | string          | one of   | Pin to a specific parent           |
| `spec.prefixSelector`        | LabelSelector   | one of   | Pick a parent by labels            |
| `spec.createChildPrefix`     | bool            | no       | Atomically create a child IPPrefix |
| `spec.childPrefixTemplate`   | object          | iff above| metadata + spec for the child      |
| `spec.reclaimPolicy`         | ReclaimPolicy   | no       | `Delete` (default) or `Retain`     |
| `spec.ownerRef`              | ObjectRef       | no       | Opaque consumer reference          |
| `status.phase`               | ClaimPhase      | -        | `Bound` after sync allocation      |
| `status.allocatedCIDR`       | CIDR string     | -        | Set on Bound                       |
| `status.boundPrefixRef.name` | string          | -        | The chosen parent                  |

## ASNClaim

| Field                | Type     | Required | Notes                                     |
|----------------------|----------|----------|-------------------------------------------|
| `spec.poolRef.name`  | string   | one of   | Pin to a pool                             |
| `spec.classRef.name` | string   | one of   | Any pool of the given class               |
| `spec.ownerRef`      | ObjectRef| no       | Opaque consumer reference                 |
| `status.phase`       | ClaimPhase | -      | `Bound` after sync allocation             |
| `status.asn`         | int64    | -        | Set on Bound                              |
| `status.boundPoolRef.name` | string | -      | The chosen pool                           |

## Errors

| HTTP | Reason                              |
|------|-------------------------------------|
| 400  | Validation error (invalid CIDR, length out of bounds)                   |
| 403  | RBAC denial                         |
| 409  | Conflict (e.g., `claim_key` clash)  |
| 507  | Pool exhausted (Insufficient Storage) |
