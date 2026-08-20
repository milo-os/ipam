# Shared helpers for the class-model e2e suites.
#
# Source from a chainsaw `script:` step. Chainsaw runs scripts with the working
# directory set to the test directory, so:
#
#   . ../lib/ipam.sh
#
# Written in POSIX shell, matching the existing suites — chainsaw executes
# script content with `sh`, so no `[[`, no `for ((`, no arrays.
#
# Prefix arithmetic goes through python3's `ipaddress` rather than string
# matching. IPv6 containment and overlap cannot be checked correctly with a
# regex, and a test that only looks like it checks containment is worse than no
# test at all.

set -e

# Every kubectl call goes through $KUBECTL so a suite can drive the apiserver as
# a different identity by setting it, e.g.
#
#   KUBECTL="kubectl --kubeconfig ../.x.kubeconfig --context tenant-project-alpha"
#
# Deliberately unquoted at the call sites: the value is a command plus flags and
# has to word-split. It is set by suites, never by user input.
KUBECTL="${KUBECTL:-kubectl}"

# Pools are read through $POOL_KUBECTL, which defaults to the same plain
# kubectl. They are split from $KUBECTL because a pool does not necessarily live
# in the project that claims from it: a class provisions its pools in the
# project holding the DEFINITION, so a consumer project referencing a platform
# class reads its own claims in one project and the pool behind them in another.
POOL_KUBECTL="${POOL_KUBECTL:-kubectl}"

# ---------------------------------------------------------------------------
# Waiting
# ---------------------------------------------------------------------------

