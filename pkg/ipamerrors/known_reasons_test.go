package ipamerrors

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// Every Reason constant declared in this package is one ReasonFor reports and
// KnownReasons lists. A reason left out would be classified as unknown, and
// any mapping held to KnownReasons, such as the failure metric's, would never
// see it.
func TestEveryDeclaredReasonIsKnown(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "errors.go", nil, 0)
	if err != nil {
		t.Fatalf("parse errors.go: %v", err)
	}

	var declared []Reason
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if typ, ok := vs.Type.(*ast.Ident); !ok || typ.Name != "Reason" {
				continue
			}
			for _, value := range vs.Values {
				lit, ok := value.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("Reason constant with a non-literal value at %s", fset.Position(value.Pos()))
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				declared = append(declared, Reason(s))
			}
		}
	}
	if len(declared) < 2 {
		t.Fatalf("found %d Reason constants; the parse is not finding them", len(declared))
	}

	known := KnownReasons()
	for _, reason := range declared {
		if reason == ReasonUnknown {
			continue
		}
		if !slices.Contains(known, reason) {
			t.Errorf("Reason %q is declared but missing from knownReasons", reason)
		}
	}
	if len(known) != len(declared)-1 {
		t.Errorf("knownReasons has %d reasons, %d are declared (excluding ReasonUnknown)", len(known), len(declared)-1)
	}
}
