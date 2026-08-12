# milo-ipam

The IPAM plugin for `datumctl`. It presents the `ipam.miloapis.com/v1alpha1`
API using the API's own nouns — class, claim, allocation, pool — turning the
common IPAM workflows — claim an address, see what you hold, look up who holds
one, release it — into single commands instead of hand-authored YAML and `jq`.

See the full enhancement at [`docs/enhancements/cli-plugin.md`](../../docs/enhancements/cli-plugin.md).

## Build

```bash
go build -o bin/milo-ipam ./cmd/milo-ipam
```

## Command surface

```text
# Classes (IPClass — cluster-scoped, operator-authored, read-only here)
milo-ipam class list [--family ipv4|ipv6] [--selector k=v]
milo-ipam class show <name>

# Claims (IPClaim — namespaced)
milo-ipam claim create --class <name> [--scope role=name ...] [--prefix-length <n>]
                       [--address <ip>] [--name <n>] [--owner <ref>]
                       [--reclaim-policy Delete|Retain] [--dry-run]
milo-ipam claim list [--class <n>] [--pool <n>] [--scope role=name ...]
milo-ipam claim show <name|address>
milo-ipam claim release <name> [--yes] [--dry-run]

# Allocations (IPAllocation — namespaced, system-created)
milo-ipam allocation list [--class <n>] [--pool <n>] [--purpose <p>] [--unclaimed]
milo-ipam allocation show <name|address>
milo-ipam allocation release <name> [--yes] [--dry-run]

# Reverse lookup
milo-ipam address show <ip|cidr>

# Pools (IPPool — cluster-scoped, operator surface)
milo-ipam pool create <name> --cidr 10.0.0.0/8 [--class <n> ...] [--scope role=name ...]
milo-ipam pool list [--selector k=v] [-o wide|json|yaml|name]
milo-ipam pool show <name>
milo-ipam pool tree [<name>] [--prefixes]
milo-ipam pool release <name> [--cascade] [--yes] [--dry-run]
```

Aliases: `ls` → `list`, `rm` → `release`, `prefix` → `claim`.

## Scope

A claim carries the references it is made for, keyed by role. For the roles the
CLI knows (`location`, `network`, `project`) a bare name is enough; any other
role takes a qualified reference:

```bash
milo-ipam claim create --class tenant-endpoint-ipv4 \
  --scope network=default --scope location=us-central-1

milo-ipam claim create --class fabric-link-ipv6 \
  --scope site=Site.infra.example.com/dc-1
```

The class names which roles it needs (`class show`, "Claims must scope by"), and
a claim missing one is refused before the round trip rather than falling back to
a wider comparison.

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
# IPPool and IPClass are cluster-scoped; claims and allocations live in a
# namespace (-n).
export KUBECONFIG=$(task test-infra:kubeconfig-path)   # or your kubeconfig

bin/milo-ipam pool create demo --cidr 10.128.0.0/20 --min-length 24 --max-length 28 \
  --class demo-subnet-ipv4
bin/milo-ipam class list
bin/milo-ipam claim create --class demo-subnet-ipv4 --prefix-length 24 -n default
bin/milo-ipam claim list -o wide -n default
bin/milo-ipam pool tree demo --prefixes -n default
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

- Every mutation supports `--dry-run`. `claim create --dry-run` is server-side:
  the apiserver resolves the pool and computes the real next address inside the
  allocation transaction, then rolls back, so the preview is exact.
- Confirmation scales to blast radius: `claim create` has none; `claim release`
  and `allocation release` confirm; `pool release` requires typing the pool name
  and refuses non-interactively without `--yes`. Prompts auto-suppress when
  stdin is not a TTY or `CI` is set.
- A named claim (`--name`) is idempotent: a retry returns the existing
  allocation instead of consuming a second address. Reusing that name for a
  different class or scope is refused rather than silently answered.
- Releasing a claim under reclaim policy `Retain` leaves its allocation held.
  `allocation list --unclaimed` is the leak check; `allocation release` is the
  deliberate hand-back.

## Plugin manifest

`datumctl` discovers the plugin via the manifest:

```bash
bin/milo-ipam --plugin-manifest
```

## Tests

```bash
go test ./cmd/milo-ipam/...
```
