# load-test overlay

IPAM with Milo quota admission ON, for the full-environment load test
(`.github/workflows/load-test.yaml`). It is one of four directories that together
make the environment; all are applied by `hack/load-test.sh`:

| Directory | Applied to | What it is |
|---|---|---|
| `config/overlays/load-test` | kind host cluster | IPAM + Postgres, `ENABLE_QUOTA=true`, kubeconfig for milo-apiserver |
| `config/overlays/load-test-latency` | kind host cluster | the above plus the `postgres-latency` component (Toxiproxy) |
| `config/overlays/load-test-milo` | kind host cluster | Milo etcd, apiserver, controller-manager (Milo's `test-infra` overlay at a pinned tag, plus aggregation wiring) |
| `config/overlays/load-test-milo-api` | **through Milo** | IPAM `APIService`, Milo webhooks, quota registration and policies |

Dev and test-infra are unchanged and still run with quota off.

```sh
export TASK_X_REMOTE_TASKFILES=1 CLUSTER_NAME=ipam-load-$USER   # unique per run
task load-test:up                       # kind + Milo + IPAM + quota + projects + smoke test
POSTGRES_LATENCY_MS=3 task load-test:up # same, with +3ms per Postgres round trip
task load-test:run                      # k6; non-zero exit if any threshold fails
task load-test:diagnostics              # logs/state into test/load/diagnostics/
task load-test:down
```

Request path: k6 -> `https://localhost:30000` (kind NodePort) -> milo-apiserver ->
`/apis/resourcemanager.miloapis.com/v1alpha1/projects/<p>/control-plane/...` ->
aggregator (proxy-client cert) -> ipam-apiserver -> quota plugin creates a
`ResourceClaim` in the project's control plane and waits for `Granted` -> Postgres.

Things that are easy to get wrong:

- The Milo ref in `load-test-milo` and `load-test-milo-api` and the image tag must equal
  the `go.miloapis.com/milo` version in `go.mod` (`task load-test:verify-milo-pin`, run in CI).
- Milo's own `test-infra` overlay leaves `PROXY_CLIENT_CERT_FILE` empty; without the
  patch in `load-test-milo` nothing can sit behind Milo's aggregator.
- Create the Organization only after Milo's webhooks are installed: the webhook creates
  the `organization-<name>` namespace.
- Host ports 30000 and 30443 are bound by the kind cluster, so one environment per Docker host.
