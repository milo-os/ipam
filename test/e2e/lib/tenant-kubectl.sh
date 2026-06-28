# shellcheck shell=sh
# tenant-kubectl.sh — reusable POSIX-sh helper that drives the IPAM aggregated
# apiserver as a specific tenant (project) by injecting the
# X-Remote-Extra-Iam.Miloapis.Com.Parent-* headers that Milo's front gate would
# normally forward. The IPAM server runs with requestheader auth
# (--requestheader-extra-headers-prefix, see config/base/deployment.yaml), so
# these headers become the UserInfo.Extra that internal/tenant.FromContext reads
# to scope storage keys to "project/<projectID>/...".
#
# Mechanism: open a local `kubectl proxy` (authenticated as the test's
# kubeconfig identity, which the requestheader front-proxy trusts) and `curl`
# the IPAM REST endpoints through it with the parent-* headers added. This is
# the exact proxy+curl pattern already used inline in multi-tenant's
# chainsaw-test.yaml, lifted here so every project-scoped suite shares one
# implementation.
#
# Usage (source it, then call the verbs):
#
#   . "$(dirname "$0")/../lib/tenant-kubectl.sh"   # or an absolute path
#   tk_start                                        # opens proxy, sets trap
#
#   # platform scope (no parent-* headers): pass project id ""  (empty)
#   tk_create ""             ippools  ""        "$pool_json"     # cluster-scoped
#   # project scope: pass the project id as the first arg
#   tk_create project-alpha  ippools  ""        "$pool_json"
#   tk_create project-alpha  ipclaims "$NS"     "$claim_json"
#   tk_get    project-alpha  ipclaims "$NS"     mt-alpha-claim
#   tk_get    project-alpha  ippools  ""        mt-alpha-pool
#   tk_delete project-alpha  ipclaims "$NS"     mt-alpha-claim
#   tk_apply  project-alpha  ipclaims "$NS"     "$claim_json"    # create-or-update
#
# Every verb prints the HTTP body (CIDR-bearing JSON for claims) to stdout and
# stores the numeric HTTP status code in the global TK_CODE. Exit code is 0 when
# the call completed (regardless of HTTP status — inspect TK_CODE / the body for
# 4xx/5xx); non-zero only on transport failure. Callers assert on TK_CODE.
#
# Contract:
#   * POSIX sh only. Deps: kubectl, curl (both already required by the suites).
#   * No global namespace pollution beyond the tk_* functions and TK_* vars.
#   * tk_start registers an EXIT trap that kills the proxy; safe to call once
#     per chainsaw `script:` block.

# ---- internal state -------------------------------------------------------

TK_PORT=""
TK_PROXY_PID=""
TK_CODE=""
TK_API_GROUP="ipam.miloapis.com"
TK_API_VERSION="v1alpha1"
# Parent api-group/type forwarded as the tenant's "kind". Projects use the
# resourcemanager.miloapis.com/Project pair; override TK_PARENT_TYPE /
# TK_PARENT_API_GROUP before a call to scope to a different parent kind.
TK_PARENT_API_GROUP="resourcemanager.miloapis.com"
TK_PARENT_TYPE="Project"

# tk_start [base_port] — launch `kubectl proxy` on a random high port and wait
# until it serves /api. Registers an EXIT trap to kill the proxy.
tk_start() {
  base="${1:-30000}"
  TK_PORT=$(awk "BEGIN{srand(); print ${base}+int(rand()*10000)}")
  kubectl proxy --port="$TK_PORT" >/dev/null 2>&1 &
  TK_PROXY_PID=$!
  # shellcheck disable=SC2064
  trap "tk_stop" EXIT
  i=0
  while [ "$i" -lt 40 ]; do
    if curl -sf "http://localhost:${TK_PORT}/api" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
    i=$((i + 1))
  done
  echo "tk_start: kubectl proxy did not become ready on port ${TK_PORT}" >&2
  return 1
}

# tk_stop — kill the proxy if running. Idempotent. Called by the EXIT trap.
tk_stop() {
  if [ -n "$TK_PROXY_PID" ]; then
    kill "$TK_PROXY_PID" 2>/dev/null || true
    TK_PROXY_PID=""
  fi
}

# _tk_url <namespace> <resource> [name] — build the IPAM REST path. An empty
# namespace produces a cluster-scoped path (used for IPPool, which is
# cluster-scoped at the API layer even though it is persisted per-tenant).
_tk_url() {
  _ns="$1"
  _res="$2"
  _name="$3"
  _base="http://localhost:${TK_PORT}/apis/${TK_API_GROUP}/${TK_API_VERSION}"
  if [ -n "$_ns" ]; then
    _path="${_base}/namespaces/${_ns}/${_res}"
  else
    _path="${_base}/${_res}"
  fi
  if [ -n "$_name" ]; then
    _path="${_path}/${_name}"
  fi
  printf '%s' "$_path"
}

