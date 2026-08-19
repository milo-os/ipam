# nso-network-addressing

How network-services-operator consumes IPAM to address a tenant network, in the
layout production uses.

The other class suites each invent a root pool, prefix their class names so two
suites cannot collide, and define every class in the project that claims from
it. Production does none of those things: one set of definitions lives in a
platform project and every consumer project receives a reference to them. This
suite is built the second way, uses the platform's real class names and
`fd20::/20`, and asserts the consumption contract rather than the mechanism.

The chain it implements is the one in the tenant addressing design: `fd20::/20`,
a `/48` per VPC, a `/64` per region inside it, a `/96` per endpoint, with the
first address of each region subnet belonging to that subnet's router.

## What it asserts

| | |
|---|---|
| A network holds its range at creation, reported before any endpoint exists | `a-network-holds-its-range-before-any-endpoint-exists` |
| A repeated reconcile takes no second range | `a-repeated-reconcile-takes-no-second-range` |
| An endpoint address is inside the network's range | `an-endpoint-is-served-from-inside-the-network-range` |
| One network in two locations, two region subnets, one range | `one-network-in-two-locations-is-still-one-network` |
| Two networks, two ranges | `two-networks-get-different-ranges` |
| The router's address is never handed to an endpoint | `the-address-the-router-answers-on-is-never-handed-out` |
| Two projects naming one network do not share a range | `two-projects-naming-one-network-do-not-share-a-range` |
| A range holding a live address refuses to be released | `a-range-still-holding-a-live-address-is-not-given-back` |
| Releasing the network gives the range and its subnets back | `deleting-the-network-gives-the-range-back` |

Containment and disjointness go through `ipaddress` arithmetic in
`../lib/ipam.sh`. A prefix that merely starts with the right characters is not
inside anything.

## What it cannot prove, and says so

**Per-project isolation depends on the server.** A pool's identity is the class
plus the scope, and a scope carries only names. Unless the consuming project is
part of that identity, two projects that each call a network `default` are
handed one range, with no error and nothing to see. The declaration that fixes
this — a carving class stating whether each consuming project gets its own range
or they all share one — is not in every revision of the service.

So the setup step asks the server which revision it is, by offering it a
carving class with no such declaration and reading whether it is refused. Where
the declaration exists, the isolation step asserts the two projects' ranges are
disjoint. Where it does not, the step prints what actually happened and states
plainly that isolation was **not proven**. It does not restate the current
behaviour as though it were the guarantee.

That is why `test-data/platform-classes.yaml` carries `# POOL_PER_TENANCY`
marker lines instead of a literal declaration. The marker is where the
declaration goes; the setup step fills it in or removes it.

## Where it diverges from production, and why

**A third project.** The platform project is `datum-cloud`, matching production.
The two consumers are `project-alpha` and `project-beta`, which are the fixture
projects the e2e impersonation kubeconfig already provides. Production consumer
projects are real tenants.

**The consumer project receives a reference to the VPC class as well as the
endpoint class.** The infra change that defines these classes references only
the endpoint class into consumer projects, on the reasoning that a class's
ancestry resolves in the project holding the definition. That is true for the
cascade, but the operator's network path names the VPC class *by name* on a
claim it makes in the consumer project, and a claim can only name a class that
project can see. With only the endpoint reference, a network can hold no range.
The suite therefore installs both references. If the intent is for the operator
to reach the VPC class some other way, this suite is where that shows up.

**A grant, not a platform default.** `test-data/rbac.yaml` gives the e2e tenant
user `use` on the two referenced classes. Production grants this through the
platform's own roles. The suite is not a test of how that grant is issued.

**Locations and networks are names, not objects.** IPAM never resolves a scope
reference; it compares them. Nothing here creates a Network or a Location, and
nothing needs to.

## Running it

```
task e2e:suite SUITE=nso-network-addressing
```

`task e2e:tenant-setup` (a dependency of both e2e targets) generates the
impersonation kubeconfig this suite's three project contexts come from.
