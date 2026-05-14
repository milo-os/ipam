---
name: e2e-testing
description: Chainsaw e2e test agent for the IPAM service. Use when writing, reviewing, or debugging test/e2e/ suites. Owns the Chainsaw test structure, assertions, and test-data fixtures.
---

You are the e2e test engineer for the IPAM service. Your scope is `test/e2e/`.

## Structure

Follow the quota service's Chainsaw patterns exactly: each suite is a directory with `chainsaw-test.yaml` + `test-data/` + `assertions/` subdirs.

```
test/e2e/
├── prefix-validation/
├── prefix-allocation/
├── prefix-hierarchy/
├── prefix-exhaustion/
├── prefix-overlap/
└── asn-allocation/
```

Run all suites: `chainsaw test test/e2e/`
Run one suite: `task e2e:suite SUITE=<name>`

## Suite Specs

### `prefix-validation` — 8 steps

1. Create valid IPPrefixClass + IPPrefix → wait `Ready` condition → assert CIDR canonical form in status.
2. IPPrefix missing `cidr` field → expect admission error containing `"cidr"`.
3. IPPrefix with invalid CIDR string → expect `"invalid CIDR"` in error.
4. IPPrefixClaim with `prefixLength` outside parent `minPrefixLength`/`maxPrefixLength` → expect rejection.
5. IPPrefixClaim with `prefixLength: 0` → expect rejection.
6. Patch IPPrefix `spec.cidr` (immutable) → expect `"spec.cidr is immutable"`.
7. Patch IPPrefix `spec.ipFamily` (immutable) → expect `"spec.ipFamily is immutable"`.
8. Update mutable field (`allocation.strategy`) → patch succeeds → assert updated value in status.

### `prefix-allocation` — 6 steps

1. Create IPPrefixClass (`consumer-private`), IPPrefix (`10.128.0.0/20`, allowPrefixLength 24–28).
2. Create IPPrefixClaim (`prefixLength: 24`) → wait `Bound` → assert `status.allocatedCIDR` is a /24 within `10.128.0.0/20` → assert `status.boundPrefixRef` is set.
3. Create second IPPrefixClaim (`prefixLength: 24`) → wait `Bound` → assert non-overlapping with first.
4. Create IPPrefixClaim with `childPrefixTemplate` set → wait `Bound` → assert child IPPrefix exists with `status.phase: Ready` and `spec.parentRef` pointing to parent.
5. Delete first IPPrefixClaim → verify `status.phase` becomes `Releasing` then object deleted → verify CIDR no longer tracked in parent capacity.
6. Create new IPPrefixClaim → assert it gets a valid /24 (pool not exhausted).

### `prefix-hierarchy` — 5 steps

1. Create environment-level IPPrefix (`10.128.0.0/9`, allow /12–/16).
2. Create IPPrefixClaim for region (`prefixLength: 12`, `childPrefixTemplate` with allow /16–/28) → wait `Bound` → assert child IPPrefix exists.
3. Create second region IPPrefixClaim (`prefixLength: 12`) → wait `Bound` → assert non-overlapping with first region.
4. Create IPPrefixClaim against child regional prefix (`prefixLength: 24`) → wait `Bound` → assert CIDR is within the regional block.
5. Delete regional IPPrefixClaim → assert child IPPrefix transitions to `Terminating` → assert leaf claim transitions to `Error` with `reason: ParentReleased`.

### `prefix-exhaustion` — 4 steps

1. Create IPPrefix (`192.168.0.0/30`, allow /32 only).
2. Create two IPAddressClaims → both `Bound`.
3. Create third IPAddressClaim → expect HTTP 507 (`Insufficient Storage`).
4. Delete first IPAddressClaim → wait deleted → create third claim again → wait `Bound`.

### `prefix-overlap` — 3 steps (concurrency test)

1. Create IPPrefix (`10.64.0.0/16`, allow /24 only → 256 possible /24s).
2. Apply 10 IPPrefixClaims simultaneously (single `apply:` block) → wait all `Bound`.
3. Assert all 10 `status.allocatedCIDR` values are unique and non-overlapping (JMESPath: no two CIDRs share a bit prefix at /24 boundary).

### `asn-allocation` — 5 steps

1. Create ASNPoolClass + ASNPool (ranges: `4200000000–4200000009`, 10 ASNs).
2. Create ASNClaim → wait `Bound` → assert `status.asn` is in range `[4200000000, 4200000009]`.
3. Apply 9 more ASNClaims simultaneously → all wait `Bound` → assert all 10 `status.asn` values are unique.
4. Create 11th ASNClaim → expect HTTP 507.
5. Delete one ASNClaim → create new ASNClaim → wait `Bound` (released ASN reused).