# wait_claim_bound <namespace> <claim> [timeout_seconds]
# Fails loudly, dumping the claim, if it never binds.
wait_claim_bound() {
  ns="$1"; name="$2"; timeout="${3:-60}"; phase=""
  for _ in $(seq 1 "$timeout"); do
    phase=$($KUBECTL get ipclaim -n "$ns" "$name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [ "$phase" = "Bound" ]; then
      return 0
    fi
    sleep 1
  done
  echo "FAIL: ipclaim $ns/$name not Bound after ${timeout}s (phase=${phase:-<none>})"
  $KUBECTL get ipclaim -n "$ns" "$name" -o yaml 2>&1 | sed 's/^/    /' || true
  return 1
}

# wait_pool_ready <pool> [timeout_seconds]
wait_pool_ready() {
  name="$1"; timeout="${2:-60}"; phase=""
  for _ in $(seq 1 "$timeout"); do
    phase=$($POOL_KUBECTL get ippool "$name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [ "$phase" = "Ready" ]; then
      return 0
    fi
    sleep 1
  done
  echo "FAIL: ippool $name not Ready after ${timeout}s (phase=${phase:-<none>})"
  $POOL_KUBECTL get ippool "$name" -o yaml 2>&1 | sed 's/^/    /' || true
  return 1
}

# wait_gone <kind> <namespace-or-empty> <name> [timeout_seconds]
wait_gone() {
  kind="$1"; ns="$2"; name="$3"; timeout="${4:-60}"
  for _ in $(seq 1 "$timeout"); do
    if [ -n "$ns" ]; then
      kubectl get "$kind" -n "$ns" "$name" >/dev/null 2>&1 || return 0
    else
      kubectl get "$kind" "$name" >/dev/null 2>&1 || return 0
    fi
    sleep 1
  done
  echo "FAIL: $kind ${ns:+$ns/}$name still present after ${timeout}s"
  return 1
}

# ---------------------------------------------------------------------------
# Reading allocations
# ---------------------------------------------------------------------------

# claim_cidr <namespace> <claim> — the block a claim was given.
claim_cidr() {
  $KUBECTL get ipclaim -n "$1" "$2" -o jsonpath='{.status.allocatedCIDR}'
}

# claim_pool <namespace> <claim> — the pool the allocator resolved.
claim_pool() {
  $KUBECTL get ipclaim -n "$1" "$2" -o jsonpath='{.status.poolRef.name}'
}

# pool_cidr <pool> — the block a pool holds.
pool_cidr() {
  $POOL_KUBECTL get ippool "$1" -o jsonpath='{.status.allocatedCIDR}'
}

# ---------------------------------------------------------------------------
# Finding cascade-provisioned pools
#
# Cascade-created pool names are the allocator's business, so these look pools
# up by what the model actually guarantees: which class provisioned them and
# which scope they exist for. A suite that hard-coded generated names would
# break on any naming change without telling us anything true.
# ---------------------------------------------------------------------------

# pools_for_scope <className> [role=value ...]
# Prints the names of pools provisioned by <className> whose spec.scope matches
# every given role=value pair, one per line, sorted.
pools_for_scope() {
  _class="$1"; shift
  _filter=".spec.classRef.name == \"$_class\""
  for _pair in "$@"; do
    _role=$(printf '%s' "$_pair" | cut -d= -f1)
    _value=$(printf '%s' "$_pair" | cut -d= -f2-)
    _filter="$_filter and (.spec.scope[\"$_role\"].name // \"\") == \"$_value\""
  done
  $POOL_KUBECTL get ippool -o json | jq -r ".items[] | select($_filter) | .metadata.name" | sort
}

# expect_pool_count <expected> <className> [role=value ...]
# The workhorse of the cascade tests. "Exactly one" is what proves reuse;
# "exactly two" is what proves a new location got its own.
expect_pool_count() {
  _want="$1"; shift
  _class="$1"; shift
  _names=$(pools_for_scope "$_class" "$@" || true)
  _got=$(printf '%s' "$_names" | grep -c . || true)
  if [ "$_got" != "$_want" ]; then
    echo "FAIL: expected $_want pool(s) of class $_class for scope [$*], got $_got"
    if [ -n "$_names" ]; then
      printf '    %s\n' $_names
    fi
    return 1
  fi
  echo "OK: exactly $_want pool(s) of class $_class for scope [$*]"
}

# ---------------------------------------------------------------------------
# Asserting on HTTP status
#
# These read the status from kubectl's own transport log (`-v=8` prints
# `Response Status:`) rather than string-matching the rendered error. Matching
# the message would pass just as happily against a 500 whose body happened to
# contain the right words, which is the exact confusion these tests exist to
# rule out.
# ---------------------------------------------------------------------------

# _status_of <kubectl -v=8 output>
# Reads the HTTP status from the `"code": N` field of the Status object
# kubectl logs as the server response body. Recent kubectl no longer prints a
# `Response Status:` line, so the code comes out of the body instead — still
# the server's own status, not the rendered message.
_status_of() {
  printf '%s\n' "$1" \
    | grep -o '"code": *[0-9][0-9]*' \
    | grep -o '[0-9][0-9]*' \
    | tail -1
}

# expect_rejected <namespace> <expected_status_or_4xx> <regex> [label]
# <regex> is an extended regex, so alternation is available where more than
# one wording would be equally correct.
# Manifest on stdin. Asserts the server refused the create with the expected
# status and that the message contains <substring> — normally the name of the
# thing the caller got wrong.
expect_rejected() {
  _ns="$1"; _want="$2"; _needle="$3"; _label="${4:-create}"
  # kubectl is called here rather than in a helper because the exit code has
  # to survive: a command substitution runs in a subshell, so an rc set inside
  # one is lost. The `&& / ||` form also keeps `set -e` from aborting on the
  # expected failure.
  _out=$($KUBECTL create -n "$_ns" -v=8 -f - 2>&1) && _create_rc=0 || _create_rc=$?
  if [ "$_create_rc" = "0" ]; then
    echo "FAIL: $_label was accepted; expected rejection"
    return 1
  fi
  _status=$(_status_of "$_out")
  if [ -z "$_status" ]; then
    echo "FAIL: $_label failed but no HTTP status could be read from the response"
    printf '%s\n' "$_out" | tail -5
    return 1
  fi
  if [ "$_status" = "500" ]; then
    echo "FAIL: $_label returned 500 Internal Server Error; a bad request must not read as a server fault"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  if [ "$_want" = "4xx" ]; then
    case "$_status" in
      4*) : ;;
      *) echo "FAIL: $_label returned $_status, expected a 4xx"; return 1 ;;
    esac
  elif [ "$_status" != "$_want" ]; then
    echo "FAIL: $_label returned $_status, expected $_want"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  if ! printf '%s\n' "$_out" | grep -Eq -- "$_needle"; then
    echo "FAIL: $_label was rejected with $_status but the error never mentions '$_needle'."
    echo "      An error that does not name what is missing sends the reader looking in the wrong place."
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  echo "OK: $_label rejected with $_status naming '$_needle'"
}

