# Managing IP address space with the CLI

This guide shows how to manage IP address space on Datum from the command line
using the `datumctl ipam` plugin — creating pools, claiming prefixes, inspecting
utilization, and releasing space.

> The plugin is a thin, task-oriented front end over the `ipam.miloapis.com`
> API. Everything here maps to real resources; add `-o yaml` to any read command
> to see them. For the full field-level reference, see [`../api.md`](../api.md).

## The resource model

IPAM manages three kinds of objects:

| Kind | Scope | What it is |
|------|-------|------------|
| **IPPool** | cluster | An allocatable block of address space. A *root* pool declares a CIDR directly (`10.0.0.0/8`); a *child* pool carves a sub-prefix from a parent, letting you build a hierarchy. |
| **IPClaim** | project (namespace) | A request for a sub-block of a given size from a pool. Creating one allocates a CIDR **synchronously** — the allocated block comes back in the same call. |
| **IPAllocation** | project (namespace) | The system-created record of the CIDR an IPClaim was granted. You don't create these directly; a bound claim points at one. |

In the CLI these read as two nouns:

- `datumctl ipam pool …` — work with **IPPool**s
- `datumctl ipam prefix …` — claim and manage sub-blocks (**IPClaim** + its **IPAllocation**)

A pool and everything under it use a single address family — IPv4 **or** IPv6,
never mixed. Dual-stack means two pools.

## Before you begin

1. **Install the plugin** (once per machine):

   ```sh
   datumctl plugin install milo-os/ipam
   datumctl ipam version
   ```

2. **Make sure IPAM is enabled for your project.** IPAM is a provider-gated
   service. The first `datumctl ipam` command in a project runs an entitlement
   preflight; if the project isn't enabled yet, the CLI tells you and can submit
   a request:

   ```sh
   datumctl services enable ipam-miloapis-com
   ```

   Enabling requires provider approval, so there may be a wait before the
   commands below work.

3. **Know your scope.** Pools are cluster-scoped and shared; claims and
   allocations live in your active project. The plugin uses your current
   `datumctl` context; override per-invocation with `--org` / `--project`, and
   target a specific namespace with `-n`.

## Working with pools

### List pools and see utilization

```sh
datumctl ipam pool list
datumctl ipam pool list -o wide                 # adds child/prefix counts and phase
datumctl ipam pool list --selector environment=prod
```

The table shows each pool's CIDR, family, utilization, and largest free block —
the quickest read on remaining headroom.

### Inspect one pool

```sh
datumctl ipam pool show prod-backbone
```

Shows the CIDR, family, phase, parent (if any), visibility, utilization,
capacity, largest free prefix, and any allocation bounds.

### Create a root pool

A root pool owns a CIDR outright:

```sh
datumctl ipam pool create prod-backbone --cidr 10.0.0.0/8
```

The address family is inferred from the CIDR; pass `--family ipv4|ipv6` to be
explicit.

### Create a child pool (delegation)

A child pool carves a sub-prefix from a parent and can constrain the claim sizes
allowed beneath it:

```sh
datumctl ipam pool create us-west \
  --parent prod-backbone \
  --prefix-length 16 \
  --min-length 24 --max-length 28 \
  --strategy BestFit
```

`--prefix-length` is the size carved from the parent; `--min-length` /
`--max-length` bound the prefixes that claims may request from this pool;
`--strategy` picks how free blocks are chosen (`FirstFit`, `BestFit`, or
`LeastUtilized`).

Use `--visibility platform|consumer|shared` to control who can allocate from the
pool, and `--dry-run` to preview without creating anything.

### Release a pool

Releasing a pool is the highest blast-radius action, so it asks you to confirm
by typing the pool name, and refuses if the pool still has child pools or active
prefixes:

```sh
datumctl ipam pool release us-west --dry-run    # show the blast radius first
datumctl ipam pool release us-west              # prompts for confirmation
datumctl ipam pool release us-west --cascade    # also release everything beneath it
```

## Claiming prefixes

Claiming allocates a sub-block and returns the CIDR **in the same command** —
no polling.

### Claim by size

