# IPAM k6 performance tests

This component bundles the k6 load tests as a Kubernetes-native test suite,
runnable by the [k6 operator](https://github.com/grafana/k6-operator).

## Workflow

```sh
# 1. Regenerate self-contained scripts from test/load/src/
task -t test/load/Taskfile.yaml generate

# 2. Apply this component to the dev overlay
task -t test/load/Taskfile.yaml k6:apply

# 3. Run a TestRun
task -t test/load/Taskfile.yaml k6:run TEST=throughput
```

## Tests

| TestRun                | Script                          | Purpose                                |
|------------------------|---------------------------------|----------------------------------------|
| `ipam-perf-setup`      | `setup-pools.js`                | One-time pool/namespace provisioning   |
| `ipam-perf-throughput` | `prefix-claim-throughput.js`    | IPPrefixClaim p95 < 500ms              |
| `ipam-perf-asn-throughput` | `asn-claim-throughput.js`   | ASNClaim p95 < 500ms                   |
| `ipam-perf-exhaustion` | `pool-exhaustion.js`            | Deny-path p95 < 200ms                  |
| `ipam-perf-reads`      | `read-latency.js`               | List/get latency under load            |
| `ipam-perf-scale`      | `pool-scale.js`                 | Latency stable across allocation depth |
