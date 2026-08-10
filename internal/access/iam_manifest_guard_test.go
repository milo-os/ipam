package access

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/yaml"
)

// This file guards the seam between the authorization checks this service
// performs and the Milo IAM manifests that make those checks grantable. They
// are separate artifacts in separate languages with nothing connecting them,
// and when they drifted apart nothing failed.
//
// # Why this compares against the CODE and not against the sibling manifest
//
// The obvious guard — assert every permission a Role grants is registered on a
// ProtectedResource — does not work, and it is worth knowing exactly how it
// fails before someone simplifies this file into it.
//
// That check was written, and it passes on the manifests as they stood before
// 2026-08-08: 25 permissions registered, 25 granted, no gap in either
// direction. The manifests were internally consistent and jointly wrong. They
// registered and granted `ipam.miloapis.com/ippools.use` — the pre-class-model
// gate, which no code path had read since a claim stopped naming a pool — and
// they registered and granted nothing at all for `ipclasses.use`, which is the
// permission every consumer claim is actually checked against. Both files were
// derived from the same stale assumption, so agreeing with each other proved
// only that the assumption was applied consistently.
//
// The consequence in a deployment that uses the shipped IAM model is that every
// consumer claim is denied, while a deployment that authorizes some other way —
// a kind cluster with a hand-written Kubernetes ClusterRole, which is what all
// the e2e and load suites run against — works perfectly. So the failure is
// invisible to every test in this repo except one that reads the manifests.
//
// A guard is only evidence about the thing it compares against. This one
// derives the required permissions from the code that issues the
// SubjectAccessReview, which is an independent source: if the manifests and the
// gate disagree, one of them is wrong and the disagreement is visible here.
//
// # Why the inventory test exists alongside the value tests
//
// Reading one SAR site tells you about one SAR site. The inventory test below
// enumerates every `authorizer.AttributesRecord` construction under internal/
// and fails when the set changes, so a new authorization check cannot be added
// without either being covered here or announcing itself. Without it this file
// would guard the path it was written for and silently ignore the next one.

// standardVerbs are the verbs the API machinery authorizes generically for any
// registered resource. A permission using one of these is meaningful whether or
// not this service's own code mentions it, so the reverse check below ignores
// them.
//
// Anything NOT in this set is a custom verb, and a custom verb means nothing
// unless some code path checks it. `use` is the only one today. That asymmetry
// is the entire reason the reverse check can exist: a granted `ippools.use`
// with no SAR behind it is not a harmless extra permission, it is a grant that
// reads as authorising something and authorises nothing.
var standardVerbs = map[string]bool{
	"get":              true,
	"list":             true,
	"watch":            true,
	"create":           true,
	"update":           true,
	"patch":            true,
	"delete":           true,
	"deletecollection": true,
	"updateStatus":     true,
}

// permission is one `<apiGroup>/<plural>.<verb>` triple, held apart so the
// error messages can name each part.
type permission struct {
	apiGroup string
	resource string
	verb     string
}

func (p permission) String() string {
	return p.apiGroup + "/" + p.resource + "." + p.verb
}

// iamDir is this repo's Milo IAM component, relative to internal/access/.
const iamDir = "../../config/components/iam"

// --------------------------------------------------------------------------
// The forward check: everything the code requires must be grantable.
// --------------------------------------------------------------------------

