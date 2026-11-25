#!/usr/bin/env bash
# Full-environment load test: deploy Milo + IPAM (quota admission ON) into a kind
# cluster, seed quota, smoke-check, run the k6 suite, collect diagnostics.
#
# Driven by Taskfile targets (`task load-test:*`) and by
# .github/workflows/load-test.yaml; every subcommand is also runnable by hand.
#
#   deploy-milo      Milo etcd + apiserver + controller-manager (config/overlays/load-test-milo)
#   deploy-ipam      IPAM + Postgres, quota ON (config/overlays/load-test[-latency])
#   register         IPAM APIService + quota fixtures, applied THROUGH Milo
#   seed             Organization + $PROJECT_COUNT Projects, wait for their quota grants
#   postgres-latency Add/clear $POSTGRES_LATENCY_MS on IPAM->Postgres round trips
#   smoke            One IPClaim through Milo; assert quota admitted it
#   run              Run every test in $LOAD_TESTS (task -x keeps k6's own exit code), keep going, exit non-zero if any failed
#   diagnostics      Dump logs/state of IPAM, Milo, Postgres into $DIAG_DIR
#
# Configuration (environment):
#   HOST_KUBECONFIG      kind host cluster kubeconfig        (.test-infra/kubeconfig)
#   MILO_KUBECONFIG      written by `seed`/`kubeconfigs`     (.milo/kubeconfig)
#   MILO_URL             where the runner reaches Milo       (https://localhost:30000)
#   MILO_TOKEN           Milo test token, system:masters     (test-admin-token)
#   LOAD_TEST_ORG        organization that owns the projects (ipam-load-test)
#   PROJECT_COUNT        perf projects                       (5)
#   PROJECT_PREFIX       project names are <prefix>-<n>      (ipam-perf; the k6 lib's default)
#   POSTGRES_LATENCY_MS  added per Postgres round trip, 0=off (0)
#   POSTGRES_LATENCY_JITTER_MS                               (0)
#   LOAD_TESTS           space-separated `task test/load:<name>` targets
#   VUS, DURATION, CASES, STEADY_NETWORKS, LOCATIONS, ...
#                        k6 knobs; `run` leaves them in the environment, and
#                        test/load/Taskfile.yaml hands them to k6 (unset = the
#                        script's default; see test/load/README.md)
#   DIAG_DIR             diagnostics output dir              (test/load/diagnostics)
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

HOST_KUBECONFIG="${HOST_KUBECONFIG:-.test-infra/kubeconfig}"
MILO_KUBECONFIG="${MILO_KUBECONFIG:-.milo/kubeconfig}"
MILO_URL="${MILO_URL:-https://localhost:30000}"
MILO_TOKEN="${MILO_TOKEN:-test-admin-token}"
LOAD_TEST_ORG="${LOAD_TEST_ORG:-ipam-load-test}"
PROJECT_COUNT="${PROJECT_COUNT:-5}"
PROJECT_PREFIX="${PROJECT_PREFIX:-ipam-perf}"
POSTGRES_LATENCY_MS="${POSTGRES_LATENCY_MS:-0}"
POSTGRES_LATENCY_JITTER_MS="${POSTGRES_LATENCY_JITTER_MS:-0}"
LOAD_TESTS="${LOAD_TESTS:-setup throughput class-chain exhaustion reads scale cleanup}"
DIAG_DIR="${DIAG_DIR:-test/load/diagnostics}"
MILO_DIR="$(dirname "${MILO_KUBECONFIG}")"
ORG_KUBECONFIG="${MILO_DIR}/org.kubeconfig"

log() { printf '==> %s\n' "$*"; }
host() { kubectl --kubeconfig "${HOST_KUBECONFIG}" "$@"; }
milo() { kubectl --kubeconfig "${MILO_KUBECONFIG}" "$@"; }
project_id() { printf '%s-%s' "${PROJECT_PREFIX}" "$1"; }
project_kubeconfig() { printf '%s/%s.kubeconfig' "${MILO_DIR}" "$(project_id "$1")"; }

