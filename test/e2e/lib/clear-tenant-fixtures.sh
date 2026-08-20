#!/usr/bin/env sh
# Delete every IPAM object in the e2e tenant projects.
#
# IPPools and IPClasses are cluster-scoped, so chainsaw's namespace teardown
# never removes them: a suite that fails partway leaves its pools behind and the
# next run fails on "already exists". These two projects hold e2e fixtures only.

set -eu

KCFG="${1:-test/e2e/.tenant-impersonation.kubeconfig}"

for ctx in tenant-project-alpha tenant-project-beta tenant-project-datum-cloud; do
  for kind in ipclaims ipallocations ippools ipclasses; do
    KUBECONFIG="$KCFG" kubectl --context "$ctx" delete "$kind" \
      --all --all-namespaces --ignore-not-found >/dev/null 2>&1 || true
  done
done
echo "cleared e2e tenant fixtures"
