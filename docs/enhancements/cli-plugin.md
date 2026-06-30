---
status: provisional
stage: alpha
latest-milestone: "v0.x"
---

# An IPAM Plugin for datumctl

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Command surface](#command-surface)
  - [Identity and context](#identity-and-context)
  - [Claiming a prefix](#claiming-a-prefix)
  - [Seeing the shape of your address space](#seeing-the-shape-of-your-address-space)
  - [Human-first output, script-friendly on demand](#human-first-output-script-friendly-on-demand)
  - [Errors that name the fix](#errors-that-name-the-fix)
  - [Previewing and confirming mutations](#previewing-and-confirming-mutations)
  - [Discoverability: help, completion, and suggestions](#discoverability-help-completion-and-suggestions)
  - [Distribution through the plugin catalog](#distribution-through-the-plugin-catalog)
  - [Roadmap surfaces: addresses and ASNs](#roadmap-surfaces-addresses-and-asns)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)

## Summary

The IPAM service gives Datum a fast, transactional, Kubernetes-native source of
truth for IP space: a user creates an `IPPool`, submits an `IPClaim`, and gets an
allocated CIDR back **synchronously in the create response** — no polling, no
conflict window. Today the only way to drive that service is raw `kubectl`:
hand-authored YAML for every claim, `-o json | jq` to read back utilization, and
manual tracing of `parentPoolRef` chains to understand a prefix hierarchy. The
power is there; the experience is not.

**`datumctl ipam`** is a `datumctl` plugin that gives IPAM a first-class
command-line experience, distributed through the same plugin catalog users
already use to extend the CLI. It turns the most common IPAM workflows — claim a
prefix, see what's allocated, understand pool utilization, release space — into
short, memorable commands that read like the
user's mental model ("give me a /24 from the backbone pool") rather than the
service's wire format. It inherits the user's existing `datumctl` login and active
org/project context, so there is no second authentication and no place to leak
credentials. And it follows the conventions developers already expect from
`kubectl`, `gh`, and `docker`: a rich human-readable default, a stable
`-o json|yaml` contract for automation, `--dry-run` previews, actionable errors,
and shell completion.

The result is that managing IP space on Datum feels like using a purpose-built
tool — `datumctl ipam prefix claim --pool prod-backbone --length 24` returns the
allocated CIDR and the new utilization in one line — instead of feeling like
operating a database through YAML.

## Motivation

IPAM is infrastructure that humans and pipelines touch constantly: an engineer
carving a subnet for a new environment, a provisioning pipeline claiming a CIDR
to hand to Terraform, a platform operator auditing how full a backbone pool is
before the next allocation. Every one of those interactions today goes through
`kubectl` against the `ipam.miloapis.com/v1alpha1` API, and `kubectl` is a generic
tool that knows nothing about IP space. That generality is the problem:

- **Every claim is hand-authored YAML.** Claiming a single /24 means writing an
  `IPClaim` manifest with `apiVersion`, `kind`, `metadata`, `spec.ipFamily`,
  `spec.prefixLength`, and `spec.poolRef`, then `kubectl create -f`. The one fact
  the user cares about — the CIDR they got — is buried in `status.allocatedCIDR`
  of the response. The service answers synchronously, but the tooling makes it
  feel like paperwork.
- **Utilization is invisible.** `kubectl get ippools` shows a pool's CIDR and
  phase but not how full it is. To answer "how much of `prod-backbone` is left?" a
  user runs `kubectl get ippool prod-backbone -o json | jq '.status.capacity'` —
  the single most important operational question requires a JSON detour.
- **The hierarchy is invisible.** Pools nest: a root pool delegates a /16 to a
  regional child pool, which serves /24 leaf claims. There is no way to *see* that
  tree. A user lists every pool and manually follows `spec.parentPoolRef` links to
  reconstruct it in their head.
- **The signature failures are opaque.** When a pool is exhausted the service
  returns HTTP 507; through `kubectl` that surfaces as a terse error with no
  indication of the largest block that *is* available or what to do next. The
  rich, transactional semantics of the service are flattened into a generic
  Kubernetes error.
- **There is no safe rehearsal.** Allocation consumes space and is not
  idempotent. A user planning capacity has no way to ask "what *would* I get, and
  how full would the pool be afterward?" without actually consuming an allocation.

None of this is a gap in the service — the API is deliberately synchronous,
transactional, and multi-tenant. It is a gap in the *experience*. A small,
focused CLI that speaks the language of IP space turns these interactions from
YAML authoring and `jq` spelunking into single commands, and does so without
inventing any new server-side surface: the plugin is a thin, well-mannered client
over the existing API.

### Goals

- **Make the common IPAM workflows single commands.** Claiming a prefix, listing
  what's allocated, inspecting a pool, understanding utilization, and releasing
  space should each be one short, memorable invocation — no hand-authored YAML for
  the everyday path.
- **Make IP space legible.** Surface the two facts `kubectl` hides — pool
  *utilization* and the prefix *hierarchy* — as first-class, at-a-glance output.
- **Inherit the user's identity and context.** The plugin reuses the existing
  `datumctl` login and active org/project; there is no second sign-in and the
  plugin never handles a long-lived credential.
- **Honor the conventions developers already know.** Resource-oriented
  (noun-verb) grammar, a human table by default with a stable `-o json|yaml`
  contract for scripts, `--dry-run`, actionable errors, confirmations on
  destructive actions, and shell completion — matching `kubectl`/`gh`/`docker`.
- **Ship as a catalog plugin, not a core command.** Deliver through the
  `datumctl` plugin mechanism so IPAM tooling can evolve on its own release
  cadence and be installed (or not) per user.
- **Stay a thin client.** Introduce no new server-side API, type, or behavior; the
  plugin is a presentation layer over the existing `ipam.miloapis.com/v1alpha1`
  API.

### Non-Goals

- **Replacing `kubectl` or the API.** Power users and GitOps pipelines keep full
  access to the raw API and declarative YAML. The plugin is the friendly path for
  the common case, not the only path.
- **Changing IPAM semantics.** Allocation strategy, multi-tenancy, exhaustion
  behavior, and the transactional model are defined by the service and are out of
  scope here. The plugin presents them; it does not redefine them.
- **Defining the plugin distribution mechanism itself.** How catalogs and the
  marketplace work is specified by the
  [datumctl plugin marketplace enhancement][marketplace]; we assume that
  mechanism and describe the IPAM plugin that ships through it.
- **Building a graphical UI or web console.** This is a terminal experience.
- **Implementation sequencing.** The focus here is the intended user
  experience; engineering order is out of scope.

## Proposal

Introduce `datumctl ipam`, a `datumctl` plugin that presents the IPAM service as a
small set of resource-oriented commands. It reuses the user's active `datumctl`
session and org/project context to call the `ipam.miloapis.com/v1alpha1` API, and
installs once from the plugin catalog:

```console
$ datumctl plugin install ipam
```

From then on, IPAM is a verb in their everyday vocabulary. The two nouns that
matter most map directly to the service's resources:

- **`pool`** — an allocatable block of address space (the `IPPool` resource).
  Users create pools, list them with utilization, view the hierarchy as a tree,
  and release them.
- **`prefix`** — a sub-block claimed from a pool (the `IPClaim`/`IPAllocation`
  pair). Users claim a prefix and get a CIDR back synchronously, list what they've
  claimed, inspect a claim, and release it.

Roadmap resources — individual **addresses** and **ASNs** — slot into the same
grammar as the service grows (see [Roadmap surfaces](#roadmap-surfaces-addresses-and-asns)).

The full experience — the command surface, how context and auth are inherited,
the claim flow, the tree and utilization views, output modes, error design, and
distribution — is detailed in [Design Details](#design-details). The stories below
make it concrete through the people it serves.

### User Stories

#### Story 1: An engineer claims a subnet without writing YAML

Maya needs a /24 for a new staging environment. Instead of authoring an `IPClaim`
manifest, she runs:

```console
$ datumctl ipam prefix claim --pool staging-backbone --length 24
✓ Claimed 10.7.4.0/24 from pool "staging-backbone"  (utilization 31% → 32%)
  prefix:      staging-env-7
  allocation:  alloc-9f2a1c
  pool:        staging-backbone (10.7.0.0/16, IPv4)
```

The CIDR she needs is the headline, returned in the same synchronous call the API
already makes. She copies `10.7.4.0/24` straight into her Terraform variables and
moves on. To script the same thing, she adds `-o json` and reads
`.status.allocatedCIDR`.

#### Story 2: An operator checks utilization before allocating

Sam is about to delegate a large block and wants to know how much room the
backbone pool has left. One command, no `jq`:

```console
$ datumctl ipam pool list
NAME               CIDR             FAMILY   UTILIZATION       LARGEST FREE   AGE
prod-backbone      10.0.0.0/8       IPv4     ███████░░░  73%   /14            312d
edge-v6            2001:db8::/32    IPv6     █░░░░░░░░░   4%    /36            44d
staging-backbone   10.7.0.0/16      IPv4     ███░░░░░░░  32%   /18            120d
```

Utilization and the largest free block — the two questions an operator actually
asks — are columns, not a JSON detour. Sam sees `prod-backbone` is getting tight
and the largest contiguous block left is a /14, then decides accordingly.

#### Story 3: An operator sees the prefix hierarchy as a tree

Before releasing a regional pool, Sam wants to understand what lives under it:

```console
$ datumctl ipam pool tree prod-backbone --prefixes
prod-backbone            10.0.0.0/8      IPv4   73% used   (root pool)
├─ us-west               10.1.0.0/16     IPv4   61% used   (child pool)
│  ├─ payments-prod      10.1.0.0/24     IPv4   ─          (prefix · team-payments)
│  └─ search-prod        10.1.1.0/24     IPv4   ─          (prefix · team-search)
└─ us-east               10.2.0.0/16     IPv4   12% used   (child pool)
   └─ analytics-prod     10.2.0.0/22     IPv4   ─          (prefix · team-analytics)
```

The nesting that `kubectl` forces Sam to reconstruct by hand — following `parentPoolRef` links across a flat list — is rendered directly, with each pool's "% used" at every level. Leaf prefixes and the consuming team on each are shown when Sam adds `--prefixes`; by default the tree shows just the pool hierarchy.

#### Story 4: A pipeline claims a CIDR and fails loudly when space runs out

A provisioning pipeline claims a prefix on every environment build. It runs the
plugin in script mode and branches on the exit code:

```console
$ datumctl ipam prefix claim --pool env-pool --length 22 -o json --quiet
```

When `env-pool` is exhausted, the plugin doesn't print a generic Kubernetes error
and exit 1 like everything else. It surfaces the IPAM-specific failure with a
distinct, documented exit code and a message a human reviewing the CI log can act
on:

```console
Error: pool "env-pool" has no free /22 block (requested length 22).
       Largest available block is /24; utilization is 98%.
Fix:   request a smaller prefix (--length 24) or free space:
       datumctl ipam prefix list --pool env-pool
exit status 7   # IPAM_POOL_EXHAUSTED
```

The pipeline distinguishes "pool full" from "auth failed" without scraping text,
and the on-call engineer who reads the log knows exactly what happened and what to
do.

#### Story 5: A capacity planner rehearses an allocation safely

Before committing, Dana wants to know what a large claim would do to a pool —
without consuming it. `--dry-run` answers exactly that:

```console
$ datumctl ipam prefix claim --pool prod-backbone --length 14 --dry-run
Dry run — no allocation was made.
Would claim:   10.4.0.0/14 from pool "prod-backbone"
Pool after:    utilization 73% → 98%, largest free block /17
```

The block Dana sees is exact: `--dry-run` is a real server-side dry run, so the apiserver runs the allocation it *would* perform and reports the precise CIDR before rolling it back — nothing is persisted and no capacity is consumed. The projected after-utilization and largest-free block shown alongside it are estimates. Dana sees the claim would nearly fill the pool and chooses a different parent. Nothing was allocated; the rehearsal was free.

### Notes/Constraints/Caveats

- **User-facing nouns track the user's mental model, not the wire types.** The
  service's allocation resource is `IPClaim` (with a system-created
  `IPAllocation`); the plugin presents this as `prefix` because users think in
  prefixes, not claims. The mapping is documented in help and in
  [Claiming a prefix](#claiming-a-prefix), and `-o yaml` always shows the real
  resource for users who need it.
- **Allocation is not idempotent; the plugin makes retries safe.** Each claim
  consumes space, so a naively retried claim would consume a second block. The
  plugin lets the user supply a stable claim name (`--name`); a retried claim with
  the same name returns the existing allocation instead of consuming more (see
  [Claiming a prefix](#claiming-a-prefix)).
- **Single address family per resource.** IPv4 and IPv6 are never mixed in one
  pool or claim; the plugin infers family from the pool or an explicit `--family`,
  and dual-stack is simply two commands. This mirrors the service constraint.
- **Multi-tenancy is inherited, never re-implemented.** The plugin scopes every
  call to the active org/project from `datumctl` and surfaces the resolved scope;
  it does not add its own tenancy model. Cross-project claims against shared pools
  use the same explicit, RBAC-gated path the API defines.

### Risks and Mitigations

#### Risk: A friendly CLI hides irreversible or capacity-consuming actions

Making allocation a one-liner also makes it easy to consume a large block or
release space with a single command.

*Mitigations:* Mutations support `--dry-run` previews (Story 5). Releasing a pool
or prefix confirms by default, with the blast radius stated ("this releases N
prefixes across M namespaces"), and pool release requires typing the pool name;
`--yes`/`--force` bypasses for automation. Friction scales to blast radius — a
single prefix claim has none, a pool release has the most.

#### Risk: Output that looks human-friendly breaks scripts

Color, progress bars, and tables corrupt piped data if emitted unconditionally.

*Mitigations:* The plugin detects TTY vs pipe and disables color/animation when
not attached to a terminal, honors `NO_COLOR`, and treats `-o json|yaml` as a
stable contract with data on stdout and all diagnostics on stderr. Machine output
is never decorated.

#### Risk: The plugin becomes a second, divergent definition of IPAM behavior

A client that reimplements allocation logic, tenancy, or validation would drift
from the service and mislead users.

*Mitigations:* The plugin performs no allocation math and no authorization
decisions — it submits claims and renders responses. Exhaustion, overlap
prevention, strategy, and RBAC are decided server-side; the plugin only presents
them. `pool tree`/utilization render `status` fields the API already computes.

#### Risk: Credential and context confusion across tenants

A user could allocate in the wrong org/project without realizing it.

*Mitigations:* The plugin inherits the active context from `datumctl` (never its
own auth), echoes the resolved org/project in verbose output and in the success
line of a claim, and accepts `--project`/`--org` to override per-invocation with
the same precedence (flags > env > config) the rest of `datumctl` uses.

#### Risk: UX and security review

The plugin changes how users authenticate to and mutate IP space, and introduces
new terminal flows.

*Mitigations:* The credential-handling path (reuse of the `datumctl` credentials
helper, no long-lived token in the plugin) and the destructive-action
confirmations should be reviewed jointly by the CLI maintainers and a
security-minded reviewer; the claim, tree, and error flows should be validated
with real users — at minimum an engineer claiming a subnet, an operator auditing a
pool, and a pipeline author scripting against `-o json`.

## Design Details

This section describes the product experience: what users, operators, and scripts
see and do. It assumes the `datumctl` plugin contract and the catalog described in
the [marketplace enhancement][marketplace].

### Command surface

The plugin uses resource-oriented (noun-verb) grammar with a small, consistent
verb vocabulary reused across every noun, capped at three levels under
`datumctl`:

```console
# Pools (IPPool)
datumctl ipam pool create <name> [--cidr 10.0.0.0/8 | --parent <name> --prefix-length <n>] [--family ipv4|ipv6] [--min-length <n>] [--max-length <n>] [--strategy FirstFit|BestFit|LeastUtilized] [--visibility platform|consumer|shared] [--dry-run]
datumctl ipam pool list [--selector k=v] [-o table|wide|json|yaml|name]
datumctl ipam pool show <name>
datumctl ipam pool tree [<name>] [--prefixes]
datumctl ipam pool release <name> [--cascade] [--dry-run] [--yes]

# Prefixes (IPClaim / IPAllocation)
datumctl ipam prefix claim (--pool <name> | --selector k=v) (--length <n> | --cidr <cidr>) [--family ipv4|ipv6] [--name <n>] [--strategy FirstFit|BestFit|LeastUtilized] [--reclaim-policy Delete|Retain] [--dry-run]
datumctl ipam prefix list [--pool <name>] [-o table|wide|json|yaml|name]
datumctl ipam prefix show <cidr|name>
datumctl ipam prefix release <name> [--dry-run] [--yes]
```

Global flags apply to every command: `-o table|wide|json|yaml|name` selects the output format (table by default), `--quiet`, `--verbose`, `--color`, and `--yes`/`--force` behave as described below, and `--org`/`--project` override the active context. Two flags exist for development and end-to-end testing only — `--kubeconfig` to point at a dev/e2e cluster directly and `-n`/`--namespace` to target a specific namespace for claims and allocations — and are not part of the everyday, context-inheriting workflow.

Design choices and their rationale:

- **Noun-verb, not verb-noun.** IPAM has a small, bounded set of nouns (`pool`,
  `prefix`, later `address`, `asn`); grouping by the *thing* matches how users
  think ("do something to a pool") and lets the verb set stay consistent across
  nouns. (`kubectl`'s verb-noun form is the deliberate exception for tools with
  hundreds of dynamic resource kinds — IPAM is not that.)
- **One verb vocabulary everywhere.** `create`, `list`, `show`, `release` (plus
  the domain verb `claim`) mean the same thing for every noun, so learning one
  resource teaches the rest. Aliases (`ls`→`list`, `rm`→`release`) reward muscle
  memory; docs and help show one canonical spelling.
- **Positionals for the subject, flags for modifiers.** The primary object is
  positional (`pool show prod-backbone`, `prefix show 10.4.16.0/24`); everything
  else is a self-documenting flag.

### Identity and context

The plugin reuses the user's existing `datumctl` session: there is no second
login, and the plugin holds no long-lived credential of its own. Every call is
scoped to the active org/project, so a user sees only their own tenant — exactly
as the service enforces.

What this buys the user is one identity and one place context lives. The plugin echoes the resolved org/project (in the claim success line and under `--verbose`) so it is always clear which tenant an allocation lands in, and `--org`/`--project` override per invocation with the standard precedence (flags > env > config). Switching context is the same `datumctl` operation users already know. The underlying contract — how `datumctl` hands a plugin its context and brokers short-lived tokens — belongs to the [marketplace enhancement][marketplace]; this plugin simply consumes it.

#### Enabling IPAM for a project

IPAM is an opt-in service, so being logged in to a project is not the same as having IPAM turned on there. The first time the plugin runs an IPAM command in a project, it checks whether IPAM is enabled for that project. If it is, the command proceeds as normal. If it isn't and the user is at an interactive terminal, the plugin explains that IPAM isn't enabled and offers to request access on the spot, so the user never has to go hunting for a separate enablement step.

Because IPAM requires provider approval, requesting access typically results in a *pending* request rather than instant access — the plugin says so plainly, points the user at `datumctl services list` to check status, and leaves IPAM commands blocked until access is granted. In non-interactive use (scripts, CI) there is nothing to prompt, so the plugin returns an actionable error telling the operator how to request access rather than hanging on a question no one can answer.

### Claiming a prefix

Claiming is the defining IPAM action, and the plugin makes it a single command
that returns the allocated CIDR synchronously — exactly mirroring the API's
transactional create:

```console
$ datumctl ipam prefix claim --pool prod-backbone --length 24
✓ Claimed 10.4.16.0/24 from pool "prod-backbone"  (utilization 73% → 74%)
  prefix:      app-net-3
  allocation:  alloc-a1b2c3
  org/project: acme / net-core
```

- **The CIDR is the headline.** The one fact the user came for leads the output,
  with the resulting utilization in parentheses so they immediately see the cost
  of what they did.
- **Inputs are flexible.** `--length 24` claims by size; `--cidr 10.4.16.0/24` is a convenience that sets the requested prefix length and family from the CIDR — the server still chooses the actual block, since the API allocates by length and has no "pin this exact block" field. `--family`/`--strategy` are available but default sensibly (family inferred from the pool, strategy from the pool's configuration). The pool can be chosen by name (`--pool`) or by label (`--selector environment=staging,region=us-west`), matching the API's `poolRef`/`poolSelector` choice.
- **Retries are safe.** Allocation is not idempotent, so the plugin lets the user
  pass a stable `--name`. A retried claim with the same name returns the existing
  allocation rather than consuming a second block — turning an inherently unsafe
  retry into a safe one, which `--help` calls out explicitly.
- **Hierarchy in one step (roadmap).** `--child-pool <name>` is intended to set
  the API's `childPrefixTemplate`, atomically claiming a block *and* standing up a
  child pool over it in the same transaction, so a delegation that is two coupled
  concepts in YAML is one intent on the command line. **Note:** the `v1alpha1`
  `IPClaimSpec` does not yet expose `childPrefixTemplate`, so this flag is not
  deliverable today; the plugin rejects `--child-pool` with a clear usage error
  rather than silently dropping the intent, and the capability lands when the API
  field does.

Under the hood a claim creates an `IPClaim` and reads back
`status.allocatedCIDR`, `status.phase`, and `status.boundAllocationRef`; `-o yaml`
shows the real resource for anyone who wants it.

### Seeing the shape of your address space

Two views surface what `kubectl` hides, computed from the pool status the API already returns:

- **`pool list` makes utilization a column.** Utilization, the largest free block, and the address family are read from the server's reported pool status — which is what makes them correct for IPv6 pools and for child pools that inherit their family from a parent — with client-side capacity math used only as a fallback for older servers that don't report them. The plugin renders a utilization bar and percentage for humans and the underlying values under `-o json`. Raw capacity totals are shown for IPv4 but hidden for IPv6, where the address counts don't fit a counter and utilization plus largest-free are the meaningful summary. `-o wide` adds child-pool and active-prefix counts.
- **`pool tree` renders the hierarchy.** The plugin fetches the pools (and, optionally, the leaf prefixes) for the active project and lays out the `parentPoolRef` graph as an indented tree, annotating each pool node with its "% used" and each leaf with its consuming owner reference. This is the view that replaces manually chasing `parentPoolRef` links across a flat `kubectl get` list.

Both are read-only and safe to run anywhere; neither introduces server-side
state.

### Human-first output, script-friendly on demand

The plugin is built for a human at a terminal first and a script second, with the
machine path explicit and stable:

- **Default:** an aligned, color-coded table sized to the terminal. Color
  reinforces meaning but is never the *only* signal — utilization shows
  `98% (HIGH)`, status shows `BOUND`/`PENDING` as text, so meaning survives
  monochrome terminals, screen readers, and color-blind users.
- **`-o json|yaml`:** a stable, versioned contract for automation. Field names and
  shapes don't change without a deprecation path; data goes to stdout, all logs and
  progress go to stderr, so `... -o json > out.json` is always clean.
- **`-o wide` / `-o name` / `--quiet`:** progressive density — extra columns for
  humans who want them, bare identifiers for `xargs`/command substitution.
- **TTY awareness and `NO_COLOR`:** color and animation auto-disable when stdout is
  piped or `NO_COLOR` is set; `--color=auto|always|never` overrides.
- **Exit codes are a contract.** `0` on success; distinct, documented non-zero
  codes for distinct failure classes (notably a dedicated code for pool
  exhaustion, Story 4), and never `0` on partial failure of a bulk operation.

### Errors that name the fix

The signature IPAM failures get first-class treatment instead of being flattened
into generic Kubernetes errors. Every error states what happened, why, and a
concrete next action:

- **Exhaustion (HTTP 507):** names the requested length, the largest block that
  *is* available, current utilization, and a remediation command (Story 4), with a
  dedicated exit code so automation can branch on "pool full" specifically.
- **Overlap / conflict (HTTP 409):** names the conflicting CIDR and the claim that
  holds it.
- **No matching pool (selector/ref):** lists the pools that *do* match nearby
  labels, or notes that the named pool isn't visible in the active project.
- **Forbidden (HTTP 403):** distinguishes "not authorized" from "not found" and
  points at the cross-project sharing path when the pool exists but isn't shared.

Stack traces are suppressed by default and available under `--verbose`/`--debug`.

### Previewing and confirming mutations

Trust is built by making consequences visible before they happen and friction
proportional to blast radius:

- **`--dry-run` on every mutation.** A claim runs a real server-side dry run and shows the exact CIDR the server *would* allocate, with the projected after-utilization shown alongside as an estimate (Story 5); a pool release lists every child pool and prefix that would be freed. Nothing is consumed.
- **Confirmation scaled to risk.** A single `prefix claim` has no prompt and a
  clear success line. `prefix release` confirms by default. `pool release`, which
  can free many allocations, states the blast radius and requires typing the pool
  name. `--yes`/`--force` bypasses prompts for non-interactive use, and prompts are
  auto-suppressed when stdin is not a TTY or `CI` is set.
- **Cascade is explicit.** Releasing a pool that still has active prefixes fails
  with a clear message unless `--cascade` is given, mirroring the API's deletion
  protection rather than silently force-deleting.

### Discoverability: help, completion, and suggestions

- **Example-led help.** Every command answers `-h`/`--help` and bare invocation
  with a one-line description, a usage synopsis, and runnable examples — the
  terminal is the documentation.
- **"Did you mean?"** Unknown subcommands and flags suggest the nearest valid one.
- **Shell completion** for bash/zsh/fish/powershell, including *dynamic*
  completion of pool names and existing prefixes by querying the API — the single
  biggest defense against fat-fingering a long CIDR.
- **Progressive disclosure.** Top-level `ipam --help` lists the nouns; niche flags
  (allocation strategy, child-pool templating) live under the specific
  subcommand's help.

### Distribution through the plugin catalog

The plugin ships through the `datumctl` plugin catalog, so it inherits that
ecosystem wholesale: users install it with `datumctl plugin install ipam`, the
download is integrity-checked, it carries the catalog's **official** trust badge,
it versions on its own cadence independent of `datumctl` core releases, and users
who don't manage IP space simply never install it. The catalog format, install
flow, trust model, and version-compatibility checks are all defined by the
[marketplace enhancement][marketplace]; the IPAM plugin simply joins that
ecosystem rather than restating it.

### Roadmap surfaces: addresses and ASNs

The service's roadmap includes individual IP addresses and AS numbers. They slot
into the same grammar with zero new concepts for the user to learn, and reserving
their shape now keeps the experience coherent as the service grows:

```console
datumctl ipam address claim --prefix 10.4.16.0/24      # individual IP from a prefix
datumctl ipam asn claim --pool private-asns            # an ASN from a pool
```

When the corresponding API resources land, these become the natural extension of
the noun-verb surface already established for pools and prefixes.

## Production Readiness Review Questionnaire

The plugin is **entirely client-side**. It introduces no IPAM
control-plane components, no API types, and no server-side behavior, so the
cluster-oriented portions of the standard PRR questionnaire do not apply. The
relevant readiness considerations are captured below.

### Feature enablement and rollback

- The capability ships as an optional plugin installed via
  `datumctl plugin install ipam`. Users who never install it see no change.
- The plugin is purely additive over the existing API; `kubectl` and raw YAML
  continue to work unchanged for every workflow.
- Rolling back is uninstalling the plugin (`datumctl plugin remove ipam`) or
  pinning a prior version. Because the plugin holds no state of its own — all state
  lives in the IPAM service — removing it has no effect on existing pools, claims,
  or allocations.

### Monitoring and supportability

- Users can always confirm what the plugin did against the source of truth: every
  pool and prefix the plugin creates is a normal `ipam.miloapis.com/v1alpha1`
  object visible via `kubectl` and the plugin's own `list`/`show`/`tree` commands.
- `--verbose` surfaces the resolved org/project, the API host, and the exact API
  calls made, which is the primary support surface for "why did I get this
  result?"
- The plugin degrades gracefully: an unreachable API or an expired session
  produces a clear, actionable message (re-run `datumctl login`) rather than a
  stack trace.

### Dependencies

- The plugin depends on a working `datumctl` installation (for dispatch,
  credentials, and active context) and on reachability of the IPAM API for the
  active org/project. An IPAM outage affects the plugin exactly as it affects
  `kubectl` against the same API; the plugin adds no new dependency.

### Scalability

- The plugin issues the same API calls a user would make through `kubectl`
  (create/get/list/delete of pools and claims). It adds no new server load beyond
  what the equivalent manual workflow generates; `pool tree` is a bounded `list`
  over the active project's pools rendered client-side.

### Security

- The reuse of `datumctl` credentials and the destructive-action confirmations
  should receive a joint CLI + security review before the plugin stabilizes (see
  [Risks and Mitigations](#risks-and-mitigations)).

## Implementation History

- (provisional) Enhancement drafted, focusing on the product experience for a
  `datumctl` IPAM plugin: resource-oriented commands over the existing
  `ipam.miloapis.com/v1alpha1` API, inherited identity/context, utilization and
  tree views, human-first/script-friendly output, and catalog distribution.

## Drawbacks

- **It adds a surface to maintain.** A purpose-built CLI must track the IPAM API
  as it evolves (notably the addition of address and ASN resources). This is
  mitigated by keeping the plugin a thin presentation layer with no business logic
  to drift, and by shipping it on its own cadence through the catalog.
- **It introduces a second way to do things.** Some users will use the plugin and
  some raw `kubectl`/YAML, which can fragment documentation and muscle memory. This
  is mitigated by `-o yaml` always exposing the real resources (so the plugin is a
  bridge to, not a replacement for, the API) and by positioning the plugin as the
  friendly path for the common case, not a parallel universe.
- **Friendly mutation is still mutation.** Making allocation a one-liner lowers the
  effort to consume or release real address space. This is mitigated by `--dry-run`
  previews, blast-radius-scaled confirmations, and exit-code discipline, but it is
  a real change in how easy these actions become.

## Alternatives

- **Do nothing; keep using `kubectl`.** Zero new surface, but it leaves
  utilization and hierarchy invisible, every claim as hand-authored YAML, and the
  signature IPAM errors opaque — exactly the gaps the plugin closes. The raw API
  remains available as the power-user path regardless.
- **Build IPAM commands into `datumctl` core instead of a plugin.** This would put
  IPAM front and center but couples its release cadence to the core CLI, bloats the
  binary for users who don't manage IP space, and bypasses the catalog's trust and
  versioning model. The plugin path gets the same UX with independent evolution.
- **Ship a `kubectl` plugin (`kubectl ipam`) instead.** This would serve
  `kubectl`-native users but would not inherit `datumctl`'s identity, active
  org/project context, or the Datum plugin catalog — reintroducing the auth and
  context problems the `datumctl` plugin model solves for free.
- **Generate a generic CLI from the API (CRUD over every field).** A mechanical
  generator would cover the resources but produce exactly the experience we're
  trying to escape: no utilization view, no tree, no IPAM-aware errors, no
  `--dry-run` semantics for allocation. The value here is the IPAM-specific UX, not
  generic CRUD.
- **Wrap the API in shell aliases or a thin script.** Cheap to start, but it can't
  deliver completion, stable machine output, TTY-aware rendering, or the tree and
  utilization views, and it has no distribution or trust story. The catalog plugin
  provides all of these.

## Infrastructure Needed

- A repository and release pipeline that publishes the plugin to the `datumctl`
  plugin catalog (per the [marketplace enhancement][marketplace]), and an entry in
  the curated catalog so it installs via `datumctl plugin install ipam` with the
  **official** trust badge.

[marketplace]: https://github.com/datum-cloud/datumctl/blob/main/docs/proposals/datumctl-plugin-marketplace/README.md
