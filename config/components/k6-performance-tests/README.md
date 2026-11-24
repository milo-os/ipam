# IPAM k6 performance tests

This component bundles the k6 load tests in `test/load/` as a Kubernetes-native
test suite, runnable by the [k6 operator](https://github.com/grafana/k6-operator).
See `test/load/README.md` for what each test measures, its thresholds, and every
variable it reads.

## Workflow

```sh
# 1. Regenerate self-contained scripts from test/load/src/
task test/load:generate

# 2. Apply the scripts ConfigMap and RBAC
task test/load:k6:apply

# 3. Run the TestRuns one at a time, setup first
task test/load:k6:run TEST=setup
task test/load:k6:run TEST=throughput
```

## Identity

The runner pod authenticates as ServiceAccount `ipam-k6-runner` and sends every
IPAM request as the impersonated user `ipam-perf-tester`, with the project in the
`iam.miloapis.com/parent-*` user extras (`IPAM_TENANCY=impersonate`). IPAM scopes
every object to a project, so a request without one cannot allocate. `rbac.yaml`
lets the runner impersonate that user and those extras and manage the claim
namespaces; `tenant-rbac.yaml` gives the impersonated user its IPAM access.

These TestRuns target the kube-apiserver the operator runs in, so they suit an
environment where IPAM is aggregated by that apiserver (the dev overlay). An
environment where IPAM sits behind Milo needs `IPAM_TENANCY=milo`, a Milo
`IPAM_API_URL` and `IPAM_TOKEN`, and existing `PROJECTS`; run those from the
Taskfile rather than from these TestRuns.

## Tests

| TestRun                  | Script                            | Gate                                         |
|--------------------------|-----------------------------------|----------------------------------------------|
| `ipam-perf-setup`        | `setup-pools.js`                  | Fixtures created and allocating              |
| `ipam-perf-throughput`   | `prefix-claim-throughput.js`      | Claim p95 < 500ms, success > 0.95            |
| `ipam-perf-class-chain`  | `class-chain-claim-throughput.js` | Steady p95 < 500ms, first-claim p95 < 1000ms |
| `ipam-perf-exhaustion`   | `pool-exhaustion.js`              | 507 deny path p95 < 200ms                    |
| `ipam-perf-reads`        | `read-latency.js`                 | Prefix list p95 < 200ms                      |
| `ipam-perf-scale`        | `pool-scale.js`                   | p95 at /28 within 3x of /20                  |
| `ipam-perf-cleanup`      | `cleanup.js`                      | Everything the tests made is deleted         |

A crossed threshold makes k6 exit 99, which the operator reports as a failed
runner pod.
