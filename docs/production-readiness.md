# IPAM Production Readiness Report

**Date:** 2026-05-10
**Scope:** IPAM aggregated apiserver (`ipam.miloapis.com/v1alpha1`) running on the Milo multi-tenant platform.
**Inputs:** Findings from four specialist analyses — allocation correctness, observability, e2e coverage, and load testing.

## Executive Summary

The IPAM service is structurally sound: allocation math is correct, the `SELECT ... FOR UPDATE` transaction pattern is faithfully implemented, multi-tenancy is enforced at the storage and registry layers, and the metric emission surface is broad and consistent. **However, the single most important pre-production check — that org A cannot interfere with org B — is not actually verified by any test suite.** The `multi-tenant` Chainsaw suite accepts both allow and deny outcomes for every cross-project step, so it will pass even if isolation is silently broken. Layered on top of that are three production-impact gaps: a stale annotation that causes the watch-lag alert to be silently routed away, no `up == 0` alert for pod crashes, and missing RBAC that will cause k6 in-cluster setup to fail mid-run. Address P0 items below before serving production traffic.

## P0 — Blockers

These can cause data loss, security violations, silent failures, or undetectable outages. **Fix before production traffic.**

| # | Area | Issue | Why it's a P0 |
|---|------|-------|---------------|
| P0-1 | e2e | `multi-tenant` suite asserts `(201 OR 400/422/403)` on every cross-project step | Suite passes whether tenant isolation is enforced or broken — the platform's most critical security property is unverified. Documented as a TODO inside the suite itself. |
| P0-2 | observability | `IPAMWatchLagHigh` alert carries stale `instrumentation: pending` annotation despite metric being live | Any Alertmanager route that suppresses `instrumentation=pending` silently drops a real alert. Watch lag will go unnoticed. |
| P0-3 | observability | No `up{job="ipam-apiserver"} == 0` alert | Pod crash is invisible until a derivative metric trips, which requires inbound traffic. Outage during low-traffic windows is undetectable. |
| P0-4 | load | k6 RBAC missing `rbac.authorization.k8s.io/clusterroles+clusterrolebindings` | `setup-pools.js` and `pool-exhaustion.js` create ClusterRoles for cross-project `use` grants. In-cluster runs 403 mid-setup and silently skip the shared-pool path, masking regressions. |
| P0-5 | observability | Runbook URLs point at `github.com/milo-os/ipam/...`; module path is `go.miloapis.com/ipam` | Operator clicking through an alert under fire hits a 404. Recovery time inflates exactly when it shouldn't. |

## P1 — Important

Degrade reliability, observability, or SLO accuracy but do not cause immediate data loss. **Fix before sustained production load.**

### Tests & load

- **`pool-scale.js` missing the O(1) ratio assertion.** Flat `p(95) < 2000` does not catch an O(N) regression — the `handleSummary` trend log alone will not fail the run.
- **`concurrent-claims.js` uses heuristic overlap detection** (507-free success rate as a proxy) instead of a hard CIDR-uniqueness check. ASN and IPAddress concurrent tests already do the strict check; prefix should match.
- **No in-cluster TestRun** wired for `concurrent-claims.js` or `cross-project-claim-throughput.js` — CI skips both.
- **No `watch-latency.js`.** The LISTEN/NOTIFY path has zero perf coverage; an event fan-out regression is undetectable from CI.
- **Five Chainsaw suites missing `finally:` cleanup** (`asn-allocation`, `prefix-exhaustion`, `prefix-overlap`, `prefix-validation`) — cluster-scoped objects leak between runs, causing flaky reruns.
- **No global Chainsaw `Configuration`** with retry/backoff. Per-suite timeout drift; 30s waits will flake under cold start.
- **`prefix-allocation` weak assertions** — does not verify `child.spec.cidr == parent.status.allocatedCIDR` membership, does not observe `Releasing` phase, no capacity-decremented assertion.

### Observability

- **Missing watcher-stuck alert** — `rate(ipam_watch_events_total[5m]) == 0` with a traffic guard would catch a wedged LISTEN/NOTIFY consumer.
- **Missing watcher-backlog alert** on `ipam_watcher_poll_batch_size` saturation against the 500-row limit.
- **`project` label on allocation counters** has no documented cardinality bound — risk of metric explosion under tenant churn.
- **`ipam_pool_utilization_ratio` lacks a tenant label** — cannot slice utilization by project or org.
- **No org-level label** anywhere — cross-project aggregation per org is impossible.
- **No staleness watchdog on the pgxpool background sampler** — if the goroutine dies, connection-pool gauges silently freeze.
- **`IPAMPoolExhaustionImminent` fires at >90%** but spec/brief says 80% — pick one and align.

