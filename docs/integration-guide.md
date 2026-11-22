# IPAM Integration Guide

This guide walks a consumer service through using the IPAM API to claim
prefixes and ASNs.

## Prerequisites

- IPAM apiserver deployed and `APIService v1alpha1.ipam.miloapis.com` Available
- A `ClusterRoleBinding` granting your service `ipam.miloapis.com` claim verbs
  (typically: `ipam-consumer` from `config/milo/rbac.yaml`). The consumer role
  also grants deleting an IPAllocation, which is how your service releases an
  address its claim retained under a lease; a bound allocation is refused with
  409, and one retained with no lease deadline with 403 (only IPAM Admin can
  release that; see [Retention leases](#retention-leases)).

## Claiming a prefix

A consumer creates an `IPPrefixClaim` referencing a parent `IPPrefix`. The
CREATE response contains the allocated CIDR — no polling required.

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPPrefixClaim
metadata:
  name: my-service-prefix
  namespace: my-tenant
spec:
  ipFamily: IPv4
  prefixLength: 24
  prefixRef:
    name: consumer-shared
  reclaimPolicy: Delete
  ownerRef:
    apiGroup: workload.miloapis.com
    kind: Workload
    namespace: my-tenant
    name: my-service
```

Apply it and the response will be:

```yaml
status:
  phase: Bound
  allocatedCIDR: 10.128.4.0/24
  boundPrefixRef:
    name: consumer-shared
```

Releasing is just `kubectl delete ipprefixclaim my-service-prefix`. With
`reclaimPolicy: Delete`, the underlying allocation row is removed and the
CIDR returns to the parent's pool.

### Reclaiming a retained address

With `reclaimPolicy: Retain` the address stays held after the claim is deleted.
Creating a claim of the same name in the same namespace and project takes it
back, synchronously, even from a pool with no free block left. It is handed
back only on the terms it was held under:

- the same class: the same defining project and name, so a local class
  replaced by a reference to another project's class of that name is another
  class;
- the same address family and the address scope the class had when the address
  was handed out;
- the same address space under the class's `uniqueWithin`;
- a pool the claim would be placed in now: one that still offers the class and
  serves the claim's scope, or for a chained class the pool its chain resolves
  to for that scope (another `location`, for a class with `poolPer:
  [location]`, is another pool);
- the same prefix length, or none stated, and the same address if one is named.

Anything else is a 409 with reason `RetainedAllocationMismatch`, naming the
IPAllocation and the field. Recreate on the retained terms, or delete the
IPAllocation to release the address and claim afresh. Deleting it needs only
the consumer role if the retained address has a lease deadline, and is refused
with 409 while a claim is bound to it. One with no deadline needs IPAM Admin.

Three consequences to plan around:

- Scope references match by `apiGroup`, `kind` and `name`. A network deleted and
  recreated under the same name is the same scope, and a claim naming it
  reclaims the address the old network's claim retained.
- Within a namespace, a claim's name is its only identity. Anyone who can
  create claims in that namespace can take a retained address back by creating
  a claim of that name, and the new claim's owner reference replaces the old
  one. A claim in another namespace or project never can.
- Until milo-os/milo#1028 lands, quota is charged on claims and not on
  retained addresses, so a reclaim is charged as a new claim: a project at its
  claim quota cannot reclaim until it deletes another claim.

### Retention leases

A retained address need not be held forever. The class's `retentionLease`, or
the pool's `maxRetentionLease`, or the shorter of the two when both are set,
bounds how long it stays retained. The deadline is fixed when the claim is
deleted and is on the IPAllocation the moment the delete returns:

```yaml
status:
  retainedAt: "2026-11-21T12:00:00Z"
  expiresAt: "2026-11-21T13:00:00Z"
  conditions:
    - type: Retained
      status: "True"
      reason: LeaseActive
```

With no lease on either, `expiresAt` is absent and the reason is `NoLease`.
Recreating the claim before the deadline reclaims the address and clears both
timestamps.

The deadline also decides who can release the address early. With an
`expiresAt`, the consumer role can delete the IPAllocation: the release only
brings the deadline forward. With none (`NoLease`, or `LeasePending` until the
sweeper evaluates it), the address is held until someone releases it, usually
because what owned it, such as a VPC block under a class that retains by
default, still uses it. Deleting that IPAllocation is refused with 403 unless
the caller has `ipam.miloapis.com/ipallocations.release-unleased`, which only
the IPAM Admin role grants. The same goes for taking it back under
`reclaimPolicy: Delete`, which would free it when that claim is deleted: the
create is refused with 403 and the address stays retained. Recreate the claim
with `reclaimPolicy: Retain`, or under a class whose default is `Retain`, to
take the address back; if your service needs to give it up, ask an admin to
release it. Deleting the namespace still frees it: the namespace controller's
`deletecollection` on IPAllocations admits the release, and no IAM role grants
that verb to a consumer.

A claim cannot retain indefinitely on its own say-so. Setting
`reclaimPolicy: Retain` on a claim whose class defaults to `Delete` is refused
with 422 and reason `RetentionLeaseRequired` unless the class sets
`retentionLease` or the pool the claim lands in sets `maxRetentionLease`. The
message names both. Ask the class or pool owner for a lease, or leave
`reclaimPolicy` to the class. A recreate that would reclaim a retained address
is held to the same rule against the pool holding it. Under a chained class,
whose pools are provisioned without a cap, only the class's `retentionLease`
admits the override. A class whose own default is `Retain` is not affected.

The rule is checked when the claim is created. The lease is read again when the
claim is deleted, so removing the class's lease or the pool's cap, deleting the
class, or changing a `Retain` class the claim agreed with to `Delete` while the
claim is bound can still leave its address retained with no lease. Do not read
an accepted `Retain` as a promise that a deadline will be set.

An expired retained address is released. Shortly after `expiresAt`, on the
retention sweeper's next pass, the IPAllocation is deleted and the address goes
back to the pool, where another claim may be given it. To keep the address,
recreate the claim before `expiresAt`. Do not rely on it being held past that
date.

## Hierarchical delegation

When a consumer needs to delegate further (e.g. a regional block to be
sub-allocated), set `childPrefixTemplate`. Its presence is the signal — no
separate boolean needed:

```yaml
spec:
  prefixLength: 16
  prefixRef:
    name: env-prefix
  childPrefixTemplate:
    metadata:
      name: us-west-region
    spec:
      classRef:
        name: consumer-shared
      allocation:
        minPrefixLength: 24
        maxPrefixLength: 28
        strategy: FirstFit
```

In a single transaction, the IPPrefixClaim is bound, an `IPPrefix`
`us-west-region` is created with `spec.parentRef` pointing back to the parent,
and the new IPPrefix can immediately be referenced by leaf claims.

## Claiming a single IP

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPAddressClaim
metadata:
  name: gateway-ip
  namespace: my-tenant
spec:
  ipFamily: IPv4
  prefixRef:
    name: my-service-prefix
  reclaimPolicy: Delete
```

Response sets `status.allocatedIP` to a single IPv4 address from the
referenced prefix.

## Claiming an ASN

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: ASNClaim
metadata:
  name: my-tenant-asn
  namespace: my-tenant
spec:
  poolRef:
    name: private-asn-pool
```

Response sets `status.asn` to a single int64 ASN from the pool.

## Watching events

The IPAM apiserver implements the standard Kubernetes watch protocol. Use
the generated client (`pkg/client/clientset/`) or `kubectl get -w`:

```sh
kubectl get ipprefixclaims -n my-tenant -w
```

## Error handling

Classify refusals with `go.miloapis.com/ipam/pkg/ipamerrors` rather than by
status code. The reason travels in the status, so a Go client asks IPAM why it
was refused instead of re-deriving it from the number:

```go
switch ipamerrors.ReasonFor(err) {
case ipamerrors.ReasonExhausted:
    // No space. Back off and retry; ExhaustedPool names the pool to widen
    // when one pool accounts for it.
    pool, named := ipamerrors.ExhaustedPool(err)
    ...
case ipamerrors.ReasonClassNotFound, ipamerrors.ReasonNoDefaultClass, ipamerrors.ReasonNoOfferingPool:
    // Nothing to allocate from until an operator configures it. Retrying
    // unchanged will not help.
case ipamerrors.ReasonScopeRolesMissing:
    // The claim is short the scope roles its class requires; the caller must
    // fill them in. MissingScopeRoles names them.
case ipamerrors.ReasonRetainedAllocationMismatch:
    // An earlier claim of this name retained its address, and this claim asks
    // for it on other terms. RetainedAllocationMismatch names the IPAllocation
    // and the field that differs: recreate on the retained terms to take the
    // address back, or delete the IPAllocation to claim afresh.
    allocation, field, _ := ipamerrors.RetainedAllocationMismatch(err)
    ...
case ipamerrors.ReasonRetentionLeaseRequired:
    // The claim set reclaimPolicy Retain against a class that defaults to
    // Delete, and no lease would bound the retention. Leave reclaimPolicy to
    // the class, or have the class or pool owner set a lease.
case ipamerrors.ReasonAllocationRetained:
    // Returned by IPAM releases that refused a recreate over a retained
    // allocation; current releases reclaim it instead. RetainedAllocation
    // names the IPAllocation that has to go for the name to be reusable.
}
```

Reasons are added over time, so treat an unrecognised one the same as
`ReasonUnknown` and fall back to the status code:

- **507 Insufficient Storage**: no space. Fall back to a different pool or back off.
- **400 Bad Request**: the request will not succeed unchanged on retry.
- **403 Forbidden**: RBAC. Investigate role bindings. Deleting a retained IPAllocation with no lease deadline, or reclaiming one under `reclaimPolicy: Delete`, is also 403 for anyone but an IPAM Admin.
- **409 Conflict**: a name is taken.
- **422 Unprocessable Entity**: a field is refused for its value; change it before retrying.
