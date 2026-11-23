#!/usr/bin/env bash
# The load-test overlays pull Milo's manifests and image at a pinned tag. That tag
# must equal the go.mod version of go.miloapis.com/milo, so the quota plugin
# compiled into IPAM talks to the Milo release it was built against.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

want="$(go list -m -f '{{.Version}}' go.miloapis.com/milo)"
bad=0
for f in config/overlays/load-test-milo/kustomization.yaml config/overlays/load-test-milo-api/kustomization.yaml; do
  while read -r got; do
    if [ "${got}" != "${want}" ]; then
      echo "FAIL: ${f} pins Milo ${got}, go.mod has ${want}"
      bad=1
    fi
  done < <(grep -oE '(ref=|newTag: )v[0-9][^ ]*' "${f}" | sed -E 's/^(ref=|newTag: )//')
done
if [ "${bad}" = 0 ]; then echo "OK: load-test overlays pin Milo ${want}"; fi
exit "${bad}"