# retry <attempts> <sleep-seconds> <cmd...>
retry() {
  local attempts="$1" delay="$2" i
  shift 2
  for i in $(seq 1 "${attempts}"); do
    if "$@"; then return 0; fi
    log "attempt ${i}/${attempts} failed: $*"
    sleep "${delay}"
  done
  return 1
}

# write_kubeconfig <path> <server-url>
write_kubeconfig() {
  mkdir -p "$(dirname "$1")"
  cat >"$1" <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: milo
    cluster:
      server: $2
      insecure-skip-tls-verify: true
users:
  - name: admin
    user:
      token: ${MILO_TOKEN}
contexts:
  - name: milo
    context:
      cluster: milo
      user: admin
current-context: milo
EOF
}

# Milo serves the organization and project control planes as URL-path prefixes
# of the one apiserver; IPAM's quota plugin builds the same project URL.
write_kubeconfigs() {
  write_kubeconfig "${MILO_KUBECONFIG}" "${MILO_URL}"
  write_kubeconfig "${ORG_KUBECONFIG}" \
    "${MILO_URL}/apis/resourcemanager.miloapis.com/v1alpha1/organizations/${LOAD_TEST_ORG}/control-plane"
  local n
  for n in $(seq 0 $((PROJECT_COUNT - 1))); do
    write_kubeconfig "$(project_kubeconfig "${n}")" \
      "${MILO_URL}/apis/resourcemanager.miloapis.com/v1alpha1/projects/$(project_id "${n}")/control-plane"
  done
}

cmd_kubeconfigs() { write_kubeconfigs; log "wrote kubeconfigs under ${MILO_DIR}"; }

cmd_deploy_milo() {
  log "applying Milo control plane"
  # Server-side not needed; client-side apply of a remote-base kustomize render.
  kustomize build config/overlays/load-test-milo | host apply -f -

  log "waiting for etcd"
  host -n milo-system wait helmrelease/etcd --for=condition=Ready --timeout=300s
  host -n milo-system wait pod -l app.kubernetes.io/component=etcd --for=condition=Ready --timeout=300s
  log "waiting for milo-apiserver and milo-controller-manager"
  host -n milo-system rollout status deployment/milo-apiserver --timeout=300s
  host -n milo-system rollout status deployment/milo-controller-manager --timeout=300s

  write_kubeconfigs
  log "waiting for Milo to answer on ${MILO_URL}"
  retry 36 5 milo get --raw /readyz >/dev/null
}

cmd_deploy_ipam() {
  local overlay=load-test
  if [ "${POSTGRES_LATENCY_MS}" -gt 0 ]; then overlay=load-test-latency; fi
  log "deploying IPAM from config/overlays/${overlay} (quota ON)"

  # The apiserver verifies Milo's aggregator by the CA that signed Milo's
  # proxy-client certificate. It must exist before the pod starts: a missing
  # ConfigMap crash-loops the container rather than failing the mount.
  host create namespace ipam-system --dry-run=client -o yaml | host apply -f -
  host -n milo-system get secret milo-ca-tls -o jsonpath='{.data.ca\.crt}' | base64 -d >"${MILO_DIR}/milo-ca.crt"
  test -s "${MILO_DIR}/milo-ca.crt"
  host -n ipam-system create configmap control-plane-ca \
    --from-file=ca.crt="${MILO_DIR}/milo-ca.crt" --dry-run=client -o yaml | host apply -f -

  kustomize build "config/overlays/${overlay}" | host apply -f -

  host -n ipam-system wait certificate/ipam-tls --for=condition=Ready --timeout=120s
  host -n ipam-system wait helmrelease/postgres --for=condition=Ready --timeout=300s
  host -n ipam-system wait pod -l app.kubernetes.io/name=postgresql --for=condition=Ready --timeout=180s
  host -n ipam-system wait pod -l app=ipam-apiserver --for=condition=Ready --timeout=300s
}

