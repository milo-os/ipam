# IPAM load tests

k6 scripts that measure claim, deny-path and read latency against a running IPAM
deployment. `src/` holds the scripts, `lib/ipam-client.js` holds the transport and
the fixtures they share, and `config/components/k6-performance-tests/` holds the
same scripts bundled for the k6 operator.

This file is the interface for anything that runs these tests, including CI.
Target names, variables and exit codes below are the contract. Change them here
and in `Taskfile.yaml` together.

## Tests

| Target        | Script                            | What it measures                                                       | Thresholds (fail the run)                                                        |
|---------------|-----------------------------------|------------------------------------------------------------------------|----------------------------------------------------------------------------------|
| `setup`       | `setup-pools.js`                  | Creates the fixtures in every project and proves each one allocates    | Any failure aborts (exit 107)                                                    |
| `throughput`  | `prefix-claim-throughput.js`      | Create and delete a /28 claim of a flat IPv4 class                     | `ipam_claim_create_latency_ms{phase:success}` p95 < 500ms, p99 < 2000ms; `ipam_claim_success_rate` > 0.95; `http_req_failed` < 0.05 |
| `class-chain` | `class-chain-claim-throughput.js` | Claims through vpc → subnet → endpoint: `steady` (pools exist) then `first` (each claim provisions its vpc and subnet pools) | `ipam_chain_claim_latency_ms{case:steady,phase:success}` p95 < 500ms; `{case:first,...}` p95 < 1000ms; success rate > 0.95 per case |
| `exhaustion`  | `pool-exhaustion.js`              | Claims against a full four-slot pool, which must all be refused with 507 | `ipam_deny_latency_ms` p95 < 200ms; `ipam_success_latency_ms` p95 < 800ms; `ipam_deny_rate` > 0.90 |
| `reads`       | `read-latency.js`                 | Pool list, claim list, claim get and cross-namespace claim list, under steady, ramp and spike load | `ipam_prefix_list_ms` p95 < 200ms; `ipam_claim_list_ms` p95 < 200ms; `ipam_claim_get_ms` p95 < 100ms; `ipam_cluster_list_ms` p95 < 2000ms; `ipam_read_success_rate` > 0.99 |
| `scale`       | `pool-scale.js`                   | Create latency at 80% fill for /20 to /28 claims in one /16 pool        | p95 at /28 within 3x of p95 at /20 (`ipam_scale_ratio_violation` == 0)            |
| `cleanup`     | `cleanup.js`                      | Deletes every claim, pool, class and namespace the tests created       | none; exits 0 unless k6 itself fails                                             |
| `ci`          | (the above)                       | `setup`, each test in `CI_SUITE`, then `cleanup`                        | non-zero if setup or any test failed                                             |

Every test that creates claims also has `ipam_quota_denied` == 0, so a run with
quota on that is short of grants fails by name instead of as a generic error.

The thresholds for `throughput`, `exhaustion` (deny path) and `reads` (prefix
list) are CLAUDE.md's acceptance criteria. The `class-chain` thresholds are
provisional; override them with `CHAIN_STEADY_P95_MS` and `CHAIN_FIRST_P95_MS`.

`class-chain` is the benchmark for milo-os/ipam#195. Compare
`ipam_chain_claim_latency_ms` for `case:steady` and `case:first` in
`results/class-chain.json` before and after a change.

## Quota-on targets

The thresholds in the table above are the strict targets, for runs without quota admission. A run through Milo with quota on (`IPAM_TENANCY=milo`, which is how the load-test workflow runs) uses the looser targets below, because every claim create then also waits for Milo to grant a ResourceClaim. `QUOTA_ON=true|false` overrides the inference. The strict targets are unchanged and still apply to quota-off runs.

