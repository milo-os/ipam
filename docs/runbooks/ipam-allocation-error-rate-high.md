# Runbook: IPAMAllocationErrorRateHigh

**Alert:** `IPAMAllocationErrorRateHigh`
**Severity:** critical (ratio > 5%); see also `IPAMAllocationErrorRateAbsolute` (warning)
**SLO:** Allocation success ratio ≥ 95%

## What this alert means

The ratio of `ipam_allocation_failures_total` to total allocation attempts
(`ipam_allocation_duration_seconds_count`) exceeded 5% over the last 5 minutes,
sustained for 2 minutes. This is the headline write-path error budget alert.

## Source signals

- `ipam_allocation_failures_total{resource, reason}` — failure counter
- `ipam_allocation_duration_seconds_count{resource, result}` — total attempts
- Provider dashboard → "Allocation throughput" row → "Allocation failures by reason"

The `reason` label distinguishes root causes:

| `reason` | Meaning | First step |
|---|---|---|
| `pool_exhausted` | The target pool is full | See `ipam-pool-exhaustion-imminent.md` |
| `pool_not_found` | The claim references a pool that doesn't exist | Config drift; check claim's `spec.poolRef` |
| `tx_error` | postgres transaction failed (rollback, deadlock, conn loss) | Database health |
| `internal` | Bug — should never occur in steady state | Open a P1; capture klog output |

## Diagnose

1. **Break down failures by reason.**
   ```promql
   topk(5, sum by (resource, reason) (rate(ipam_allocation_failures_total[5m])))
   ```
2. **For `tx_error`:** check postgres connectivity and lock contention.
   ```sql
   SELECT pid, now() - xact_start AS duration, wait_event_type, wait_event, query
     FROM pg_stat_activity
     WHERE wait_event_type = 'Lock'
        OR (state != 'idle' AND now() - xact_start > interval '5 seconds')
     ORDER BY duration DESC LIMIT 20;
   ```
   The IPAM allocation transaction holds `SELECT ... FOR UPDATE` on
   `ipam_objects`; lock waits on this table block writes.
3. **For `internal`:** capture the apiserver logs and surrounding metric
   values, then page the IPAM service owner.
4. **Apiserver-side view.** `apiserver_request_total{resource=~".*claim.*", code=~"5.."}`
   gives the HTTP-level breakdown (507 = pool exhausted, 5xx = server error).
   Provider dashboard → "Apiserver request mix" → "Apiserver responses by code".

## Mitigate

- Pool exhaustion: see the dedicated runbook.
- Postgres lock contention: identify and cancel the blocking PID.
  ```sql
  SELECT pg_cancel_backend(<pid>);
  ```
- Connection loss / pgxpool exhaustion: bump pgxpool `MaxConns` or scale
  apiserver replicas. Once `ipam_pgxpool_*` metrics land, see
  `ipam-db-connection-pool-saturated.md`.

## Related

- `ipam-pool-exhaustion-imminent.md`
- `ipam-claim-latency-high.md`
- `ipam-db-connection-pool-saturated.md` (pending instrumentation)
