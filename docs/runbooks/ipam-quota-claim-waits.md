# Runbook: IPAMQuotaClaimWaitsHigh and IPAMQuotaClaimWaitGetsAfterGone

**Alerts:** `IPAMQuotaClaimWaitsHigh`, `IPAMQuotaClaimWaitGetsAfterGone`
**Severity:** warning (both, per apiserver pod)
**SLO:** claim latency (`IPAMQuotaClaimWaitsHigh` only)

## What these alerts mean

With `--enable-quota`, IPAM runs Milo's quota admission plugin in-process
(`cmd/ipam/admission.go`). A create of a quota-tracked IPAM resource creates a
ResourceClaim on the Milo control plane, then waits in admission until Milo
grants or denies it. Each open wait holds one watch on its claim, started from
the resourceVersion the claim's create returned. A wait ends when the claim is
judged (normally about a second), when the claim wait timeout runs out (30s by
default), or 4s before the request's own deadline, whichever comes first.

These are Milo's `MiloQuotaClaimWaitsHigh` and `MiloQuotaClaimWaitGetsAfterGone`
rules, scoped to `job="ipam-apiserver"`. Milo's own rules fire for Milo's API
servers; these fire for IPAM's. Milo's
[`quota-claim-waits.md`](https://github.com/milo-os/milo/blob/main/docs/runbooks/quota-claim-waits.md)
covers the plugin itself.

**IPAMQuotaClaimWaitsHigh** fires when one IPAM pod has held more than 100
open waits for 10 minutes. Nothing in admission caps concurrent waits, and
each costs the pod memory and goroutines (the rule's comment gives Milo's
measurement). Many open waits mean Milo is judging claims slowly, or a caller
creates in bulk faster than Milo judges.

**IPAMQuotaClaimWaitGetsAfterGone** fires when the claim watches from one IPAM
pod often answer `410 Gone`: Milo's claim cache no longer holds the version a
wait watches from, typically because a Milo API server started again. After a
410 the wait reads its claim, at most three times.

## Impact

Quota-tracked creates on IPAM are slow, and fail with "Your request took too
long to be checked against your quota" when a wait runs out. The wait happens
in admission, before the registry's Create, so it shows in
`apiserver_request_duration_seconds{verb="POST"}` but not in
`ipam_allocation_duration_seconds`: `IPAMClaimLatencyHigh` does not fire for it.

## Diagnose

1. **Is Milo judging claims?** On the Milo control plane, new claims should
   become Granted or Denied within seconds:
   ```sh
   kubectl get resourceclaims -A --sort-by=.metadata.creationTimestamp | tail
   ```
   Claims that stay Pending point at Milo's quota controllers; follow Milo's
   runbook from there.
2. **Who is creating?** `milo_quota_admission_waiter_registrations_total{job="ipam-apiserver"}`
   by pod, and `apiserver_request_total{job="ipam-apiserver",verb="POST"}` by
   resource, show whether one caller creates in bulk.
3. **For 410s: did a Milo claim cache start again?** Check
   `apiserver_watch_cache_initializations_total{resource="resourceclaims"}` on
   the Milo API servers around the alert's start. A Milo rollout starts every
   cache cold.

## Mitigate

- Slow judging is fixed on the Milo side; IPAM has no knob for it.
- A bulk caller can be slowed with API Priority and Fairness on the IPAM
  apiserver, or asked to spread its creates.
- 410s after a Milo restart stop by themselves once its caches are warm; the
  alert resolves when the watches stop answering 410.

## Metric notes

`milo_quota_admission_waiter_duration_seconds` changed in Milo v0.36.26: it
now starts after the ResourceClaim create returns, not before it, and its
buckets changed. Do not compare it across that upgrade.