cmd_register() {
  log "registering IPAM behind Milo and installing quota fixtures"
  # Registration and policies reference each other (a ClaimCreationPolicy needs
  # an Active ResourceRegistration), so the first pass can partly fail.
  retry 12 10 bash -c "kustomize build config/overlays/load-test-milo-api | kubectl --kubeconfig '${MILO_KUBECONFIG}' apply -f -"

  milo wait resourceregistration/ipam-load-test-ipclaims-per-project --for=condition=Active --timeout=120s
  # A policy applied in the same pass as its registration can be validated
  # before the controllers see the registration, and then stays Ready=False
  # ("resource type is not available for quota management") without being
  # re-evaluated. With the registration Active, apply the policies again.
  milo delete claimcreationpolicy/ipam-load-test-ipclaim grantcreationpolicy/ipam-load-test-ipclaim-grant --ignore-not-found
  retry 12 5 bash -c "kustomize build config/overlays/load-test-milo-api | kubectl --kubeconfig '${MILO_KUBECONFIG}' apply -f -"
  milo wait claimcreationpolicy/ipam-load-test-ipclaim --for=condition=Ready --timeout=120s
  milo wait grantcreationpolicy/ipam-load-test-ipclaim-grant --for=condition=Ready --timeout=120s
  milo wait apiservice/v1alpha1.ipam.miloapis.com --for=condition=Available --timeout=180s
}

cmd_seed() {
  write_kubeconfigs
  # Created here, after `register` has installed Milo's webhooks: the
  # Organization webhook is what creates its organization-<name> namespace.
  log "creating organization ${LOAD_TEST_ORG}"
  retry 12 5 milo apply -f - <<EOF
apiVersion: resourcemanager.miloapis.com/v1alpha1
kind: Organization
metadata:
  name: ${LOAD_TEST_ORG}
  labels:
    ipam.miloapis.com/load-test: "true"
spec:
  type: Standard
EOF
  log "waiting for organization ${LOAD_TEST_ORG}"
  retry 24 5 milo get namespace "organization-${LOAD_TEST_ORG}" >/dev/null

  local n id
  for n in $(seq 0 $((PROJECT_COUNT - 1))); do
    id="$(project_id "${n}")"
    log "creating project ${id}"
    retry 12 5 kubectl --kubeconfig "${ORG_KUBECONFIG}" apply -f - <<EOF
apiVersion: resourcemanager.miloapis.com/v1alpha1
kind: Project
metadata:
  name: ${id}
  labels:
    ipam.miloapis.com/load-test: "true"
spec:
  ownerRef:
    kind: Organization
    name: ${LOAD_TEST_ORG}
EOF
  done
  for n in $(seq 0 $((PROJECT_COUNT - 1))); do
    id="$(project_id "${n}")"
    kubectl --kubeconfig "${ORG_KUBECONFIG}" wait "project/${id}" --for=condition=Ready --timeout=300s
    # The grant is created by the GrantCreationPolicy once the project exists;
    # without an Active grant the first claim would be denied for quota, which
    # would look like a benchmark failure.
    log "waiting for ${id} quota grant"
    retry 36 5 kubectl --kubeconfig "$(project_kubeconfig "${n}")" -n milo-system \
      wait "resourcegrant/ipam-load-test-${id}" --for=condition=Active --timeout=10s
  done
}

