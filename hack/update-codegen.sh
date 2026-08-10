#!/usr/bin/env bash
set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
MODULE_NAME="go.miloapis.com/ipam"

# The `|| true` is what makes the diagnostic below reachable. Under
# `set -o errexit`, an assignment from a failing command substitution takes the
# substitution's exit status, so without it the script died silently right here
# — no message, no hint, just exit 1. Found by deliberately removing the tool
# directive to check that this guard fires; it did not.
CODEGEN_PKG=$(go list -m -f '{{.Dir}}' k8s.io/code-generator 2>/dev/null || true)

if [ -z "${CODEGEN_PKG}" ]; then
    echo "ERROR: k8s.io/code-generator is not in the module graph." >&2
    echo "" >&2
    echo "  It is a build-time-only dependency — no Go file imports it — so" >&2
    echo "  'go mod tidy' drops a plain require and this script stops working." >&2
    echo "  It is held by a 'tool' directive in go.mod, which tidy preserves." >&2
    echo "" >&2
    echo "  Restore it with:  go get -tool k8s.io/code-generator@v0.35.3" >&2
    echo "  (match the k8s.io/apimachinery version in go.mod)" >&2
    echo "" >&2
    exit 1
fi

echo "Using code-generator from: ${CODEGEN_PKG}"

echo "Cleaning old generated code..."
rm -rf "${SCRIPT_ROOT}/pkg/client"

source "${CODEGEN_PKG}/kube_codegen.sh"

echo "Generating clientset, listers, informers, and deepcopy..."
kube::codegen::gen_client \
  --with-watch \
  --output-dir "${SCRIPT_ROOT}/pkg/client" \
  --output-pkg "${MODULE_NAME}/pkg/client" \
  --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
  "${SCRIPT_ROOT}/pkg/apis"

echo "Generating deepcopy..."
kube::codegen::gen_helpers \
  --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
  "${SCRIPT_ROOT}/pkg/apis"

# OpenAPIModelName() accessors make the generated definition names match
# Scheme.ToOpenAPIDefinitionName(); server-side apply depends on that alignment.
#
# We run openapi-gen in two passes because the kube-openapi pinned for k8s 0.35
# predates the --readonly-pkg flag. Without that flag, the single combined
# invocation tries to (re)write zz_generated.model_name.go into every tagged
# input package, including the k8s dependency packages that live in the
# read-only module cache -> "permission denied". Those dependency packages
# already ship their committed accessors, so we never need to regenerate them.
#
# Pass 1 (model names): only our v1alpha1 package is an input, so the accessor
# file is written solely into our writable tree.
# Pass 2 (definitions): all packages are inputs but --output-model-name-file is
# omitted, so nothing is written into the dependencies. The definition writer
# still keys $refs by model name because it reads the +k8s:openapi-model-package
# tag from each package's comments, not from the generated accessor files.

echo "Generating OpenAPI model name accessors..."
go run k8s.io/kube-openapi/cmd/openapi-gen \
  --go-header-file "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
  --output-dir "${SCRIPT_ROOT}/pkg/generated/openapi" \
  --output-pkg "${MODULE_NAME}/pkg/generated/openapi" \
  --output-file zz_generated.openapi.go \
  --output-model-name-file zz_generated.model_name.go \
  --report-filename /dev/null \
  "${MODULE_NAME}/pkg/apis/ipam/v1alpha1"

echo "Generating OpenAPI definitions..."
go run k8s.io/kube-openapi/cmd/openapi-gen \
  --go-header-file "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
  --output-dir "${SCRIPT_ROOT}/pkg/generated/openapi" \
  --output-pkg "${MODULE_NAME}/pkg/generated/openapi" \
  --output-file zz_generated.openapi.go \
  --report-filename /dev/null \
  "${MODULE_NAME}/pkg/apis/ipam/v1alpha1" \
  "k8s.io/apimachinery/pkg/apis/meta/v1" \
  "k8s.io/apimachinery/pkg/api/resource" \
  "k8s.io/apimachinery/pkg/runtime" \
  "k8s.io/apimachinery/pkg/version"

echo ""
echo "Code generation complete!"
echo ""
echo "Generated:"
echo "  - Deepcopy functions: pkg/apis/ipam/zz_generated.deepcopy.go"
echo "  - Deepcopy functions: pkg/apis/ipam/v1alpha1/zz_generated.deepcopy.go"
echo "  - Clientset: pkg/client/clientset/versioned/"
echo "  - Listers: pkg/client/listers/"
echo "  - Informers: pkg/client/informers/"
echo "  - OpenAPI: pkg/generated/openapi/"
