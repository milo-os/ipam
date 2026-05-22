// watch-latency.js
//
// SLO probe for the IPClaim watch pipeline (LISTEN ipam_changelog + polling
// cursor): how long after a CREATE commits does the server start streaming
// the ADDED event to a watcher?
//
// Implementation note: k6's HTTP client buffers the entire response body —
// there is no true streaming. So we cannot timestamp individual events as
// they arrive. We can, however, observe `timings.waiting` (TTFB), which is
// the gap between sending the request and receiving the first byte. The
// apiserver does not begin emitting watch events until at least one event
// matching the resourceVersion cursor exists in its changelog, so TTFB on
// a `?watch=true&resourceVersion=R` request is effectively
// `(time_event_committed_to_changelog) - (request_send_time)`.
//
// Scenario:
//   - Two interleaved single-VU loops via shared-iterations:
//     - listAndCreate: lists current RV, creates one IPClaim with a
//       `created-at-ms` label, deletes it, sleeps, repeats.
//     - watch: in lockstep, opens a watch with resourceVersion=<previous-RV>
//       and timeoutSeconds=W. Computes lag = TTFB-anchored arrival time of
//       the first ADDED event minus the createdAt label value.
//   - Coordinated via a counter (creator runs first; watcher reads the
//     created-at value from its first ADDED event).
//
// Threshold:
//   - p(95) of ipam_watch_event_lag_ms < 1000ms
//
// Run setup-pools.js first.
//
// Configuration:
//   IPAM_API_URL    - Apiserver URL
//   ITERATIONS      - Number of probe iterations (default 30)
//   WATCH_TIMEOUT   - timeoutSeconds for each watch call (default 5)
//   POOL_NAME       - Source pool (default perf-prefix-0)
//   PROJECT         - Project tenant header (default ipam-perf-0)

import http from 'k6/http';
import { sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  API_BASE,
  deleteIPClaimForProject,
  nsFor,
  ipClaimPath,
  projectIDFor,
  withProjectTagged,
} from '../lib/ipam-client.js';

const ITERATIONS = parseInt(__ENV.ITERATIONS || '30');
const WATCH_TIMEOUT_S = parseInt(__ENV.WATCH_TIMEOUT || '5');
const PROJECT = __ENV.PROJECT || projectIDFor(0);
const POOL_NAME = __ENV.POOL_NAME || 'perf-prefix-0';
const NS = nsFor(0);
const CREATED_AT_LABEL = 'test.ipam.miloapis.com/created-at-ms';

const watchLag = new Trend('ipam_watch_event_lag_ms', true);
const watchTTFB = new Trend('ipam_watch_ttfb_ms', true);
const watchAdded = new Counter('ipam_watch_added_seen');
const watchMissing = new Counter('ipam_watch_missing_label');
const watchErrors = new Counter('ipam_watch_errors');
// Rate so the threshold scales with iteration count (rate<0.1 = up to 10%
// of successful watch responses may carry no ADDED event).
const watchEmpty = new Rate('ipam_watch_empty_responses');

export const options = {
  insecureSkipTLSVerify: __ENV.K6_INSECURE_SKIP_TLS_VERIFY !== 'false',
  scenarios: {
    probe: {
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
      maxDuration: '15m',
      exec: 'probe',
      tags: { scenario: 'probe' },
    },
  },
  thresholds: {
    'ipam_watch_event_lag_ms': ['p(95)<1000'],
    'ipam_watch_errors': ['count==0'],
    'ipam_watch_missing_label': ['count==0'],
    // If we see lots of empty watch responses, the changelog cursor is
    // wrong or events aren't propagating. Express as a rate so the bound
    // scales with iteration count (allow up to 10% empty responses).
    'ipam_watch_empty_responses': ['rate<0.1'],
  },
};

// Issue a GET against the IPClaim list to obtain the current
// resourceVersion. Returned as a string (k8s RVs are opaque).
function currentResourceVersion() {
  const params = withProjectTagged(PROJECT, 'list_ipclaims_rv');
  const res = http.get(`${API_BASE}${ipClaimPath(NS)}?limit=1`, params);
  if (res.status !== 200) {
    return '';
  }
  try {
    const body = JSON.parse(res.body);
    return (body && body.metadata && body.metadata.resourceVersion) || '';
  } catch (_e) {
    return '';
  }
}