cmd_postgres_latency() {
  if ! host -n ipam-system get deploy/toxiproxy-postgres >/dev/null 2>&1; then
    if [ "${POSTGRES_LATENCY_MS}" -gt 0 ]; then
      echo "POSTGRES_LATENCY_MS=${POSTGRES_LATENCY_MS} but toxiproxy is not deployed;" \
        "deploy-ipam must run with the same value so it picks the load-test-latency overlay" >&2
      return 1
    fi
    log "no toxiproxy deployed; Postgres latency is off"
    return 0
  fi
  host -n ipam-system rollout status deploy/toxiproxy-postgres --timeout=120s
  tox() { host -n ipam-system exec deploy/toxiproxy-postgres -- /toxiproxy-cli "$@"; }
  tox toxic delete --toxicName pg-latency postgres >/dev/null 2>&1 || true
  if [ "${POSTGRES_LATENCY_MS}" -gt 0 ]; then
    # Latency on the downstream (server -> client) direction only, so one
    # request/response round trip costs exactly LATENCY_MS extra.
    tox toxic add --type latency --toxicName pg-latency --downstream \
      --attribute "latency=${POSTGRES_LATENCY_MS}" \
      --attribute "jitter=${POSTGRES_LATENCY_JITTER_MS}" postgres
    log "IPAM->Postgres round trips now +${POSTGRES_LATENCY_MS}ms (jitter ${POSTGRES_LATENCY_JITTER_MS}ms)"
  else
    log "cleared Postgres latency toxic"
  fi
  tox inspect postgres
}

# Create one IPClaim through Milo's project control plane and prove quota
# admission ran: a ResourceClaim must exist for it, created by the quota plugin,
# and be Granted, and the project's bucket must show it allocated.
cmd_smoke() {
  local kc ns=load-test-smoke
  kc="$(project_kubeconfig 0)"
  [ -f "${kc}" ] || write_kubeconfigs
  pk() { kubectl --kubeconfig "${kc}" "$@"; }
  cleanup() {
    pk -n "${ns}" delete ipclaim smoke-claim --ignore-not-found --wait=false >/dev/null 2>&1 || true
    pk delete ippool smoke-pool --ignore-not-found --wait=false >/dev/null 2>&1 || true
    pk delete ipclass smoke-class --ignore-not-found --wait=false >/dev/null 2>&1 || true
    pk delete namespace "${ns}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  }
  trap cleanup RETURN

  log "smoke: IPClaim through Milo ($(project_id 0)) with quota ON"
  pk create namespace "${ns}" --dry-run=client -o yaml | pk apply -f -
  pk apply -f - <<EOF
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPClass
metadata:
  name: smoke-class
spec:
  ipFamily: IPv4
---
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPPool
metadata:
  name: smoke-pool
spec:
  classNames: [smoke-class]
  cidr: 10.250.0.0/20
  ipFamily: IPv4
  visibility: consumer
  allocation:
    minPrefixLength: 24
    maxPrefixLength: 28
    strategy: FirstFit
EOF
  retry 30 2 bash -c "[ \"\$(kubectl --kubeconfig '${kc}' get ippool smoke-pool -o jsonpath='{.status.phase}')\" = Ready ]"
  pk apply -f - <<EOF
apiVersion: ipam.miloapis.com/v1alpha1
kind: IPClaim
metadata:
  name: smoke-claim
  namespace: ${ns}
spec:
  className: smoke-class
  prefixLength: 24
  reclaimPolicy: Delete
EOF
  local phase cidr
  phase="$(pk -n "${ns}" get ipclaim smoke-claim -o jsonpath='{.status.phase}')"
  cidr="$(pk -n "${ns}" get ipclaim smoke-claim -o jsonpath='{.status.allocatedCIDR}')"
  [ "${phase}" = Bound ] || { echo "FAIL: claim phase=${phase}, want Bound" >&2; return 1; }
  log "claim Bound with ${cidr} (synchronously)"

  local granted
  granted="$(pk -n "${ns}" get resourceclaims.quota.miloapis.com \
    -l quota.miloapis.com/policy=ipam-load-test-ipclaim \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Granted")].status}{"\n"}{end}')"
  [ "${granted}" = True ] || { echo "FAIL: no Granted ResourceClaim from quota admission (got '${granted}')" >&2; return 1; }
  log "quota admission created a Granted ResourceClaim"

  local allocated
  allocated="$(pk -n milo-system get allowancebuckets.quota.miloapis.com \
    -o jsonpath='{.items[0].status.allocated}')"
  log "AllowanceBucket allocated=${allocated}"
  [ "${allocated:-0}" -ge 1 ] || { echo "FAIL: AllowanceBucket shows nothing allocated" >&2; return 1; }
  log "smoke passed"
}

