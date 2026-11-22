package ipamerrors

import (
	"errors"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"
	"testing"
)

// Every reason constant declared in this package is one ReasonFor reports and
// KnownReasons lists. A reason left out would be classified as unknown, and
// any mapping held to KnownReasons, such as the failure metric's, would never
// see it.
//
// A reason constant need not be declared with the Reason type: an untyped
// string constant passed to New, keyed into knownReasons, or converted to a
// Reason or a cause type is a reason too. So the package is type-checked, and
// a constant counts as a reason when it has the Reason type or any use of it
// takes the Reason type or names a cause type.
func TestEveryDeclaredReasonIsKnown(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}

	declared, err := reasonConstants(fset, files, importer.ForCompiler(fset, "source", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(declared) < 2 {
		t.Fatalf("found %d reason constants; the type check is not finding them", len(declared))
	}

	known := KnownReasons()
	want := 0
	for name, reason := range declared {
		if reason == ReasonUnknown {
			continue
		}
		want++
		if !slices.Contains(known, reason) {
			t.Errorf("reason %s (%q) is declared but missing from knownReasons", name, reason)
		}
	}
	if len(known) != want {
		t.Errorf("knownReasons has %d reasons, %d are declared (excluding ReasonUnknown)", len(known), want)
	}
}

// The collector finds an untyped reason constant by its use, and leaves alone a
// string constant that is never used as a reason.
func TestReasonConstantsFindsUntypedReasons(t *testing.T) {
	const src = `package p

type Reason string

type CauseType string

const ReasonTyped Reason = "Typed"

const (
	reasonPassed    = "Passed"
	reasonKeyed     = "Keyed"
	reasonConverted = "Converted"
	reasonCause     = "Cause"
	reasonSwitched  = "Switched"
	notAReason      = "spec.field"
)

const reasonString string = "String"

var known = map[Reason]bool{reasonKeyed: true}

func New(reason Reason, field string) {}

func f(r Reason) {
	New(reasonPassed, notAReason)
	_ = Reason(reasonConverted)
	_ = CauseType(reasonCause)
	_ = Reason(reasonString)
	switch r {
	case reasonSwitched:
	}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reasonConstants(fset, []*ast.File{file}, importer.Default())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Reason{
		"ReasonTyped":     "Typed",
		"reasonPassed":    "Passed",
		"reasonKeyed":     "Keyed",
		"reasonConverted": "Converted",
		"reasonCause":     "Cause",
		"reasonSwitched":  "Switched",
		"reasonString":    "String",
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("reason constant %s: got %q, want %q", name, got[name], value)
		}
	}
	if _, ok := got["notAReason"]; ok {
		t.Error("notAReason is a field name, not a reason, but was collected")
	}
}

// reasonConstants type-checks files as one package and returns, by name, every
// package-level string constant that is a reason: declared with the package's
// Reason type, or used anywhere where it takes the Reason type or is converted
// to a type named CauseType.
func reasonConstants(fset *token.FileSet, files []*ast.File, imp types.Importer) (map[string]Reason, error) {
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(files[0].Name.Name, fset, files, info)
	if err != nil {
		return nil, err
	}
	reasonName, ok := pkg.Scope().Lookup("Reason").(*types.TypeName)
	if !ok {
		return nil, errors.New("package declares no Reason type")
	}
	reasonType := reasonName.Type()

	isReasonUse := map[*types.Const]bool{}
	mark := func(expr ast.Expr) {
		ident, ok := ast.Unparen(expr).(*ast.Ident)
		if !ok {
			return
		}
		if c, ok := info.Uses[ident].(*types.Const); ok {
			isReasonUse[c] = true
		}
	}
	for ident, obj := range info.Uses {
		c, ok := obj.(*types.Const)
		if !ok || c.Pkg() != pkg {
			continue
		}
		// An untyped constant takes the type its context gives it: an argument
		// to a Reason parameter, a knownReasons key, a switch case on a Reason.
		if tv, ok := info.Types[ident]; ok && types.Identical(tv.Type, reasonType) {
			isReasonUse[c] = true
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			tv, ok := info.Types[call.Fun]
			if !ok || !tv.IsType() {
				return true
			}
			// A conversion to Reason or to a cause type, which a typed string
			// constant passes through without taking the Reason type.
			if named, ok := tv.Type.(*types.Named); ok &&
				(types.Identical(named, reasonType) || named.Obj().Name() == "CauseType") {
				mark(call.Args[0])
			}
			return true
		})
	}

	out := map[string]Reason{}
	for _, name := range pkg.Scope().Names() {
		c, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok || c.Val().Kind() != constant.String {
			continue
		}
		if types.Identical(c.Type(), reasonType) || isReasonUse[c] {
			out[name] = Reason(constant.StringVal(c.Val()))
		}
	}
	return out, nil
}
