# Runbook: IPAMPoolExhaustionImminent / IPAMPoolExhausted

**Alerts:** `IPAMPoolExhaustionImminent` (warning, > 90%), `IPAMPoolExhausted` (critical, ≥ 100%)
**SLO:** Address space utilization headroom

## What these alerts mean

A specific IPAM pool is running out of free address space. At >90%
(`IPAMPoolExhaustionImminent`) consumers are at risk of denials; at 100%
(`IPAMPoolExhausted`) all CREATE attempts against the pool are returning
HTTP 507 Insufficient Storage and customer impact is active.

The alert label set identifies the affected pool:
- `pool_key` — fully qualified pool identifier (`namespace/name` or similar)
- `ip_family` — `IPv4`, `IPv6`, or `N/A` (for ASN pools)

## Source signal

- `ipam_pool_utilization_ratio{pool_key, ip_family}` — gauge in [0,1]
- Provider dashboard → "Pool utilization" row

## Diagnose

1. **Identify the pool.** From the alert: `pool_key={{ $labels.pool_key }}`.
   In the cluster:
   ```bash
   kubectl get ipprefixpool {{ pool_key }} -o yaml      # IPv4/IPv6 prefix pools
   kubectl get ipaddresspool {{ pool_key }} -o yaml     # individual IP pools
   kubectl get asnpool      {{ pool_key }} -o yaml      # ASN pools
   ```
2. **Inspect outstanding claims.**
   ```bash
   kubectl get ipprefixclaim,ipaddressclaim,asnclaim \
     -A -o json \
     | jq '.items[] | select(.spec.poolRef.name == "{{ pool_key }}")'
   ```
3. **Check for stale claims.** Look for claims whose owning workload has
   been deleted or moved. Pool-cleanup of dangling claims is the fastest way
   to free capacity.
4. **Check for wide allocations.** A single `/24` claim against a `/16` pool
   eats 1/256 of capacity. Wide claims are common during initial bringup;
   they may need to be replaced with narrower claims.
5. **Confirm the metric.** The gauge is read back from the pool's committed
   `status.capacity` every 15s, so it matches `kubectl get ippool` within one
   interval. If it does not, the read is failing and the gauge holds its last
   values: look for `Failed to refresh pool gauges` in the apiserver logs.

## Mitigate

- **Expand the pool.** Edit the parent pool's `spec.cidrs` (or `asnRanges`)
  to add a new range. The allocator picks up new ranges automatically.
- **Reclaim stale claims.** Delete claim objects whose workloads are gone.
- **Split the pool.** If one pool serves multiple consumer classes,
  introduce a second pool and migrate one class to it.
- **Add a parent prefix.** Request a new range and add it to the pool's
  `spec.cidrs` (or `asnRanges`). The allocator picks up new ranges automatically.

## Customer communication

For `IPAMPoolExhausted` (critical), the workloads consuming this pool are
being denied with HTTP 507. Page the platform on-call and notify the consumer
team(s) directly using the pool's owning labels.

## Related

- `ipam-allocation-error-rate-high.md` — 507s show up here as well
- Provider dashboard → "Top 10 most utilized pools" panel
