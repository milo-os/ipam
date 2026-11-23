# Runbook: IPAMRetainedSetUnread

**Alert:** `IPAMRetainedSetUnread`
**Severity:** warning
**SLO:** Retention

## What this alert means

No apiserver replica has read the retained set for more than three sweep
intervals, and at least five minutes. Every replica runs that read at the
start of each retention pass, whether or not it wins the sweep lock, and
publishes the `ipam_retained_*` gauges from it. When only that read fails,
the old gauges stay and the pass carries on, so passes still complete and
[`IPAMRetentionSweeperStalled`](ipam-retention-sweeper-stalled.md) stays
quiet. When passes stop too, as in a database outage, that alert fires and
this one stands down. A fleet that exports no read timestamp at all, such as
one rolled back to an image from before it, fires this alert as never read.

While no replica can read it:

- [`IPAMRetentionReleasesStalled`](ipam-retention-releases-stalled.md) drops
  every replica as stale and cannot fire;
- [`IPAMRetainedWithoutLeaseRising`](ipam-retained-without-lease-rising.md)
  sees frozen counts and cannot fire;
- the provider dashboard's "Retention" row shows the last values read.

The sweeper may still be releasing addresses: the read feeds only the gauges.
A `$value` of decades means no replica has read the set since it started.

## Source signals

- `ipam_retained_gauges_last_success_timestamp_seconds`, per replica; 0 until
  the first read succeeds
- `ipam_retention_sweep_errors_total`: counts each failed read, as well as
  failed passes and pool transactions
- `ipam_postgres_query_duration_seconds{query_name="retained_set_gauges"}`
- Logs: `Could not read the retained set; keeping the previous gauges`

## Diagnose

1. **Why does the read fail?** Read the error logged with
   `Could not read the retained set`. The read is one aggregate query over
   `ipam_cidr_allocations`. A timeout means the table or the database is
   slow, and a missing column means the schema is behind the binary.
2. **Is the database itself failing?** If the allocation alerts fire too,
   follow [`ipam-db-connection-pool-saturated.md`](ipam-db-connection-pool-saturated.md).
3. **Is the schema behind the binary?** A failed `migrate up` holds new pods
   in `Init:Error`, so look for a rollout that is stuck. Then compare the
   database's version with the binary's migrations:
   ```sh
   kubectl -n ipam-system exec deploy/ipam-apiserver -c apiserver -- /ipam migrate status
   ```
4. **What do the gauges hide?** Run the read by hand to see the current counts:
   ```sql
   SELECT ip_family,
          count(*) FILTER (WHERE retained_at IS NULL OR expires_at = 'infinity') AS no_lease,
          count(*) FILTER (WHERE retained_at IS NOT NULL AND expires_at <= now()) AS overdue,
          min(expires_at) FILTER (WHERE retained_at IS NOT NULL AND expires_at <= now()) AS oldest_deadline
     FROM ipam_cidr_allocations
    WHERE claim_key IS NULL AND purpose = 'Claim'
    GROUP BY ip_family;
   ```
   Rows overdue by more than a few intervals in enforce mode are the stall
   [`IPAMRetentionReleasesStalled`](ipam-retention-releases-stalled.md) would
   have raised. Follow it.

## Mitigate

- Fix the cause the log names. The next pass reads the set again and the
  alert resolves within a minute or two.
- If migrations are behind, roll the deployment so the `migrate` init
  container runs them.
