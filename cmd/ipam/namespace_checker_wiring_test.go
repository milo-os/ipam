package main

// The namespace-liveness checker must be gated on --kubeconfig, not on
// ClientConfig being non-nil (#86).
//
// # Why this guard exists, and it is a bug I shipped
//
// The first wiring built the checker whenever genericConfig.ClientConfig was
// non-nil. That reads as "we have a control plane to ask" and is false:
// CoreAPIOptions.ApplyTo falls back to rest.InClusterConfig() when --kubeconfig
// is empty, and in a pod that SUCCEEDS. So ClientConfig is non-nil in every
// in-cluster deployment whether or not Milo exists.
//
// The consequence, measured against the dev cluster's config: KUBECONFIG is
// empty and CoreAPI is never disabled, so ClientConfig would have been the kind
// cluster's own API server. The checker would rewrite its Host to a
// /apis/resourcemanager.miloapis.com/.../projects/<id>/control-plane path that
// does not exist there, take the 404 as "namespace missing", and REFUSE EVERY
// PROJECT-SCOPED CLAIM WITH A NAMESPACE.
//
// That is the exact hazard #86's brief warned about — validating against the
// root cluster rather than the project's control plane — reached by a different
// route than the one it warned about.
//
// The signal that means what is needed is --kubeconfig, which the deployment's
// own comment says MUST point at milo-apiserver for a real control plane.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestNamespaceCheckerIsGatedOnKubeconfigNotClientConfig asserts the gate
// structurally, over the package's own syntax tree.
//
// A behavioural test would need a running apiserver with and without
// --kubeconfig, which is not available here. What CAN be checked is the thing
// that went wrong: that the construction sits behind a test of the kubeconfig
// PATH rather than being called unconditionally on ClientConfig.
func TestNamespaceCheckerIsGatedOnKubeconfigNotClientConfig(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	var callFound, gateFound bool
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewNamespaceChecker" {
					return true
				}
				callFound = true
				// The construction must be reachable only under a condition
				// mentioning the kubeconfig path. Found by looking for the
				// identifier anywhere in the enclosing file's if-statements,
				// which is coarse but catches the regression: removing the gate
				// removes the identifier.
				return true
			})
			ast.Inspect(file, func(n ast.Node) bool {
				ifs, ok := n.(*ast.IfStmt)
				if !ok || ifs.Cond == nil {
					return true
				}
				var mentions bool
				ast.Inspect(ifs.Cond, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && id.Name == "CoreAPIKubeconfigPath" {
						mentions = true
					}
					if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "CoreAPIKubeconfigPath" {
						mentions = true
					}
					return true
				})
				if !mentions {
					return true
				}
				ast.Inspect(ifs.Body, func(m ast.Node) bool {
					call, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewNamespaceChecker" {
						gateFound = true
					}
					return true
				})
				return true
			})
		}
	}

	// Positive control: the scan must find the construction at all, or a
	// deleted call would look like a satisfied gate.
	if !callFound {
		t.Fatal("no call to NewNamespaceChecker found; the scan is broken, or the checker was " +
			"removed — either way this guard is not checking what it claims")
	}
	if !gateFound {
		t.Error("NewNamespaceChecker is not constructed inside a condition testing " +
			"CoreAPIKubeconfigPath.\n" +
			"ClientConfig is NOT a valid signal: CoreAPIOptions.ApplyTo falls back to " +
			"rest.InClusterConfig() when --kubeconfig is empty, and in a pod that succeeds — so " +
			"it is non-nil in every in-cluster deployment whether or not Milo exists. A checker " +
			"built on it points at the ROOT apiserver, 404s on the project control-plane path, " +
			"reads that as \"namespace missing\", and refuses every project-scoped claim.")
	}
}
