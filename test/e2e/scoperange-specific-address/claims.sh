# Helpers for scoperange-specific-address. Source after ../lib/ipam.sh.

# claim_yaml <name> <class> <Block|ScopeRange> <address|-> <network> [location] [tier]
# Prints an IPClaim manifest. tier labels the claim so cleanup can delete
# endpoints before the ranges holding them.
claim_yaml() {
  _n="$1"; _c="$2"; _t="$3"; _a="$4"; _net="$5"; _loc="${6:-}"; _tier="${7:-misc}"
  printf 'apiVersion: ipam.miloapis.com/v1alpha1\nkind: IPClaim\nmetadata:\n  name: %s\n  labels:\n    suite: scoperange-specific-address\n    ssa-tier: %s\nspec:\n  className: %s\n' "$_n" "$_tier" "$_c"
  if [ "$_t" = "ScopeRange" ]; then
    printf '  target: ScopeRange\n'
  fi
  if [ "$_a" != "-" ]; then
    printf '  address: "%s"\n' "$_a"
  fi
  printf '  scope:\n    network:\n      apiGroup: networking.datumapis.com\n      kind: Network\n      name: %s\n' "$_net"
  if [ -n "$_loc" ]; then
    printf '    location:\n      apiGroup: networking.datumapis.com\n      kind: Location\n      name: %s\n' "$_loc"
  fi
}

# level_cidr <class> role=value... — the CIDR of the one pool <class>
# provisioned for that scope. Fails unless there is exactly one.
level_cidr() {
  _names=$(pools_for_scope "$@")
  _count=$(printf '%s' "$_names" | grep -c . || true)
  if [ "$_count" != "1" ]; then
    echo "FAIL: expected one pool of class $1 for [$*], found $_count" >&2
    return 1
  fi
  pool_cidr "$_names"
}

# ssa_pool_count — every IPPool a class of this suite provisioned. Compared
# before and after a refusal or a dry-run, it proves nothing was built.
ssa_pool_count() {
  $POOL_KUBECTL get ippool -o json \
    | jq '[.items[] | select((.spec.classRef.name // "") | startswith("ssa-"))] | length'
}

# root_allocated <pool> — what the root has handed out. A carve left behind by
# a refused request would show up here and nowhere else a tenant can read.
root_allocated() {
  $POOL_KUBECTL get ippool "$1" -o jsonpath='{.status.capacity.allocated}'
}

# expect_unchanged <label> <before> <after>
expect_unchanged() {
  if [ "$2" != "$3" ]; then
    echo "FAIL: $1 changed from $2 to $3"
    return 1
  fi
}

# ssa_snapshot — every suite pool with what it has handed out, as one line.
ssa_snapshot() {
  $POOL_KUBECTL get ippool -o json \
    | jq -c '[.items[] | select((.metadata.name | startswith("ssa-")) or ((.spec.classRef.name // "") | startswith("ssa-")))
              | {n: .metadata.name, a: (.status.capacity.allocated // "0")}] | sort_by(.n)'
}