## P2 — Nice to have

Improve before GA but not blocking.

### Metric & dashboard reconciliation

- Rename `ipam_allocation_attempts_total` + `ipam_allocation_failures_total` back to spec-aligned `ipam_allocation_total` (or update the spec — pick one source of truth).
- Add `ip_family` label to `ipam_allocation_duration_seconds` so IPv4 vs IPv6 latency is separable.
- Add absolute `ipam_pool_capacity_total` and `ipam_pool_allocated_total` gauges (currently only ratio is exposed).
- Reconcile dashboard names (`ipam-provider`/`ipam-consumer`) with spec (`ipam-overview`/`ipam-pool-utilization`/…) to prevent external doc cross-link rot.
- Add a dedicated panel for `ipam_watcher_poll_batch_size` saturation.
- Add a "start here" SRE entry-point runbook and an apiserver-down runbook.

### Tests

- **Replace `namespace: default`** in all 37 claim fixtures with Chainsaw per-test namespaces (`($namespace)`).
- **Parameterize multi-tenant suite proxy ports 18101–18106** (currently hardcoded — parallel runs collide).
- **Flip `pool-exhaustion.js` to `visibility: shared`** instead of `visibility: platform` (the TODO fallback exercises the wrong production semantics for the cross-project deny path).
- **Wire 1000-project scale TestRun** into in-cluster CI (currently only invokable via local Taskfile).
- **Expand ASN throughput pool** beyond 1023 ASNs or document the VUS≤10 cap.
- **Fix `CROSS_RATIO` doc/code mismatch** (comment says 0.3, code is 0.1).
- **Reconcile `prefix-hierarchy` step 5** with the requirements doc — currently tests deletion-protection (409) instead of cascade-delete.
- **Tighten `prefix-validation`** — error-class assertions are inconsistent across steps.

### Allocation & API

- **Document ASNClaim cross-project intent** — registry uses `LocalRef` only (no cross-project allocation). Likely intentional but undocumented.
- **Verify `/ipam.miloapis.com/asnclaims/**` watch exclusion** is applied symmetrically with the prefix-claims path.
- **Move IPAddressClaim `prefixRef` / `prefixSelector` nil-checks to the strategy layer** for parity with other claim types.
- **Confirm `IPPrefixClass` and `ASNPoolClass` deletions are safe** — deletion protection covers `IPPrefix`/`ASNPool` but not the class objects.
- **Investigate `MaxConns=10` pgxpool root cause.** It was set as a workaround for intermittent heap corruption under 4–8k req/s and caps achievable concurrency. Capacity planning must either fix or document the ceiling.

## Positive Findings

What's already production-quality and should not be regressed:

- **Allocation correctness.** Pure-Go `internal/allocation/` library compiles with stdlib only, math is correct, table-driven tests pass.
- **Transaction safety.** Pool-row `SELECT ... FOR UPDATE` (O(1) lock) is faithfully implemented; child prefix creation is atomic with the claim; HTTP 507 on exhaustion is wired.
- **Multi-tenant enforcement at storage/registry.** Tenant key prefixing is applied to every CRUD/Watch path; ownerRef is overwritten from `UserInfo.Extra` (not trusted from client); cross-project SubjectAccessReview gate is enforced.
- **Zero forbidden imports.** No `datum-cloud/milo` or `datum-cloud/quota` references.
- **Metric emission breadth.** Every claim Create handler emits attempt/failure/duration, with consistent defer+late-mutation pattern. ServiceMonitor and auth-delegator RBAC correct. Defensive clamping in `internal/metrics/`.
- **Runbook quality.** Existing runbooks have actionable kubectl/SQL commands — quality is high where they exist.
- **k6 multi-tenant fixtures.** 5-project topology with cross-project ClusterRoleBindings; deny-path latency tagged by `mode` (same-project vs cross-project); ASN and IPAddress concurrent tests have hard-zero duplicate guards.
- **Strong e2e suites.** `address-allocation` records freed IP and asserts reuse; `prefix-overlap` does CIDR format + uniqueness in a shell loop; `asn-allocation` and `asn-selector` enforce range + uniqueness with explicit FAIL/OK.
- **SLO threshold enforcement.** All three k6 acceptance-criteria thresholds are present with correct tags.

## Remediation Checklist

