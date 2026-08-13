#!/usr/bin/env bash
#
# Refuse overlapping root-pool ranges across every fixture suite.
#
# The e2e suites share one project and chainsaw runs them concurrently, so two
# suites holding overlapping roots is a create-time conflict — and an
# intermittent one, because it depends on which suite gets there first.
#
# The check itself is a Go test, so it also runs in CI with the rest of the
# suite. This wrapper exists for --live, which needs a cluster: it adds root
# ranges the cluster holds that no fixture declares.
#
# Usage:
#   hack/verify-fixture-ranges.sh            # declared fixtures only
#   hack/verify-fixture-ranges.sh --live     # also compare against the cluster

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
readonly POOL_RESOURCE="ippools.ipam.miloapis.com"
readonly TEST_PACKAGE="./test/fixtures/"
readonly TEST_NAME="TestFixtureRootRangesAreDisjoint"

err() {
  echo "verify-fixture-ranges: $*" >&2
}

usage() {
  echo "Usage: $(basename "$0") [--live]" >&2
}

# Root CIDRs the cluster already holds, space separated. A child pool nests
# inside its parent by construction, so only parentless pools are roots.
#
# Prints nothing when no cluster is reachable: the declared fixtures are the
# check, and the cluster is a courtesy on top of them.
live_root_cidrs() {
  local pools
  if ! pools="$(kubectl get "${POOL_RESOURCE}" -o json 2>/dev/null)"; then
    err "no reachable cluster; comparing declared fixtures only"
    return 0
  fi
  jq -r '.items[] | select(.spec.parentPoolRef == null) | .spec.cidr // empty' <<<"${pools}" \
    | tr '\n' ' '
}

main() {
  local live="false"

  case "${1:-}" in
    --live) live="true" ;;
    "") ;;
    *)
      usage
      return 2
      ;;
  esac

  cd "${REPO_ROOT}"

  local extra=""
  if [[ "${live}" == "true" ]]; then
    extra="$(live_root_cidrs)"
  fi

  IPAM_EXTRA_ROOT_CIDRS="${extra}" go test "${TEST_PACKAGE}" -run "${TEST_NAME}" -count=1 -v
}

main "$@"