# expect_update_rejected <namespace> <expected_status_or_4xx> <regex> [label]
# Manifest on stdin. The UPDATE counterpart of expect_rejected, for immutable
# fields — an object that already exists is re-applied with one field changed.
#
# `apply` rather than `create`: create on an existing object is refused as
# AlreadyExists before the immutability check is ever reached, so a suite built
# on create would pass against a server that had no immutability rule at all.
expect_update_rejected() {
  _ns="$1"; _want="$2"; _needle="$3"; _label="${4:-update}"
  _out=$($KUBECTL apply -n "$_ns" -v=8 -f - 2>&1) && _rc=0 || _rc=$?
  if [ "$_rc" = "0" ]; then
    echo "FAIL: $_label was accepted; the field is supposed to be immutable"
    return 1
  fi
  _status=$(_status_of "$_out")
  if [ "$_status" = "500" ]; then
    echo "FAIL: $_label returned 500; a rejected update must not read as a server fault"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  if [ "$_want" = "4xx" ]; then
    case "${_status:-none}" in
      4*) : ;;
      *) echo "FAIL: $_label returned ${_status:-none}, expected a 4xx"; return 1 ;;
    esac
  elif [ "$_status" != "$_want" ]; then
    echo "FAIL: $_label returned ${_status:-none}, expected $_want"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  if ! printf '%s\n' "$_out" | grep -Eq -- "$_needle"; then
    echo "FAIL: $_label was rejected with $_status but the error never names '$_needle'"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  echo "OK: $_label rejected with $_status naming '$_needle'"
}

# expect_delete_rejected <kind> <namespace-or-empty> <name> <status_or_4xx> <regex> [label]
#
# For the delete guards, where the assertion is that the server REFUSED to
# remove something still in use. Status is read from the transport log for the
# same reason expect_rejected does it: a 500 whose body happens to contain
# "active allocation" would satisfy a message match while meaning the opposite.
expect_delete_rejected() {
  _kind="$1"; _ns="$2"; _name="$3"; _want="$4"; _needle="$5"; _label="${6:-delete}"
  if [ -n "$_ns" ]; then
    _out=$($KUBECTL delete "$_kind" -n "$_ns" "$_name" -v=8 2>&1) && _rc=0 || _rc=$?
  else
    _out=$($KUBECTL delete "$_kind" "$_name" -v=8 2>&1) && _rc=0 || _rc=$?
  fi
  if [ "$_rc" = "0" ]; then
    echo "FAIL: $_label succeeded; it is still in use and must be refused"
    return 1
  fi
  _status=$(_status_of "$_out")
  if [ "$_status" = "404" ]; then
    echo "FAIL: $_label returned 404 — the object is absent, so this proves nothing"
    echo "      about the guard. Check the identity the delete ran as."
    return 1
  fi
  if [ "$_status" = "500" ]; then
    echo "FAIL: $_label returned 500; a refused delete must not read as a server fault"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  if [ "$_want" = "4xx" ]; then
    case "${_status:-none}" in
      4*) : ;;
      *) echo "FAIL: $_label returned ${_status:-none}, expected a 4xx"; return 1 ;;
    esac
  elif [ "$_status" != "$_want" ]; then
    echo "FAIL: $_label returned ${_status:-none}, expected $_want"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  if ! printf '%s\n' "$_out" | grep -Eq -- "$_needle"; then
    echo "FAIL: $_label was refused with $_status but the message never matches '$_needle'"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  echo "OK: $_label refused with $_status matching '$_needle'"
}

