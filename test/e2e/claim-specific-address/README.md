# claim-specific-address

Covers milo-os/ipam#183: an IPClaim that sets `spec.address` must be bound to exactly that address, or refused with a reason that says why. Before the fix the field was accepted and ignored, and the claim was bound to the next free block.

Run it alone with `task e2e:suite SUITE=claim-specific-address`.

## Address plan

| Pool | Range | Classes | Used for |
|---|---|---|---|
| `csa-v4` | 172.18.30.0/24 | `csa-host-v4` (/32), `csa-block-v4` (/28–/29) | exact bind, CIDR form, adoption, release, automatic claims, most refusals |
| `csa-two-a` | 172.18.31.0/24 | `csa-two` (/32) | one class offered by two pools |
| `csa-two-b` | 172.18.33.0/24 | `csa-two` (/32) | one class offered by two pools |
| `csa-rsv` | 172.18.32.0/28 | `csa-rsv-v4` (/32) | first address withheld (`reservations.leading: 1`) |
| `csa-v6` | fd83:183::/64 | `csa-host-v6` (/128, `uniqueWithin: [network]`) | the network-services-operator internal-DNS claim shape |
| `csa-range-root` | fd83:184::/64 | `csa-range` (`poolPer: [network]`) | ScopeRange regression check |

No other fixture uses these ranges (`go test ./test/fixtures/ -run TestFixtureRootRangesAreDisjoint`).

## What is asserted

- **Positive cases.** The bound block is compared with python `ipaddress`, never as a string, and is read from the CREATE response, so binding has to be synchronous. `status.address` is the bare address whenever the block is a single address, and empty for anything bigger.
- **Refusals.** Each one checks the HTTP code, the `status.reason` that code implies (409 means `Conflict`, never `AlreadyExists`), and the IPAM reason in `details.causes[0].reason`. It then checks that no IPClaim and no new IPAllocation was left behind. kubectl's rendered error does not show causes, so `expect_refusal`, `expect_invalid` and `expect_patch_invalid` in `../lib/ipam.sh` read the server's Status object from kubectl's `-v=9` transport log instead.

## Cleanup

IPClass and IPPool are cluster-scoped from the tenant's view and are applied by script, so chainsaw's own teardown never tries to delete them. The `cleanup` block on the `setup` step runs when the test ends, whichever step it ended on. It deletes the claims first, then releases any leftover allocations, then deletes the pools and the classes.
