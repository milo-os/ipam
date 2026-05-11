# Runbook: IPAM Triage — Start Here

You're paged on something IPAM-related. This page is the entry point: it
maps every alert to its dedicated runbook, defines the severity bands, and
gives you a 5-minute checklist before you go deep on any one symptom.

## Severity definitions

| Severity | Customer impact | Response time | Examples |
|---|---|---|---|
| **critical** | Active outage or imminent data risk; allocations failing for some/all callers | Page primary on-call, ack within 5 min | apiserver down, error-rate above budget, pool fully exhausted |
| **warning** | Degraded but not broken; SLO at risk if trend continues | Investigate within the hour during business hours, next business day otherwise | latency above target, pool > 80%, watch lag |

Anything tagged `slo:` is part of the documented SLO surface. Anything in the
`ipam-availability` group blocks the apiserver itself — treat it as critical
even if the explicit `severity:` label says otherwise.

## Alert → runbook map

Every PrometheusRule in `config/components/observability/alerts/ipam-alerts.yaml`
is reflected here. If a new alert is added there without a corresponding
runbook, the on-call experience regresses.

| Alert | Severity | Runbook |
|---|---|---|
| `IPAMApiserverDown` | critical | [`ipam-apiserver-down.md`](ipam-apiserver-down.md) |
| `IPAMClaimLatencyHigh` | warning | [`ipam-claim-latency-high.md`](ipam-claim-latency-high.md) |
| `IPAMAllocationErrorRateHigh` | critical | [`ipam-allocation-error-rate-high.md`](ipam-allocation-error-rate-high.md) |
| `IPAMAllocationErrorRateAbsolute` | warning | [`ipam-allocation-error-rate-high.md`](ipam-allocation-error-rate-high.md) |
| `IPAMPoolExhaustionImminent` | warning | [`ipam-pool-exhaustion-imminent.md`](ipam-pool-exhaustion-imminent.md) |
| `IPAMPoolExhausted` | critical | [`ipam-pool-exhaustion-imminent.md`](ipam-pool-exhaustion-imminent.md) |
| `IPAMDBConnectionPoolSaturated` | critical | [`ipam-db-connection-pool-saturated.md`](ipam-db-connection-pool-saturated.md) |
| `IPAMPgxpoolMetricsStale` | warning | [`ipam-db-connection-pool-saturated.md`](ipam-db-connection-pool-saturated.md) |
| `IPAMWatchLagHigh` | warning | [`ipam-watch-lag-high.md`](ipam-watch-lag-high.md) |
| `IPAMWatcherStuck` | warning | [`ipam-watcher-stuck.md`](ipam-watcher-stuck.md) |
| `IPAMWatcherBacklogSaturated` | warning | [`ipam-watch-lag-high.md`](ipam-watch-lag-high.md) |
| `IPAMReadLatencyHigh` | warning | [`ipam-read-latency-high.md`](ipam-read-latency-high.md) |

## First 5 minutes — universal checklist

Run these regardless of which alert paged you. They catch the high-impact
"is the service even there" failure modes that derivative alerts can mask.

1. **Pod / apiserver health** (30s):
   ```sh
   kubectl -n ipam-system get pods -l app.kubernetes.io/name=ipam
   kubectl get apiservice v1alpha1.ipam.miloapis.com -o jsonpath='{.status.conditions}' | jq
   ```
   If the pod is not `Running`/`Ready` or the APIService is not `Available=True`,
   stop here and go to [`ipam-apiserver-down.md`](ipam-apiserver-down.md). Other
   alerts are noise until the apiserver is back.

2. **Recent deploy / config change** (30s):
   ```sh
   kubectl -n ipam-system get events --sort-by=.lastTimestamp | tail -20
   kubectl -n ipam-system rollout history deploy/ipam-apiserver
   ```
   If the alert started within ~10 minutes of a deploy or ConfigMap update,
   roll back first and investigate after.
   ```sh
   kubectl -n ipam-system rollout undo deploy/ipam-apiserver
   ```