# expect_507 <namespace> [substring] [label] — manifest on stdin.
expect_507() {
  _ns="$1"; _needle="${2:-}"; _label="${3:-claim}"
  _out=$($KUBECTL create -n "$_ns" -v=8 -f - 2>&1) && _create_rc=0 || _create_rc=$?
  if [ "$_create_rc" = "0" ]; then
    echo "FAIL: $_label succeeded; the pool was supposed to be exhausted"
    return 1
  fi
  _status=$(_status_of "$_out")
  if [ "$_status" = "500" ]; then
    echo "FAIL: $_label returned 500; exhaustion is an expected outcome and must be 507"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  if [ "$_status" != "507" ]; then
    echo "FAIL: $_label returned '${_status:-none}', expected 507"
    printf '%s\n' "$_out" | grep -i 'response status\|message' | tail -10
    return 1
  fi
  if [ -n "$_needle" ] && ! printf '%s\n' "$_out" | grep -Eq -- "$_needle"; then
    echo "FAIL: 507 did not name '$_needle' — the level that ran out"
    printf '%s\n' "$_out" | grep -i 'message' | tail -5
    return 1
  fi
  echo "OK: $_label returned 507${_needle:+ naming '$_needle'}"
}

# ---------------------------------------------------------------------------
# Prefix arithmetic (python3 ipaddress — never string matching)
# ---------------------------------------------------------------------------

# assert_within <child_cidr> <parent_cidr> [label]
assert_within() {
  python3 -c '
import ipaddress, sys
child, parent, label = sys.argv[1], sys.argv[2], sys.argv[3]
c = ipaddress.ip_network(child, strict=False)
p = ipaddress.ip_network(parent, strict=False)
if not p.supernet_of(c):
    print(("FAIL: %s %s is not within %s" % (label, child, parent)).strip())
    sys.exit(1)
print(("OK: %s %s within %s" % (label, child, parent)).strip())
' "$1" "$2" "${3:-}"
}

# assert_prefix_len <cidr> <length> [label]
assert_prefix_len() {
  python3 -c '
import ipaddress, sys
cidr, want, label = sys.argv[1], int(sys.argv[2]), sys.argv[3]
n = ipaddress.ip_network(cidr, strict=False)
if n.prefixlen != want:
    print(("FAIL: %s %s is a /%d, expected /%d" % (label, cidr, n.prefixlen, want)).strip())
    sys.exit(1)
print(("OK: %s %s is a /%d" % (label, cidr, want)).strip())
' "$1" "$2" "${3:-}"
}

# assert_disjoint <cidr> <cidr> [...]
# Fails if any pair overlaps. Also fails on a duplicate, since a network
# overlaps itself.
assert_disjoint() {
  python3 -c '
import ipaddress, sys
nets = [ipaddress.ip_network(a, strict=False) for a in sys.argv[1:]]
for i in range(len(nets)):
    for j in range(i + 1, len(nets)):
        if nets[i].overlaps(nets[j]):
            print("FAIL: %s overlaps %s" % (nets[i], nets[j]))
            sys.exit(1)
print("OK: %d prefixes are pairwise disjoint" % len(nets))
' "$@"
}

