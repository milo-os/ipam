# IPAM Service

A standalone, Kubernetes-native IP Address Management service implemented as an aggregated API server backed by PostgreSQL. Manages IP prefixes, individual IP addresses, and AS numbers across the platform — from infrastructure backbone to consumer workloads.

## Reference Repositories

- `/Users/scotwells/repos/datum-cloud/quota` — **Primary structural template.** Follow its aggregated apiserver wiring, PostgreSQL storage pattern, and AllocatingREST wrapper exactly.
- `/Users/scotwells/repos/datum-cloud/activity` — Secondary layout reference for config, Dockerfile, Taskfile, and deployment patterns.

**Requirements doc:** `/Users/scotwells/repos/datum-cloud/infra/docs/enhancements/ipam/README.md` — read before implementing anything.

## Why Aggregated Apiserver (not CRD operator)

- **Atomic allocation** — no eventual-consistency conflict window under concurrent claims
- **Synchronous status** — caller gets the allocated CIDR/ASN in the CREATE response body, no polling
- **Proven pattern** — quota service benchmarks show 37+ claims/s stable under `SELECT ... FOR UPDATE`

## Hard Constraints

1. **API group:** `ipam.miloapis.com/v1alpha1`
2. **Module path:** `go.miloapis.com/ipam`
3. **Zero Milo/Quota imports.** No dependencies on `datum-cloud/milo` or `datum-cloud/quota`.
4. **Consumer refs are opaque:** `{apiGroup, kind, name}` strings, not Go type imports.
5. **`internal/allocation/` has zero non-stdlib imports.** Must compile with only the Go standard library (`net`, `math/big`, `sort`).

## Repo Layout (key paths)

```
cmd/ipam/           # main.go + serve.go (subcommand pattern)
pkg/apis/ipam/      # internal types + v1alpha1/ versioned types + generated clients
internal/
  allocation/       # Pure Go CIDR/ASN math library — ZERO non-stdlib imports
  allocator/        # Kubernetes-aware wrappers: PostgresPrefixAllocator, PostgresASNAllocator
  apiserver/        # Aggregated apiserver setup (follow quota pattern)
  registry/ipam/    # Per-resource storage: ipprefix/, ipprefixclaim/, ipaddress/, ipaddressclaim/, asnpool/, asnclaim/
  storage/          # PostgreSQL RESTOptionsGetter implementation
  watch/            # LISTEN/NOTIFY changelog-based Watch
  metrics/          # Prometheus metrics
migrations/         # Numbered SQL files + migrate.sh
config/
  base/             # Deployment, Service, SA, RBAC
  components/       # kind: Component kustomizations (api-registration, postgres, observability, k6-performance-tests, …)
  dependencies/     # External Helm charts via FluxCD
  overlays/dev/     # Composes base + components for local kind cluster
test/e2e/           # Chainsaw test suites (see e2e-testing agent)
test/load/          # k6 performance scripts (see performance-testing agent)
```

## Allocation Transaction Sequence (CIDR)

The registry's Create method for IPPrefixClaim executes atomically:

```
BEGIN
  SELECT * FROM ipam_objects WHERE key = $poolKey FOR UPDATE   -- lock parent pool row
  SELECT allocated_cidr FROM ipam_prefix_allocations WHERE pool_key = $poolKey
  -- FindFirstAvailableBlock(parents, existing, claimPrefixLen, strategy) in Go
  -- Returns error → HTTP 507 Insufficient Storage if pool is full
  INSERT INTO ipam_prefix_allocations (pool_key, allocated_cidr, claim_key, ...)
  IF createChildPrefix:
    INSERT INTO ipam_objects (key, kind='IPPrefix', data=childPrefixJSON, ...)
    INSERT INTO ipam_changelog (key, event_type='ADDED', ...)
  UPDATE ipam_objects SET data=$claimWithStatus WHERE key=$claimKey
  INSERT INTO ipam_changelog (key, event_type='ADDED', ...)
  UPDATE ipam_objects SET data=$updatedPoolStatus WHERE key=$poolKey
  INSERT INTO ipam_changelog (key, event_type='MODIFIED', ...)
COMMIT
```

**Why `SELECT ... FOR UPDATE` on the pool row:** locks one row regardless of pool size (O(1)), eliminates phantom read risk. CAS regressed to 13.85/s in quota benchmarks; FOR UPDATE held at 37.6/s. See quota ADR at `/Users/scotwells/repos/datum-cloud/quota/docs/adr/0001-postgres-first-architecture.md`.

## AllocatingREST Pattern

`internal/registry/ipam/ipprefixclaim/storage.go` — replicate for `ipaddressclaim` and `asnclaim`:

```go
type REST struct {
    store     *genericregistry.Store
    allocator allocator.PrefixAllocator
    db        *pgxpool.Pool
}

func (r *REST) Create(...) (runtime.Object, error) {
    tx, _ := r.db.Begin(ctx)
    cidr, err := r.allocator.AllocatePrefix(ctx, tx, poolKey, claim.Spec.PrefixLength, ...)
    if err != nil {
        tx.Rollback(ctx)
        return nil, errors.NewInsufficientStorage(...)
    }
    claim.Status.AllocatedCIDR = cidr
    claim.Status.Phase = ipam.ClaimBound
    tx.Commit(ctx)
    return claim, nil
}
```

## Apiserver Wiring

Follow `internal/apiserver/apiserver.go` from quota service exactly. Key addition:

```go
type ExtraConfig struct {
    PrefixAllocator allocator.PrefixAllocator  // required
    ASNAllocator    allocator.ASNAllocator     // required
    AllocatorPool   *pgxpool.Pool              // required
}
```

All claim REST constructors receive the allocator and the pool; both are
required (postgres is the only backend).

