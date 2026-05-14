# Runbook: IPAMWatcherStuck

**Alert:** `IPAMWatcherStuck`
**Severity:** warning
**SLO:** Watch freshness

## What this alert means

The IPAM apiserver is producing changelog rows (allocation traffic is flowing,
`ipam_allocation_attempts_total` is incrementing) but its LISTEN/NOTIFY watcher
has dispatched **zero** events for 5 minutes (`ipam_watch_events_total` is
flat). Watch consumers — controllers using `kubectl -w`, the
`PostgresWatcher` cache in any client — are silently stale.

## Source signals

- `ipam_watch_events_total` — primary (rate must be > 0 when traffic is flowing)
- `ipam_allocation_attempts_total` — traffic guard
- `ipam_watch_lag_seconds_bucket` — companion (will trip `IPAMWatchLagHigh` shortly)
- `ipam_watcher_poll_batch_size` — does the poll fallback see anything?
- Provider dashboard → "Watch health" row

## Diagnose

1. **Is the apiserver actually writing changelog rows?**
   ```sh
   kubectl exec -n ipam-system <postgres-pod> -- \
     psql -d ipam -c "SELECT COUNT(*) FROM ipam_changelog WHERE created_at > now() - interval '1 minute';"
   ```
   If this is 0 despite `rate(ipam_allocation_attempts_total[5m]) > 0`, the
   problem is on the write path, not the watcher — escalate to a different
   investigation.

2. **Is the LISTEN connection still alive?**
   ```sh
   kubectl exec -n ipam-system <postgres-pod> -- \
     psql -d ipam -c "SELECT pid, application_name, state, query_start, query
                        FROM pg_stat_activity
                        WHERE query ILIKE '%LISTEN%' OR query ILIKE '%ipam_changelog%';"
   ```
   Expected: at least one connection with `query` showing the LISTEN command
   for the `ipam_changelog` channel. **No row** ⇒ the watcher's pgx
   connection has dropped and isn't being re-established. **Row in `idle in
   transaction` for hours** ⇒ the watcher goroutine is wedged.

3. **Manual NOTIFY test.** Inject a dummy NOTIFY and see if the watcher reacts
   (this requires apiserver restart privileges — only do this in a soak
   environment, not prod, unless you've cleared it with the on-call):
   ```sh
   kubectl exec -n ipam-system <postgres-pod> -- \
     psql -d ipam -c "NOTIFY ipam_changelog, 'manual-probe';"
   ```
   If `ipam_watch_events_total` stays flat for the next 30s, the apiserver
   side of the listen is broken — it cannot recover without a restart.

4. **Goroutine dump.** If the apiserver exposes pprof (the standard
   `apiserver_request_*` setup does on `/debug/pprof/`), pull a goroutine
   dump and look for the watcher's poll loop:
   ```sh
   kubectl exec -n ipam-system <ipam-pod> -- \
     curl -s http://localhost:8080/debug/pprof/goroutine?debug=2 \
     | grep -A 8 "internal/watch/postgres"
   ```
   A goroutine blocked on a select with no activity for minutes is the
   smoking gun.

## Mitigate

1. **Roll the apiserver pod.** Restart re-establishes the LISTEN connection
   and clears any wedged goroutine state. The watcher's catch-up logic
   re-reads the changelog from the last cursor on startup, so no events are
   lost.
   ```sh
   kubectl -n ipam-system rollout restart deploy/ipam-apiserver
   ```
2. After restart, confirm:
   - `rate(ipam_watch_events_total[1m]) > 0` returns non-zero in Prometheus
   - This alert clears within 5 minutes
   - `IPAMWatchLagHigh` is not firing
3. If a restart did not resolve the alert, the cause is upstream (postgres
   connection, NetworkPolicy blocking the LISTEN return path, replication
   slot exhaustion). Escalate to DBA on-call.

## Related

- `ipam-watch-lag-high.md` — the lag alert that this one frequently precedes
- `ipam-apiserver-down.md` — if the watcher is stuck and the pod is also unhealthy
- `ipam-db-connection-pool-saturated.md` — connection-pool exhaustion can starve the watcher's listen connection
