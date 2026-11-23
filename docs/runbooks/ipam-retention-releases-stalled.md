# Runbook: IPAMRetentionReleasesStalled

**Alert:** `IPAMRetentionReleasesStalled` (per `ip_family`)
**Severity:** warning
**SLO:** Retention

## What this alert means

The retention sweeper is in enforce mode and its passes complete. Even so,
retained allocations of this family are **late**: more than five sweep
intervals past their lease deadline. Their number has not fallen: the
lowest late count of the last 15 minutes is no lower than the lowest of the
15 minutes before. That has held for 30 minutes, so in practice rows have
been late for about 45 minutes before it fires.

A healthy enforcing sweep releases a row within about one interval of its
deadline, so the late count is zero however many rows fall due. Here, due
rows are being left held while the sweeper reports that it is working. Their
addresses do not go back to their pools.

Two sibling alerts cover the other ways releases stop.
[`IPAMRetentionSweeperStalled`](ipam-retention-sweeper-stalled.md) fires when
passes stop completing, and this alert is held off while that one's condition
holds. [`IPAMRetentionRowsStuck`](ipam-retention-rows-stuck.md) fires when
the sweeper counts rows it failed to act on; the two can fire together.

This alert does not fire for a backlog that is draining. After a switch from
observe to enforce, rows overdue by days are released at the pass's row
budget (2000 rows), oldest deadline first, one interval apart. The late count
falls on every pass, so the alert holds off however large the backlog is. If
rows are left behind once the drain ends, it fires about 45 minutes later.

It never fires in observe mode, where due rows are counted and held by
design, or when the sweeper is disabled.

### Limits

- **Rollouts.** Any replica that wins the sweep lock runs the pass in its own
  mode, and the alert is armed while any replica enforces. A switch to
  enforce whose rollout leaves an observe replica winning the lock for more
  than about 30 minutes can fire it. Finish the rollout; the alert clears
  as the backlog drains.
- **Replicas whose read fails.** Each replica reads the retained set at the
  start of every pass. If the read fails, the replica keeps its previous
  gauges and its pass still completes; the failure is counted in
  `ipam_retention_sweep_errors_total` and logged as `Could not read the
  retained set; keeping the previous gauges`. The alert ignores a replica
  whose `ipam_retained_gauges_last_success_timestamp_seconds` is more than
  three intervals old (five minutes at least), so a frozen replica neither
  fires it nor hides it. If every replica's read fails, the alert has no
  data and stays silent, and
  [`IPAMRetainedSetUnread`](ipam-retained-set-unread.md) fires instead.
- **Long intervals.** The two 15-minute windows each need a pass in them, so
  the alert is meant for sweep intervals under 15 minutes. The default is 1m.

## Source signals

- `ipam_retained_late_allocations{ip_family}`: retained allocations more
  than five sweep intervals past their deadline. The alert is computed from
  this count.
- `ipam_retained_oldest_overdue_seconds{ip_family}`: how long the most
  overdue retained allocation has been past its deadline, by the database
  clock; 0 when none is due. It is the alert's value.
- `ipam_retained_overdue_allocations{ip_family}`: all retained allocations
  past a finite deadline, which the next enforcing pass should release. It
  churns as rows fall due between passes, so the alert does not use it.
- `ipam_retained_gauges_last_success_timestamp_seconds`: per replica, when
  its read of the retained set last succeeded
- `ipam_retention_sweep_enforcing`: per replica, 1 in enforce mode, 0 in
  observe mode or when disabled
- `ipam_retention_sweep_interval_seconds`: the configured interval
- `ipam_retention_releases_total{mode="enforce"}`: releases made
- `ipam_retention_sweep_pools_skipped_total`: pools a pass left because a
  live claim held their lock
- `ipam_retention_sweep_row_errors_total`, `ipam_retention_sweep_errors_total`
- Logs: `Retention sweep pass complete` (at `-v=2`, with `due`, `released`,
  `poolsSkipped`, `errors` and `rowErrors` per pass), `Retention sweep of a pool failed`
- Traces: span `ipam.retention.sweep`, with an `ipam.retention.sweep_pool`
  child per pool transaction
