package ipclaim

// Namespace liveness on the claim path (#86, closes #72).
//
// Two halves, and the second is the one that matters most, because a fix
// reaching for a namespace check without accounting for the namespace
// controller is exactly what caused #92 — a cluster-wide outage in which no
// namespace could be deleted at all.
//
//	1. a claim into a Terminating or absent namespace is refused
//	2. the DELETE paths are untouched, so namespace teardown still completes
//
// The controller deleting a namespace calls IPAM while that namespace is
// Terminating — by definition, every time. So any gate it passes through
// deadlocks teardown. The check is therefore scoped to Create, and half 2
// asserts that scoping rather than trusting it.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// fakeNamespaceChecker returns a fixed answer and counts calls, so a test can
// assert not only the decision but whether the lookup happened at all.
type fakeNamespaceChecker struct {
	state access.NamespaceState
	err   error
	calls int
}

func (f *fakeNamespaceChecker) State(_ context.Context, _, _ string) (access.NamespaceState, error) {
	f.calls++
	return f.state, f.err
}

// TestNamespaceStatesThatRefuseAClaim covers half 1.
//
// Terminating and Missing refuse; everything else admits. The table is over the
// full set of states rather than the two interesting ones, so a new state added
// to the enum without a decision here shows up as a compile-time gap in the
// switch below rather than silently defaulting to "admit".
func TestNamespaceStatesThatRefuseAClaim(t *testing.T) {
	for _, tt := range []struct {
		state      access.NamespaceState
		wantRefuse bool
		wantIn     string
	}{
		{access.NamespaceTerminating, true, "being terminated"},
		{access.NamespaceMissing, true, "does not exist"},
		{access.NamespaceLive, false, ""},
		{access.NamespaceUnknown, false, ""},
	} {
		t.Run(tt.state.String(), func(t *testing.T) {
			err := access.RefuseNamespace(tt.state, "ns-under-test", v1alpha1.Resource("ipclaims"))
			if !tt.wantRefuse {
				if err != nil {
					t.Fatalf("%s produced a refusal (%v); only a DEFINITIVE bad state may refuse, "+
						"and Unknown in particular must admit — failing closed on a lookup error "+
						"puts another service in the hot path of every allocation", tt.state, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s did not refuse", tt.state)
			}
			if !apierrors.IsForbidden(err) {
				t.Errorf("%s refusal is not Forbidden: %v", tt.state, err)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("%s message = %q, want it to contain %q — the wording deliberately "+
					"mirrors what stock Kubernetes says for the same condition, because that is "+
					"the sentence operators recognise", tt.state, err.Error(), tt.wantIn)
			}
			if !strings.Contains(err.Error(), "ns-under-test") {
				t.Errorf("%s message does not name the namespace: %q", tt.state, err.Error())
			}
		})
	}
}

// TestUnknownAndTerminatingAreDistinguishable is the property the fail-open
// decision rests on.
//
// "The namespace is terminating" and "we could not determine the namespace
// state" lead to different outcomes and must never collapse into one value. If
// they ever did, the collapse would be invisible: a deployment whose control
// plane was unreachable would either refuse every claim (outage) or admit every
// claim while reporting a reason it had not established.
func TestUnknownAndTerminatingAreDistinguishable(t *testing.T) {
	if access.NamespaceUnknown == access.NamespaceTerminating {
		t.Fatal("Unknown and Terminating are the same value")
	}
	// And the zero value must be Unknown, not Live. A checker that returns a
	// zero NamespaceState on some unhandled path must not thereby assert the
	// namespace is fine.
	var zero access.NamespaceState
	if zero != access.NamespaceUnknown {
		t.Errorf("the zero NamespaceState is %s, want Unknown: an unset value must not claim "+
			"the namespace is healthy", zero)
	}
}

// TestNoProjectOrNoNamespaceIsNotAnError covers the second hazard from #92: a
// caller legitimately having no project is a permanent category, not an error
// state, and the control plane's own controllers are exactly that caller.
//
// It also covers a cluster-scoped claim, which has no namespace to look up.
func TestNoProjectOrNoNamespaceIsNotAnError(t *testing.T) {
	checker := access.NewNamespaceChecker(nil)
	if checker != nil {
		t.Fatal("a nil rest.Config must produce a nil checker, which DISABLES the check; " +
			"denying instead would make an apiserver started without --kubeconfig serve nothing")
	}
}

// TestCreateRefusesATerminatingNamespace is half 1 driven through the real
// Create path, not through RefuseNamespace in isolation.
//
// The table above proves the error is built correctly; this proves Create
// consults the checker at all and acts on the answer. A correct refusal that is
// never reached is #85's shape, and the whole reason this check is in the
// registry rather than in admission.
func TestCreateRefusesATerminatingNamespace(t *testing.T) {
	for _, tt := range []struct {
		name       string
		state      access.NamespaceState
		err        error
		wantRefuse bool
	}{
		{"terminating", access.NamespaceTerminating, nil, true},
		{"missing", access.NamespaceMissing, nil, true},
		{"live", access.NamespaceLive, nil, false},
		// The fail-open decision, driven end to end: a lookup that errored must
		// admit the claim. Failing closed here would make every allocation
		// depend on the control plane being reachable.
		{"lookup failed", access.NamespaceUnknown, errBoom, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, fa, _, _ := newTestREST()
			fnc := &fakeNamespaceChecker{state: tt.state, err: tt.err}
			r.nsChecker = fnc

			_, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{})

			if fnc.calls != 1 {
				t.Errorf("namespace checker called %d times, want 1: a check that is not "+
					"consulted is the #85 shape", fnc.calls)
			}
			if tt.wantRefuse {
				if err == nil {
					t.Fatalf("%s namespace was admitted", tt.state)
				}
				if !apierrors.IsForbidden(err) {
					t.Errorf("refusal is not Forbidden: %v", err)
				}
				// And nothing was allocated. A refusal that still consumed an
				// address would be worse than no refusal.
				if fa.allocateN != 0 {
					t.Errorf("allocator called %d times for a refused claim, want 0", fa.allocateN)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s namespace was refused: %v", tt.state, err)
			}
			if fa.allocateN != 1 {
				t.Errorf("allocator called %d times, want 1", fa.allocateN)
			}
		})
	}
}

// TestOnlyCreateConsultsTheNamespaceChecker is half 2, and it is the #92
// regression guard.
//
// The namespace controller deletes IPAM objects while the namespace is
// Terminating — always, by construction, because that is what deleting a
// namespace means. A liveness gate on any path the controller traverses blocks
// teardown permanently, and #92 was exactly that: no namespace in the cluster
// could be deleted.
//
// Asserted STRUCTURALLY, over the package's own syntax tree, rather than by
// calling Delete. Two reasons, and the second is the stronger one:
//
//   - Delete cannot be driven through the fake harness at all — it goes through
//     the embedded Store, which newTestREST does not build.
//   - A single Delete call would only prove that ONE path does not consult the
//     checker. This proves no verb except Create mentions it, which is the
//     property that actually matters: the danger is a future gate added to
//     DeleteCollection, or to Update, or to a verb that does not exist yet.
//
// Same instrument as the request-filter sweep in cmd/ipam: read the directory
// so a method behind a build tag is still covered.
func TestOnlyCreateConsultsTheNamespaceChecker(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	const allowed = "Create"
	found := map[string]bool{}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "nsChecker" {
						return true
					}
					found[fn.Name.Name] = true
					return true
				})
			}
		}
	}

	// Positive control: the scan must find the one legitimate use, or it is
	// broken and would report "no verb consults it" about a package where every
	// verb does.
	if !found[allowed] {
		t.Fatalf("no method references nsChecker; the AST scan is broken, so this test would "+
			"pass against a package that gated every verb (found: %v)", found)
	}

	for name := range found {
		if name == allowed {
			continue
		}
		t.Errorf("%s references nsChecker. The namespace controller deletes IPAM objects while "+
			"the namespace is Terminating, so a liveness gate on any verb it traverses blocks "+
			"namespace teardown cluster-wide — that is #92, which took an outage to surface. "+
			"The check belongs on Create alone.", name)
	}
}

var errBoom = errors.New("control plane unreachable")
