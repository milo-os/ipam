# Runbook: IPAMRetainedWithoutLeaseRising

**Alert:** `IPAMRetainedWithoutLeaseRising`
**Severity:** warning
**SLO:** Retention

## What this alert means

In one address family (the alert's `ip_family` label), the number of claim
allocations retained with no retention lease is more than a tenth, and at
least 50, above what it was 24 hours ago, and that has held for 6 hours. Each
family is evaluated on its own, so a fall in one cannot hide a rise in the
other. This is a capacity trend to look at in business hours, not a page:
some classes retain without a lease by design, and the threshold is set to
ignore their ordinary churn. A retained allocation with no lease is held
until someone deletes its IPAllocation or a claim of the same name takes it
back. Nothing releases it on a schedule. On a scarce range, such as public
IPv4, that capacity does not come back on its own.

`ipam_retained_allocations{lease="none"}` counts two kinds of row:

- retained under a class and pool that set no lease (`expires_at = 'infinity'`);
- retained before IPAM timestamped retention (`retained_at IS NULL`). No lease
  can be measured for these, so they never expire.

A claim overriding a Delete class to Retain is refused
(`RetentionLeaseRequired`) unless the class or pool sets a lease, but only when
the claim is created; the lease is resolved again when the claim is deleted.
New unleased rows therefore come from:

- a class whose own default is Retain, by design;
- claims created before that refusal shipped;
- the class's `retentionLease` or the pool's `maxRetentionLease` removed while
  a claim that relied on it was bound;
- the class deleted while its claims were bound: a missing class sets no
  lease, so only the pool's cap applies;
- a class whose default was Retain changed to Delete while claims that stated
  Retain were bound: they were never checked, and retain under the class's
  lease, which a class that relied on Retain usually does not set.

To tell which, compare the classes and pools the first query below returns
with their current `retentionLease`, `maxRetentionLease` and `reclaimPolicy`,
and check whether the class still exists.

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
2. **Which are claims that overrode their class?** A claim that set
   `reclaimPolicy: Retain` on a class whose default is Delete, created before
   that override needed a lease, is held for good: the class owner never chose
   to keep it. These are the rows to release first. The query lists every
   unleased row whose class does not default to Retain, including rows
   retained before IPAM timestamped retention and rows whose class is gone:
   ```sql
   SELECT a.allocation_key, a.pool_key, a.class_name, a.retained_at,
          CASE WHEN c.key IS NULL THEN '(class not found)'
               ELSE coalesce(convert_from(c.data, 'UTF8')::jsonb #>> '{spec,reclaimPolicy}', 'Delete')
          END AS class_default
     FROM ipam_cidr_allocations a
     LEFT JOIN ipam_objects c
       ON c.key = 'project/' || coalesce(nullif(a.class_project, ''), split_part(a.pool_key, '/', 2))
                  || '/ipam.miloapis.com/ipclasses/' || a.class_name
    WHERE a.claim_key IS NULL AND a.purpose = 'Claim'
      AND (a.expires_at = 'infinity' OR a.retained_at IS NULL)
      AND coalesce(convert_from(c.data, 'UTF8')::jsonb #>> '{spec,reclaimPolicy}', '') <> 'Retain'
    ORDER BY a.retained_at NULLS FIRST, a.allocation_key;
   ```
   A row that did not record its class's project is matched to a class in its
   pool's project, as the sweeper does. Two errors follow, both on rows written
   before migration 007. A pool provisioned under another project's class can
   show `(class not found)` for a class that exists. A class in the pool's
   project that is a reference (`spec.source`) shows `Delete`, though the class
   it refers to may retain by default.
3. **Who holds them?** The IPAllocations keep `spec.ownerRef`. In a project,
   this lists every retained allocation with no deadline, including those
   whose class retains by default:
   ```sh
   datumctl ipam allocation list --no-lease -o wide
   ```
4. **Is it one consumer creating and deleting Retain claims under new names?**
   That holds a new address per name and never reclaims one. Look for a high
   `ipam_retentions_total` rate with a low `ipam_reclaims_total{result="reclaimed"}`.

## Mitigate

- Set a lease. `spec.retentionLease` on the class, or `spec.maxRetentionLease`
  on the pool, which also caps classes that set none. Lowering a pool's cap
  applies to what the pool already retains. A class's lease applies from the
  next retention.
- Release what is not needed: `datumctl ipam allocation release <name>`. An
  allocation with no lease deadline can be released only by an IPAM Admin
  (permission `ipallocations.release-unleased`), or by deleting its namespace.
  Ask the owner first: an address held with no lease is often still in use.
- Once a lease applies, the sweeper releases the allocation on a pass shortly
  after `status.expiresAt`, usually the next; a pass skips a pool a live
  claim holds, so a release can wait for a later one. That holds in the default enforce mode; a
  deployment running `RETENTION_SWEEP_MODE=observe` shows the deadline and
  releases nothing.
