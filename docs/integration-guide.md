# IPAM Integration Guide

This guide walks a consumer service through using the IPAM API to claim
prefixes and ASNs.

## Prerequisites

- IPAM apiserver deployed and `APIService v1alpha1.ipam.miloapis.com` Available
- A `ClusterRoleBinding` granting your service `ipam.miloapis.com` claim verbs
  (typically: `ipam-consumer` from `config/milo/rbac.yaml`)

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

## Hierarchical delegation (createChildPrefix)

When a consumer needs to delegate further (e.g. a regional block to be
sub-allocated), set `createChildPrefix: true`:

```yaml
spec:
  prefixLength: 16
  prefixRef:
    name: env-prefix
  createChildPrefix: true
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

Map HTTP status codes from the API to recoverable / non-recoverable:

- **507 Insufficient Storage**: pool exhausted. Fall back to a different pool or back off.
- **400 Bad Request**: validation error. Won't succeed on retry.
- **403 Forbidden**: RBAC. Investigate role bindings.
- **409 Conflict**: rare race; safe to retry.
