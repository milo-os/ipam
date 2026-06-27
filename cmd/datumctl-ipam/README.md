# datumctl-ipam

The IPAM plugin for `datumctl`. It presents the `ipam.miloapis.com/v1alpha1`
API as a small set of resource-oriented commands (pools and prefixes), turning
the common IPAM workflows — claim a prefix, see utilization, view the hierarchy,
release space — into single commands instead of hand-authored YAML and `jq`.

See the full enhancement at [`docs/enhancements/cli-plugin.md`](../../docs/enhancements/cli-plugin.md).

## Build

```bash
go build -o bin/datumctl-ipam ./cmd/datumctl-ipam
```

## Command surface

```text
# Pools (IPPool — cluster-scoped)
datumctl-ipam pool create <name> --cidr 10.0.0.0/8 [--family ipv4]
datumctl-ipam pool list [--selector k=v] [-o wide|json|yaml|name]
datumctl-ipam pool show <name>
datumctl-ipam pool tree [<name>] [--prefixes]
datumctl-ipam pool release <name> [--cascade] [--yes] [--dry-run]

# Prefixes (IPClaim / IPAllocation — namespaced)
datumctl-ipam prefix claim --pool <name> --length <n> [--name <n>] [--dry-run]
datumctl-ipam prefix list [--pool <name>] [-o wide|json|yaml|name]
datumctl-ipam prefix show <cidr|name>
datumctl-ipam prefix release <name> [--yes] [--dry-run]
```

Aliases: `ls` → `list`, `rm` → `release`.

## Transport

The plugin reaches the IPAM API in one of two ways:

1. **Datum (production)** — when `datumctl` dispatches to it, the active context
   arrives as environment variables and a short-lived token is fetched per call:

   | Variable | Meaning |
   |---|---|
   | `DATUM_API_HOST` | IPAM API host |
   | `DATUM_CREDENTIALS_HELPER` | Path to the helper; the plugin runs `<helper> auth get-token` |
   | `DATUM_ORG` / `DATUM_PROJECT` | Active org/project; selects the control-plane URL path (`…/projects/<id>/control-plane`). Empty = platform root. |

   The plugin never holds a long-lived credential.

2. **Kubeconfig (dev / e2e)** — standard `KUBECONFIG` / `--kubeconfig` /
   in-cluster config via client-go. This is how the plugin is exercised against
   the dev kind cluster, which has no Datum front door. An explicit
   `--kubeconfig` always forces this mode.

### Pointing at the dev cluster

The dev kind cluster (see the repo `Taskfile.yaml` and `task test-infra:cluster-up`)
serves the aggregated API directly. With its kubeconfig active:

```bash
# IPPool is cluster-scoped; claims live in a namespace (-n).
export KUBECONFIG=$(task test-infra:kubeconfig-path)   # or your kubeconfig

bin/datumctl-ipam pool create demo --cidr 10.128.0.0/20 --min-length 24 --max-length 28
bin/datumctl-ipam pool list -o wide
bin/datumctl-ipam prefix claim --pool demo --length 24 -n default
bin/datumctl-ipam pool tree demo --prefixes -n default
```

No `DATUM_*` variables are required for kubeconfig mode.

## Output and exit codes

- Default output is a human table sized to the terminal. `-o json|yaml` is a
  stable contract: data on **stdout**, all diagnostics/prompts on **stderr**.
  `-o wide|name` and `--quiet` give progressive density.
- Color auto-disables off a TTY, honors `NO_COLOR`, and obeys
  `--color=auto|always|never`. Color is never the sole signal (utilization shows
  `98% (HIGH)`; phase shows `BOUND`/`PENDING` as text).

Exit codes are a contract:

| Code | Name | Meaning |
|---|---|---|
| 0 | OK | success |
| 1 | IPAM_ERROR | generic / unexpected |
| 2 | IPAM_USAGE | invalid flags or arguments |
| 3 | IPAM_FORBIDDEN | HTTP 403 RBAC denial |
| 4 | IPAM_NOT_FOUND | HTTP 404 / no matching pool |
| 5 | IPAM_CONFLICT | HTTP 409 overlap / conflict |
| 6 | IPAM_INVALID | HTTP 400 validation error |
| 7 | IPAM_POOL_EXHAUSTED | HTTP 507 pool exhausted |
| 8 | IPAM_UNAVAILABLE | transport / connection failure |
| 9 | IPAM_ABORTED | user declined a confirmation |

## Safety

- Every mutation supports `--dry-run`.
- Confirmation scales to blast radius: a `prefix claim` has none; `prefix release`
  confirms; `pool release` requires typing the pool name and refuses
  non-interactively without `--yes`. Prompts auto-suppress when stdin is not a
  TTY or `CI` is set.
- A named claim (`--name`) is idempotent: a retry returns the existing
  allocation instead of consuming a second block.

## Plugin manifest

`datumctl` discovers the plugin via the manifest:

```bash
bin/datumctl-ipam --plugin-manifest
```

## Tests

```bash
go test ./cmd/datumctl-ipam/...
```
