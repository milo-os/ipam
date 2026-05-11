# Runbook: IPAMReadLatencyHigh

**Alert:** `IPAMReadLatencyHigh`
**Severity:** warning
**SLO targets:** prefix list p95 < 200ms; claim GET p95 < 100ms
**Alert threshold:** apiserver p95 > 500ms for 5m on `verb=~"list|get"` against IPAM resources

## Background — known steady-state issue

The 2026-05-09 perf-tester baseline showed the read path is **already failing
the consumer SLO**, even at idle:

| Test | Measured p95 | SLO | Overshoot |
|---|---|---|---|
| `read-latency` prefix list | 544ms | 200ms | 2.7× |
| `read-latency` claim GET | 311ms | 100ms | 3.1× |

The write path is healthy — `prefix-claim-throughput` p95 was 42ms with
12× headroom against the 500ms threshold. The problem is read-side only.

The alert threshold (500ms) is intentionally permissive: it does **not**
fire on the existing steady-state issue (which is real but acknowledged),
only on a fresh regression on top of it.

## Diagnostic theory

The fact that **claim GET p95 (311ms) is close to prefix list p95 (544ms)**
despite the result-set sizes differing by orders of magnitude is suspicious.
If the bottleneck were DB scan time, GET would be much faster than LIST
because it's a single-row lookup. The two latencies converging suggests a
**per-request fixed cost** — most likely:

- Apiserver authentication / authorization middleware
- Object serialization (encoding/json or protobuf)
- TLS handshake / connection setup (less likely — pings would catch this)
- Aggregator-layer plumbing (RequestHeader→aggregator→IPAM apiserver round trip)

Confirmation strategy: profile the read path with `pprof` and split the
request-duration histogram by `subresource` and by webhook latency.

## Source signals

- `apiserver_request_duration_seconds_bucket{verb=~"list|get", resource=~"ipprefixes|ipprefixclaims|ipaddresses|ipaddressclaims|asnpools|asnclaims|ippools"}` — primary
- `apiserver_request_total` — request counts per verb/resource for context
- Provider dashboard → "Apiserver request mix" row → "Apiserver request p95 by verb"
- Consumer dashboard → "Read path (list / get / watch)" row

## Diagnose

1. **Confirm the regression is fresh, not the baseline.** Compare current
   p95 to the 2026-05-09 baseline (~544ms list, ~311ms GET). Anything well
   above those is a new regression; anything close is the steady-state issue.
2. **Localize by resource.** Run the alert query without `sum by` and find
   the worst {verb, resource} combination.
   ```promql
   topk(5, histogram_quantile(0.95,
     sum by (le, verb, resource) (
       rate(apiserver_request_duration_seconds_bucket{
         verb=~"list|get",
         resource=~"ipprefixes|ipprefixclaims|ipaddresses|ipaddressclaims|asnpools|asnclaims|ippools"
       }[5m])
     )
   ))
   ```
3. **Check correlated signals.**
   - Apiserver request rate spike? `sum(rate(apiserver_request_total[5m]))`
   - Pod CPU saturation? Provider dashboard → Pod resources row
   - Postgres latency on the read path? (Once `ipam_postgres_query_duration_seconds`
     lands, split by `query_name`.)
4. **Profile if persistent.** `kubectl exec` into the apiserver pod and grab
   a CPU profile:
   ```bash
   kubectl exec -n ipam-system deploy/ipam-apiserver -- \
     curl -s 'http://localhost:6060/debug/pprof/profile?seconds=30' \
     > /tmp/ipam-cpu.pprof
   go tool pprof -top /tmp/ipam-cpu.pprof
   ```
5. **Check apiserver flag-set.** Discovery / OpenAPI generation and
   admission chains can dominate per-request cost on small resources.

## Mitigate

Steady-state mitigations that the platform team is likely to track separately:

- **Reduce admission overhead.** Audit the admission chain for IPAM resources
  and remove webhooks that aren't needed on read paths.
- **Cache discovery / OpenAPI.** Make sure the aggregator-layer caches are
  warm and not being invalidated under load.
- **Field selectors / partial-object metadata.** If consumers list large
  collections, push them toward `?limit=N` paging or PartialObjectMetadata.
- **Indexer review.** Verify the registry's postgres indexers are populated
  and not being rebuilt on every list.

For an active regression alert (over and above the baseline):

- Check recent apiserver pod restarts; cold start can show as elevated p95
  for a few minutes.
- Roll the apiserver if you suspect leaked goroutines or a stuck cache.
- Correlate with the deploy SHA — most read-path regressions ride in on a
  middleware change.

## Related

- `ipam-claim-latency-high.md` — write-path counterpart
- Provider dashboard "Apiserver request mix" row carries the same panels
- Consumer dashboard "Read path" row breaks the signal down by namespace
