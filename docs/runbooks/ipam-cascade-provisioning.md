# Runbook: automatic pool provisioning

**Alerts:** `IPAMCascadeProvisioningFailing`, `IPAMCascadeProvisioningThrashing`, `IPAMCascadeResolutionLatencyHigh`
**Severity:** warning
**SLO:** claim availability, claim latency

## Background

A claim names a class, not a pool. If the class sits on a chain whose pools do
not exist yet, IPAM builds the chain in-line while serving the claim: it carves
a block out of each parent pool and creates the child, one level at a time, root
first.

Two properties matter when reading these metrics:

- **Each level commits on its own**, outside the allocation transaction. That
  cost is charged to the claim but appears in none of the Postgres query
  timings — it shows up only as an unexplained gap in end-to-end claim latency.
- **Levels are serialised on an identity row**, so a herd of simultaneous first
  claims into one scope produces exactly one pool. One caller wins and creates
  the pool; the rest block on the identity row, then read the winner's pool and
  carry on. **Losing is a normal outcome, not an error.**

## Metrics

`ipam_cascade_levels_total{class, outcome}` counts every level of every claim.
`outcome` is one of:

| outcome | meaning |
|---|---|
| `reused` | the pool already existed; one indexed read. The steady state. |
| `provisioned` | this claim created the pool. |
| `lost` | this claim raced another into the same scope and lost. Expected. |
| `error` | the level could not be resolved; the claim failed. |

`ipam_cascade_resolution_duration_seconds{result, provisioned}` measures the
whole resolution, with `provisioned="true"` marking claims that built something.

Neither metric carries the pool key, the pool name, or the scope digest. Those
are unbounded and must never become labels — filter by `class` and then use
`kubectl` to find the pool.

Provider dashboard → **Automatic pool provisioning** row.

## "A pool appeared that nobody created"

Every automatically created pool is labelled with the class that caused it:

```bash
kubectl get ippools -l ipam.miloapis.com/provisioned-by --show-labels
kubectl get ippool <name> -o jsonpath='{.spec.classRef.name} {.spec.parentPoolRef.name} {.status.allocatedCIDR}{"\n"}'
```

The "Pools created automatically" panel shows the rate over time and attributes
it to a class. If pools keep appearing for a class whose chain should already be
built, each one is a new scope — check the class's `poolPer` roles against the
scope values the claims are sending.

## IPAMCascadeProvisioningFailing

Claims of the named class are failing before allocation is attempted, because
the pool they draw from cannot be created.

1. **Find the failing class.** The alert carries it as `$labels.class`.
2. **Check the parent has space to carve.** A level is a block taken out of its
   parent, so a parent at capacity fails every child.
   ```bash
   kubectl get ippools -o custom-columns=\
   NAME:.metadata.name,CLASS:.spec.classRef.name,CIDR:.status.allocatedCIDR,PHASE:.status.phase
   ```
   Cross-check `ipam_pool_utilization_ratio` for the parent. If it is at 1.0,
   see [ipam-pool-exhaustion-imminent.md](ipam-pool-exhaustion-imminent.md).
3. **Check the root of the chain is backed at all.** The root-most class must be
   offered by an operator-authored pool. A class chain whose root nothing offers
   fails identically on every claim, from the first one.
   ```bash
   kubectl get ipclasses <class> -o yaml   # follow .spec.parentClassName up
   ```
4. **Read the apiserver logs** for the class name; provisioning failures log the
   level and the underlying Postgres error.
   ```bash
   kubectl logs -n ipam-system -l app.kubernetes.io/name=ipam --tail=200 | grep -i provision
   ```

## IPAMCascadeProvisioningThrashing

Claims keep losing the race with nothing being created. Each affected claim
waits on the identity row and then proceeds, so consumers see latency rather
than errors, and nothing else reports a fault.

The usual cause is a winner that keeps aborting: it takes the identity row,
fails while carving or writing the pool, and rolls back — releasing the waiters,
one of which becomes the next winner and repeats.

1. Check `ipam_cascade_levels_total{outcome="error"}` for the same class. Errors
   alongside the losses confirm the aborting-winner pattern; go to
   `IPAMCascadeProvisioningFailing` above for the cause.
2. If there are no errors, look for contention on the source pool row:
   ```sql
   SELECT pid, now() - xact_start AS duration, wait_event_type, wait_event, query
     FROM pg_stat_activity
     WHERE state != 'idle'
     ORDER BY duration DESC LIMIT 20;
   ```
   A long-running transaction holding the parent pool row blocks every carve out
   of it.

## IPAMCascadeResolutionLatencyHigh

Claims whose pools already existed are spending too long finding them. That path
is a single indexed read against `ipam_pool_identity` and should be a few
milliseconds.

1. Confirm the split on the "Pool resolution p95" panel. If only
   `provisioned="true"` is slow, this is first claims building chains and is
   expected — no action.
2. Check database health first; resolution latency tracks it directly. See
   [ipam-db-connection-pool-saturated.md](ipam-db-connection-pool-saturated.md).
3. Confirm the identity lookup is using its index:
   ```sql
   EXPLAIN ANALYZE
   SELECT pool_key FROM ipam_pool_identity
    WHERE class_name = '<class>' AND scope_digest = '<digest>';
   ```
4. Compare against `ipam_postgres_query_duration_seconds{query_name="lookup_pool_identity"}`
   to separate query time from transaction setup and connection acquisition.

## Escalation

Provisioning failures block all claims of the affected class. If the cause is an
exhausted or missing parent pool, the fix is operator action on the pool — page
the platform network team. Do not delete a provisioned pool to "retry": the
address space it holds is carved out of its parent and its children reference
it.

## Related

- [ipam-triage.md](ipam-triage.md)
- [ipam-claim-latency-high.md](ipam-claim-latency-high.md)
- [ipam-pool-exhaustion-imminent.md](ipam-pool-exhaustion-imminent.md)
