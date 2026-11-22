# Runbook: IPAMRetainedWithoutLeaseRising

**Alert:** `IPAMRetainedWithoutLeaseRising`
**Severity:** warning
**SLO:** Retention

## What this alert means

More claim allocations are retained with no retention lease than 24 hours ago,
and that has held for 6 hours. A retained allocation with no lease is held
until someone deletes its IPAllocation or a claim of the same name takes it
back. Nothing releases it on a schedule. On a scarce range, such as public
IPv4, that capacity does not come back on its own.

`ipam_retained_allocations{lease="none"}` counts two kinds of row:

- retained under a class and pool that set no lease (`expires_at = 'infinity'`);
- retained before IPAM timestamped retention (`retained_at IS NULL`). No lease
  can be measured for these, so they never expire.

## Source signals

- `ipam_retained_allocations{lease="none"}`, by `ip_family`
- `ipam_retentions_total`: the rate at which claims are retained at all
- `ipam_retained_oldest_age_seconds`
- Provider dashboard → "Retention" row

## Diagnose

1. **Which classes and pools?**
   ```sql
   SELECT pool_key, class_name, class_project, count(*),
          min(retained_at) AS oldest, count(*) FILTER (WHERE retained_at IS NULL) AS untimestamped
     FROM ipam_cidr_allocations
    WHERE claim_key IS NULL AND purpose = 'Claim'
      AND (expires_at = 'infinity' OR retained_at IS NULL)
    GROUP BY 1, 2, 3
    ORDER BY 4 DESC
    LIMIT 20;
   ```
2. **Who holds them?** The IPAllocations keep `spec.ownerRef`:
   ```sh
   datumctl ipam allocation list --unclaimed
   ```
3. **Is it one consumer creating and deleting Retain claims under new names?**
   That holds a new address per name and never reclaims one. Look for a high
   `ipam_retentions_total` rate with a low `ipam_reclaims_total{result="reclaimed"}`.

## Mitigate

- Set a lease. `spec.retentionLease` on the class, or `spec.maxRetentionLease`
  on the pool, which also caps classes that set none. Lowering a pool's cap
  applies to what the pool already retains. A class's lease applies from the
  next retention.
- Release what is not needed: `datumctl ipam allocation release <name>`.
- Leases are not enforced until the sweeper runs in enforce mode
  (`RETENTION_SWEEP_MODE=enforce`). Until then a lease shows a deadline
  (`status.expiresAt`) but releases nothing.
