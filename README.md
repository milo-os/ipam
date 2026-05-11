# IPAM

IP Address Management for the Milo platform. Allocates IP prefixes, individual IP addresses, and AS numbers to workloads and infrastructure on demand.

## What it does

- Claim an IP prefix (e.g. a `/24` carved from a `/16` pool) — get the allocated CIDR back in the same API response, no polling.
- Claim a single IP address from a prefix.
- Claim an AS number from a pool.
- Release any of the above, returning the address space to the pool for reuse.

Prefixes can be hierarchical: a regional block can itself be sub-allocated into smaller workload prefixes in a single atomic operation.

## How it fits in Milo

The IPAM service is an API server running alongside the Milo control plane. Callers use the standard Kubernetes API (kubectl, generated clients, or raw HTTP). All allocations are synchronous — the response body contains the allocated address, so callers never need to poll for status.

Multi-tenancy is enforced at the API layer: each organization and project sees only its own address space, and cross-project sharing requires an explicit grant.

## Status

Under active development. Not yet ready for production use.