- Provider dashboard → "Retention leases" row: "Overdue retained
  allocations", "Most overdue retained allocation", "Replicas enforcing"

The retained gauges come from the one read of the retained set that every
replica makes at the start of each pass. They use the same predicate as the
query the sweeper releases from: `claim_key IS NULL`, `purpose = 'Claim'`,
`retained_at IS NOT NULL`, `expires_at <= now()`.

## Diagnose

1. **Is the sweeper releasing anything?**
   `sum(increase(ipam_retention_releases_total{mode="enforce"}[30m]))`.
   - Zero: no release is happening at all. Go to step 2.
   - Non-zero, with the late count flat or rising: releases are happening
     but not reaching every due row, or not keeping up. Go to step 3.

2. **Is the replica that holds the lock in enforce mode?**
   Any replica that wins the advisory lock runs the pass in its own mode. A
   rollout or an overlay that leaves one replica on `RETENTION_SWEEP_MODE=observe`
   makes that replica's passes count due rows instead of releasing them.
   ```sh
   kubectl -n ipam-system get pods -l app.kubernetes.io/name=ipam \
     -o custom-columns=POD:.metadata.name,MODE:'.spec.containers[0].env[?(@.name=="RETENTION_SWEEP_MODE")].value'
   ```
   Also query `ipam_retention_sweep_enforcing` by `pod`, or read "Replicas
   enforcing" on the dashboard: every replica should read 1. If they all read 1 and no releases happen, read the `-v=2` pass
   logs. `due=0` on passes while
   `ipam_retained_overdue_allocations` is non-zero means the release query
   is not finding the rows the gauges count, which is a bug; capture both and
   escalate to an engineer.

3. **Which rows are due, and where?**
   ```sql
   SELECT pool_key, ip_family, count(*) AS due,
          min(expires_at) AS oldest_deadline,
          now() - min(expires_at) AS overdue_by
     FROM ipam_cidr_allocations
    WHERE claim_key IS NULL AND purpose = 'Claim'
      AND retained_at IS NOT NULL AND expires_at <= now()
    GROUP BY 1, 2
    ORDER BY oldest_deadline ASC;
   ```
   - **One or a few pools hold all of it.** The sweeper takes each pool's
     row with `FOR UPDATE SKIP LOCKED` and skips the pool when a claim holds
     it. If
     `ipam_retention_sweep_pools_skipped_total` keeps rising, that pool is
     never free. Look for a long-running transaction on it:
     ```sql
     SELECT pid, state, xact_start, now() - xact_start AS age, left(query, 120)
       FROM pg_stat_activity
      WHERE datname = current_database() AND xact_start IS NOT NULL
      ORDER BY xact_start ASC
      LIMIT 10;
     ```
   - **The pool's transaction fails.** `Retention sweep of a pool failed` in
     the logs, with `ipam_retention_sweep_errors_total` rising, names the
     pool and the error.
   - **Rows spread across many pools, count growing.** More rows fall due
     each interval than one pass's 2000-row budget releases. Releases run at
     roughly 2000 per interval and the backlog still grows.

## Mitigate

- **A replica in observe mode:** remove the `RETENTION_SWEEP_MODE=observe`
  override from its overlay, or finish the rollout. The next enforcing pass
  starts on the backlog, oldest deadline first.
- **A pool that is never free:** end the transaction holding it
  (`SELECT pg_terminate_backend(<pid>)`) if it is wedged, or find the client
  that keeps it busy. The next pass releases the pool's due rows.
- **A pool whose transaction fails:** fix the cause the log names. A single
  allocation that cannot be released is
  [`IPAMRetentionRowsStuck`](ipam-retention-rows-stuck.md)'s case; follow that
  runbook.
- **Inflow above the budget:** shorten `RETENTION_SWEEP_INTERVAL`. The budget
  is per pass, so more passes release more rows. A pass's timeout is the
  larger of the interval and one minute, so an interval under a minute does
  not cut a pass short.
- **To release a specific address now:**
  `datumctl ipam allocation release <name>` in its project. This frees it
  the same way the sweeper would.

Until the alert clears, the cost is capacity: the due addresses stay held
and claims cannot be given them. No address is released early, and none is
released twice.
