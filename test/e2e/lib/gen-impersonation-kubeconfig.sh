#!/usr/bin/env sh
# gen-impersonation-kubeconfig.sh — generate a kubeconfig whose contexts carry
# Kubernetes impersonation (act-as + act-as-groups + act-as-user-extra) so the
# native-chainsaw multi-tenant / tenant-isolation suites can drive the IPAM
# aggregated apiserver as a specific project tenant WITHOUT any curl/proxy bash.
#
# WHY IMPERSONATION (not requestheader curl):
#   IPAM scopes storage per tenant from UserInfo.Extra keys
#     iam.miloapis.com/parent-name / parent-type / parent-api-group
#   (see internal/tenant.FromContext). Milo's front gate normally forwards
#   these as X-Remote-Extra-* requestheader extras. Kubernetes impersonation
#   carries the SAME extras: a kubeconfig AuthInfo's act-as-user-extra becomes
#   Impersonate-Extra-* request headers, which the kube-apiserver front-proxy
#   re-emits to the aggregated apiserver as X-Remote-Extra-* — i.e. it lands in
#   UserInfo.Extra exactly like the front gate. The allocator only reads
#   UserInfo.Extra, so an impersonated extra is equivalent to a front-gate
#   requestheader extra for tenant scoping. Impersonation lets every op be a
#   native chainsaw create/apply/assert/error instead of a curl in a script.
#
# OUTPUT: a flattened, self-contained kubeconfig at $1 with three contexts,
# all named deterministically so chainsaw `clusters:` can reference them
# regardless of which underlying cluster/context the e2e run targets:
#   * tenant-platform      — the unimpersonated base context (platform scope)
#   * tenant-project-alpha — act-as e2e-tenant-tester, parent-name=project-alpha
#   * tenant-project-beta  — act-as e2e-tenant-tester, parent-name=project-beta,
#                            plus group system:project:project-beta so the
#                            cross-project `use` ClusterRoleBinding matches.
#
# The impersonated user (e2e-tenant-tester) is deliberately NOT cluster-admin;
# the suites' RBAC (resources/impersonation-rbac.yaml) grants it exactly the
# ipam verbs it needs, so the cross-project deny assertions are strict (no
# cluster-admin bypass).
#
# Usage:
#   gen-impersonation-kubeconfig.sh <out-path> [source-context]
# source-context defaults to $E2E_KUBE_CONTEXT, then the current context.

set -eu

OUT="${1:?usage: gen-impersonation-kubeconfig.sh <out-path> [source-context]}"
SRC_CTX="${2:-${E2E_KUBE_CONTEXT:-}}"

# The impersonated tenant user and the project parent identity constants. These
# mirror internal/tenant.Extra* and the resourcemanager Project parent pair.
TENANT_USER="e2e-tenant-tester"
PARENT_API_GROUP="resourcemanager.miloapis.com"
PARENT_TYPE="Project"

# Resolve the source context (the cluster the e2e run targets).
if [ -z "$SRC_CTX" ]; then
  SRC_CTX="$(kubectl config current-context)"
fi

# Flatten the source context into a self-contained base kubeconfig (embeds CA +
# client creds), then assemble the OUTPUT file by hand. We cannot round-trip the
# final file through `kubectl config view`: recent kubectl (v1.36) silently
# drops the act-as / act-as-groups / act-as-user-extra impersonation fields when
# re-serialising a kubeconfig, so the impersonation would be lost. Instead we
# read the concrete cluster + base-credential material via jsonpath (which
# kubectl renders faithfully) and write the impersonation users/contexts as
# literal YAML — client-go reads act-as* on load even though kubectl-view omits
# them on output.
base="$(mktemp)"
kubectl --context "$SRC_CTX" config view --minify --flatten >"$base"

# Discover the cluster + base-user names inside the flattened config.
BASE_CLUSTER="$(KUBECONFIG="$base" kubectl config view -o jsonpath="{.contexts[?(@.name=='${SRC_CTX}')].context.cluster}")"
BASE_USER="$(KUBECONFIG="$base" kubectl config view -o jsonpath="{.contexts[?(@.name=='${SRC_CTX}')].context.user}")"

# Cluster connection material (server + embedded CA).
C_SERVER="$(KUBECONFIG="$base" kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='${BASE_CLUSTER}')].cluster.server}")"
C_CA="$(KUBECONFIG="$base" kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='${BASE_CLUSTER}')].cluster.certificate-authority-data}")"
C_INSECURE="$(KUBECONFIG="$base" kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='${BASE_CLUSTER}')].cluster.insecure-skip-tls-verify}")"