func TestEverySARTheCodeIssuesIsRegisteredAndGrantable(t *testing.T) {
	required := sarsIssuedByCode(t)
	registered, _ := readProtectedResources(t)
	granted := readRoleGrants(t)

	// Populations first. A pass computed from an empty set is not a pass, and
	// all three of these read from the filesystem, so all three can come back
	// empty for reasons that have nothing to do with the invariant — a moved
	// directory, a failed checkout, a renamed field. This guard has already
	// been observed reporting "0 registered, 0 granted, no gaps" against a tree
	// that did not contain the manifests.
	if len(required) == 0 {
		t.Fatal("found no SARs in the code: the scanner is broken, not the service")
	}
	if len(registered) == 0 {
		t.Fatalf("found no ProtectedResource permissions under %s: nothing was read", iamDir)
	}
	if len(granted) == 0 {
		t.Fatalf("found no Role permissions under %s: nothing was read", iamDir)
	}

	for _, p := range sortedPermissions(required) {
		if !registered[p.String()] {
			t.Errorf("%s is checked by the code but is not registered on any ProtectedResource.\n"+
				"Milo cannot grant a permission it does not know about, so every caller needing this "+
				"check is denied. Add %q to the permissions list of the %s ProtectedResource in %s.",
				p, p.verb, p.resource, iamDir+"/protected-resources")
		}
		if !granted[p.String()] {
			t.Errorf("%s is checked by the code but no Role grants it.\n"+
				"The permission exists and nothing can hold it, so every caller needing this check is "+
				"denied. Add it to whichever role in %s should confer it.",
				p, iamDir+"/roles")
		}
	}
}

// --------------------------------------------------------------------------
// The reverse check: a custom verb nothing checks is a grant that grants
// nothing. This is how ippools.use outlived the code that read it.
// --------------------------------------------------------------------------

func TestNoCustomVerbIsShippedThatNoCodePathChecks(t *testing.T) {
	required := sarsIssuedByCode(t)
	registered, _ := readProtectedResources(t)
	granted := readRoleGrants(t)

	if len(required) == 0 || len(registered) == 0 || len(granted) == 0 {
		t.Fatal("empty population: see the forward test for why this is a failure and not a pass")
	}

	shipped := map[string]string{}
	for p := range registered {
		shipped[p] = "registered on a ProtectedResource"
	}
	for p := range granted {
		if existing, ok := shipped[p]; ok {
			shipped[p] = existing + " and granted by a Role"
		} else {
			shipped[p] = "granted by a Role"
		}
	}

	for _, name := range sortedKeys(shipped) {
		verb := name[strings.LastIndex(name, ".")+1:]
		if standardVerbs[verb] {
			continue
		}
		if required[name] {
			continue
		}
		t.Errorf("%s is %s, but no code path issues that SubjectAccessReview.\n"+
			"A custom verb is only meaningful because something checks it. This one reads as "+
			"authorising something and authorises nothing, so an operator who grants it believes "+
			"they have opened access they have not. Either delete it from the manifests, or point "+
			"it at the check that reads it.", name, shipped[name])
	}
}

// --------------------------------------------------------------------------
// The population guard: every SAR site in the service, not just the one this
// file was written for.
// --------------------------------------------------------------------------

// knownSARSites is every place the service builds authorization attributes, as
// "<file>:<APIGroup>/<Resource>.<Verb>". A new entry here is a new
// authorization boundary and should be a deliberate act.
//
// If this test fails because you added a check, add the site — and make sure
// the manifests in config/components/iam/ grant it, which the forward test
// above will tell you about next.
var knownSARSites = []string{
	"class.go:ipam.miloapis.com/ipclasses.use",
}

