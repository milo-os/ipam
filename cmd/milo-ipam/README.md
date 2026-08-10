# milo-ipam

The IPAM plugin for `datumctl`. It presents the `ipam.miloapis.com/v1alpha1`
API using the API's own nouns — class, claim, allocation, pool — turning the
common IPAM workflows into single commands instead of hand-authored YAML and
`jq`.

The vocabulary is deliberately identical to the API's, so a reader of the API
docs and a reader of `--help` are learning the same words. The plugin previously
said "prefix" where the API says claim and allocation; `prefix` survives only as
an alias.

See the full enhancement at [`docs/enhancements/cli-plugin.md`](../../docs/enhancements/cli-plugin.md).

## Build

```bash
go build -o bin/milo-ipam ./cmd/milo-ipam
```

## Command surface

```text
# Classes (IPClass — cluster-scoped, operator-authored, read-only here)
milo-ipam class list [--family ipv4|ipv6] [-o wide|json|yaml|name]
milo-ipam class show <name>

# Claims (IPClaim — namespaced)
milo-ipam claim create --class <name> [--scope role=name ...] [--name <n>] [--dry-run]
milo-ipam claim list [--class <n>] [--pool <n>] [--scope role=name] [-o wide|json|yaml|name]
milo-ipam claim show <name|address>
milo-ipam claim release <name> [--yes] [--dry-run]

# Allocations (IPAllocation — namespaced, system-created)
milo-ipam allocation list [--class <n>] [--pool <n>] [--purpose Claim|Reservation] [--unclaimed]
milo-ipam allocation show <name|address>
milo-ipam allocation release <name> [--yes] [--dry-run]

# Reverse lookup
milo-ipam address show <address>

# Pools (IPPool — cluster-scoped, operator surface)
milo-ipam pool create <name> --cidr 10.0.0.0/8 --class <name> [--scope role=name ...]
milo-ipam pool list [--selector k=v] [-o wide|json|yaml|name]
milo-ipam pool show <name>
milo-ipam pool tree [<name>] [--allocations]
milo-ipam pool release <name> [--cascade] [--yes] [--dry-run]
```

Aliases: `ls` → `list`, `rm` → `release`, `prefix` → `claim`, `prefix claim` →
`claim create`.

### Claiming names a class and a scope, never a pool

```bash
milo-ipam claim create --class tenant-endpoint-ipv4 \
  --scope network=default --scope location=us-central-1
```

There is no `--pool`, `--cidr`, or `--selector` on the claim path: which pool
serves a claim follows from its class and its scope, and the server reports the
one it resolved in `status.poolRef`.

Scope entries are `role=value` pairs. For a role the CLI knows (`location`,
`network`, `project`) a bare name is enough; any other role takes a qualified
reference, `--scope site=Site.infra.example.com/dc-1`. Append `#<uid>` to pin a
reference to one instance of the named object — the CLI leaves it unset by
default, because it resolves no consumer objects and a stale UID silently splits
one address space into two.

`--owner Kind.apiGroup/name[#uid]` records the consumer object an address is
held for. An owner UID takes no part in allocation identity, so unlike a scope
UID it is recorded whenever you supply one: it is what keeps `address show`
able to name the holder after a delete and recreate under the same name.

Before creating, the CLI checks the scope you gave against the class's
`status.requiredScopeRoles` and names any missing role rather than spending a
round trip on a claim the server will reject.

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

bin/milo-ipam pool create demo --cidr 10.128.0.0/20 --class demo-class \
  --min-length 24 --max-length 28
bin/milo-ipam class list
bin/milo-ipam claim create --class demo-class -n default
bin/milo-ipam pool tree demo --allocations -n default
```

No `DATUM_*` variables are required for kubeconfig mode.

### Scope of the reverse lookup

`address show` reads allocations in **one namespace of one control plane**. Both
bounds are named in the not-found message, and the remedy it offers depends on
the transport: on the Datum path `--project` re-targets the lookup, because the
active project is carried in the control-plane URL path and Milo's front gate
turns that path into the caller's tenant identity. On the kubeconfig path there
is no front gate and nothing can stamp the tenant extras, so `--project` cannot
reach the server at all — it is not offered there, and passing it prints a
warning on stderr saying it was ignored.

There is no cluster-wide reverse lookup, and the message does not imply one.

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
| 4 | IPAM_NOT_FOUND | HTTP 404 / no such class, claim, pool, or address |
| 5 | IPAM_CONFLICT | HTTP 409 overlap / conflict |
| 6 | IPAM_INVALID | HTTP 400 validation error |
| 7 | IPAM_POOL_EXHAUSTED | HTTP 507 pool exhausted |
| 8 | IPAM_UNAVAILABLE | transport / connection failure |
| 9 | IPAM_ABORTED | user declined a confirmation |

## Safety

- Every mutation supports `--dry-run`.
- Confirmation scales to blast radius: `claim create` has none; `claim release`
  and `allocation release` confirm; `pool release` requires typing the pool name
  and refuses non-interactively without `--yes`. Prompts auto-suppress when
  stdin is not a TTY or `CI` is set.
- A named claim (`--name`) is idempotent: a retry returns the existing
  allocation instead of consuming a second address. That is a promise about a
  retry, not about the name — reusing the name for a *different* request (a
  different class, scope, family, address, size or reclaim policy) is refused
  with the fields that differ, rather than answered with the old address.
  Omitted flags are never a mismatch, and neither is a field the existing claim
  left to the server.
- `pool release` counts allocations, not claims, when computing blast radius: a
  retained allocation whose claim is gone still holds an address out of the pool.
- `pool release` searches every namespace for those allocations. An `IPPool` is
  cluster-scoped and an `IPAllocation` is not, so a pool serves any namespace
  that claims from it. When authorization confines the search to one namespace
  the blast radius is reported as `UNKNOWN` and the release is refused: an
  undercount rendered as "none" is a dry run calling a destructive operation
  safe, and `--cascade` on a partial view dismantles what it can see and is then
  refused for what it cannot.
- `allocation release` refuses while a claim is still bound, so a claim and its
  address never disagree.
- `-n` is rejected when the value cannot name a namespace. A LIST against a
  namespace that does not exist returns 200 and an empty list — the Kubernetes
  contract, not ours — so an unusable `-n` would otherwise print
  "No allocations found." and exit 0, reading as a tenant that holds nothing.
  Only the syntactic half is checked; a well-formed name for a namespace that
  does not exist still lists empty.

## Plugin manifest

`datumctl` discovers the plugin via the manifest:

```bash
bin/milo-ipam --plugin-manifest
```

## Tests

```bash
go test ./cmd/milo-ipam/...
```
