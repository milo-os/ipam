# Runbook: IPAMAllocationErrorRateHigh

**Alert:** `IPAMAllocationErrorRateHigh`
**Severity:** critical (ratio > 5%); see also `IPAMAllocationErrorRateAbsolute` (warning)
**SLO:** Allocation success ratio ≥ 95%

## What this alert means

The ratio of server-fault failures (`ipam_allocation_failures_total{reason=~"internal|tx_error"}`)
to total allocation attempts (`ipam_allocation_duration_seconds_count`) exceeded
5% over the last 5 minutes, sustained for 2 minutes. This is the headline
write-path error budget alert. Requests the server refused (every other
`reason`) do not count toward it: a caller sending malformed claims, or a full
pool, cannot fire it.

## Source signals

- `ipam_allocation_failures_total{resource, reason}` — failure counter
- `ipam_allocation_duration_seconds_count{resource, result}` — total attempts
- Provider dashboard → "Allocation throughput" row → "Allocation failures by reason"

The `reason` label distinguishes root causes:

| `reason` | Meaning | First step |
|---|---|---|
| `pool_exhausted` | The target pool, or an ancestor the cascade provisions from, is full (507) | See `ipam-pool-exhaustion-imminent.md` |
| `pool_not_found` | No pool offers the claim's class | Config drift; check the class's offering pools |
| `class_not_found` | The class, or the default class, does not exist | Config drift; check `spec.className` |
| `invalid` | The request was refused as malformed (400/422): bad `spec.address`, wrong family, unaligned or out-of-pool address, missing scope role, unplaced range | Caller error; find the caller in audit logs |
| `conflict` | The name or the address is taken (409) | Caller error, or a retained allocation under the name |
| `forbidden` | Admission refused the claim (403): quota, policy | Check the project's quota |
| `namespace_not_live` | The claim's namespace is missing or terminating | Caller error |
| `class_holds_no_range` | A ScopeRange claim under a class with no `poolPer` | Caller error |
| `tx_error` | postgres transaction failed (rollback, deadlock, conn loss) | Database health |
| `internal` | Bug — should never occur in steady state | Open a P1; capture klog output |

Only `tx_error` and `internal` are server faults, and only they fire this alert and `IPAMAllocationErrorRateAbsolute`. Every other reason is a request the server refused and is shown on the dashboard for context. A full pool is covered by `IPAMPoolExhaustionImminent` and `IPAMPoolExhausted` instead.

## Diagnose

1. **Break down server faults by reason.** The second query shows refusals
   alongside, which can point at what the failing requests were doing.
   ```promql
   sum by (resource, reason) (rate(ipam_allocation_failures_total{reason=~"internal|tx_error"}[5m]))
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