# Extract the base user's concrete auth material so each impersonation user can
# authenticate as the trusted identity (the one the front-proxy accepts) and
# then impersonate the tenant. Either cert+key or a token will be present.
B_CERT="$(KUBECONFIG="$base" kubectl config view --raw -o jsonpath="{.users[?(@.name=='${BASE_USER}')].user.client-certificate-data}")"
B_KEY="$(KUBECONFIG="$base" kubectl config view --raw -o jsonpath="{.users[?(@.name=='${BASE_USER}')].user.client-key-data}")"
B_TOKEN="$(KUBECONFIG="$base" kubectl config view --raw -o jsonpath="{.users[?(@.name=='${BASE_USER}')].user.token}")"

# Emit the YAML for one impersonation user. Writes the act-as* fields directly
# (kubectl config set cannot express the act-as-user-extra map-of-list type).
emit_user() {
  _proj="$1"   # project id, e.g. project-alpha
  _group="$2"  # single extra group or empty
  printf '%s\n' "- name: tenant-${_proj}-as"
  printf '%s\n' "  user:"
  if [ -n "$B_CERT" ]; then
    printf '%s\n' "    client-certificate-data: ${B_CERT}"
  fi
  if [ -n "$B_KEY" ]; then
    printf '%s\n' "    client-key-data: ${B_KEY}"
  fi
  if [ -n "$B_TOKEN" ]; then
    printf '%s\n' "    token: ${B_TOKEN}"
  fi
  printf '%s\n' "    act-as: ${TENANT_USER}"
  if [ -n "$_group" ]; then
    printf '%s\n' "    act-as-groups:"
    printf '%s\n' "      - ${_group}"
  fi
  printf '%s\n' "    act-as-user-extra:"
  printf '%s\n' "      iam.miloapis.com/parent-api-group:"
  printf '%s\n' "        - ${PARENT_API_GROUP}"
  printf '%s\n' "      iam.miloapis.com/parent-type:"
  printf '%s\n' "        - ${PARENT_TYPE}"
  printf '%s\n' "      iam.miloapis.com/parent-name:"
  printf '%s\n' "        - ${_proj}"
}

emit_context() {
  _name="$1"; _user="$2"
  printf '%s\n' "- name: ${_name}"
  printf '%s\n' "  context:"
  printf '%s\n' "    cluster: ${BASE_CLUSTER}"
  printf '%s\n' "    user: ${_user}"
}

{
  printf '%s\n' "apiVersion: v1"
  printf '%s\n' "kind: Config"
  printf '%s\n' "current-context: tenant-platform"
  printf '%s\n' "clusters:"
  printf '%s\n' "- name: ${BASE_CLUSTER}"
  printf '%s\n' "  cluster:"
  printf '%s\n' "    server: ${C_SERVER}"
  if [ -n "$C_CA" ]; then
    printf '%s\n' "    certificate-authority-data: ${C_CA}"
  fi
  if [ "$C_INSECURE" = "true" ]; then
    printf '%s\n' "    insecure-skip-tls-verify: true"
  fi
  printf '%s\n' "users:"
  # Base (unimpersonated) user, reused by the platform context.
  printf '%s\n' "- name: ${BASE_USER}"
  printf '%s\n' "  user:"
  if [ -n "$B_CERT" ]; then printf '%s\n' "    client-certificate-data: ${B_CERT}"; fi
  if [ -n "$B_KEY" ];  then printf '%s\n' "    client-key-data: ${B_KEY}"; fi
  if [ -n "$B_TOKEN" ];then printf '%s\n' "    token: ${B_TOKEN}"; fi
  # project-alpha: no extra groups (only owns its own pools).
  emit_user project-alpha ""
  # project-beta: carries system:project:project-beta so the cross-project
  # `use` ClusterRoleBinding (subjects: Group system:project:project-beta)
  # authorizes shared-pool claims. Private-pool claims still fail because the
  # grant is scoped by resourceNames, keeping deny assertions strict.
  emit_user project-beta "system:project:project-beta"
  printf '%s\n' "contexts:"
  emit_context tenant-project-alpha tenant-project-alpha-as
  emit_context tenant-project-beta tenant-project-beta-as
  # Stable-named platform (unimpersonated) context reusing the base user.
  emit_context tenant-platform "$BASE_USER"
} >"$OUT"

rm -f "$base"

echo "wrote impersonation kubeconfig: $OUT (source context: $SRC_CTX)"
echo "  contexts: tenant-platform (platform), tenant-project-alpha, tenant-project-beta"
