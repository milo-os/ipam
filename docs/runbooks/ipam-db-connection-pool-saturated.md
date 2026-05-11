# Runbook: IPAMDBConnectionPoolSaturated

**Alert:** `IPAMDBConnectionPoolSaturated`
**Severity:** critical
**SLO:** ≥ 10% of pgxpool connection slots idle

## What this alert means

Less than 10% of postgres connections in the apiserver's pgxpool are idle,
sustained for 3 minutes. Allocation transactions queue on connection
acquire, driving up CREATE latency until the pool drains.

The synchronous allocation path (`internal/registry/ipam/ipprefixclaim/storage.go`
and siblings) acquires a connection per CREATE; if the pool is saturated,
every claim becomes a queue wait.

The alert fires when:

```promql
(ipam_pgxpool_idle_connections / clamp_min(ipam_pgxpool_max_connections, 1)) < 0.10
```

The pool stats are sampled by a background goroutine in `cmd/ipam/serve.go`
that calls `(*pgxpool.Pool).Stat()` on a fixed tick and publishes the four
gauges via `metrics.ObservePgxpoolStat`. The same expression drives the
"pgxpool saturation (acquired / max)" panel on the provider dashboard, so
the alert and the panel always agree.

## Diagnose

1. **Pull up the live ratio.** Provider dashboard → "Dependencies (DB +
   watch)" row → "pgxpool saturation (acquired / max)". The series breaks
   down per replica (`{{instance}}`); a single hot replica vs. fleet-wide
   saturation point at different mitigations.
2. **Cross-reference query latency.** "DB query p95 by query_name" on the
   same row. Saturation usually shows up first as a climb in
   `select_pool_for_update` p95 — that's the FOR UPDATE wait stacking up.
3. **Check active connections from the apiserver to postgres.**
   ```sql
   SELECT application_name,
          count(*) AS open,
          sum(case when state = 'active' then 1 else 0 end) AS active,
          sum(case when state = 'idle'   then 1 else 0 end) AS idle
     FROM pg_stat_activity
     WHERE application_name LIKE 'ipam%'
     GROUP BY application_name;
   ```
   `open` should equal what `ipam_pgxpool_total_connections` reports.
4. **Look for slow queries holding connections.**
   ```sql
   SELECT pid, now() - query_start AS duration, state, query
     FROM pg_stat_activity
     WHERE application_name LIKE 'ipam%'
       AND state != 'idle'
       AND now() - query_start > interval '1 second'
     ORDER BY duration DESC LIMIT 20;
   ```
5. **Check for connection leaks.** If `idle in transaction` connections
   exceed a handful, the apiserver is leaking — usually a missing
   `tx.Rollback(ctx)` on an error path.
   ```sql
   SELECT count(*) FROM pg_stat_activity
     WHERE state = 'idle in transaction'
       AND application_name LIKE 'ipam%';
   ```
6. **Correlate with allocation latency.** Provider dashboard →
   "Allocation latency" row. Pool saturation manifests as a sharp p95/p99
   climb without a corresponding throughput change.

## Mitigate

- **Identify and cancel the slow query.** `pg_cancel_backend(<pid>)`.
- **Bump pgxpool `MaxConns`.** If the workload genuinely needs more
  connections, increase `pool_max_conns` in `--postgres-dsn` and restart.
  Watch out for postgres-side `max_connections` ceiling — `MaxConns *
  replicas` must fit under the server limit with headroom.
- **Scale apiserver replicas.** Each replica has its own pgxpool;
  scaling out distributes the load.
- **Fix a leak.** Audit transaction-handling code in
  `internal/registry/ipam/*claim/storage.go` for missing
  `defer tx.Rollback(ctx)` patterns.

## Related

- Provider dashboard → "Dependencies (DB + watch)" row →
  "pgxpool saturation (acquired / max)" and "DB query p95 by query_name"
- Metric definitions: `internal/metrics/metrics.go`
  (`PgxpoolTotalConnections`, `PgxpoolIdleConnections`,
  `PgxpoolAcquiredConnections`, `PgxpoolMaxConnections`)
- Sampler: `cmd/ipam/serve.go` (`metrics.ObservePgxpoolStat` on a tick)
- `ipam-claim-latency-high.md`
- `.claude/agents/observability.md` — canonical metric spec