func TestSARSiteInventory(t *testing.T) {
	found := scanSARSites(t, "..")
	if len(found) == 0 {
		t.Fatal("scanned internal/ and found no authorizer.AttributesRecord at all; " +
			"the scanner has stopped matching and every other assertion here is vacuous")
	}

	got := sortedKeys(found)
	want := append([]string(nil), knownSARSites...)
	sort.Strings(want)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the set of authorization checks has changed.\n got:\n  %s\nwant:\n  %s\n"+
			"Update knownSARSites if this is deliberate, then confirm config/components/iam/ "+
			"registers and grants the new permission.",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// --------------------------------------------------------------------------
// The #56 decision, asserted rather than left as a convention.
// --------------------------------------------------------------------------

// TestIPClassIsNotBindableAtAProject pins the one line in the IAM manifests
// that is a security boundary rather than a configuration choice.
//
// The ipclass ProtectedResource declares no parentResources, which is what
// makes classes platform-only: without a parent, no PolicyBinding can confer
// class permissions at a project, so no tenant can author a class. That matters
// because the allocator's default-class lookup scans every IPClass in the
// database with no tenant predicate, while the registry's "at most one default
// per family" guard lists through the tenant-scoped store — so a tenant-authored
// class carrying the default annotation is resolved by other tenants' claims and
// by the platform's own, and the object doing it does not appear in a
// platform-scoped list.
//
// Adding `parentResources: [Project]` here is one line and looks like a
// symmetry fix, since every other resource has it. See the manifest for what
// breaks.
func TestIPClassIsNotBindableAtAProject(t *testing.T) {
	_, parents := readProtectedResources(t)
	got, ok := parents["ipclasses"]
	if !ok {
		t.Fatalf("no ipclass ProtectedResource found under %s: the class gate is not grantable at all", iamDir)
	}
	if len(got) != 0 {
		t.Errorf("the ipclass ProtectedResource declares parentResources %v, making classes bindable "+
			"at a project. Classes are operator-authored policy and must stay platform-only; see the "+
			"comment in %s/protected-resources/ipclass.yaml.", got, iamDir)
	}
	// A positive control: the assertion above must be capable of seeing a
	// parent, or it would pass for a parser that returns nothing for everyone.
	if len(parents["ippools"]) == 0 {
		t.Error("ippools reports no parentResources either, so the check above proves nothing — " +
			"the parser is not reading parentResources")
	}
}

// --------------------------------------------------------------------------
// Reading the code.
// --------------------------------------------------------------------------

// sarsIssuedByCode returns every permission the service checks, read from the
// authorizer.AttributesRecord literals under internal/.
func sarsIssuedByCode(t *testing.T) map[string]bool {
	t.Helper()
	sites := scanSARSites(t, "..")
	out := map[string]bool{}
	for site := range sites {
		out[site[strings.Index(site, ":")+1:]] = true
	}
	return out
}

// scanSARSites walks root for authorizer.AttributesRecord composite literals and
// returns "<file>:<group>/<resource>.<verb>" for each.
//
// A field whose value is not a string literal is a hard failure rather than a
// skipped entry. The pre-class-model checker built its attributes with
// `Resource: resource` from a parameter, and a scanner that quietly ignored
// that would have reported an empty set and a clean pass for the exact code it
// was written to check.
func scanSARSites(t *testing.T, root string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isAttributesRecord(lit.Type) {
				return true
			}
			fields := map[string]*ast.KeyValueExpr{}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok {
					fields[key.Name] = kv
				}
			}
			var parts [3]string
			for i, name := range []string{"APIGroup", "Resource", "Verb"} {
				kv, ok := fields[name]
				if !ok {
					t.Errorf("%s: authorizer.AttributesRecord has no %s field; this guard cannot "+
						"tell what permission it requires", fset.Position(lit.Pos()), name)
					return false
				}
				val, ok := stringLiteral(kv.Value)
				if !ok {
					t.Errorf("%s: authorizer.AttributesRecord builds %s from a non-literal "+
						"expression, so this guard cannot read the permission it requires. Either "+
						"make it a constant, or assert this site's permission explicitly here.",
						fset.Position(lit.Pos()), name)
					return false
				}
				parts[i] = val
			}
			found[filepath.Base(path)+":"+parts[0]+"/"+parts[1]+"."+parts[2]] = true
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan internal/ for authorization checks: %v", err)
	}
	return found
}

