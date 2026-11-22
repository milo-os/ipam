# Runbook: IPAMRetentionSweeperStalled

**Alert:** `IPAMRetentionSweeperStalled`
**Severity:** warning
**SLO:** Retention

## What this alert means

No replica has completed a retention sweep pass for more than five sweep
intervals. A pass that carries on past a pool or a row it could not act on
still completes, and counts those in `ipam_retention_sweep_errors_total`,
`ipam_retention_sweep_row_errors_total` and
`ipam_retention_lease_unevaluable_total`. This alert therefore means passes
are not running, or are failing outright; rows that keep failing inside
completed passes raise
[`IPAMRetentionRowsStuck`](ipam-retention-rows-stuck.md) instead.

The sweeper runs in every apiserver replica as a post-start hook; each pass
is taken by whichever replica gets the Postgres advisory lock
`pg_try_advisory_lock(0x6970616d5f727377)` first, and only that replica stamps `ipam_retention_sweep_last_success_timestamp_seconds`.

While it is stalled:

- in enforce mode, retained allocations whose lease has run out stay held;
- in observe mode, `ipam_retention_releases_total{mode="observe"}` stops
  counting them;
- leases left unevaluated (`ipam_retained_allocations{lease="unevaluated"}`)
  stay unevaluated.

Claims are unaffected. The sweeper yields to them, never the other way round.

## Source signals

- `ipam_retention_sweep_last_success_timestamp_seconds` (max across replicas)
- `ipam_retention_sweep_interval_seconds`: the configured interval; 0 means
  disabled, which this alert ignores
- `ipam_retention_sweep_errors_total`: a pass that failed outright, or one
  pool's transaction within a pass
- `ipam_retention_sweep_row_errors_total`: single allocations a pass could not
  act on; each is undone alone and retried on a later pass
- `ipam_retention_lease_unevaluable_total`: rows whose class could not be read;
  they stay held and unevaluated, and passes page past them
- `ipam_retention_sweep_pools_skipped_total`: pools left for the next pass
  because a claim held them; not an error
- `ipam_retention_sweep_duration_seconds`
- Traces: span `ipam.retention.sweep`, with an `ipam.retention.sweep_pool`
  child per pool transaction
- Logs: `Retention sweep pass failed`, `Retention sweep of a pool failed`,
  `Could not evaluate a retained allocation's lease`
- Provider dashboard → "Retention" row

## Diagnose

1. **Are passes failing, or not running?**
   `rate(ipam_retention_sweep_errors_total[15m]) > 0` with no new
   `ipam_retention_sweep_duration_seconds_count` means passes start and fail.
   Go to step 3. A flat error counter with no new
   `ipam_retention_sweep_duration_seconds_count` means no replica is running a
   pass. Go to step 2.

2. **Who holds the advisory lock?**
   ```sh
   kubectl exec -n ipam-system <postgres-pod> -- psql -d ipam -c "
     SELECT l.pid, a.application_name, a.state, a.backend_start, a.state_change
       FROM pg_locks l JOIN pg_stat_activity a USING (pid)
      WHERE l.locktype = 'advisory'
        AND l.classid = x'6970616d'::int AND l.objid = x'5f727377'::int;"
   ```
   A pass holds the lock for seconds. A session holding it for minutes is a
   wedged pass: restart the replica it belongs to (match `client_addr` to the
   pod IP). No row and no passes means the loop is not running. Look for
   `Retention sweeper started` in the logs, and check that
   `RETENTION_SWEEP_INTERVAL` is not `0`. A pass that panics is recovered
   and logged as `Retention sweep pass panicked`, and the loop retries after
   a backoff of up to ten intervals.

   The lock is a session-level advisory lock. A connection pooler between
   IPAM and Postgres must run in session mode: in transaction mode the lock
   lands on whichever server connection the statement got, and the lock and
   the per-pool check that the session is still alive both stop meaning
   anything. If the session holding the lock dies, the pass stops before its
   next pool. Another replica may sweep meanwhile, which is safe, because
   every write is made under the pool's row lock and only to rows still
   retained and due.

3. **What is failing?** Read the errors in the apiserver logs. Two common
   causes:
   - a class that cannot be decoded, which leaves its rows' leases
     unevaluated. Passes page past them, but they stay held. Find the rows:
     ```sql
     SELECT allocation_key, pool_key, class_name, class_project, retained_at
       FROM ipam_cidr_allocations
      WHERE claim_key IS NULL AND purpose = 'Claim'
        AND retained_at IS NOT NULL AND expires_at IS NULL;
     ```
   - a database that is unreachable or failing writes. This alert fires
     alongside the allocation alerts in that case; follow
     [`ipam-db-connection-pool-saturated.md`](ipam-db-connection-pool-saturated.md).

## Mitigate

- Fix or delete the class that cannot be read. The next pass evaluates the
  rows it held back.
- Restart a replica holding the lock with a wedged pass:
  `kubectl -n ipam-system delete pod <pod>`. Its session ends and the lock with
  it.
- To stop the sweeper while you investigate, set `RETENTION_SWEEP_INTERVAL=0`.
  Nothing is lost: deadlines are stored on the rows, and the next pass after
  re-enabling acts on everything that fell due meanwhile.