cmd_run() {
  # k6 reaches IPAM through Milo's front door, one control-plane path per
  # project, so quota admission sees a project context. These are the interface
  # to test/load/ (see test/load/lib/ipam-client.js and test/load/README.md);
  # the Taskfile targets read them from the environment.
  export IPAM_TENANCY="${IPAM_TENANCY:-milo}"
  export PROJECT_PREFIX
  export IPAM_API_URL="${IPAM_API_URL:-${MILO_URL}}"
  export IPAM_TOKEN="${IPAM_TOKEN:-${MILO_TOKEN}}"
  export KUBECONFIG="${MILO_KUBECONFIG}"
  export PROJECT_COUNT
  mkdir -p test/load/results

  local failed=() t rc setup_failed=0
  for t in ${LOAD_TESTS}; do
    if [ "${setup_failed}" = 1 ] && [ "${t}" != cleanup ]; then continue; fi
    log "k6: test/load:${t}"
    # -x makes task exit with the failing command's own code: k6's 99 for a
    # crossed threshold, 107 for a script error, 108 for an abort. Without it
    # task reports 201 for all of them.
    set +e
    task --yes -x "test/load:${t}" 2>&1 | tee "test/load/results/${t}.log"
    rc=${PIPESTATUS[0]}
    set -e
    echo "${rc}" >"test/load/results/${t}.exit"
    if [ "${rc}" -ne 0 ]; then
      case "${rc}" in
        99) log "FAILED test/load:${t}: a k6 threshold was crossed (exit 99)" ;;
        107) log "FAILED test/load:${t}: script error (exit 107)" ;;
        108) log "FAILED test/load:${t}: the run aborted itself (exit 108)" ;;
        *) log "FAILED test/load:${t}: exit ${rc}" ;;
      esac
      # cleanup failing does not invalidate the numbers already collected.
      if [ "${t}" != cleanup ]; then failed+=("${t}:${rc}"); fi
      # Nothing after a failed setup can produce a meaningful number; only
      # cleanup still runs.
      if [ "${t}" = setup ]; then setup_failed=1; fi
    fi
  done
  if [ "${#failed[@]}" -gt 0 ]; then
    log "failed (test:exit): ${failed[*]}"
    return 1
  fi
  log "all load tests passed their thresholds"
}

