#!/usr/bin/env bash
# Fail if the committed generated code is not what the generators produce.
#
# Usage: hack/verify-codegen.sh
#
# Runs hack/update-codegen.sh against a COPY of the tree and diffs the result
# against what is committed. The working tree is never modified, so this is
# safe to run mid-edit and safe to run in CI.
#
# WHY THIS EXISTS
# ---------------
# CLAUDE.md has listed this script as a verification step for some time and it
# did not exist. Meanwhile update-codegen.sh could not run at all —
# k8s.io/code-generator is a build-time-only dependency that no Go file
# imports, so `go mod tidy` dropped it, so `go list -m` returned empty, so the
# script exited 1. That is now held by a `tool` directive in go.mod (Go 1.24+),
# which tidy preserves.
#
# For the months in between, generated files were hand-edited — conversions,
# deepcopy and openapi for the platform-as-a-project change among them. Hand-
# maintained generated code drifts silently and in a particularly nasty way:
# the next person to get the generator working produces a diff nobody can
# distinguish from a real change, so the drift and the fix arrive as one
# indistinguishable blob.
#
# (The audit that came with this script found ZERO drift — the hand edits were
# byte-identical to the generator's output. That is a good result and not a
# reason to skip the check; it is a reason to keep it from being needed again.)

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Everything update-codegen.sh writes. Adding a generator means adding its
# output here, or this passes while checking nothing — the silent-non-failure
# shape: a verify script that verifies an empty set exits 0.
GENERATED_PATHS=(
  "pkg/client"
  "pkg/generated/openapi"
  "pkg/apis/ipam/zz_generated.deepcopy.go"
  "pkg/apis/ipam/v1alpha1/zz_generated.deepcopy.go"
  "pkg/apis/ipam/v1alpha1/zz_generated.model_name.go"
)

# `pwd -P` is load-bearing on macOS, not tidiness. mktemp -d returns a path
# under /var, which is a symlink to /private/var. openapi-gen resolves its
# input packages to physical paths and then compares them against the main
# module's directory as given, so the two spellings do not match and it dies
# with "directory ... outside main module or its selected dependencies".
WORKDIR="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "${WORKDIR}"' EXIT

echo "Copying the tree to ${WORKDIR} ..."
tar -C "${SCRIPT_ROOT}" \
  --exclude='./.git' \
  --exclude='./node_modules' \
  --exclude='./ui/node_modules' \
  --exclude='./bin' \
  --exclude='./.test-infra' \
  -cf - . | tar -C "${WORKDIR}" -xf -

# Prove the copy actually carried the generated code across. Without this, a
# broken exclude or a failed copy produces a temp tree with no generated files,
# the generators recreate them from the types, and everything matches — a
# green run that compared the generator against itself.
for p in "${GENERATED_PATHS[@]}"; do
  if [ ! -e "${WORKDIR}/${p}" ]; then
    echo "ERROR: ${p} did not make it into the working copy — refusing to" >&2
    echo "       report a result, because a missing baseline compares clean." >&2
    exit 1
  fi
done

echo "Running the generators against the copy ..."
(cd "${WORKDIR}" && ./hack/update-codegen.sh) >/dev/null

failed=0
for p in "${GENERATED_PATHS[@]}"; do
  if ! diff -Naur "${SCRIPT_ROOT}/${p}" "${WORKDIR}/${p}" > "${WORKDIR}/.diff.out" 2>&1; then
    echo ""
    echo "ERROR: ${p} differs from generated output:" >&2
    head -60 "${WORKDIR}/.diff.out" >&2
    failed=1
  fi
done

if [ "${failed}" -ne 0 ]; then
  echo "" >&2
  echo "Committed generated code does not match the generators." >&2
  echo "Run './hack/update-codegen.sh' and commit the result." >&2
  echo "" >&2
  exit 1
fi

echo "codegen up to date (${#GENERATED_PATHS[@]} paths checked)"
