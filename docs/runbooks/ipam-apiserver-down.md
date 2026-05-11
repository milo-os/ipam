# Runbook: IPAMApiserverDown

**Alert:** `IPAMApiserverDown`
**Severity:** critical
**SLO:** Apiserver availability

## Overview

The IPAM aggregated apiserver is unreachable. The alert fires when either:

- the `up{job="ipam-apiserver"}` series is **absent** for at least 1 minute
  (Prometheus has no scrape target — pod crashloop, empty endpoints, scrape
  config drift), or
- the `up{job="ipam-apiserver"}` series exists but has **value 0** (Prometheus
  reaches the target but the scrape itself fails — TLS, port mismatch, kube-apiserver
  refusing the aggregation handshake).

While the apiserver is down, **all** claim CREATE / GET / LIST / WATCH calls
fail. Workloads attempting to allocate IP/prefix/ASN resources will hang or
error. Customer impact is active.

## Alert conditions

```promql
absent(up{job="ipam-apiserver"}) or up{job="ipam-apiserver"} == 0
```

`for: 1m` — short hold deliberately, so a true outage pages quickly. A pod
restart that recovers in under a minute will not page.

## Source signals

- `up{job="ipam-apiserver"}` — primary
- `kube_pod_status_phase{namespace="ipam-system"}` — pod state
- `kube_pod_container_status_restarts_total` — restart count
- Provider dashboard → "Apiserver health" row

## Immediate diagnosis (first 60 seconds)

1. **Pod state.**
   ```sh
   kubectl -n ipam-system get pods -l app.kubernetes.io/name=ipam -o wide
   ```
   Expected: at least one Pod `Running`/`Ready`. If you see `CrashLoopBackOff`,
   `ImagePullBackOff`, `Pending`, or `Error`, jump to the matching cause below.

2. **Pod events.**
   ```sh
   kubectl -n ipam-system describe pod -l app.kubernetes.io/name=ipam | tail -60
   ```
   The `Events:` block at the bottom is the highest-signal place to look:
   OOMKilled, FailedScheduling, FailedMount, BackOff, Liveness probe failed.

3. **Recent logs.**
   ```sh
   # Current container
   kubectl -n ipam-system logs -l app.kubernetes.io/name=ipam --tail=200
   # Previous container (after a crash)
   kubectl -n ipam-system logs -l app.kubernetes.io/name=ipam --tail=200 -p
   ```

4. **APIService aggregation status.** A healthy pod that the kube-apiserver
   can't talk to is just as bad as a down pod.
   ```sh
   kubectl get apiservice v1alpha1.ipam.miloapis.com -o yaml | yq '.status'
   ```
   `Available=True` is required. `FailedDiscoveryCheck` means kube-apiserver
   cannot reach the IPAM Service.

## Common causes

### OOMKilled

`kubectl describe pod` shows `Last State: Terminated, Reason: OOMKilled`.

- Cause: a large LIST or a memory-leaky watch consumer.
- Mitigation: bump the Deployment memory limit by 50%, redeploy, then
  investigate. Capture a heap profile via the pprof endpoint if reproducible.

### CrashLoopBackOff (config / startup error)

Pod restarts every few seconds. Logs typically show a panic or fatal `klog.Fatalf`.

- Cause: bad `--postgres-dsn`, missing TLS cert, schema migration not applied,
  invalid flag in the Deployment env.
- Mitigation: read the panic / fatal message in `kubectl logs -p`. Roll back
  the most recent Deployment / ConfigMap / Secret change.

### ImagePullBackOff

Pod stuck in `ContainerCreating`; describe shows `ErrImagePull` or
`ImagePullBackOff`.

- Cause: image tag drift after a release, registry auth expired, or the
  `images:` transformer in `config/base/` was bumped to a tag that wasn't
  actually pushed.
- Mitigation: verify the image exists in the registry; roll back to the
  previous tag if needed.

### TLS / cert-manager issue

Logs contain `tls: failed to verify certificate` or
`x509: certificate has expired`.

- Cause: cert-manager CSI driver hasn't refreshed the serving cert; the
  Issuer/ClusterIssuer is broken; the apiservice `caBundle` is stale.
- Mitigation:
  ```sh
  kubectl -n ipam-system describe csi /var/run/secrets/...
  kubectl get certificaterequest,certificate -A | grep ipam
  ```
  Restart the pod once the cert is regenerated:
  `kubectl -n ipam-system rollout restart deploy/ipam-apiserver`.

### Postgres unreachable

Logs contain `failed to connect to postgres` / `dial tcp ... refused`.

- Cause: the postgres dependency (Helm chart in `config/dependencies/`) is
  down or the DSN secret was rotated without restarting the apiserver.
- Mitigation: verify the postgres pod / Service is healthy, then
  `kubectl -n ipam-system rollout restart deploy/ipam-apiserver`.

### Aggregation handshake failure (pod healthy, alert still firing)

The pod is `Ready`, but `kubectl get apiservice` shows `Available=False`.

- Cause: kube-apiserver cannot reach the Service IP (NetworkPolicy, broken
  Service selector, wrong port in `APIService.spec.service`), or the
  `caBundle` doesn't match the serving cert.
- Mitigation: from the kube-apiserver pod (or any pod in the cluster network),
  `curl -kv https://<service>.ipam-system.svc:443/healthz`. Compare the cert
  to the `caBundle` in the APIService.

## Mitigation steps (general)

1. If a recent change is the suspected cause (Deployment image bump, ConfigMap
   change, migration), **roll back first, investigate second**. The IPAM
   apiserver is in the IP-allocation hot path — every minute down is a minute
   of customer impact.
   ```sh
   kubectl -n ipam-system rollout undo deploy/ipam-apiserver
   ```
2. If the cause is OOM or pod-crash unrelated to recent change, scale to
   2 replicas while you investigate so a single crash doesn't take the
   service down again:
   ```sh
   kubectl -n ipam-system scale deploy/ipam-apiserver --replicas=2
   ```
3. Once the root cause is fixed, confirm recovery:
   - `up{job="ipam-apiserver"} == 1` returns 1 in Prometheus
   - `kubectl get apiservice v1alpha1.ipam.miloapis.com` shows `Available=True`
   - A trivial `kubectl get ipprefixclaim -A` succeeds

## Escalation

- **Primary:** ipam on-call (PagerDuty service `ipam-apiserver`)
- **Secondary:** platform infra on-call (cluster-level / kube-apiserver issues)
- **DBA on-call:** if root cause is postgres
- File a postmortem if the outage exceeded 5 minutes or had customer-visible
  impact.

## Related

- `ipam-claim-latency-high.md`
- `ipam-allocation-error-rate-high.md`
- `ipam-db-connection-pool-saturated.md`
