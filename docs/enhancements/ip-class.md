---
status: provisional
stage: alpha
latest-milestone: "v0.x"
---

# IPClass: a policy layer for claiming IP space

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [What it feels like](#what-it-feels-like)
  - [User Stories](#user-stories)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [What an IPClass looks like](#what-an-ipclass-looks-like)
  - [How pools offer capacity to a class](#how-pools-offer-capacity-to-a-class)
  - [How a consumer claims from a class](#how-a-consumer-claims-from-a-class)
  - [The default class](#the-default-class)
  - [The provisioner: designing for future address sources](#the-provisioner-designing-for-future-address-sources)
  - [What else it changes](#what-else-it-changes)
  - [Migration and backward compatibility](#migration-and-backward-compatibility)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)

## Summary

Today a consumer who wants IP space has to know how the platform carved it. An
`IPClaim` either names a specific `IPPool` or matches pools by label, so the
person asking for an address has to understand the platform's pool topology —
pool names, label conventions, which project owns which range. That knowledge
leaks into every consumer manifest and differs per environment, so a claim that
works in staging fails in production.

**`IPClass`** adds the missing layer of indirection: a platform-owned policy
object, the direct analog of a Kubernetes `StorageClass`. It names a *kind of
address space* and the rules for handing it out — which pools back it, how blocks
are placed, allowed prefix sizes, what happens on release. Consumers stop saying
"give me a /24 from `prod-backbone-us-east`" and say "give me a /24 of class
`public-egress`"; the platform decides, once and centrally, what that means and
where it comes from, and can change it without touching a consumer manifest.

The effect is to separate three concerns tangled together today: the claim
carries *intent*, the class carries *policy*, the pool carries *topology*.
Manifests become portable, the platform gets a real ownership boundary, quota and
utilization read in terms people actually use, and — because a class names a
*provisioner* — IPAM gains a clean seam to satisfy claims from external or cloud
address sources later, without changing the claim experience.

## Motivation

The claim experience works, but it exposes platform internals to consumers and
bakes environment-specific topology into resources that ought to be portable.

- **Topology leaks into consumer manifests.** Whether they select by label or by
  pool name, consumers have to encode decisions that belong to the platform team.
- **Manifests aren't portable.** A claim written against staging's pools doesn't
  move to production unchanged — there's no stable name for "the kind of address
  I want."
- **Policy is scattered and un-owned.** Strategy, prefix bounds, reclaim
  behavior, and family live partly on the pool and partly on each claim. No single
  object says how a kind of space is handed out.
- **No natural unit for governance.** Quota, access control, and utilization all
  want to reason about "egress space" as a first-class thing; today it exists only
  as a label convention.
- **No room to grow.** Every claim is satisfied by the platform's own pools —
  there's no seam for bring-your-own-IP, cloud, or an external system of record.

### Goals

- Give consumers a single, stable, environment-independent way to ask for
  address space: a class name.
- Move allocation policy (strategy, prefix bounds, reclaim policy, family,
  sharing) out of individual claims and pools and into a named, operator-owned
  object.
- Establish a clean platform/consumer boundary: consumers reference class names;
  only the platform sees pools and topology.
- Reserve a growth path (the provisioner) so future address sources can be added
  without changing how consumers claim.
- Remain fully backward compatible: existing claims keep working unchanged.

### Non-Goals

- **Deferred binding.** IPAM's defining property is synchronous, atomic
  allocation returned in the create response. A "wait until the workload lands
  somewhere before allocating" mode is explicitly out of scope (see Alternatives).
- **Dynamic pool creation.** Unlike `StorageClass` dynamic provisioning, a class
  does not create pools on demand. Pools remain pre-provisioned capacity.
- **Retiring pools or claims.** `IPPool` and `IPClaim` remain; only their
  relationship gains a layer.

## Proposal

Introduce `IPClass`, a platform-owned resource that names an allocation policy and
the provisioner that satisfies it. Pools offer their capacity by listing the
classes they'll back; claims select a class by name. When a claim names a class,
IPAM finds the pools offering it, applies the class's policy to place the
allocation, and returns the CIDR synchronously — the same fast, atomic experience
as today, with the pool choice made on the consumer's behalf.

Claiming by class name becomes the standard path. Label selection on the claim is
deprecated — that job moves into the class. Naming a pool directly survives as a
narrow, advanced escape hatch for "I need this exact pool" cases: migrations,
debugging, pinned infrastructure.

### What it feels like

The simplification shows up immediately at the command line. Today, claiming space
starts with a question the consumer shouldn't have to answer — *which pool?* —
and the answer is different in every environment, so the command doesn't travel:

```console
# Today: you have to know the pool, and its name differs per environment.
$ datumctl ipam prefix claim --pool prod-egress-us-east --length 26
```

With `IPClass`, the consumer names a *kind* of address space and the platform
decides the rest. The starting point is a catalog of what you're allowed to ask
for, described in plain policy terms:

```console
$ datumctl ipam class list
NAME            FAMILY   PREFIXES     RECLAIM   DEFAULT
internal-ipv4   IPv4     /24 – /28    Delete    *
public-egress   IPv4     /24 – /28    Retain

$ datumctl ipam prefix claim --class public-egress --length 26
✓ Claimed 203.0.113.0/26 from class "public-egress"  (utilization 61% → 62%)
  pool:         prod-egress-us-east
  org/project:  acme / app-team
```

The command names *what* the address is for, never *where* it comes from, so the
same command works in dev, staging, and production. Cross-project sharing looks
identical too: when a class is backed by another project's space, the consumer
never learns that project's identity or pool names — the platform's sharing rules
decide access.

```console
# Cross-project is invisible — same command, space drawn from shared capacity.
$ datumctl ipam prefix claim --class public-egress --length 26
```

### User Stories

- **As a platform operator**, I define `IPClass/public-egress` once — its
  placement, allowed sizes, reclaim policy, and backing pools — and every team
  consumes it by name without knowing those pools exist.
- **As a service developer**, I write `className: internal-ipv4` and the same
  manifest works in every environment, because each platform team has pointed that
  class at the right local pools.
- **As a platform operator**, I migrate egress space to a new pool by attaching it
  to the `public-egress` class and draining the old one — no consumer changes, no
  coordinated rollout.
- **As a service developer**, I claim from space shared by another project just by
  naming the class — I never learn that project's identity or pool names, and its
  sharing rules decide whether I'm allowed.
- **As a governance owner**, I set a per-project quota of "16 addresses of
  `public-egress`" and reason about consumption in terms consumers understand.
- **As a platform operator (future)**, I create `IPClass/aws-byoip` backed by the
  BYOIP flow, and claims of that class draw from bring-your-own ranges — using the
  same claim experience consumers already know.

### Notes/Constraints/Caveats

- A **default class** lets a claim omit the class entirely and still get sensible
  behavior, exactly as a default `StorageClass` does. At most one default may
  exist.
- The class is **policy and pointer only** — it carries no CIDRs and no
  environment-specific topology. That is what keeps it portable.
- A claim still resolves to exactly one pool, as today. A class may be backed by
  many pools; the class's placement strategy chooses among them deterministically,
  so repeated claims behave predictably.
- The class a claim used is fixed at creation and does not change afterward,
  matching the immutability consumers already expect from a claim's pool
  selection.

### Risks and Mitigations

- **Two ways to select a pool (class vs. naming a pool).** Mitigation: claiming
  by class is documented as the standard path; naming a pool is marked advanced;
  label selection is deprecated with a clear migration note. A claim may use only
  one of them.
- **A class points at no pools (misconfiguration).** Mitigation: the claim fails
  with a specific, actionable error — "no pool backs class X for this project" —
  the same style of error a label selector that matches nothing produces today.
- **A class is deleted while claims reference it.** Mitigation: existing
  allocations are recorded independently and are unaffected; deleting a class only
  affects *future* claims. Class deletion can optionally be blocked while claims
  still reference it.
- **The provisioner field invites premature complexity.** Mitigation: only the
  native platform provisioner ships and it is the default; a class naming an
  unimplemented provisioner is rejected until that provisioner is available.

## Design Details

### What an IPClass looks like

An `IPClass` is a platform-owned object that names a kind of address space and the
policy for allocating it. It carries no addresses of its own — only rules and a
pointer to how they are satisfied.

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPClass
metadata:
  name: public-egress
spec:
  # Which allocator satisfies claims of this class. Defaults to the platform's
  # native allocator. A reserved growth path for future address sources.
  provisioner: ipam.miloapis.com/native

  # Allocation policy, lifted off the pool and the claim:
  ipFamily: IPv4                       # a class is single-family
  strategy: LeastUtilized              # how a free block is chosen
  allowedPrefixLengths: { min: 24, max: 28 }
  defaultPrefixLength: 26              # used when a claim omits the size
  reclaimPolicy: Retain                # what happens to space on release

  # Who may consume this class, reusing the platform's existing sharing model:
  visibility: shared                   # platform | consumer | shared
```

The class is deliberately free of any pool list or CIDR. Pools offer themselves to
a class, not the other way around — which is what keeps the class portable across
environments.

### How pools offer capacity to a class

A pool advertises the classes it is willing to back. The pool owner — not the
class author — decides whether a given range serves `public-egress`. A pool may
back several classes, and a class may be backed by many pools.

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPPool
metadata:
  name: egress-us-east
spec:
  cidr: 203.0.113.0/24
  ipFamily: IPv4
  classNames: [ public-egress ]        # this pool's capacity backs this class
```

This is the same relationship a `PersistentVolume` has with a `StorageClass`,
rendered for pre-provisioned address capacity: the class is the named policy, and
concrete capacity opts in to serving it.

### How a consumer claims from a class

The consumer names a class and a size. Everything else — which pool, how the block
is placed, what happens on release — comes from the class.

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPClaim
metadata:
  name: my-service-egress
spec:
  className: public-egress
  prefixLength: 26          # optional; the class default applies if omitted
```

The response returns the allocated CIDR synchronously, exactly as today — but the
consumer never names a pool or sees the platform's topology. Naming a pool
directly remains available as an advanced escape hatch; label selection on the
claim is deprecated in favor of the class.

### The default class

A platform may mark one class as the default. A claim that names no class and no
pool is satisfied from the default class, so the simplest possible claim — "give
me a /26" — works out of the box. This mirrors the default `StorageClass`
experience and keeps small or single-pool deployments from having to think about
classes at all.

### The provisioner: designing for future address sources

Every class names a *provisioner* — the allocator that satisfies its claims. Only
the platform's native allocator ships initially, and it is the default. The field
exists now so that future address sources are **additive**, never a breaking
change to how consumers claim:

- **bring-your-own-IP** — claims backed by the BYOIP flow (BYOIP lives outside
  IPAM by design; the class is the clean point to plug it in).
- **cloud-provider address management** — hybrid or managed ranges from a cloud
  IPAM.
- **an external system of record** — an enterprise IPAM as the source of truth.

Each of these becomes a new class with a different provisioner. The consumer
experience — name a class, ask for a size, get a CIDR back — is identical
regardless of where the address actually comes from. No plugin ecosystem is built
now; only the growth path is reserved, and a class naming an unavailable
provisioner is rejected until that provisioner exists.

### What else it changes

A change this small at the surface has some pleasant knock-on effects deeper in
the platform.

The biggest is that it quiets an open scoping question. We'd been weighing whether
pools should be scoped per project to give consumers a cleaner way to address
space. `IPClass` mostly settles it: once consumers reference only class names,
pools become an internal detail they never touch. Pools can stay as they are,
per-project isolation keeps working, and the class layer sits above it all.

It also gives access control a boundary it lacks today. Consumers only need to see
the classes available to them and to create claims — no visibility into pools at
all, so pool names stop leaking through permissions. Because a class carries its
own visibility (private, offered to consumers, or shared across projects), the
sharing model that governs pools today governs classes too, cross-project
included.

And classes are the natural unit for governance and monitoring. Quota reads in
human terms — a budget against `public-egress`, not an opaque pool name — and
plugs into the platform's existing enforcement. Utilization can be reported per
class ("egress space is 78% consumed across its backing pools"), the view
operators actually want and can't easily assemble today; a class with no pool
behind it surfaces as a clear signal rather than a mysterious failure.

On the command line, beyond the claim flow above, the plugin gains a `class` noun
for discovery — list the catalog, read a class's policy — so consumers see what
they can ask for without touching a pool. The claim command keeps `--pool` as an
advanced path, deprecates `--selector` in favor of `--class`, and lets operators
group utilization and tree views by class.

### Migration and backward compatibility

The change is fully additive and requires no data migration:

1. Existing claims continue to resolve exactly as before — the class path is
   additive, not a replacement.
2. Classes, and a pool's list of backed classes, are new; their absence means
   today's behavior.
3. An operator adopts classes at their own pace: create classes, attach pools to
   them, optionally mark a default, then let consumers switch their claims to
   class names over time.
4. Label selection on claims is marked deprecated but not removed; naming a pool
   directly is retained indefinitely as the advanced escape hatch.

## Production Readiness Review Questionnaire

- **Feature enablement/rollback:** additive and off by default — no class means no
  behavior change. Rollback is removing the classes and the pools' class
  attachments; existing allocations are unaffected.
- **Observability:** per-class allocation counts, per-class utilization, and a
  distinct "no pool for class" failure signal. Existing per-pool signals are
  unchanged.
- **Scalability:** claiming by class adds a lookup and a scoped pool search to the
  existing allocation flow; it does not change the synchronous, atomic guarantee
  or the constant-time locking behavior that gives IPAM its throughput.
- **Dependencies:** none beyond the existing platform backend.

## Implementation History

- (pending) Provisional draft.

## Drawbacks

- It adds a resource and a concept. Small deployments with a single pool get no
  benefit and now have one more object type to understand — mitigated by the
  default-class path, which lets those users ignore classes entirely.
- Two selection mechanisms coexist during the deprecation window (class vs. label
  selection), a temporary cognitive cost.
- Server-side pool selection is less transparent than a consumer naming a pool
  directly; the per-class utilization views and clear, actionable errors are the
  mitigation.

## Alternatives

- **Per-project pools instead of a class layer.** Scoping pools per project was
  the other candidate for cleaner consumer-facing addressing. It solves less (only
  scoping — not policy centralization, portability, quota, or the growth path) and
  carries migration cost. `IPClass` achieves the same consumer-facing goal and
  more, at lower risk.
- **A pool selector on the class (the class points at pools).** Rejected: putting
  environment-specific pool labels on the class drags topology back into the
  object whose entire value is portability. Having pools offer themselves to a
  class keeps the class clean and gives capacity owners control.
- **Deferred, topology-aware binding.** Rejected as a non-goal: it contradicts
  IPAM's synchronous, atomic-allocation design and reintroduces the pending window
  the service was built to eliminate. If a topology-aware case ever arises, it
  belongs to a future provisioner, not the core experience.
- **A policy blob on the claim instead of a named class.** Rejected: an unnamed,
  per-claim policy gives none of the ownership boundary, portability, defaulting,
  or governance benefits that a named, operator-owned class provides.

## Infrastructure Needed

None. `IPClass` reuses the existing platform backend, allocation flow, and access
control.