cmd_diagnostics() {
  mkdir -p "${DIAG_DIR}"
  local d="${DIAG_DIR}"
  # Everything here is best effort: this runs on a failing environment.
  hostc() { host "$@" || true; }
  miloc() { milo "$@" || true; }

  log "collecting diagnostics into ${d}"
  hostc get pods -A -o wide >"${d}/pods.txt" 2>&1
  hostc get events -A --sort-by=.lastTimestamp >"${d}/events.txt" 2>&1
  hostc get helmrelease -A >"${d}/helmreleases.txt" 2>&1
  hostc get certificates,certificaterequests -A >"${d}/certificates.txt" 2>&1
  hostc -n ipam-system describe pods >"${d}/ipam-describe-pods.txt" 2>&1
  hostc -n milo-system describe pods >"${d}/milo-describe-pods.txt" 2>&1
  hostc -n ipam-system get networkpolicy -o yaml >"${d}/ipam-networkpolicy.yaml" 2>&1

  # IPAM apiserver: current and previous container, init container included.
  hostc -n ipam-system logs -l app=ipam-apiserver --all-containers --tail=-1 >"${d}/ipam-apiserver.log" 2>&1
  hostc -n ipam-system logs -l app=ipam-apiserver --all-containers --previous --tail=-1 >"${d}/ipam-apiserver.previous.log" 2>&1
  # Metrics: request latency, allocator, retention, and the quota plugin's
  # milo_quota_* admission counters. Served unauthenticated at /metrics.
  hostc get --raw "/api/v1/namespaces/ipam-system/services/https:ipam-apiserver:443/proxy/metrics" >"${d}/ipam-metrics.txt" 2>&1

  # Milo
  hostc -n milo-system logs -l app.kubernetes.io/name=milo-apiserver --all-containers --tail=-1 >"${d}/milo-apiserver.log" 2>&1
  hostc -n milo-system logs -l app.kubernetes.io/name=milo-apiserver --all-containers --previous --tail=-1 >"${d}/milo-apiserver.previous.log" 2>&1
  hostc -n milo-system logs -l app.kubernetes.io/name=milo-controller-manager --all-containers --tail=-1 >"${d}/milo-controller-manager.log" 2>&1
  hostc -n milo-system logs -l app.kubernetes.io/component=etcd --all-containers --tail=2000 >"${d}/etcd.log" 2>&1
  # Milo's /metrics needs a credential, unlike IPAM's.
  miloc get --raw /metrics >"${d}/milo-apiserver-metrics.txt" 2>&1
  # Milo's own view: is IPAM reachable behind it, and what is quota doing.
  miloc get apiservice v1alpha1.ipam.miloapis.com -o yaml >"${d}/milo-ipam-apiservice.yaml" 2>&1
  miloc get resourceregistrations,claimcreationpolicies,grantcreationpolicies -o wide >"${d}/milo-quota-config.txt" 2>&1
  local n kc
  for n in $(seq 0 $((PROJECT_COUNT - 1))); do
    kc="$(project_kubeconfig "${n}")"
    [ -f "${kc}" ] || continue
    {
      echo "### $(project_id "${n}")"
      kubectl --kubeconfig "${kc}" get resourcegrants,allowancebuckets -n milo-system -o wide || true
      # Claims that are not Granted are the ones that blocked or denied a create.
      kubectl --kubeconfig "${kc}" get resourceclaims.quota.miloapis.com -A \
        -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,GRANTED:.status.conditions[0].status,REASON:.status.conditions[0].reason \
        | awk 'NR==1 || $3 != "True"' | head -50 || true
    } >>"${d}/milo-quota-projects.txt" 2>&1
  done

  # Postgres
  hostc -n ipam-system logs -l app.kubernetes.io/name=postgresql --all-containers --tail=-1 >"${d}/postgres.log" 2>&1
  hostc -n ipam-system logs -l app.kubernetes.io/name=toxiproxy-postgres --all-containers --tail=500 >"${d}/toxiproxy.log" 2>&1
  hostc -n ipam-system exec postgres-postgresql-0 -- env PGPASSWORD=devpassword psql -U ipam -d ipam -x -c \
    "select state, wait_event_type, wait_event, count(*) from pg_stat_activity where datname='ipam' group by 1,2,3 order by 4 desc" \
    -c "select count(*) as waiting_locks from pg_locks where not granted" \
    -c "select relname, n_live_tup, n_dead_tup from pg_stat_user_tables order by n_live_tup desc limit 20" \
    >"${d}/postgres-state.txt" 2>&1

  # Node pressure is the usual cause of a flaky benchmark on a shared runner.
  docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}' >"${d}/docker-stats.txt" 2>&1 || true
  log "diagnostics written to ${d}"
}

case "${1:-}" in
  kubeconfigs) cmd_kubeconfigs ;;
  deploy-milo) cmd_deploy_milo ;;
  deploy-ipam) cmd_deploy_ipam ;;
  register) cmd_register ;;
  seed) cmd_seed ;;
  postgres-latency) cmd_postgres_latency ;;
  smoke) cmd_smoke ;;
  run) cmd_run ;;
  diagnostics) cmd_diagnostics ;;
  *)
    sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//' >&2
    exit 2
    ;;
esac