## Storage Backend (`cmd/ipam/serve.go`)

PostgreSQL is the only supported storage backend. There is no etcd or
dual-write mode.

```
--postgres-dsn   PostgreSQL connection string (required)
```

## Watch (`internal/watch/postgres.go`)

`LISTEN ipam_changelog` + polling with an xmin-horizon cursor. Implements `watch.Interface` (`ResultChan()`, `Stop()`). Same pattern as quota service.

## Key Design Decisions

1. **Aggregated apiserver over CRD.** Synchronous allocation, no conflict window, no polling.
2. **`internal/allocation/` zero non-stdlib imports.** Other services (VLAN, port) can import it without pulling in Kubernetes or PostgreSQL.
3. **Pool-level `SELECT ... FOR UPDATE`.** O(1) lock regardless of pool utilization.
4. **CIDR arithmetic in Go, not SQL.** GiST index on `(pool_key, allocated_cidr)` is a secondary overlap check, not the primary mechanism.
5. **PostgreSQL is the only backend.** Synchronous allocation in the request path is the whole point of the service; no etcd or dual-write mode.
6. **Atomic child prefix creation.** `createChildPrefix=true` inserts the child IPPrefix in the same transaction as the claim.
7. **Single address family per resource.** IPv4 and IPv6 are never mixed; dual-stack = two resources.

## Multi-Agent Teams

When the user asks to "spin up a team", "use a team", or have agents "work together / coordinate / collaborate", always use the `TeamCreate` tool — not the `Agent` tool with `run_in_background`. Background agents are isolated; teams share a task list and can message each other.

**Correct workflow:**

1. `TeamCreate` — creates the team and a shared task list
2. `TaskCreate` (once per work item) — populates the shared task list
3. `Agent` with `team_name` and `name` — spawns each teammate into the team
4. `TaskUpdate` with `owner` — assigns tasks to teammates by name
5. Teammates message back when done; assign follow-on work or shut down with `SendMessage {type: "shutdown_request"}`

**Specialist subagent types for this project:**

| Role | `subagent_type` |
|---|---|
| Grafana dashboards, alerts, runbooks | `observability` |
| k6 load tests, thresholds | `performance-testing` |
| Chainsaw e2e test suites | `e2e-testing` |
| Read-only code search | `Explore` |
| Architecture / planning | `Plan` |
| General implementation | `general-purpose` |

Never spawn background `Agent` calls as a substitute for a team when the user asks for coordinated multi-agent work.

## Conventions

- **Error types:** `errors.NewConflict`, `errors.NewForbidden`, `errors.NewInsufficientStorage` (507), `errors.NewBadRequest`
- **Logging:** `klog.V(2).InfoS(...)` for operational, `klog.ErrorS(...)` for errors
- **Metrics:** `k8s.io/component-base/metrics`. See `.claude/agents/observability.md` for the full spec.
- **Tests:** table-driven unit tests for `internal/allocation/`; Chainsaw e2e in `test/e2e/`. See `.claude/agents/e2e-testing.md`.
- **Performance tests:** k6 scripts in `test/load/`. See `.claude/agents/performance-testing.md`.
- **Dependencies:** match quota service's `k8s.io/apiserver` and `k8s.io/apimachinery` versions.
- **Deployment:** env vars for all config, cert-manager CSI for TLS, security context: nonroot, readonly rootfs, drop-all-caps, seccomp RuntimeDefault.
- **Kustomize patterns:** `config/base/` uses `images:` transformer + `replacements:` for image tag propagation; `config/components/` are `kind: Component`; overlays compose base + components.

## Verification Commands

```bash
go build ./cmd/ipam/...
go test ./pkg/... ./internal/... -count=1
go vet ./...
golangci-lint run ./...
./hack/verify-codegen.sh
grep -r "datum-cloud/milo\|datum-cloud/quota" . && echo "FAIL: unwanted imports" || echo "OK"
kustomize build config/overlays/dev/
```

## Dev Setup (kind cluster)

```bash
task test-infra:cluster-up
task install-observability       # optional
task dev:build && task dev:load
task dev:install-dependencies
task dev:deploy
```

Key Taskfile targets: `dev:setup`, `dev:build`, `dev:load`, `dev:deploy`, `e2e`, `e2e:suite SUITE=<name>`, `test/load:setup`, `test/load:throughput`, `test/load:exhaustion`, `test/load:reads`, `test/load:scale`, `test/load:cleanup`.

The Taskfile includes the test-infra remote Taskfile (`datum-cloud/test-infra v0.6.0`) for `cluster-up`/`cluster-down`/`kubectl`.

## Acceptance Criteria

- `go build ./cmd/ipam/` succeeds
- `go test ./internal/allocation/...` passes (pure Go, no external deps)
- `go vet ./...` clean; zero `datum-cloud/milo` or `datum-cloud/quota` imports
- Binary starts with `--postgres-dsn=...` and serves discovery for `ipam.miloapis.com/v1alpha1`
- IPPrefixClaim CREATE returns allocated CIDR in status synchronously
- Concurrent IPPrefixClaim CREATEs produce non-overlapping CIDRs under load
- IPPrefixClaim against exhausted pool returns HTTP 507
- `createChildPrefix=true` creates the child IPPrefix atomically in the same transaction
- `kustomize build config/overlays/dev/` renders valid manifests
- `chainsaw test test/e2e/` passes all suites
- k6 `prefix-claim-throughput.js`: p95 < 500ms, success rate > 0.95
- k6 `pool-exhaustion.js` deny path: p95 < 200ms
- k6 `read-latency.js` prefix list: p95 < 200ms
- Deployment: nonroot, readonly rootfs, cert-manager CSI, drop-all-caps
