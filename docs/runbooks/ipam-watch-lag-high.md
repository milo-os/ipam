# Runbook: IPAMWatchLagHigh

**Alert:** `IPAMWatchLagHigh`
**Severity:** warning
**SLO:** Watch consumers see changes within 30 seconds of commit

## What this alert means

The IPAM Watch consumer (`internal/watch/postgres.go`) is more than 30
seconds behind the newest row in the `ipam_changelog` table. Watch
subscribers — informers and any controllers that consume IPAM watch events
— will see stale state.

The alert fires when:

```promql
histogram_quantile(0.99, rate(ipam_watch_lag_seconds_bucket[5m])) > 30
```

`ipam_watch_lag_seconds` is a Histogram emitted by
`internal/watch/postgres.go` at the moment the watcher hands an event off to
its subscriber channel; the observation is `now() − changelog.created_at`.
Bookmark events bypass the histogram, so the metric measures user-visible
event lag, not internal bookkeeping.

The companion counter `ipam_watch_events_total{kind, event_type}` confirms
whether the watcher is dispatching anything at all — a flatline there
combined with a healthy alloc rate means the watcher is stuck rather than
just slow.

## Background

The IPAM apiserver implements `watch.Interface` over a postgres
LISTEN/NOTIFY channel plus an xmin-horizon polling cursor against
`ipam_changelog`. Lag is the difference between `now()` and the timestamp
of the oldest unprocessed `ipam_changelog` row. A NOTIFY kick normally
keeps lag in the sub-millisecond range; the periodic safety poll is the
backstop when LISTEN drops.

## Diagnose

1. **Check the lag distribution and dispatch rate side-by-side.**
   Provider dashboard → "Dependencies (DB + watch)" row →
   "Watch lag p99" and "Watch events dispatched (by kind)". If p99 is
   above the threshold but the dispatch rate is non-zero, the watcher is
   draining slowly. If the dispatch rate is zero while p99 climbs, the
   watcher is stuck.
2. **Inspect the changelog horizon directly.**
   ```sql
   SELECT min(created_at) AS oldest_unprocessed,
          max(created_at) AS newest,
          count(*)        AS rows
     FROM ipam_changelog
     WHERE id > (SELECT last_watched_id FROM ipam_watch_cursor); -- or equivalent
   ```
3. **Check for long-running transactions blocking changelog vacuum.**
   The changelog table grows unbounded if vacuum can't run.
   ```sql
   SELECT pid, now() - xact_start AS duration, state, query
     FROM pg_stat_activity
     WHERE state != 'idle' AND now() - xact_start > interval '1 minute'
     ORDER BY duration DESC LIMIT 20;
   ```
4. **Confirm LISTEN/NOTIFY connectivity.**
   ```sql
   SELECT pid, application_name, state, query
     FROM pg_stat_activity
     WHERE query ILIKE 'LISTEN ipam_changelog%';
   ```
   If no rows: the apiserver's listener is not connected. Check apiserver
   pod logs for postgres reconnect errors.
5. **Inspect apiserver Watch goroutine state.** klog emits periodic
   progress lines from `internal/watch/postgres.go` (see
   `maybeWarnHorizonStall`). A WARN about the snapshot horizon being
   frozen for minutes is the smoking gun for a long-running transaction.

## Mitigate

- If a long-running transaction is blocking vacuum, identify and cancel
  the offending PID.
- If the listener is disconnected, restart the apiserver pod
  (rolling). The Watch consumer is replicated per-pod.
- If the changelog table has grown beyond a few hundred MB, run
  `VACUUM (FULL, ANALYZE) ipam_changelog;` during a maintenance window.

## Related

- Provider dashboard → "Dependencies (DB + watch)" row → "Watch lag p99"
  and "Watch events dispatched (by kind)"
- Metric definitions: `internal/metrics/metrics.go` (`WatchLag`,
  `WatchEvents`)
- Emission sites: `internal/watch/postgres.go`
  (`metrics.ObserveWatchLag`, `metrics.RecordWatchEvent`)
- `.claude/agents/observability.md` — canonical metric spec