func isAttributesRecord(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "AttributesRecord" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "authorizer"
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// --------------------------------------------------------------------------
// Reading the manifests.
// --------------------------------------------------------------------------

type protectedResourceDoc struct {
	Kind string `json:"kind"`
	Spec struct {
		ServiceRef struct {
			Name string `json:"name"`
		} `json:"serviceRef"`
		Plural          string   `json:"plural"`
		Permissions     []string `json:"permissions"`
		ParentResources []struct {
			APIGroup string `json:"apiGroup"`
			Kind     string `json:"kind"`
		} `json:"parentResources"`
	} `json:"spec"`
}

type roleDoc struct {
	Kind string `json:"kind"`
	Spec struct {
		IncludedPermissions []string `json:"includedPermissions"`
	} `json:"spec"`
}

// readProtectedResources returns the set of registered permission strings and,
// separately, each resource's declared parent kinds.
func readProtectedResources(t *testing.T) (map[string]bool, map[string][]string) {
	t.Helper()
	perms := map[string]bool{}
	parents := map[string][]string{}
	for _, path := range yamlFilesIn(t, filepath.Join(iamDir, "protected-resources")) {
		var doc protectedResourceDoc
		decodeYAML(t, path, &doc)
		if doc.Kind != "ProtectedResource" {
			continue
		}
		group := doc.Spec.ServiceRef.Name
		for _, verb := range doc.Spec.Permissions {
			perms[group+"/"+doc.Spec.Plural+"."+verb] = true
		}
		kinds := []string{}
		for _, p := range doc.Spec.ParentResources {
			kinds = append(kinds, p.Kind)
		}
		parents[doc.Spec.Plural] = kinds
	}
	return perms, parents
}

// readRoleGrants returns the union of every Role's includedPermissions.
//
// The union, not a per-role view: the question this guard asks is whether
// anything at all can confer the permission, which is what decides between "a
// caller might be denied" and "every caller is denied whatever they hold".
// Inherited roles need no special handling, since inheritance can only widen
// what some role in this same set already lists.
func readRoleGrants(t *testing.T) map[string]bool {
	t.Helper()
	granted := map[string]bool{}
	for _, path := range yamlFilesIn(t, filepath.Join(iamDir, "roles")) {
		var doc roleDoc
		decodeYAML(t, path, &doc)
		if doc.Kind != "Role" {
			continue
		}
		for _, p := range doc.Spec.IncludedPermissions {
			granted[p] = true
		}
	}
	return granted
}

func yamlFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "kustomization.yaml" {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

func decodeYAML(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// --------------------------------------------------------------------------
// Cross-check: the scanner's reading of class.go against what the checker
// actually hands the authorizer.
// --------------------------------------------------------------------------

type recordingAuthorizer struct{ got authorizer.Attributes }

func (r *recordingAuthorizer) Authorize(_ context.Context, a authorizer.Attributes) (authorizer.Decision, string, error) {
	r.got = a
	return authorizer.DecisionAllow, "", nil
}

// TestClassCheckerIssuesTheScannedPermission runs the real checker against a
// recording authorizer and compares what arrives with what the source scanner
// read.
//
// The two derivations are independent — one reads the syntax, the other watches
// the value reach the authorizer — so this catches the case the AST scan cannot
// see on its own: a literal that is present and correct in the source but not
// the one that ends up being asked, because some wrapper rewrites it.
func TestClassCheckerIssuesTheScannedPermission(t *testing.T) {
	rec := &recordingAuthorizer{}
	checker := NewClassAccessChecker(rec)

	ctx := request.WithUser(context.Background(), &user.DefaultInfo{Name: "someone"})
	if _, err := checker.CanUseClass(ctx, "any-class"); err != nil {
		t.Fatalf("CanUseClass: %v", err)
	}
	if rec.got == nil {
		t.Fatal("the authorizer was never called, so this test asserts nothing")
	}

	observed := rec.got.GetAPIGroup() + "/" + rec.got.GetResource() + "." + rec.got.GetVerb()
	if !sarsIssuedByCode(t)[observed] {
		t.Errorf("the checker asks for %q at runtime, which is not among the permissions the source "+
			"scanner found (%v). The manifests are being checked against a permission the service does "+
			"not actually request.", observed, sortedKeys(sarsIssuedByCode(t)))
	}
}

// --------------------------------------------------------------------------

// sortedKeys is generic over the value type so the same helper serves the
// set-of-permissions maps and the permission-to-provenance map.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPermissions(m map[string]bool) []permission {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]permission, 0, len(names))
	for _, n := range names {
		slash := strings.Index(n, "/")
		dot := strings.LastIndex(n, ".")
		out = append(out, permission{apiGroup: n[:slash], resource: n[slash+1 : dot], verb: n[dot+1:]})
	}
	return out
}