```sh
datumctl ipam prefix claim --pool prod-backbone --length 24
```

You get back the allocated CIDR, the prefix (IPClaim) name, the bound
allocation, and the pool's before→after utilization.

Select the pool by label instead of by name with `--selector`/`-l`. Request a
specific family with `--family` when it can't be inferred from the pool.

### Make retries safe (idempotency)

Claims are **not** idempotent by default — each one consumes space. Pass a
stable `--name` so a retried claim returns the *existing* allocation instead of
consuming a second block:

```sh
datumctl ipam prefix claim --pool prod-backbone --length 24 --name app-net-3
```

This is the recommended form for scripts and CI.

### Preview without consuming space

`--dry-run` runs a server-side dry run: the server computes the exact block it
*would* allocate and rolls back, so the preview is accurate and nothing is
consumed:

```sh
datumctl ipam prefix claim --pool prod-backbone --length 14 --dry-run
```

### Other claim options

- `--cidr 10.1.0.0/24` — convenience that sets `--length` and `--family` from a
  CIDR (the server allocates by length; it does not pin an exact block).
- `--reclaim-policy Delete|Retain` — whether the underlying allocation is freed
  when the claim is deleted (`Delete` is the default).

### List, inspect, and release prefixes

```sh
datumctl ipam prefix list                       # your claimed prefixes
datumctl ipam prefix list --pool prod-backbone -o wide

datumctl ipam prefix show app-net-3             # by claim name…
datumctl ipam prefix show 10.24.0.0/24          # …or by allocated CIDR

datumctl ipam prefix release app-net-3 --dry-run
datumctl ipam prefix release app-net-3          # prompts for confirmation
```

## Output formats and scripting

Every read command supports the same output contract — data on stdout,
diagnostics on stderr:

- `-o table` (default) / `-o wide` — human tables
- `-o json` / `-o yaml` — the real resources, a stable contract for scripts
- `-o name` — just `kind/name`, for piping
- `--quiet` on `prefix claim` prints **only the allocated CIDR** — the one fact a
  script usually wants:

  ```sh
  CIDR=$(datumctl ipam prefix claim --pool prod-backbone --length 24 \
           --name app-net-3 --quiet)
  ```

Exit codes are stable and distinct per failure class, so scripts can branch on
them:

| Code | Name | Meaning |
|------|------|---------|
| 0 | — | Success |
| 1 | `IPAM_ERROR` | Generic / unexpected error |
| 2 | `IPAM_USAGE` | Invalid flags or arguments |
| 3 | `IPAM_FORBIDDEN` | Permission denied (RBAC) |
| 4 | `IPAM_NOT_FOUND` | No such pool/prefix (or not visible in this project) |
| 5 | `IPAM_CONFLICT` | Name or block already exists |
| 6 | `IPAM_INVALID` | Validation error (bad CIDR, length out of bounds) |
| 7 | `IPAM_POOL_EXHAUSTED` | The pool has no block of the requested size |
| 8 | `IPAM_UNAVAILABLE` | Transport / connection failure |
| 9 | `IPAM_ABORTED` | You declined a confirmation prompt |

## Troubleshooting

- **`IPAM_UNAVAILABLE` mentioning service entitlement** — IPAM isn't enabled for
  the project yet. Run `datumctl services enable ipam-miloapis-com` and wait for
  approval.
- **`IPAM_NOT_FOUND` on a pool you expect to see** — pools are visibility-scoped;
  it may not be visible in your active project. Run `datumctl ipam pool list` to
  see what you can allocate from.
- **`IPAM_POOL_EXHAUSTED` (exit 7)** — the pool has no free block of the
  requested size. `datumctl ipam pool show <pool>` reports the largest free
  prefix; request a smaller block or free space.
- **A claim consumed space you didn't expect** — claims aren't idempotent unless
  you pass `--name`. Re-running without a stable name allocates again.

## See also

- [IPAM API reference](../api.md) — kinds, fields, and error codes
- [IPAM integration guide](../integration-guide.md) — using the API directly
- `datumctl ipam <command> --help` — full, current flag reference for any command