3. **Provider dashboard** (1 min): open the "IPAM — Provider" Grafana
   dashboard. The `Service health` row at the top shows `up`, request rate,
   error rate, and 507 rate side-by-side. A single-number anomaly here often
   localises the cause faster than reading the alert annotation.

4. **Postgres health** (1 min). Almost every IPAM symptom traces back to
   postgres in some form (lock contention, connection saturation, vacuum
   blocked).
   ```sh
   kubectl exec -n ipam-system <postgres-pod> -- \
     psql -d ipam -c "SELECT state, count(*) FROM pg_stat_activity GROUP BY state;"
   kubectl exec -n ipam-system <postgres-pod> -- \
     psql -d ipam -c "SELECT pid, now()-xact_start AS dur, state, query
                        FROM pg_stat_activity WHERE state != 'idle'
                        ORDER BY dur DESC LIMIT 10;"
   ```

5. **Read the runbook for the actual alert**. Use the table above. Don't
   start mitigating from generic instinct — the per-alert runbooks list the
   specific signals to confirm before acting.

## Common cross-cutting symptoms

Use these when the alert label isn't a clean match for what you're seeing.

- **"Latency is up everywhere"** → start with
  [`ipam-claim-latency-high.md`](ipam-claim-latency-high.md), then check the
  pgxpool gauges (sampler may have died — see
  [`ipam-db-connection-pool-saturated.md`](ipam-db-connection-pool-saturated.md))
  and confirm the apiserver isn't restarting in a loop.
- **"Watch consumers see stale state"** → triage path is
  [`ipam-watcher-stuck.md`](ipam-watcher-stuck.md) →
  [`ipam-watch-lag-high.md`](ipam-watch-lag-high.md). Stuck-then-lag is a
  much faster fix than diagnosing lag on its own.
- **"507 Insufficient Storage"** → almost always pool exhaustion; see
  [`ipam-pool-exhaustion-imminent.md`](ipam-pool-exhaustion-imminent.md). If
  the affected pool is < 80% utilized, allocation arithmetic may be broken
  and it's a code bug — escalate to the dev on-call.
- **"Reads are slow but writes are fine"** → see
  [`ipam-read-latency-high.md`](ipam-read-latency-high.md). The known issue
  pattern is per-request middleware overhead, not DB scan time.

## Escalation path

1. **Primary:** ipam on-call (PagerDuty service `ipam-apiserver`)
2. **Secondary:** platform infra on-call — for cluster-level / kube-apiserver
   issues (TLS, NetworkPolicy, aggregation-layer health)
3. **DBA on-call:** for postgres-rooted issues (connection saturation,
   lock contention, replication, vacuum blockers)
4. **Dev on-call (IPAM team):** for suspected logic bugs (allocation math
   wrong, ip_family mis-tagged). Page only after the immediate user-facing
   impact is contained.

Contacts and PD service links: `<placeholder — fill in with the runbook
ownership row from your team's source-of-truth>`. Avoid stamping live
addresses into tree; PD/Slack handles drift fastest if it lives in the
team's wiki, not here.

## When to file a postmortem

- Any `critical` alert that fired for more than 5 minutes
- Any incident with customer-visible impact (allocations rejected,
  controllers stuck, caller-facing latency above the consumer SLO)
- Any incident where the runbook proved insufficient — that's a runbook
  bug, not just an outage; updating the runbook is part of the postmortem
  action items.

## All runbooks

- [`ipam-apiserver-down.md`](ipam-apiserver-down.md)
- [`ipam-allocation-error-rate-high.md`](ipam-allocation-error-rate-high.md)
- [`ipam-claim-latency-high.md`](ipam-claim-latency-high.md)
- [`ipam-db-connection-pool-saturated.md`](ipam-db-connection-pool-saturated.md)
- [`ipam-pool-exhaustion-imminent.md`](ipam-pool-exhaustion-imminent.md)
- [`ipam-read-latency-high.md`](ipam-read-latency-high.md)
- [`ipam-watch-lag-high.md`](ipam-watch-lag-high.md)
- [`ipam-watcher-stuck.md`](ipam-watcher-stuck.md)
