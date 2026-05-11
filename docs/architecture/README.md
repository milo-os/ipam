# IPAM Architecture

A standalone, Kubernetes-native IP Address Management service implemented as
an aggregated API server backed by PostgreSQL.

## Components

```
+--------------------+      +--------------------+
|  kube-apiserver    |<---->|  IPAM apiserver    |
|  (front-proxy)     |      |  (aggregated)      |
+--------------------+      +---------+----------+
                                      |
                                      v
                                +-----+------+
                                | postgres   |
                                | (sync      |
                                |  alloc)    |
                                +-----+------+
                                      |
                                      v
                              +-------+--------+
                              | LISTEN/NOTIFY  |
                              | watchers       |
                              +----------------+
```

## Why aggregated apiserver

Compared to a CRD + controller operator:

- **Atomic allocation.** No eventual-consistency conflict window; concurrent
  claims for the same `/24` cannot both succeed.
- **Synchronous status.** The CREATE response body contains the allocated
  CIDR. Consumers don't poll for status.
- **Proven pattern.** The quota service uses the same approach; benchmarks
  show 37+ claims/s stable under `SELECT ... FOR UPDATE`.

## Allocation library (`internal/allocation/`)

Pure Go (stdlib only — `net`, `math/big`, `sort`). No Kubernetes or
PostgreSQL dependencies. The library:

- Loads parent CIDRs and existing allocations into memory
- Walks the address space according to the chosen `Strategy`
- Returns a free sub-CIDR or an error

Other allocation services (VLAN, port, etc.) can import this library
directly.

## Allocation transaction

```
BEGIN
  SELECT * FROM ipam_objects WHERE key = $poolKey FOR UPDATE  -- lock parent pool row
  SELECT allocated_cidr FROM ipam_prefix_allocations WHERE pool_key = $poolKey
  -- in-memory: FindFirstAvailableBlock(parents, existing, claimLen, strategy)
  INSERT INTO ipam_prefix_allocations (...)
  -- optional: INSERT INTO ipam_objects (kind='IPPrefix', ...) for child prefix
  UPDATE ipam_objects SET data=$claimWithStatus WHERE key=$claimKey
  INSERT INTO ipam_changelog (event_type='ADDED', ...)
  UPDATE ipam_objects SET data=$updatedPoolStatus WHERE key=$poolKey
  INSERT INTO ipam_changelog (event_type='MODIFIED', ...)
COMMIT
```

The pool-row lock is O(1) regardless of pool utilization. We do **not**
lock individual allocation rows. The GiST index on `(pool_key, allocated_cidr)`
provides a secondary safety check for overlaps but is not the primary
concurrency mechanism.

## Watch protocol

Same pattern as the quota service: `LISTEN ipam_changelog` plus
xmin-horizon polling on the `ipam_changelog` table. The cursor advances as
old transactions commit, ensuring no events are missed even under
high write concurrency.

## File layout (high level)

```
cmd/ipam/                        Binary entrypoint and serve subcommand
pkg/apis/ipam/v1alpha1/          API types (9 resources)
pkg/client/                      Generated clientset/informers/listers
internal/allocation/             Pure Go CIDR/ASN math
internal/allocator/              PostgreSQL-aware wrappers
internal/apiserver/              Aggregated apiserver wiring
internal/registry/ipam/...       Per-resource storage (AllocatingREST)
internal/storage/postgres/       PostgreSQL RESTOptionsGetter
internal/watch/                  Watch protocol via LISTEN/NOTIFY
migrations/*.sql                 PostgreSQL schema
config/                          Kustomize base + components + overlays
test/e2e/                        Chainsaw suites
test/load/                       k6 perf scripts
```