In kind, with Milo v0.36.22, the wait for the grant dominated: across 1,757 creates the IPAM request averaged 930ms and the quota plugin's wait for the grant averaged 920ms (p50 up to 0.5s, p95 up to 5s), while Postgres queries stayed under 5ms and the namespace check was cached. With Milo v0.36.26 the grant wait is small (averages of 54ms and 56ms over about 1,800 creates), but the request still averages about 900ms. The IPAM log shows why: the Milo clients that the quota plugin and the namespace check use are throttled on the client side (client-go's default of 5 requests per second per project client), with waits of 1 to 2.2s on the ResourceClaim POST and GET. Creates therefore queue at about 10 per second at 10 VUs, and the wait comes before allocation, so a claim that is then refused with 507 pays it too.

The quota-on limits are the worst p95 seen across short runs (30s, 10 VUs, with and without 3ms added to Postgres round trips, on both Milo versions) with roughly 2x headroom, so a healthy scheduled run passes and a change that multiplies latency still fails. Rate, failure and `ipam_quota_denied` thresholds are the same in both modes.

| Threshold                                            | Strict  | Quota-on | Worst p95 measured |
|------------------------------------------------------|---------|----------|--------------------|
| `throughput` create p95 / p99                        | 500ms / 2000ms | 6000ms / 10000ms | 3.28s (max 3.76s) |
| `class-chain` `steady` p95                           | 500ms   | 7000ms   | 3.51s              |
| `class-chain` `first` p95                            | 1000ms  | 7000ms   | 2.88s              |
| `exhaustion` deny p95                                | 200ms   | 6000ms   | 2.80s              |
| `exhaustion` success p95                             | 800ms   | 2000ms   | 197ms (few samples) |
| `reads` prefix list p95 / claim list p95             | 200ms   | 500ms    | 223ms / 184ms      |
| `reads` claim get p95                                | 100ms   | 300ms    | 137ms              |
| `reads` cluster list p95                             | 2000ms  | 2000ms   | 219ms              |
| `scale` p95 per depth                                | 2000ms  | 5000ms   | 479ms (one sequential sweep, so little queueing) |
| `scale` p95 ratio, /28 against /20                   | 3x      | 15x      | 7.28x (p95 1587ms at /28, of which IPAM's own work was 37ms) |

Reads do not wait on quota; their limits are wider only because the extra hop through Milo and the added Postgres latency pushed them past the strict ones. These numbers come from one shared 4-CPU machine and short runs. Treat them as a regression guard, not as a target to design to, and tighten them once the grant wait is lower.

## Fixtures

`setup` creates these in every project. Root pools of one project may not
overlap; two projects may hold the same range, so every project gets the same
set.

| Objects                                         | Shape                                                                                         |
|-------------------------------------------------|-----------------------------------------------------------------------------------------------|
| IPClass `perf-v4`, IPPool `perf-v4-root`        | Flat IPv4 leaf class, /20 to /28, offered directly by `10.0.0.0/8`                            |
| IPClasses `perf-vpc`, `perf-subnet`, `perf-endpoint`, IPPool `perf-vpc-root` | `perf-vpc` /48 per (network, project) from `fd20::/32` with the first /64 withheld; `perf-subnet` /64 per (network, location, project); `perf-endpoint` /96, unique within network. Same shape as `internal/allocator/placement_postgres_test.go` |
| Namespaces `ipam-perf-0` to `ipam-perf-3`       | Where claims are created                                                                       |

`exhaustion` creates `perf-exhaust` and `perf-exhaust-pool` (`192.168.100.0/28`, /30
only), and `scale` creates `perf-scale` and `perf-scale-pool` (`172.16.0.0/16`).
Each deletes its own in teardown. Every claim carries the label
`ipam.miloapis.com/load-test=true`.

## Running

```sh
# Against a kind dev deployment, from a workstation
task test/load:rbac                 # once per cluster
task test/load:proxy &              # kubectl proxy on :8001, the default IPAM_API_URL
task test/load:setup
task test/load:throughput VUS=20 DURATION=1m
task test/load:cleanup

# Everything, continuing past failures
task -x test/load:ci DURATION=30s
```

The same targets exist as `load:<name>` from the repository root and as
`<name>` with `task -t test/load/Taskfile.yaml`.

## Interface contract

### Targets

Run `setup` before any test and `cleanup` after the last. The tests share fixed
fixture names, so run one target at a time per environment. Tests are
independent of each other once `setup` has run, so any subset in any order is
valid. `ci` does all of this and runs every test in `CI_SUITE` even if an
earlier one fails.

| Target (root Taskfile)       | Notes                                                         |
|------------------------------|---------------------------------------------------------------|
| `test/load:setup`            | Idempotent                                                    |
| `test/load:throughput`       |                                                               |
| `test/load:class-chain`      | Runs its cases back to back: total time is about cases x `DURATION` |
| `test/load:exhaustion`       | Creates and deletes its own fixture                           |
| `test/load:reads`            | Fixed length of about `DURATION` plus setup                   |
| `test/load:scale`            | Holds up to 3276 claims at once; slow (see below)             |
| `test/load:cleanup`          | Idempotent; safe after a failed or aborted run                |
| `test/load:ci`               | `CI_SUITE` defaults to `throughput class-chain exhaustion reads` |
| `test/load:rbac`             | Impersonate mode only; applies `tenant-rbac.yaml` with `KUBECONFIG` |
| `test/load:verify-generated` | Fails if the bundles under `config/components/k6-performance-tests/generated/` are stale |

### Exit codes

`task` exits **201** when a command fails, whatever the command's code. Run
`task -x` to get the k6 code itself:

| k6 exit | Meaning                                                                |
|---------|------------------------------------------------------------------------|
| 0       | Every threshold passed                                                 |
| 99      | At least one threshold was crossed                                     |
| 107     | Script error, including a `setup` that could not build or prove its fixture |
| 108     | The run aborted itself (`scale` when a fill or sample claim is refused) |

`task -x test/load:ci` exits 1 if setup or any test failed, and 0 otherwise. A
cleanup failure is logged but does not change the exit code.

Under the k6 operator, the runner pod's container exit code is the k6 exit code.

### Results

Every test writes a k6 summary to `$RESULTS_DIR/<target>.json` (default
`test/load/results/`, which is gitignored). `RESULTS_DIR` can be passed as a task
variable or exported.

### Variables

Pass these as task variables (`task test/load:throughput VUS=20`) or export
them. Both reach k6.

Transport and tenancy, used by every script:

| Variable                 | Default                  | Meaning                                                              |
|--------------------------|--------------------------|----------------------------------------------------------------------|
| `IPAM_API_URL`           | `http://localhost:8001`  | Base URL: a kube-apiserver aggregating IPAM, or Milo's front door     |
| `IPAM_TENANCY`           | `impersonate`            | How the project reaches IPAM: `impersonate` or `milo` (see below)     |
| `IPAM_TOKEN`             | (none)                   | Bearer token                                                          |
| `IPAM_TOKEN_FILE`        | in-cluster SA token      | File holding the bearer token; read only if `IPAM_TOKEN` is unset     |
| `IPAM_IMPERSONATE_USER`  | `ipam-perf-tester`       | Impersonate mode only                                                 |
| `IPAM_IMPERSONATE_GROUP` | (none)                   | Impersonate mode only; one extra group                                |
| `K6_INSECURE_SKIP_TLS_VERIFY` | skip                | `false` to verify TLS                                                 |
| `PROJECTS`               | (none)                   | Comma-separated project names; overrides the next two                 |
| `PROJECT_PREFIX`         | `ipam-perf`              | Projects are `<prefix>-0` .. `<prefix>-<count-1>`                    |
| `PROJECT_COUNT`          | `5`                      |                                                                       |
| `NAMESPACE_PREFIX`       | `ipam-perf`              | Claim namespaces are `<prefix>-0` ..                                  |
| `NAMESPACE_COUNT`        | `4`                      |                                                                       |
| `CREATE_NAMESPACES`      | `true`                   | `setup` creates the claim namespaces                                  |
| `DELETE_NAMESPACES`      | `true`                   | `cleanup` deletes them                                                |

Per test:

| Variable                | Used by                           | Default            |
|-------------------------|-----------------------------------|--------------------|
| `VUS`                   | throughput, class-chain, exhaustion | 10, 10, 20       |
| `DURATION`              | throughput, class-chain (per case), exhaustion, reads | 2m, 1m, 1m, 3m |
| `CLAIM_PREFIX_LENGTH`   | throughput                        | 28                 |
| `CASES`                 | class-chain                       | `steady,first`     |
| `STEADY_NETWORKS`, `LOCATIONS` | class-chain                | 4, 2               |
| `PRUNE_POOLS`           | class-chain                       | `true` (teardown deletes the pools the `first` case made) |
| `CHAIN_STEADY_P95_MS`, `CHAIN_FIRST_P95_MS` | class-chain   | 500, 1000          |
| `EXHAUST_CIDR`          | exhaustion                        | `192.168.100.0/28` |
| `READ_VUS`, `SPIKE_DURATION`, `READ_CLAIMS` | reads         | 10, 30s, 10        |
| `PREFIX_STEPS`, `FILL_PCT`, `SAMPLES`, `MAX_DEPTH_RATIO` | scale | `20,22,24,26,28`, 0.8, 50, 3 (15 with quota on) |
| `SCALE_CIDR`, `SCALE_PROJECT`, `FILL_BATCH` | scale         | `172.16.0.0/16`, first project, 20 |
| `CI_SUITE`              | ci                                | `throughput class-chain exhaustion reads` |
| `SETUP_TIMEOUT`, `TEARDOWN_TIMEOUT` | all                   | per script         |

The load-test workflow (`.github/workflows/load-test.yaml`) exposes `VUS`, `DURATION`, `CASES`, `STEADY_NETWORKS` and `LOCATIONS` as dispatch inputs (`vus`, `duration`, `chain_cases`, `steady_networks`, `locations`), next to `project_count` and `postgres_latency_ms`. Left empty they keep the defaults above, which is what the nightly run uses. For a run at scale, raise `vus` and lower `steady_networks` and `locations` to put more claims on each pool's lock, for example:

```bash
gh workflow run load-test.yaml --ref <branch> \
  -f tests="setup class-chain cleanup" -f chain_cases=steady \
  -f vus=100 -f duration=3m -f steady_networks=1 -f locations=1
```

A run with any of these set is comparable only with runs that set the same values, and a quota-on threshold crossed at high concurrency fails the job without losing the numbers.

## Tenancy: how a request names its project

IPAM scopes every object to a project, read from the `iam.miloapis.com/parent-*`
user extras, and resolving a claim's class needs one. A request without a
project cannot allocate. The scripts send every IPAM request as a project in one
of two ways.

**`IPAM_TENANCY=impersonate`** (default). `IPAM_API_URL` is a kube-apiserver
that aggregates IPAM. Each request carries `Impersonate-User` and
`Impersonate-Extra-iam.miloapis.com%2fparent-{api-group,type,name}`; the
kube-apiserver turns these into the user extras on the request it forwards.
The e2e suites use the same mechanism. The caller needs `impersonate` on the
user and the three userextras; the kind admin has it, and
`config/components/k6-performance-tests/rbac.yaml` grants it to the in-cluster
runner. The impersonated user needs `tenant-rbac.yaml`. Projects do not have to
exist anywhere. Namespaces are created in the host cluster with the caller's own
credentials.

**`IPAM_TENANCY=milo`**. `IPAM_API_URL` is Milo's front door. Each project is
reached at `/apis/resourcemanager.miloapis.com/v1alpha1/projects/<project>/control-plane`,
and Milo writes the extras. `IPAM_TOKEN` must be a Milo credential that can use
those projects, and the projects must already exist (`PROJECTS`, or
`PROJECT_PREFIX` and `PROJECT_COUNT` matching what was seeded). `setup` creates
the claim namespaces inside each project's control plane.

## Quota on

With Milo quota admission enabled, every IPClaim create makes a ResourceClaim
in the project's control plane, in the claim's namespace, and waits up to 30s
for it to be granted. A quota-on run needs:

- `IPAM_TENANCY=milo`. Quota admission reads the project from the request and
  routes the ResourceClaim to that project's control plane. That only works
  where the project exists in Milo.
- The claim namespaces in each project's control plane. `setup` creates them
  (`CREATE_NAMESPACES=true`). IPAM also refuses a claim into a namespace the
  project's control plane does not have.
- A ClaimCreationPolicy for `ipam.miloapis.com/IPClaim` and an
  `ipam.miloapis.com/ipclaims` allowance per project at least as large as the
  most claims a test holds at once, plus the claims still waiting to be
  released. The service-catalog default (100 per project) is not enough for
  `scale`, which holds 3276 claims at /28 in one project, and is tight for the
  others. `config/overlays/load-test-milo-api/` grants 100,000,000.
- Unique claim names, which every script already uses. Milo names the
  ResourceClaim after the IPClaim and refuses a name whose previous claim is
  still being cleaned up.

A quota refusal is a 403 whose message mentions quota. The scripts count it in
`ipam_quota_denied` and fail that threshold. They do not treat it as a 507.

## Known limits

- **Dev latency is capped by a namespace check, not by allocation.** On a
  cluster without Milo (the kind dev and test-infra overlays), every claim
  create looks the claim's namespace up through a project control-plane path
  that does not exist there (`internal/access/namespace.go`). Only a Live
  answer is cached, so each create makes that lookup through a per-project
  client-go client with its default rate limit. Creates therefore queue at about
  5 per second per project in 200ms steps. On kind, `throughput`, `class-chain`
  and `exhaustion` exceed their latency thresholds for that reason, while every
  IPAM Postgres query stays under a millisecond. The numbers measure IPAM's
  allocation path only where the project control plane exists.
- `scale` at the default steps creates about 4,600 claims. At the dev create
  rate it takes tens of minutes. Use a smaller `SCALE_CIDR` (for example
  `172.16.0.0/22` with `PREFIX_STEPS=24,26,28`) for a quick run.
- k6 limits concurrent requests per host within `http.batch` (`batchPerHost`),
  so a VU-level test does not prove allocation uniqueness under contention.
  `test/e2e/class-cascade-concurrency` does, with separate processes.

## Removed scripts

| Script                               | Why                                                                          |
|--------------------------------------|------------------------------------------------------------------------------|
| `asn-claim-throughput.js` and the ASN setup | ASNPoolClass, ASNPool and ASNClaim no longer exist                     |
| `cross-project-claim-throughput.js`  | Claims no longer name a pool (`spec.poolRef.projectRef` is gone). Cross-project use is now a class reference (`IPClass.spec.source`), which is a different scenario |
| `ipv6-claim-throughput.js`           | Its pool-per-project IPv6 shape no longer exists; IPv6 allocation is covered by `class-chain` |
| `concurrent-claims.js`, `host-prefix-claim-concurrent.js` | Built on `spec.poolRef`. Their uniqueness check used `http.batch`, which k6 throttles to six requests per host, so it never tested the contention it claimed to. `test/e2e/class-cascade-concurrency` covers it |
| `mixed-load.js`                      | `throughput` and `reads` cover its write and read mixes separately, and each has its own thresholds |
| `watch-latency.js`                   | It opened the watch after the create had returned, so it measured replay of a committed event, not propagation. k6 buffers whole responses and cannot time an event as it arrives. Watch lag is exported by the server as `ipam_watch_lag_seconds` |
