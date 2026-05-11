# Runbook: IPAMClaimLatencyHigh

**Alert:** `IPAMClaimLatencyHigh`
**Severity:** warning
**SLO:** Successful claim CREATE p95 < 500ms

## What this alert means

The 95th percentile of successful (`result="success"`) IPAM claim allocation
latency exceeded 500ms for at least 5 minutes. This is the headline write-path
SLO; sustained breach causes consumer k6 throughput tests to fail and degrades
every namespace that creates claims.

The histogram is filtered to `result="success"` so failure-path latency does
not skew the tail. If failures dominate, see `ipam-allocation-error-rate-high.md`.

## Source signals

- `ipam_allocation_duration_seconds_bucket{result="success"}` — primary
- `ipam_allocation_duration_seconds_count` — denominator for failure ratio
- Provider dashboard → "Allocation latency" row → "Allocation latency quantiles"

## Diagnose

1. **Confirm scope.** Is the latency spike on every `resource` value
   (`ipprefixclaim`, `ipaddressclaim`, `asnclaim`) or one? Per-resource panel:
   `histogram_quantile(0.95, sum by (le, resource) (rate(ipam_allocation_duration_seconds_bucket{result="success"}[5m])))`
2. **Correlate with throughput.** Is the cluster doing more work, or the same
   work slower? Provider dashboard → "Allocation throughput" row.
3. **Check postgres.** The synchronous allocation transaction holds
   `SELECT ... FOR UPDATE` on the pool row.
   ```sql
   SELECT pid, now() - xact_start AS duration, state, query
     FROM pg_stat_activity
     WHERE state != 'idle' AND query ILIKE '%ipam_objects%'
     ORDER BY duration DESC LIMIT 20;
   ```
   Long-running transactions or lock waits on `ipam_objects` block all claims
   against that pool.
4. **Check pod resources.** Provider dashboard → "Pod resources" row. CPU
   throttling or memory pressure shows up as latency before it shows up as errors.
5. **Check apiserver-side latency.** If `apiserver_request_duration_seconds`
   for verb=create, resource=*claim* is also elevated, the latency is genuinely
   end-to-end (not just the postgres path).

## Mitigate

- Identify the slow query / blocking transaction in `pg_stat_activity` and
  cancel it (`pg_cancel_backend(pid)`) if safe.
- If pgxpool is exhausted (when that metric exists, see
  `ipam-db-connection-pool-saturated.md`), bump `MaxConns` or shed load by
  scaling apiserver replicas.
- If a single pool is the source, check whether it is near exhaustion — long
  scans of the `ipam_prefix_allocations` table (during `FindFirstAvailableBlock`)
  scale with allocation count. Splitting an over-utilized pool can restore
  latency.

## Related

- `ipam-allocation-error-rate-high.md`
- `ipam-pool-exhaustion-imminent.md`
- `ipam-db-connection-pool-saturated.md` (pending instrumentation)