# assert_equal_addr <cidr> <cidr> [label]
# Two addresses that must be the *same* — the uniqueWithin proof, where two
# networks legitimately hold one address.
assert_equal_addr() {
  python3 -c '
import ipaddress, sys
a, b, label = sys.argv[1], sys.argv[2], sys.argv[3]
na = ipaddress.ip_network(a, strict=False)
nb = ipaddress.ip_network(b, strict=False)
if na != nb:
    print(("FAIL: %s expected the same address, got %s and %s" % (label, a, b)).strip())
    sys.exit(1)
print(("OK: %s both hold %s" % (label, a)).strip())
' "$1" "$2" "${3:-}"
}

# assert_nth_subnet <parent_cidr> <index> <prefix_len> <actual_cidr> [label]
# Asserts <actual> is exactly the Nth aligned block of <prefix_len> inside
# <parent>. Used to pin FirstFit results and reservation edges.
assert_nth_subnet() {
  python3 -c '
import ipaddress, sys
parent, idx, plen, actual, label = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4], sys.argv[5]
p = ipaddress.ip_network(parent, strict=False)
want = next(x for i, x in enumerate(p.subnets(new_prefix=plen)) if i == idx)
got = ipaddress.ip_network(actual, strict=False)
if got != want:
    print(("FAIL: %s got %s, expected %s (block %d of %s)" % (label, got, want, idx, parent)).strip())
    sys.exit(1)
print(("OK: %s is %s" % (label, got)).strip())
' "$1" "$2" "$3" "$4" "${5:-}"
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------

# release_pool_allocations <namespace> <pool> [passes]
# Deletes every allocation drawn from <pool>, converging in more than one pass.
#
# One pass is not enough, and the reason is a real property rather than a
# flake: a *bound* allocation is deliberately refused with 409, because
# releasing an address out from under a live claim would leave the claim naming
# nothing. So if claim deletion has not yet landed, the first sweep is refused;
# the claims then unbind and the next sweep succeeds. This is the same
# convergence namespace termination relies on.
release_pool_allocations() {
  _ns="$1"; _pool="$2"; _passes="${3:-4}"
  for _ in $(seq 1 "$_passes"); do
    _left=$($KUBECTL get ipallocation -n "$_ns" -o json 2>/dev/null \
      | jq -r --arg p "$_pool" '.items[] | select(.spec.poolRef.name == $p) | .metadata.name' || true)
    # `if` rather than `[ -z "$_left" ] && return 0`: under `set -e` that form
    # is a complete command, so a *non-empty* list makes it exit non-zero and
    # kills the caller — aborting precisely when there is work to do.
    if [ -z "$_left" ]; then
      return 0
    fi
    for _a in $_left; do
      kubectl delete ipallocation -n "$_ns" "$_a" --ignore-not-found >/dev/null 2>&1 || true
    done
    sleep 2
  done
  return 0
}

# expect_created <namespace> <label> — manifest on stdin.
# The positive counterpart to expect_rejected. Used where an *accept* is the
# assertion, so that a deny-everything regression cannot pass a suite whose
# other steps all assert denials.
expect_created() {
  _ns="$1"; _label="${2:-create}"
  _out=$($KUBECTL create -n "$_ns" -f - 2>&1) && _rc=0 || _rc=$?
  if [ "$_rc" != "0" ]; then
    echo "FAIL: $_label was rejected; expected it to be allowed"
    printf '%s\n' "$_out" | tail -3
    return 1
  fi
  echo "OK: $_label allowed"
}

