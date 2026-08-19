---
status: provisional
stage: alpha
latest-milestone: "v0.x"
tracking-issue: milo-os/ipam#114
---

# Pool Identity Must Key Off the Consuming Project

- [Summary](#summary)
- [Problem Statement](#problem-statement)
- [Current Behaviour](#current-behaviour)
  - [Where the identity is derived](#where-the-identity-is-derived)
  - [What the two projects actually share](#what-the-two-projects-actually-share)
  - [A second, worse outcome the same cause produces](#a-second-worse-outcome-the-same-cause-produces)
  - [Why nothing catches it](#why-nothing-catches-it)
- [What Sharing Actually Means](#what-sharing-actually-means)
- [Worked Example: A Public IPv4 Chain](#worked-example-a-public-ipv4-chain)
- [Proposal](#proposal)
  - [1. Pool identity carries owner and consumer](#1-pool-identity-carries-owner-and-consumer)
  - [2. Who the pools are for is a required answer in PoolPer](#2-who-the-pools-are-for-is-a-required-answer-in-poolper)
  - [3. The pool object stays in the defining project](#3-the-pool-object-stays-in-the-defining-project)
  - [4. The consumer is recorded on what the cascade writes](#4-the-consumer-is-recorded-on-what-the-cascade-writes)
- [API Changes](#api-changes)
- [Alternatives Considered](#alternatives-considered)
- [Migration and Compatibility](#migration-and-compatibility)
- [Test Plan](#test-plan)
- [Open Questions](#open-questions)

## Summary

A cascade-provisioned pool is identified by `(class name, scope digest)`, and the scope digest folds in the project that **defined** the class. For a platform class offered to every project, that project is the same for every caller, so two consumers whose claims carry the same scope reach one pool. Two projects that each call their network `default` therefore share one `/64`.

The fix has two halves. The mechanism: `scope.PoolDigest` must be able to carry the **consuming** project as well as the defining one — today it folds in only `class.Project`, and no per-class configuration can express a distinction the digest cannot represent. The declaration: a class already has a field that says how many pools it provisions and per what — `IPClassSpec.PoolPer` — and who the pools are for becomes a required answer in it. `poolPer: [location, allProjects]` means one pool per location that every project draws from; `poolPer: [location, project]` means one per location per project. A class that provisions pools and gives neither answer is refused, so "undeclared" is not a state a class can be in.

## Problem Statement

Two projects each reference the same platform `IPClass`. Each names a network `default`. Each claims an address.

Both claims resolve to the same provisioned pool: the same `/48`, and the same `/64` beneath it. Nothing fails, nothing warns, and (in the configuration the platform is being built with) the two addresses differ, so no duplicate is ever observed.

The promise the tenant addressing design rests on is one range per network. Here one range backs two tenants' networks:

- the `/64` maps to one routing domain, so the two tenants share that too;
- what one tenant holds constrains what the other can be given;
- the lifetime of the range is shared — reclaiming it for one tenant reclaims it for both;
- consumption cannot be attributed to either tenant.

Pool identity is not rewritable after the fact: a provisioned pool's name embeds its digest, the identity row is immutable by design, and the model promises subnets appear on first use and are **never renumbered** (`internal/scope/scope.go:47-51`). So this has to be fixed before the first real tenant address is handed out; afterwards it is a migration of live address space rather than a change of behaviour. Nothing is allocating yet.

## Current Behaviour

### Where the identity is derived

`PlanCascade` computes, for every provisioning class in a claim's ancestry, the pool that class must have for this claim's scope:

- `internal/allocator/cascade.go:83` — `project := class.Project`
- `internal/allocator/cascade.go:84` — `poolNameFor(project, class.Name, projected)`
- `internal/allocator/cascade.go:90` — `ScopeDigest: scope.PoolDigest(project, projected)`
- `internal/allocator/cascade.go:92` — `PoolKey: tenant.Identity{Name: project}.ResourceKey("ippools", name)`

`class.Project` is the project holding the class **definition**, not the caller's. `resolveClassIn` sets it that way deliberately when a claim reaches a class through a reference: `internal/allocator/class.go:113` returns `&ResolvedClass{IPClass: target, Project: src.Project}`, and the type's own comment says so (`internal/allocator/class.go:41-47`). The choice is stated as intentional at `internal/allocator/cascade.go:68-70`:

> Pools live in the project holding the class DEFINITION, not the claimant's. Two projects referencing one class must reach one pool; provisioning into the caller's project would give them separate address space under a shared name.

That paragraph is correct about *where the pool object lives* and wrong about *what identifies it*. Two projects referencing one class must reach one **class**; whether they reach one **pool** depends on what the class carves per, and the claim's scope refs are project-scoped objects that the pool digest does not qualify by project.

`scope.PoolDigest` (`internal/scope/scope.go:180-199`) takes a single `tenant` string and emits it once, before the role count (`internal/scope/scope.go:114-133`). Its documentation insists the tenant is folded in unconditionally because a pool lives in a tenant-prefixed key space — which is true, and is satisfied today by passing the defining project, because that is also the key prefix. Nothing in the package is wrong; it is being handed one project where the question needs two.

The identity row itself is `ipam_pool_identity (class_name, scope_digest) -> pool_key` (`migrations/002_class_model.sql:253-326`). Two consumers producing one digest for one class name means the second consumer's `INSERT ... ON CONFLICT DO NOTHING` loses, reads the first's `pool_key`, and allocates through it (`internal/allocator/cascade.go:304-335`).

### What the two projects actually share

The claiming project *is* available on this path and *is* used elsewhere. The claim registry pulls it from the request identity and passes it to the address-space digest:

- `internal/registry/ipam/ipclaim/storage.go:284` — `scope.ProjectAddressSpaceDigest(id.Name, claim.Spec.Scope, class.Spec.UniqueWithin, "uniqueWithin")`
- `internal/registry/ipam/ipclaim/storage.go:346` — `OwnerProject: id.Name` on the allocation row

`AddressSpaceDigest` (`internal/scope/scope.go:229`) qualifies **each ref** by the claiming tenant rather than emitting the tenant once, precisely so that "a network named `default` in project A is a different NETWORK from `default` in project B". The pool digest makes the opposite assumption about the same two refs. One digest treats the claim's scope as project-qualified and the other does not; that disagreement is the bug.

### A second, worse outcome the same cause produces

Because the two projects' claims land in one pool but in two *address spaces*, overlap between them is permitted rather than prevented. The search filter ignores `Claim` rows in other spaces (`internal/allocator/boundedsearch.go:110`) and the exclusion constraint is per space (`migrations/002_class_model.sql:525-533`).

So the observed "the addresses differ, so nothing fails" holds only for a class whose `uniqueWithin` is empty. For a class carving per network — `uniqueWithin: [network]`, which is what a per-network `/64` wants — two projects sharing one pool are handed **the same address**, silently, and both allocations are valid. Same root cause, worse failure, and reachable with the configuration the platform is being built with.

### Why nothing catches it

`ipam_pool_identity` has a `UNIQUE (pool_key)` constraint, and the schema explicitly disclaims it as a guard against exactly this (`migrations/002_class_model.sql:271-296`): two callers proposing different `pool_key`s for one `(class_name, scope_digest)` conflict on the primary key first, `DO NOTHING` suppresses the insert, and the unique index is never consulted. The invariant that pool identity is derived correctly lives in Go, not in the schema.

## What Sharing Actually Means

An earlier draft of this document implied that sharing a pool across projects is sound only when `uniqueWithin` is non-empty. That is backwards, and getting it backwards is how a design ends up preventing the safe case and permitting the dangerous one.

Every cascade pool is shared by many claims. The question is never "shared or not"; it is **shared across what** — which is exactly what `PoolPer` enumerates. Crossed with `UniqueWithin`, the combinations are:

| | `uniqueWithin: []` | `uniqueWithin: [network]` |
|---|---|---|
| **One pool per location, all consumers** | One address space. The `EXCLUDE` constraint keeps every consumer apart: no two claims can hold the same address, whoever they belong to. **The safest combination there is.** Public IPv4 is exactly this. | Per-consumer address spaces inside one pool. Two tenants may legitimately hold the same address. Correct only where that is genuinely intended — shared tenant endpoint IPv4, where the two addresses live in separate routing domains. |
| **One pool per location per consumer** | One address space per consumer, in a pool of their own. Costs address space in proportion to tenant count; buys attribution and independent lifetime. | Redundant: the pool split and the space split say the same thing twice. |

The schema documents the top-right case as deliberate (`migrations/002_class_model.sql:509-522`):

> `tenant-endpoint-ipv4` is uniqueWithin [network] over a /20 every network in the location shares: two networks both holding 10.128.0.2 out of one pool is the intended behaviour, not a bug. Constraining on pool_key alone would reject it, and IPv4 tenant space would exhaust at ~4000 addresses per location in total instead of per network.

So the danger in #114 is **not sharing**. It is sharing that was never *declared* — a class that says nothing about consumers getting one pool for all of them because that is what the digest happens to compute — plus the top-right variant, where undeclared sharing silently duplicates addresses across tenants who were promised isolation. A design that made every class per-consumer would fix the second failure by destroying the top-left case, multiplying scarce IPv4 consumption by the tenant count.

## Worked Example: A Public IPv4 Chain

Public IPv4 is the case the model must not break, and it is the clearest illustration of safe sharing. A location obtains an announceable block; instances in that location claim single unicast addresses out of it.

The root pool holds the RIR aggregate and offers it to the provisioning class. It lives in the platform project:

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPPool
metadata:
  name: public-ipv4-aggregate
spec:
  cidr: 198.18.0.0/16
  ipFamily: IPv4
  visibility: platform
  classNames:
    - public-ipv4-location
  allocation:
    minPrefixLength: 24
    maxPrefixLength: 24
    strategy: FirstFit
```

The provisioning class carves one announceable block per location:

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPClass
metadata:
  name: public-ipv4-location
spec:
  ipFamily: IPv4
  poolPer:
    - location
    - allProjects
  uniqueWithin: []
  allowedPrefixLengths:
    min: 24
    max: 24
  defaultPrefixLength: 24
  reservations:
    leading: 1
    trailing: 1
    unitPrefixLength: 32
  routing:
    internal: None
    external: Aggregate
  strategy: FirstFit
```

`allowedPrefixLengths` pins `/24` because that is the BGP-announceable floor: a longer prefix is filtered by most of the internet and an operator cannot originate it. `reservations` withholds the network and broadcast addresses of each provisioned `/24` — one `/32` at each edge, which becomes a real `Reservation` allocation held by the pool rather than an invisible hole. It is stated on the class rather than the pool because the pool does not exist until a claim conjures it.

The leaf class hands out single addresses:

```yaml
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPClass
metadata:
  name: public-ipv4-unicast
spec:
  ipFamily: IPv4
  parentClassName: public-ipv4-location
  uniqueWithin: []
  allowedPrefixLengths:
    min: 32
    max: 32
  defaultPrefixLength: 32
  routing:
    internal: Host
    external: None
  reclaimPolicy: Delete
```

The two `routing` blocks are the split `RoutingSpec`'s own documentation describes verbatim (`pkg/apis/ipam/v1alpha1/types.go:182-188`):

> RoutingSpec states advertisement inside a location and beyond it separately, because the two are frequently opposite: a per-instance address is a distinct route within its location and must never appear outside it — only the covering block leaves.
>
> An aggregate must be originated with a discard route. A class advertising an aggregate it cannot fully resolve blackholes the unallocated space inside it.

`internal: Host, external: None` on the leaf is the `/32` inside the location. `internal: None, external: Aggregate` on the provisioning class is the `/24` that leaves it. The discard-route requirement is why the `/24` must be the unit of provisioning: originating an aggregate means owning every address inside it, including the ones nobody has claimed.

**The flow.** An instance in `location: iad` claims `public-ipv4-unicast`. No pool exists for `iad` yet, so the cascade projects the claim's scope onto `public-ipv4-location`'s `poolPer` — `{location: iad}` — derives the pool identity, wins the `ipam_pool_identity` insert, and carves `198.18.0.0/24` out of the aggregate. Two `Reservation` allocations are placed at `198.18.0.0/32` and `198.18.0.255/32`. The claim then binds a `/32` from the new pool and the create returns it. Every later claim in `iad` — from any project — resolves the same identity row and draws from the same `/24`.

That `/24` is shared by every project with instances in `iad`, and that is the intended, safe arrangement: `uniqueWithin: []` puts all of them in one address space, so the exclusion constraint guarantees no two instances anywhere ever hold the same public address. Per-consumer `/24`s would be actively wrong here — a project with one instance would burn 256 announceable addresses, and the aggregate would exhaust after 256 projects rather than 256 locations.

**Why this constrains the fix.** `poolPer: [location, allProjects]` here means the **unqualified** location. Two projects claiming in `iad` must reach one pool. That works today only because the digest folds in the defining project, which is the same platform project for both callers — it works *by accident*, for the same reason the `/64` bug happens. The fix must keep it working on purpose: the digest has to distinguish owner from consumer so that a class can say the consumer is not part of its identity and still get one pool, rather than getting one pool because there was no way to ask for anything else.

## Proposal

### 1. Pool identity carries owner and consumer

Change the pool digest from one tenant field to two, and bump the encoding version:

```go
// PoolTenancy is the pair of projects a pool's identity depends on.
type PoolTenancy struct {
    // Owner holds the class DEFINITION. It keeps two classes that share a
    // name in different projects from colliding on
    // ipam_pool_identity(class_name, scope_digest).
    Owner string
    // Consumer is the project whose claim triggered provisioning. It is set
    // exactly when the class names the reserved `project` role in PoolPer,
    // and empty otherwise.
    Consumer string
}

func PoolDigest(t PoolTenancy, s map[string]ipam.ScopeRef) string
```

`canonicalPoolVersion` becomes `ipam.scope.v4` and emits `version, owner, consumer, roleCount, [role, apiGroup, kind, name]…`. Role groups keep their fixed arity of four, so v4 remains unparseable as v3's arity of five and every length-prefix unforgeability property carries over unchanged.

The consumer is a **top-level digest field, not a role group**. A role group carries a client-supplied `apiGroup` and `kind`; the consumer is a server-supplied fact from the request tenant, and it should not be encoded in a shape that has fields a client could vary. This is also what makes the reserved roles of the next section safe: the roles are the declaration, the tenancy field is the mechanism.

A struct rather than two strings is deliberate: the package already warns that "a call that passes an empty tenant to mean 'not applicable' is one refactor away from a call that passes an empty tenant to mean 'platform'" (`internal/scope/scope.go:258-266`), and two adjacent project-name arguments are the same hazard with a swap added.

`PlanCascade` obtains the consumer from `tenant.RequireTenant(ctx)` — `Require`, not `FromContext`, so an untenanted caller cannot provision into a shared identity by accident. It is already the value the claim path uses at `internal/registry/ipam/ipclaim/storage.go:284`, and it must be the same discriminator the storage key prefix uses (`tenant.Identity.Name` today, `KeyPrefix()` if the prefix ever distinguishes more than the name).

Keeping `Owner` is not optional. `ipam_pool_identity`'s primary key is `(class_name, scope_digest)`, and class names are project-scoped objects: two projects may each define a class called `tenant-ipv6`. Today the defining project inside the digest is what keeps them apart. Replacing it with the consuming project — the literal reading of the issue — makes one consumer's claims against two different classes of the same name collide on the primary key and merge into one pool. That would be a strictly worse version of this bug, so the fix adds a field rather than swapping one.

### 2. Who the pools are for is a required answer in PoolPer

No new field. `PoolPer` already means "one pool per distinct combination of these references" (`pkg/apis/ipam/v1alpha1/types.go:329-338`), which is precisely the axis in question. Two reserved role names are added to it, and a class that provisions pools must name exactly one:

- `project` — one pool per consuming project. Per-tenant `/48`s and `/64`s.
- `allProjects` — one pool that every consuming project draws from. The public IPv4 chain above.

So `poolPer: [location, allProjects]` is one pool per location for everyone, `poolPer: [location, project]` is one per location per project, `poolPer: [allProjects]` is one pool for the class, and `poolPer: [location]` is refused. Empty `poolPer` is unchanged and means the class provisions nothing at all — a leaf.

`allProjects` contributes nothing to the digest. It is a declaration, not an axis, and that is deliberate: writing it down must not renumber the pools it describes, so a class that says `allProjects` derives exactly the digest a class saying nothing would have. Its whole job is that saying nothing is no longer allowed.

**Why require rather than warn.** Requiring does not need the server to know which answer is right, and it cannot know: scope references are opaque `{apiGroup, kind, name}` strings, and telling a project-scoped kind from a platform one would need a table of kinds constraint #4 forbids. It only needs to refuse a class that gives no answer. The three facts that make a warning insufficient are all about the moment: `poolPer` is immutable and a pool is never renumbered, so a class that gets this wrong is replaced rather than corrected — which for a class already serving addresses is a renumbering; a warning does not stop the class being created; and the platform's own classes are being written now, in datum-cloud/infra#4111, so a warning would ship the bug into the platform with the fix in the engine.

Both answers stay available and sharing stays expressible, which is the constraint the public IPv4 chain sets. Nothing about the shared case gets harder — it gets one more line.

**The wrinkle, stated plainly.** Every other `PoolPer` role is projected from the claim's `spec.scope` by `scope.ProjectFor`, which fails a claim that does not supply it. Neither reserved role is a `ScopeRef` and neither ever will be: `project`'s value comes from the request tenant, and `allProjects` has no value at all. So both need server-side handling — `PlanCascade` sets `PoolTenancy.Consumer` when the class names `project`, and drops both before calling `scope.ProjectFor` on the remainder. That is special-casing, and it costs:

- `ProjectFor` must not look for either in the claim's scope, or every claim fails a missing-role check it cannot satisfy;
- `status.requiredScopeRoles` (`pkg/apis/ipam/v1alpha1/types.go:448-457`) must exclude both, since a client reading that field is being told what to put in `spec.scope`;
- validation must reserve both names in `uniqueWithin` and in a claim's `spec.scope`, or a class or a claim can collide with them.

It is defensible anyway. The consuming project is server-supplied and unspoofable — a claimant cannot name another project's pool by writing a scope ref, because the field is not read from the request body at all. It is two reserved names in one namespace of names the operator controls. And the alternative — a second spec field answering "how many pools?" in a second vocabulary — is a duplicate mechanism with a worse property: two fields that can disagree, where an operator reading `poolPer: [location]` cannot tell from that line how many pools the class actually has.

**Where the rule is enforced.** Twice, and the second is what makes it a guarantee rather than a convention. `validateDefinition` refuses the class at write time, which is where an author is told. `PlanCascade` refuses to plan a level whose class gives no answer, because validation runs on writes and a class stored before the rule was never subject to one — and `poolPer` being immutable means it cannot be brought into line by an update. Refusing its claims is unpleasant but bounded: migration 003 deletes every pool such a class provisioned, and aborts if any of them holds a live address, so the refusal can only ever meet a class nobody has allocated from.

Consistency with `uniqueWithin` is deliberately *not* offered. Both names are meaningful only for `poolPer`; `uniqueWithin` is already implicitly per-project, because `AddressSpaceDigest` qualifies each ref by the claiming tenant.

### 3. The pool object stays in the defining project

`level.PoolKey` keeps its `class.Project` prefix. The digest becomes strictly *finer* than the key prefix for a class naming `project` and exactly as fine for one that does not — never coarser, which is the direction `PoolDigest`'s documentation warns about (two spaces the storage layer keeps apart sharing one digest).

This keeps the blast radius to identity. Relocating pools into consumer projects is a separate change with its own consequences (see Alternatives), and it is not needed to fix the sharing.

### 4. The consumer is recorded on what the cascade writes

Once identity can depend on the consumer, the consumer has to be legible on the objects and rows the cascade produces — but only when it is part of the identity:

- **Pool name** (`poolNameFor`, `internal/allocator/cascade.go:414-428`): insert the consumer project after the class name for a class naming `project`, so `kubectl get ippools` distinguishes `tenant-ipv6-projx-default-<digest8>` from `tenant-ipv6-projy-default-<digest8>`. The existing 253-character truncation already covers the growth. A shared pool's name is unchanged.
- **Pool label**: add `ipam.miloapis.com/provisioned-for: <consumer project>` beside the two labels at `internal/allocator/cascade.go:377-380`, on per-consumer pools only, so teardown and operator queries can select a consumer's pools without decoding specs. Its absence is then meaningful: this pool is shared.
- **Carve row**: `poolCarve.OwnerProject` is currently `level.Class.Project` (`internal/allocator/cascade.go:352`). For a per-consumer level it becomes the consumer, so `owner_project` attributes carved space to the tenant that caused it. This is the column per-project consumption reporting reads, and what a quota model over addresses rather than claims (issue #108) would need.

## API Changes

**No new field.** `IPClassSpec.PoolPer` gains two reserved values, one of which a provisioning class must state, and a documentation paragraph; `IPClassStatus.RequiredScopeRoles` gains a sentence saying neither reserved role appears there. No change to `IPClaim`, `IPAllocation`, or `IPPool.spec`. No change to any consumer-facing request: the claiming project already arrives on the request and stays out of the API surface, so consumer refs remain opaque (constraint #4).

Validation, in `internal/registry/ipam/ipclass/strategy.go`: a non-empty `poolPer` must name exactly one of `project` and `allProjects`; both are rejected in `uniqueWithin`; `poolPer` remains immutable on update (`ValidateUpdate`), which is what keeps identity stable. In `internal/registry/ipam/ipclaim/strategy.go`: both names rejected in `spec.scope`. In `internal/allocator/cascade.go`: a class in a claim's ancestry that gives neither answer is refused rather than planned.

`internal/allocation/` is untouched and stays standard-library-only (constraint #5). `internal/scope` keeps importing nothing but the API types.

## Alternatives Considered

**A `poolSharing: Shared | PerConsumer` enum on `IPClassSpec`** — the shape of the first draft of this document. Rejected. It answers "how many pools does this class provision?" in a second vocabulary alongside `PoolPer`, which already answers it; an operator reading `poolPer: [location]` would have to read a second field to know whether that means one pool per location or one per location per tenant, and the two can be written to disagree. "Shared" is also the wrong word: every cascade pool is shared by many claims, so the term names no distinction. The real question is *shared across what*, and `PoolPer` is the field that enumerates exactly that.

**Swap the defining project for the claiming project in the digest** (the issue's literal suggestion). Rejected on two counts. It drops the only thing separating two same-named classes in different defining projects inside `ipam_pool_identity`'s primary key, merging their pools for a consumer that references both. And it makes every class per-consumer, which turns the public IPv4 `/24` per location into a `/24` per project per location.

**Qualify each pool-scope ref by the claiming project, mirroring `AddressSpaceDigest` v3.** Elegant, and wrong for the same input the address-space form gets away with: it assumes every ref names an object in the claimant's project. `network` does; `location` does not. A class carving per location would silently split per project — the same class of error in the other direction, and the one the public IPv4 example shows is unaffordable.

**Give `ScopeRef` an explicit `project` field** and qualify both digests by it. This is the shape `internal/scope/scope.go:224-226` already anticipates, and it is the honest long-term model. Rejected for now: it puts a security-relevant qualifier in the claimant's hands (the server would have to overwrite or reject it, at which point it carries no information the request did not already have) and it changes every client. Revisit if cross-project scope refs ever become real.

**Provision per-consumer pools into the consumer's project key space.** Attractive for visibility: today a tenant cannot see the pool that holds its own `/48`. Rejected as part of this fix because a pool the consumer can see is a pool the consumer can delete, and `ipam_pool_identity`'s foreign key is `ON DELETE CASCADE` (`migrations/002_class_model.sql:318-323`) — deleting the pool retires the identity and the next claim provisions a fresh range, which is renumbering, the one thing the model forbids. Worth doing deliberately, separately, with a delete guard.

**Warn at class-write time instead of refusing.** The shape an earlier draft of this document resolved on. Rejected. `poolPer` is immutable, so a class created despite the warning can never be corrected — it is replaced, which for a class already serving addresses is a renumbering. A warning does not stop the creation, and the classes this has to protect are being written right now in datum-cloud/infra#4111, which would ship the bug into the platform with the fix in the engine. The objection to refusing was that the server cannot tell which answer is right; it does not have to, because it refuses only the class that gives none.

**Detect and refuse instead of fixing identity** — reject a claim whose resolved pool was provisioned for a different project. Rejected: it converts a design gap into an outage for whichever tenant claims second, and leaves the first holding space the model says is not theirs alone.

## Migration and Compatibility

Every cascade-provisioned pool's digest changes, because the canonical form gains a field whether or not the class names `project`. The digest is embedded in the pool's name, in `ipam_pool_identity.scope_digest`, in `IPPool.status.scopeDigest`, and in the `scope_digest` of the `PoolCarve` row the pool left against its parent. A digest is a SHA-256 over a string the schema does not store, so there is no backfill — only a reset (`internal/scope/scope.go:36-43`).

That is acceptable exactly because nothing is allocating yet, and it is the reason the issue puts a deadline on the fix.

Proposed `migrations/003_pool_identity_consumer.sql`:

1. Abort with a clear message if any `ipam_cidr_allocations` row with `purpose = 'Claim'` exists against a pool named by `ipam_pool_identity` — i.e. if any real tenant address has been handed out. At that point the reset is not safe and a human has to decide.
2. Otherwise delete the `PoolCarve` rows for those pools, the pool objects, and (by cascade) the identity rows, the consumption rows, and the search floors.
3. Leave operator-authored pools untouched: they are identified by their own names and are never provisioned by a class.

**Existing `IPClass` objects must be replaced, not edited.** A provisioning class that names neither reserved role is refused from here on: at write time, and — because `poolPer` is immutable and a stored class was never validated against a rule that did not exist — when planning a cascade for a claim that reaches it. So a class written before this change keeps serving nothing rather than keeping the bug.

That is affordable at exactly this moment and no other. The migration above deletes every pool a class provisioned and aborts if any of them holds a live address, so a class that has to be replaced has nothing allocated out of it to lose. The classes in the platform's definitions are the ones this applies to, and they are not serving tenants yet.

`allProjects` is the answer for anything whose behaviour should not change: it derives the same digest a silent class did, so declaring sharing is a one-line edit that renumbers nothing — the edit just has to be made as a replacement, since `poolPer` cannot be updated in place.

## Test Plan

**`internal/scope` (pure unit).**

- Golden canonical v4 form, alongside the existing v2/v3 goldens (`scope_test.go:17,40`).
- `PoolDigest` separates two consumers with identical refs; collapses them when `Consumer` is empty.
- `PoolDigest` still separates two owners with identical refs and no scope — the `ipam_pool_identity` primary-key case (extends `TestPoolDigestSeparatesTenantsWithNoScope`, `scope_test.go:541`).
- v4 pool digests never equal a v3 address-space digest or a v2 pool digest over any input (extends `TestPoolAndAddressSpaceDigestsNeverCollide`, `scope_test.go:614`).
- Forgery: no owner or consumer name can be constructed that reproduces another `(owner, consumer, scope)` triple (extends `TestTenantCannotBeForged`, `scope_test.go:691`).
- `ProjectFor` ignores the reserved `project` role and does not report it missing.

**`internal/allocator` (postgres).**

- Two projects referencing one class with `poolPer: [network, project]`, both claiming with `network: default`, get two pools with disjoint CIDRs — the issue, as a regression test.
- The same two projects against `poolPer: [location, allProjects]` get one pool — the public IPv4 case, as a regression test against over-correcting.
- A class in the ancestry naming neither reserved role provisions nothing and leaves no identity row behind — the stored-class case validation cannot reach.
- A herd of first claims from *two* projects against a per-consumer class produces exactly two pools with no lost-race errors (extends `TestAHerdOfFirstClaimsProducesOnePool`, `cascade_postgres_test.go:75`).
- Two classes with the same name in two defining projects, claimed by one consumer, get two pools — the primary-key collision the `Owner` field prevents.
- `ResolveExistingPool` (dry-run) reports the same pool and the same pending levels as `ResolvePool` for both shapes.
- The `PoolCarve` row for a per-consumer level carries the consumer in `owner_project`; a shared level carries the owner.

**`internal/registry/ipam` (validation).**

- `ipclass`: a non-empty `poolPer` naming neither reserved role rejected, and naming both rejected; either one alone accepted; both rejected in `uniqueWithin`; `poolPer` still immutable; `requiredScopeRoles` omits both.
- `ipclaim`: both reserved names rejected in `spec.scope`.

**Chainsaw e2e (`test/e2e/`).**

- New suite `class-consumer-isolation`: two projects, each referencing one platform class with `poolPer: [network, project]`, each with a network named `default`; assert the two `IPAllocation`s report different pool names and non-overlapping CIDRs.
- New suite `class-shared-pool`: the public IPv4 chain, two projects claiming in one location; assert both allocations name one pool, that the `/24` is announceable-sized, that the network and broadcast `/32`s are held as `Reservation`s, and that no two claims share an address.
- Both belong beside `class-cascade-concurrency` and `tenant-isolation`, which already carry the multi-project fixtures.

**Load (`test/load/`).** No new script. The existing throughput script should be re-run with claims spread across several projects, since per-consumer identity changes which identity row a herd contends on — contention should fall, not rise.

## Open Questions

1. **Are `project` and `allProjects` the right reserved names?** `project` is the tenant kind `tenant.Identity` carries today and is what an operator would write; `consumer` and `tenant` are the alternatives. `allProjects` is chosen to pair with it so the either/or is obvious on the line; `sharedAcrossProjects` says the same thing at more length. Pick before any class ships — both names are immutable in practice, since the field is.
2. **Should `project` be a qualified tenant instead?** `tenant.Identity` carries a kind as well as a name, and `KeyPrefix()` currently writes `project/` for both. A class that wants one pool per *organization* has no way to say so. If organizations become a real tenancy level, this becomes two reserved roles rather than one.
3. ~~**How loud should the omission be?**~~ **Resolved: there is no omission.** A class that provisions pools names `project` or `allProjects`, and one that names neither is refused — at write time, and again when planning a cascade, so a class stored before the rule cannot fall through to the shared identity either. The objection that the server cannot tell which answer is right stands and does not apply: it refuses only the class that gives no answer, and both answers remain available.
4. **Where does the consumer belong on the pool object** — the label proposed here, a `status.consumerProject`, or a first-class `spec` field?
5. **Should per-consumer pools eventually live in the consumer's project?** It is the only way a tenant sees the range holding its own addresses. It needs a delete guard first.
6. **Does `IPClass.status` need to report the effective `poolPer`** for a project that holds only a reference and cannot read the definition? `requiredScopeRoles` already leaks part of it.