### P0 — Blockers
- [ ] **e2e:** Rewrite `test/e2e/multi-tenant/chainsaw-test.yaml` so cross-project steps assert deny-only (HTTP 4xx); remove the `(201 OR 4xx)` conditional accept.
- [ ] **observability:** Remove `instrumentation: pending` annotation from `IPAMWatchLagHigh` in `config/components/observability/`.
- [ ] **observability:** Add `up{job="ipam-apiserver"} == 0` alert with appropriate `for:` window and runbook link.
- [ ] **load:** Add `rbac.authorization.k8s.io/clusterroles+clusterrolebindings` (verbs: create, get, list, watch, delete) to `config/components/k6-performance-tests/rbac.yaml`.
- [ ] **observability:** Replace `github.com/milo-os/ipam/...` runbook URLs with the canonical `go.miloapis.com/ipam` mapping (or set up the redirect).

### P1 — Important
- [ ] **load:** Add an O(1) ratio assertion to `pool-scale.js` that fails the run on regression (don't rely on `handleSummary` logging).
- [ ] **load:** Replace `concurrent-claims.js` heuristic with hard CIDR-uniqueness assertion (mirror ASN/IPAddress tests).
- [ ] **load:** Add in-cluster TestRun YAMLs for `concurrent-claims.js` and `cross-project-claim-throughput.js`.
- [ ] **load:** Add `watch-latency.js` covering LISTEN/NOTIFY event fan-out under load.
- [ ] **e2e:** Add `finally:` cleanup blocks to `asn-allocation`, `prefix-exhaustion`, `prefix-overlap`, `prefix-validation`.
- [ ] **e2e:** Add a global Chainsaw `Configuration` with retry/backoff and standardized timeouts.
- [ ] **e2e:** Strengthen `prefix-allocation` — assert child CIDR within parent, child `spec.cidr == claim.status.allocatedCIDR`, observe `Releasing` phase, assert capacity decrement.
- [ ] **observability:** Add watcher-stuck alert on `rate(ipam_watch_events_total[5m]) == 0` with a traffic guard.
- [ ] **observability:** Add watcher-backlog alert on `ipam_watcher_poll_batch_size` saturation.
- [ ] **observability:** Document the `project` label cardinality bound (or move to `_info`-style metric).
- [ ] **observability:** Add tenant (`project`, `org`) label to `ipam_pool_utilization_ratio`.
- [ ] **observability:** Add `org` label across allocation/utilization metrics for org-level aggregation.
- [ ] **observability:** Add staleness watchdog on the pgxpool background sampler goroutine.
- [ ] **observability:** Reconcile `IPAMPoolExhaustionImminent` threshold to spec value (80%) or update the spec.

### P2 — Nice to have
- [ ] **observability:** Reconcile `ipam_allocation_attempts_total`/`_failures_total` naming with spec (`ipam_allocation_total`).
- [ ] **observability:** Add `ip_family` label to `ipam_allocation_duration_seconds`.
- [ ] **observability:** Add `ipam_pool_capacity_total` and `ipam_pool_allocated_total` gauges.
- [ ] **observability:** Reconcile dashboard names with spec (`ipam-overview`, `ipam-pool-utilization`, etc.).
- [ ] **observability:** Add dedicated `ipam_watcher_poll_batch_size` saturation panel.
- [ ] **observability:** Author "start here" SRE entry-point runbook.
- [ ] **observability:** Author apiserver-down runbook.
- [ ] **e2e:** Replace hardcoded `namespace: default` with Chainsaw per-test namespaces in all 37 fixtures.
- [ ] **e2e:** Parameterize multi-tenant proxy ports (18101–18106).
- [ ] **load:** Flip `pool-exhaustion.js` to `visibility: shared`.
- [ ] **load:** Wire 1000-project scale TestRun into in-cluster CI.
- [ ] **load:** Expand ASN throughput pool beyond 1023 ASNs (or document VUS≤10 cap).
- [ ] **load:** Fix `CROSS_RATIO` doc/code mismatch (0.3 vs 0.1).
- [ ] **e2e:** Reconcile `prefix-hierarchy` step 5 (deletion-protection vs cascade-delete) with requirements doc.
- [ ] **e2e:** Tighten `prefix-validation` error-class assertions for consistency.
- [ ] **allocation:** Document ASNClaim `LocalRef`-only intent (no cross-project allocation).
- [ ] **allocation:** Verify `/ipam.miloapis.com/asnclaims/**` watch exclusion is in place.
- [ ] **allocation:** Move IPAddressClaim `prefixRef`/`prefixSelector` nil-checks to strategy layer.
- [ ] **allocation:** Confirm `IPPrefixClass` and `ASNPoolClass` deletion safety; add deletion protection if needed.
- [ ] **allocation:** Investigate `MaxConns=10` pgxpool root cause; produce capacity-planning note.

---

## Second-Pass Audit — 2026-05-10

A second specialist sweep was performed across allocation/security/migration/watch, observability, e2e, and k6 load. Findings are appended below; original P0–P2 items above remain open unless explicitly closed.

### Verified clean (no new issues)

- **Core allocation, security, migration, and watch layers.** Two-phase Delete is correct; org labels are correctly sourced from `UserInfo.Extra`; API registration is complete; tenant isolation has no bypass vectors at the storage or registry layer; SQL migrations are idempotent with correct indexes; error handling is solid; watch/changelog is resilient under cursor advance and reconnect.

### New P0 — Blockers

| # | Area | Issue | Why it's a P0 |
|---|------|-------|---------------|
| P0-6 | observability | `IPAMPgxpoolMetricsStale` PromQL `time() - timestamp(ipam_pgxpool_total_connections) > 90` is broken | `timestamp()` returns the *scrape* timestamp, not the time of the app's last `.Set()`. A dead sampler goroutine with a frozen gauge value still gets a fresh scrape timestamp every cycle, so the expression evaluates to ~0 regardless of sampler health. **The alert does nothing — sampler death is undetected.** Fix: add a `ipam_pgxpool_sampler_last_run_seconds` gauge set by the sampler each tick, alert on `time() - ipam_pgxpool_sampler_last_run_seconds > 90`. |

### New P1 — Important

#### e2e — cleanup leaks that break cross-suite reruns

- **Bug A — `multi-tenant` `finally:` leaks 7 cluster-scoped resources.** `test/e2e/multi-tenant/chainsaw-test.yaml` `seed-classes-pools-rbac` creates `IPPrefixClass/mt-consumer-private`, `IPPrefixClass/mt-consumer-shared`, `IPPrefix/mt-alpha-pool`, `IPPrefix/mt-beta-pool`, `IPPrefix/mt-shared-pool`, `ClusterRole/mt-shared-pool-user`, `ClusterRoleBinding/mt-shared-pool-user-project-beta` — none are deleted in `finally:`. Re-runs fail with AlreadyExists.
- **Bug B — `prefix-hierarchy` `finally:` leaks `IPPrefix/hier-env` (10.128.0.0/9) and `IPPrefixClass/platform-shared`.** Cleanup deletes hier-region-1/region-2/leaf-claim but misses the /9 supernet and its class. The leaked /9 overlaps `prefix-allocation`'s 10.128.0.0/20, plus `prefix-selector` and `prefix-validation` pools — causing cross-suite failures in sequential runs after `prefix-hierarchy`.

#### k6 load — operator-facing infrastructure gaps

- **G1 — Taskfile missing entries for 3 new scripts** (`concurrent`, `cross-project-throughput`, `watch-latency`). Operators can't trigger them via `task k6:run TEST=...`.
- **G2 — `cleanup` task leaks shared/exhaust resources.** `perf-shared-prefix` IPPrefix, `perf-shared` IPPrefixClass, per-project `perf-prefix-N` pools, and ClusterRoles/Bindings for shared access are not deleted by `task cleanup`.

#### Observability — dashboard-join breakage from label inconsistency

- **`ipam_pool_utilization_ratio` is missing the `resource` label.** Its siblings `ipam_pool_capacity_total` and `ipam_pool_allocated_total` carry `resource`. Dashboard joins of the form `* on (pool_key, resource)` will fail. Fix: add `resource` label to `PoolUtilization` metric definition and all call sites.
- **`ipam_allocation_attempts_total` and `ipam_allocation_failures_total` are missing the `ip_family` label.** `ipam_allocation_duration_seconds` carries it but the counters do not. Dashboards can't break down attempt/failure rate by IP family. Fix: add `ip_family` to both counters and all call sites.

#### k6 load — coverage gaps with production-shaped risk

- **G4 — No IPv6 load test coverage.** Every script hardcodes `ipFamily: 'IPv4'`. IPv6-specific bugs (128-bit math, larger carve-outs) won't be caught.
- **G7 — `read-latency.js` cluster-list threshold of 2000ms is too loose.** A full table scan would pass. Tighten to 200ms like other list thresholds.

### New P2 — Nice to have

#### e2e quality

- **Quality E — `prefix-overlap` is not actually concurrent.** A single `create:` block serializes POSTs and does not exercise `SELECT FOR UPDATE` contention. It's a uniqueness test, not a concurrency test. Update the suite description to match (or restructure to be truly concurrent).
- **Quality F — `prefix-hierarchy` leaf CIDR-within-parent not asserted.** Step says "CIDR within regional block" but only checks `phase: Bound`. Add a `subnet_of` shell check like `prefix-allocation` does.
- **Quality G — `prefix-hierarchy` and `prefix-selector` `finally:` scripts are missing a `check:` block.** Cleanup failures are silent. Add `check: ($error == null): true`.

#### k6 load

- **G3 — `PROJECT_COUNT` not pinned in 9 of 11 cluster TestRun YAMLs.** If `setup.yaml` is patched with a different `PROJECT_COUNT`, consumer tests 404 on missing per-project pools.
- **G6 — `watch-latency.js` `ipam_watch_empty_responses < 3` is a fragile absolute count.** A single transient blip fails the run. Make it rate-based.
- **G8 — `pool-exhaustion.js` deny rate has no threshold.** `ipam_deny_rate` is recorded but not gated, so a partially-filled pool (deny rate < 1.0) passes silently. Add `rate>0.95`.
- **G9 — `ipam_success_latency_ms` not split by mode in exhaustion.** Cross-project vs same-project success latency are mixed; minor but limits diagnosability.

#### Forward-compat

- **D — ASN pool range overlap between `asn-allocation` and `asn-selector`.** Both use 4200000000-range overlapping pools. Not breaking today but would fail if overlap validation is added to ASNPool.

### Second-Pass Remediation Checklist

- [ ] **observability (P0):** Replace `IPAMPgxpoolMetricsStale` expression — add `ipam_pgxpool_sampler_last_run_seconds` gauge written by the sampler each tick, alert on `time() - ipam_pgxpool_sampler_last_run_seconds > 90`.
- [ ] **e2e (P1):** Extend `test/e2e/multi-tenant/chainsaw-test.yaml` `finally:` to delete the 7 leaked cluster-scoped resources (2 IPPrefixClasses, 3 IPPrefixes, ClusterRole, ClusterRoleBinding).
- [ ] **e2e (P1):** Extend `test/e2e/prefix-hierarchy/chainsaw-test.yaml` `finally:` to delete `IPPrefix/hier-env` and `IPPrefixClass/platform-shared`.
- [ ] **k6 (P1):** Add Taskfile entries for `concurrent`, `cross-project-throughput`, and `watch-latency` scripts so operators can run `task k6:run TEST=…`.
- [ ] **k6 (P1):** Extend the k6 `cleanup` task to delete `perf-shared-prefix`, `perf-shared` IPPrefixClass, per-project `perf-prefix-N` pools, and shared-access ClusterRoles/Bindings.
- [ ] **observability (P1):** Add `resource` label to `ipam_pool_utilization_ratio` metric definition and every call site.
- [ ] **observability (P1):** Add `ip_family` label to `ipam_allocation_attempts_total` and `ipam_allocation_failures_total` and every call site.
- [ ] **k6 (P1):** Add IPv6 coverage — duplicate at least one throughput/exhaustion script with `ipFamily: 'IPv6'`.
- [ ] **k6 (P1):** Tighten `read-latency.js` cluster-list threshold from 2000ms to 200ms.
- [ ] **e2e (P2):** Update `prefix-overlap` description to "uniqueness" (or restructure for genuine concurrency).
- [ ] **e2e (P2):** Add `subnet_of` shell assertion to `prefix-hierarchy` leaf step.
- [ ] **e2e (P2):** Add `check: ($error == null): true` to `prefix-hierarchy` and `prefix-selector` `finally:` scripts.
- [ ] **k6 (P2):** Pin `PROJECT_COUNT` in the 9 cluster TestRun YAMLs that omit it.
- [ ] **k6 (P2):** Convert `watch-latency.js` `ipam_watch_empty_responses < 3` to a rate-based threshold.
- [ ] **k6 (P2):** Add `rate>0.95` threshold on `ipam_deny_rate` in `pool-exhaustion.js`.
- [ ] **k6 (P2):** Split `ipam_success_latency_ms` by `mode` (same-project vs cross-project) in `pool-exhaustion.js`.
- [ ] **e2e (P2):** Resolve ASN range overlap between `asn-allocation` and `asn-selector` (preempts future overlap-validation work).

### Updated Overall Verdict

Core implementation is production-ready: allocation, security, migration, and watch layers passed a second independent pass with no findings. **Resolve the P0 metrics bug (`IPAMPgxpoolMetricsStale` is currently a no-op alert) and the P1 test-infrastructure gaps (e2e cleanup leaks, k6 Taskfile/cleanup gaps, dashboard-join label inconsistencies, missing IPv6 coverage, loose cluster-list threshold) before running sustained production load.** The remaining P2 items are quality/coverage improvements that should land before GA but do not block a controlled rollout.