# expect_507_details <namespace> <expected pool name> [label] — manifest on stdin.
#
# The status code alone says "something ran out". The body says WHICH pool,
# which for a cascade is frequently not the level the claim asked for. A claim
# names a class and never a pool, so without the name the caller cannot tell
# which pool to widen.
#
# Only details.name is asserted. A richer payload — the requested length, the
# largest free block, utilization — would also distinguish a genuinely full pool
# from a merely fragmented one, but the server does not publish those yet, and a
# helper that demands them fails every caller instead of the one under test.
#
# Reads the server's own Status object out of the -v=8 transport log rather
# than the rendered error text.
expect_507_details() {
  _ns="$1"; _pool="$2"; _label="${3:-claim}"
  _out=$($KUBECTL create -n "$_ns" -v=8 -f - 2>&1) && _rc=0 || _rc=$?
  if [ "$_rc" = "0" ]; then
    echo "FAIL: $_label succeeded; the pool was supposed to be exhausted"
    return 1
  fi

  _body=$(printf '%s\n' "$_out" | grep -o '{"kind":"Status".*}' | head -1)
  if [ -z "$_body" ]; then
    echo "FAIL: could not read a Status body from the response for $_label"
    return 1
  fi

  _code=$(printf '%s' "$_body" | jq -r '.code // ""')
  if [ "$_code" != "507" ]; then
    echo "FAIL: $_label returned $_code, expected 507"
    return 1
  fi

  _name=$(printf '%s' "$_body" | jq -r '.details.name // ""')
  if [ "$_name" != "$_pool" ]; then
    echo "FAIL: 507 details.name is '$_name', expected '$_pool' — the pool that ran out"
    return 1
  fi

  echo "OK: $_label returned 507 with details naming pool '$_name'"
}

# ---------------------------------------------------------------------------
# Reconciling a claim the way a controller does
# ---------------------------------------------------------------------------

# reconcile_claim <namespace> <name> [label] — manifest on stdin.
#
# Mirrors what network-services-operator does on every pass: the claim's name is
# derived from the object it belongs to, and the controller asks whether the
# claim exists before creating it. A reconcile that lost its answer therefore
# finds the same address again instead of taking a second one, and a repeated
# pass is not an error.
#
# Calling this twice must be indistinguishable from calling it once. A suite
# that only ever created claims once would pass against a server on which the
# second pass of a controller took a second address.
reconcile_claim() {
  _ns="$1"; _name="$2"; _label="${3:-claim}"
  _manifest=$(cat)
  if $KUBECTL get ipclaim -n "$_ns" "$_name" >/dev/null 2>&1; then
    echo "OK: $_label already held, reconcile is a no-op"
    return 0
  fi
  if ! printf '%s\n' "$_manifest" | $KUBECTL create -n "$_ns" -f - >/dev/null 2>&1; then
    # Lost a race with another writer of the same name. The controller re-reads
    # rather than failing, and so does this.
    if $KUBECTL get ipclaim -n "$_ns" "$_name" >/dev/null 2>&1; then
      echo "OK: $_label was created concurrently, reconcile adopted it"
      return 0
    fi
    echo "FAIL: $_label could not be created and does not exist"
    printf '%s\n' "$_manifest" | $KUBECTL create -n "$_ns" -f - 2>&1 | tail -3
    return 1
  fi
  echo "OK: $_label created"
}

# ---------------------------------------------------------------------------
# The address a subnet keeps for itself
# ---------------------------------------------------------------------------

# subnet_gateway <cidr> — the first usable address of a subnet, which the
# tenant addressing plan gives to the subnet's virtual router.
subnet_gateway() {
  python3 -c '
import ipaddress, sys
print(ipaddress.ip_network(sys.argv[1], strict=False).network_address + 1)
' "$1"
}

# assert_excludes_address <cidr> <address> [label]
# Fails if <address> falls inside <cidr>. The gateway assertion: an endpoint
# handed the block its router answers on is a conflict nothing reports — the
# claim binds, the pool is Ready, and both holders believe the address is
# theirs.
assert_excludes_address() {
  python3 -c '
import ipaddress, sys
cidr, addr, label = sys.argv[1], sys.argv[2], sys.argv[3]
net = ipaddress.ip_network(cidr, strict=False)
ip = ipaddress.ip_address(addr)
if ip in net:
    print(("FAIL: %s %s contains %s" % (label, cidr, addr)).strip())
    sys.exit(1)
print(("OK: %s %s does not contain %s" % (label, cidr, addr)).strip())
' "$1" "$2" "${3:-}"
}
