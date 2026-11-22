# scoperange-specific-address

Covers the explicit-placement half of milo-os/ipam#183: a `target: ScopeRange` claim that sets `spec.address` places its class's parent chain at the range it names, so an address that already exists can be adopted under a chained class. `claim-specific-address` covers Block claims naming an address.

Run it alone with `task e2e:suite SUITE=scoperange-specific-address`.

## Address plan

| Class | Pools | Range |
|---|---|---|
| `ssa-vpc` | one /48 per network and project, first /64 withheld | from `ssa-root`, fd83:185::/32 |
| `ssa-subnet` | one /64 per network, location and project, first /96 withheld | carved from `ssa-vpc` |
| `ssa-endpoint` | /96 Block claims | from `ssa-subnet` |
| `ssa-shared` | one /48 per network, shared by every project | from `ssa-shared-root`, fd83:186::/32 |

| Network | Placed by | VPC | Subnet |
|---|---|---|---|
| `ssa-net-a` | VPC range, then subnet range (the network-services-operator flow) | fd83:185:a::/48 | loc-1 fd83:185:a:10::/64 |
| `ssa-net-b` | one subnet range placing both levels | fd83:185:b::/48 | loc-1 fd83:185:b:20::/64 |
| `ssa-net-auto` | VPC range and endpoint without addresses | first free (fd83:185::/48) | first free |
| `ssa-net-r` | two racing VPC ranges, fd83:185:21::/48 and fd83:185:22::/48 | the winner's | – |
| `ssa-net-g` | an addressed endpoint refused `RangeUnplaced` first, then the subnet range it names | fd83:185:7::/48 | loc-1 fd83:185:7:10::/64 |

## What is asserted

- **Positive cases.** The claim's CIDR and the CIDR of every provisioned IPPool, compared with python `ipaddress`.
- **Refusals.** HTTP code, `status.reason` and the IPAM reason in `details.causes[0]`, then that no IPClaim was stored and that `ssa_snapshot` (every suite pool and its allocated capacity) is unchanged, which is what shows no pool and no carve was left behind.
- **Dry-run.** The response reports the range, and `ssa_snapshot` is unchanged, with and without an address.

## Cleanup

The `cleanup` block on `setup` deletes endpoint claims, then subnet ranges, then VPC ranges (a range with anything inside it refuses release), then any leftover provisioned pools, the roots and the classes.