# _tk_headers <project> — emit the curl -H args for the given project id. An
# empty project id emits NO parent-* headers, i.e. a platform-scoped request.
# The headers are written one per line and consumed via the calling function's
# `set --`.
_tk_curl_headers() {
  _project="$1"
  set -- -H "Content-Type: application/json" -H "Accept: application/json"
  if [ -n "$_project" ]; then
    set -- "$@" \
      -H "X-Remote-Extra-Iam.Miloapis.Com.Parent-Api-Group: ${TK_PARENT_API_GROUP}" \
      -H "X-Remote-Extra-Iam.Miloapis.Com.Parent-Type: ${TK_PARENT_TYPE}" \
      -H "X-Remote-Extra-Iam.Miloapis.Com.Parent-Name: ${_project}"
  fi
  # Re-emit args NUL-safe is overkill for header strings; print one per line.
  for _h in "$@"; do
    printf '%s\n' "$_h"
  done
}

# _tk_request <method> <project> <namespace> <resource> <name> <body>
# Core driver: performs the curl call, captures body on stdout and status in
# TK_CODE. <body> may be empty for GET/DELETE.
_tk_request() {
  _method="$1"
  _project="$2"
  _ns="$3"
  _res="$4"
  _name="$5"
  _body="$6"
  _url=$(_tk_url "$_ns" "$_res" "$_name")

  # Build the curl argv. We cannot easily round-trip an array in POSIX sh, so
  # assemble positional params directly.
  set -- -s -o /dev/stdout -w '\n__TK_CODE__%{http_code}' -X "$_method" "$_url" \
    -H "Content-Type: application/json" -H "Accept: application/json"
  if [ -n "$_project" ]; then
    set -- "$@" \
      -H "X-Remote-Extra-Iam.Miloapis.Com.Parent-Api-Group: ${TK_PARENT_API_GROUP}" \
      -H "X-Remote-Extra-Iam.Miloapis.Com.Parent-Type: ${TK_PARENT_TYPE}" \
      -H "X-Remote-Extra-Iam.Miloapis.Com.Parent-Name: ${_project}"
  fi
  if [ -n "$_body" ]; then
    set -- "$@" -d "$_body"
  fi

  _raw=$(curl "$@") || {
    echo "tk: curl transport failure for ${_method} ${_url}" >&2
    TK_CODE=""
    return 1
  }
  # Split trailing "\n__TK_CODE__<code>" off the response.
  TK_CODE=$(printf '%s' "$_raw" | sed -n 's/.*__TK_CODE__\([0-9][0-9]*\)$/\1/p')
  printf '%s' "$_raw" | sed 's/__TK_CODE__[0-9]*$//'
  return 0
}

# tk_create <project> <resource> <namespace> <body>
tk_create() { _tk_request POST "$1" "$3" "$2" "" "$4"; }

# tk_get <project> <resource> <namespace> <name>
tk_get() { _tk_request GET "$1" "$3" "$2" "$4" ""; }

# tk_list <project> <resource> <namespace>
tk_list() { _tk_request GET "$1" "$3" "$2" "" ""; }

# tk_delete <project> <resource> <namespace> <name>
tk_delete() { _tk_request DELETE "$1" "$3" "$2" "$4" ""; }

# tk_apply <project> <resource> <namespace> <body>
# Create-or-update: POST first; on 409 (already exists) fall back to PUT using
# the name parsed from the body via python3 (already used by the suites).
tk_apply() {
  _ap_project="$1"
  _ap_res="$2"
  _ap_ns="$3"
  _ap_body="$4"
  _out=$(_tk_request POST "$_ap_project" "$_ap_ns" "$_ap_res" "" "$_ap_body")
  if [ "$TK_CODE" = "409" ]; then
    _ap_name=$(printf '%s' "$_ap_body" | python3 -c 'import sys,json;print(json.load(sys.stdin)["metadata"]["name"])')
    _out=$(_tk_request PUT "$_ap_project" "$_ap_ns" "$_ap_res" "$_ap_name" "$_ap_body")
  fi
  printf '%s' "$_out"
}

# tk_cidr <json> — extract status.allocatedCIDR from a claim response body.
# Prints empty string when absent. Uses python3 (already a suite dependency).
tk_cidr() {
  printf '%s' "$1" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin)
    print(d.get("status",{}).get("allocatedCIDR",""))
except Exception:
    print("")'
}

# tk_jsonpath <json> <dotted.path> — tiny JSON reader for assertions, e.g.
# tk_jsonpath "$body" status.phase. Missing keys print empty string.
tk_jsonpath() {
  printf '%s' "$1" | python3 -c 'import sys,json
path=sys.argv[1].split(".") if len(sys.argv)>1 else []
try:
    d=json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
cur=d
for p in path:
    if isinstance(cur,dict) and p in cur:
        cur=cur[p]
    else:
        print(""); sys.exit(0)
print(cur if not isinstance(cur,(dict,list)) else json.dumps(cur))' "$2"
}