// Issue a watch and parse the FIRST ADDED event from the buffered response.
// `timings.waiting` is k6's TTFB measurement: time between request send and
// the first response byte. Combined with our recorded request-send time, it
// pinpoints when the server started emitting events for our resourceVersion
// cursor — which is when our committed CREATE became visible to the watch.
function watchOnce(rv, expectedCreatedAtMs) {
  const params = withProjectTagged(PROJECT, 'watch_ipclaims');
  // Buffer the connection generously so the server can drive timeoutSeconds
  // without us cutting it off early.
  params.timeout = `${WATCH_TIMEOUT_S + 30}s`;

  const url =
    `${API_BASE}${ipClaimPath(NS)}?watch=true` +
    `&resourceVersion=${encodeURIComponent(rv)}` +
    `&timeoutSeconds=${WATCH_TIMEOUT_S}` +
    `&allowWatchBookmarks=true`;

  const sendAt = Date.now();
  const res = http.get(url, params);
  if (res.status !== 200) {
    watchErrors.add(1);
    console.error(`watch status=${res.status} body=${res.body}`);
    return;
  }

  const ttfbMs = (res.timings && res.timings.waiting) || 0;
  watchTTFB.add(ttfbMs);

  // Parse newline-delimited watch events. We only inspect the FIRST ADDED
  // event because TTFB anchors the moment the server began streaming.
  const body = typeof res.body === 'string' ? res.body : '';
  const lines = body.split('\n');
  let firstAdded = null;
  for (const line of lines) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    let evt;
    try {
      evt = JSON.parse(trimmed);
    } catch (_e) {
      continue;
    }
    if (evt.type === 'ADDED') {
      firstAdded = evt;
      break;
    }
  }
  if (!firstAdded) {
    watchEmpty.add(true);
    return;
  }
  watchEmpty.add(false);
  watchAdded.add(1);

  const labels = (firstAdded.object && firstAdded.object.metadata && firstAdded.object.metadata.labels) || {};
  const createdAt = labels[CREATED_AT_LABEL];
  if (!createdAt) {
    watchMissing.add(1);
    return;
  }
  const createdAtMs = parseInt(createdAt);
  if (Number.isNaN(createdAtMs) || createdAtMs <= 0) {
    watchMissing.add(1);
    return;
  }
  if (createdAtMs !== expectedCreatedAtMs) {
    // Our cursor is one event behind real time; the first ADDED event isn't
    // ours but a leftover. Don't record lag from a stale event.
    return;
  }

  // The server emitted the first byte at sendAt + sending + waiting. waiting
  // is TTFB. Anchor lag against the createdAt label value.
  const sending = (res.timings && res.timings.sending) || 0;
  const serverEmitAt = sendAt + sending + ttfbMs;
  const lag = serverEmitAt - createdAtMs;
  if (lag >= 0) {
    watchLag.add(lag);
  }
}

function createClaim(name, createdAtMs) {
  const labels = {};
  labels[CREATED_AT_LABEL] = String(createdAtMs);
  const body = {
    apiVersion: 'ipam.miloapis.com/v1alpha1',
    kind: 'IPClaim',
    metadata: { name, namespace: NS, labels },
    spec: {
      ipFamily: 'IPv4',
      prefixLength: 28,
      poolRef: { name: POOL_NAME },
      reclaimPolicy: 'Delete',
    },
  };
  const params = withProjectTagged(PROJECT, 'watch_ipclaim_create');
  return http.post(`${API_BASE}${ipClaimPath(NS)}`, JSON.stringify(body), params);
}

export function probe() {
  for (let i = 0; i < ITERATIONS; i++) {
    const name = `watch-probe-${i}`;
    // 1. Capture the current RV BEFORE creating, so the subsequent watch
    //    using this RV will see our CREATE.
    const rv = currentResourceVersion();
    if (!rv) {
      watchErrors.add(1);
      continue;
    }
    // 2. Issue the CREATE and stamp the label with the moment we sent it.
    const createdAtMs = Date.now();
    const createRes = createClaim(name, createdAtMs);
    if (createRes.status !== 201) {
      watchErrors.add(1);
      if (i < 5) {
        console.error(`create iter ${i}: status=${createRes.status} body=${createRes.body}`);
      }
      continue;
    }
    // 3. Open the watch from the pre-create RV. The server should emit our
    //    ADDED event as the first byte.
    watchOnce(rv, createdAtMs);
    // 4. Cleanup so the next iteration starts from a known state.
    deleteIPClaimForProject(NS, name, PROJECT);
    // Small spacing so consecutive probes don't pile up on the changelog.
    sleep(0.25);
  }
}
