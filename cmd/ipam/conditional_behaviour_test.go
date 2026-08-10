package main

// What does an aggregated apiserver NOT get for free?
//
// Two defects came from that question and neither was in any filing:
//
//   - #85. The untenanted-write gate was installed inside `if enableQuota`, and
//     the dev overlay sets ENABLE_QUOTA=false — so a security gate was inert on
//     every dev cluster, in every e2e suite and in every load run, after it had
//     supposedly landed.
//   - #72. IPAM accepts writes into a namespace that does not exist. Stock
//     Kubernetes refuses that, and IPAM does not, because the plugin that
//     refuses it is one of the ones this server turns off.
//
// They look unrelated and they are the same shape: **a behaviour that exists in
// the platform IPAM is built on, switched off somewhere IPAM did not look.** In
// one case the switch is our own flag; in the other it is an upstream default we
// overrode wholesale.
//
// So this file enumerates the two populations where that can happen, and makes
// each one fail when it grows:
//
//	A. request filters, which may be conditional on configuration
//	B. upstream admission plugins, which this server disables entirely
//
// Both are read from the real source rather than transcribed — the filter names
// out of the package's own syntax tree, the plugin list out of
// options.NewAdmissionOptions(). A transcribed list is a list that goes stale
// the first time somebody adds an item, which is precisely the failure being
// guarded against.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/options"

	"go.miloapis.com/ipam/internal/tenant"
)

// ---------------------------------------------------------------------------
// A. Request filters
// ---------------------------------------------------------------------------

// filterClassification records whether a request filter is installed
// unconditionally, and why.
//
// `probe` is what makes an "unconditional" claim checkable. It runs against a
// chain built by installRequestFilters with quota OFF — the configuration every
// dev cluster, e2e suite and load run actually uses — and must observe the
// filter's effect. A classification with no probe is a comment, and #85 was a
// correct filter with a correct comment that never ran.
type filterClassification struct {
	// unconditional is the claim: this filter is installed whatever the flags say.
	unconditional bool
	// why explains the verdict. For a conditional filter it must say why being
	// switched off is not a security decision.
	why string
	// probe observes the filter's effect on a quota-off chain. It is handed a
	// builder rather than a chain because the observation has to happen INSIDE
	// the chain, where the REST layer would sit: a probe that ran after the
	// chain returned would see nothing a filter did to the request context, and
	// would pass against a filter that was never installed.
	//
	// Required for an unconditional filter, and must be nil for a conditional
	// one — there is nothing to observe when it is not installed.
	probe func(t *testing.T, build func(inner http.Handler) http.Handler)
}

var filterClassifications = map[string]filterClassification{
	"installPlatformProjectFilter": {
		unconditional: true,
		why: "every request needs to know which project is the platform. It decides who clears " +
			"the class-visibility gate and the class-consumption SAR, so a request judged " +
			"without it is judged against the wrong identity.",
		probe: func(t *testing.T, build func(http.Handler) http.Handler) {
			t.Helper()
			var seen string
			// Innermost in the chain, so it sees the context as the REST layer
			// would.
			chain := build(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				if p, ok := tenant.PlatformProjectFromContext(r.Context()); ok {
					seen = p
				}
			}))
			req := withUser(httptest.NewRequest(
				http.MethodGet, "/apis/ipam.miloapis.com/v1alpha1/ippools", nil), "milo-platform")
			chain.ServeHTTP(httptest.NewRecorder(), req)
			if seen != "milo-platform" {
				t.Errorf("platform project absent from the context at dispatch with quota off "+
					"(got %q); the filter is not installed", seen)
			}
		},
	},
	"installUntenantedWriteFilter": {
		unconditional: true,
		why: "#85. Tenancy is not a quota concern: quota decides whether a project may have " +
			"another address, this decides whether there is a project at all. A deployment " +
			"that turns quota off is not asking to accept writes from nobody.",
		probe: func(t *testing.T, build func(http.Handler) http.Handler) {
			t.Helper()
			reached := false
			chain := build(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			req := withUser(httptest.NewRequest(
				http.MethodPost, "/apis/ipam.miloapis.com/v1alpha1/ippools", nil), "")
			rec := httptest.NewRecorder()
			chain.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("untenanted write with quota off: status = %d, want 403", rec.Code)
			}
			if reached {
				t.Error("untenanted write with quota off reached the handler")
			}
		},
	},
	"installConsumerContextFilter": {
		unconditional: false,
		why: "conditional on --enable-quota, and legitimately so: it mirrors the tenant onto " +
			"milo's request keys, which nothing reads unless the quota plugin is installed. " +
			"It grants no access and refuses none — the write gate above is what refuses, and " +
			"that one is unconditional. If this ever starts DENYING anything, it moves.",
	},
}

// installedFilterNames reads the package's own syntax tree for every
// `install<Something>Filter` function.
//
// Read rather than listed on purpose. A hand-maintained list of filters is a
// list that stops matching the code the first time somebody adds one, and
// "somebody added a filter and nobody asked whether it should be conditional"
// is exactly the miss #85 was.
//
// # Why parser.ParseDir, which is deprecated, and not go/packages
//
// The deprecation notice says ParseDir does not consider build tags when
// associating files with packages, and points at golang.org/x/tools/go/packages.
// For most callers that is the right move. For THIS caller it is backwards, and
// the reason is worth stating because the compiler will keep suggesting it.
//
// ParseDir reads the DIRECTORY. It parses every .go file present, whatever
// build constraints they carry, so a filter behind a build tag still has to be
// classified. go/packages resolves what the CURRENT build includes, so a filter
// compiled only under a tag this test run does not set would silently vanish
// from the population — a completeness guard that stops seeing the thing it
// guards, which is the failure mode of every item in this file.
//
// So the over-approximation is deliberate and is the safe direction: the worst
// ParseDir can do is demand a classification for a filter that is not currently
// built, which is a visible nuisance. The worst go/packages can do is not ask.
//
// There are no build tags in this tree today, so neither instrument differs
// right now — which is precisely why this is written down rather than
// discovered later. If ParseDir is eventually removed, the replacement must
// still walk the directory rather than the build.
func installedFilterNames(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	// installRequestFilters is the orchestrator, not a filter. It is excluded by
	// name rather than by pattern because the pattern that excludes it
	// ("Filters" plural) would also exclude a future installTenancyFilters that
	// really did install several.
	const orchestrator = "installRequestFilters"
	re := regexp.MustCompile(`^install[A-Z].*Filter$`)

	var names []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil {
					continue
				}
				name := fn.Name.Name
				if name == orchestrator || !re.MatchString(name) {
					continue
				}
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// TestEveryRequestFilterIsClassified is the completeness half.
func TestEveryRequestFilterIsClassified(t *testing.T) {
	found := installedFilterNames(t)
	if len(found) == 0 {
		t.Fatal("no install*Filter functions found; the AST scan is broken, so this test " +
			"would pass against a package that lost every filter")
	}

	for _, name := range found {
		if _, ok := filterClassifications[name]; !ok {
			t.Errorf("request filter %s is not classified in conditional_behaviour_test.go.\n"+
				"Answer one question and add it: if an operator turns a flag off, does this "+
				"filter stop running, and does that remove a refusal?\n"+
				"  removes a refusal -> install it unconditionally, classify it unconditional, "+
				"and write a probe that observes it on a quota-off chain\n"+
				"  removes no refusal -> classify it conditional and say why being off is safe\n"+
				"This is #85: a correct, tested filter that never ran because of where it was "+
				"installed.", name)
		}
	}
	for name := range filterClassifications {
		if !contains(found, name) {
			t.Errorf("filterClassifications names %s, which no longer exists; a stale entry "+
				"makes this guard look more complete than it is", name)
		}
	}
}

// TestUnconditionalFiltersRunWithQuotaOff is the verification half, and it runs
// each classification's own probe.
//
// The configuration under test is the one that actually ships to dev, e2e and
// the load suite. #85 was not a broken filter — it was a correct filter in the
// wrong branch, and only building the chain the installer produces can tell the
// difference.
func TestUnconditionalFiltersRunWithQuotaOff(t *testing.T) {
	for name, class := range filterClassifications {
		if !class.unconditional {
			if class.probe != nil {
				t.Errorf("%s is classified conditional but carries a probe; a probe asserts the "+
					"filter runs, which is not true when it is not installed", name)
			}
			continue
		}
		if class.probe == nil {
			t.Errorf("%s is classified unconditional with no probe. The claim has to be "+
				"observable or it is a comment, and #85 was a comment that was true of the "+
				"filter and false of the wiring", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			class.probe(t, quotaOffChainTo(t))
		})
	}
}

// quotaOffChainTo returns a builder that wraps any inner handler in exactly the
// chain installRequestFilters produces with --enable-quota=false.
//
// The chain THE INSTALLER PRODUCED, not the filters called directly. Calling a
// filter directly tests the filter, which in #85 was never broken, and would
// pass with the gate wired back inside the quota branch — the exact defect.
func quotaOffChainTo(t *testing.T) func(http.Handler) http.Handler {
	t.Helper()
	return func(inner http.Handler) http.Handler {
		cfg := &genericapiserver.RecommendedConfig{}
		// Seeded with an identity chain so the installers compose against it
		// rather than DefaultBuildHandlerChain, which needs a fully populated
		// Config and would put apiserver machinery under test instead of our
		// wiring.
		cfg.BuildHandlerChainFunc = func(h http.Handler, _ *genericapiserver.Config) http.Handler { return h }
		installRequestFilters(cfg, "milo-platform", false)
		return cfg.BuildHandlerChainFunc(inner, &genericapiserver.Config{})
	}
}

// ---------------------------------------------------------------------------
// B. Upstream admission plugins this server turns off
// ---------------------------------------------------------------------------

// disabledPluginVerdict records what an upstream admission plugin would have
// enforced, and IPAM's decision about losing it.
type disabledPluginVerdict struct {
	// enforces is what the plugin does in a stock apiserver.
	enforces string
	// verdict is why IPAM is willing to run without it — or, where it is not,
	// what the resulting gap is and which task tracks it.
	verdict string
	// gap marks a plugin whose absence is a KNOWN missing behaviour rather than
	// one legitimately delegated elsewhere.
	gap bool
}

// disabledPluginVerdicts classifies every plugin in upstream's
// RecommendedPluginOrder, all of which disableAllAdmission removes.
//
// The list is not transcribed — the test reads it from
// options.NewAdmissionOptions(), so a Kubernetes bump that adds a plugin fails
// this test until somebody says what IPAM loses by not running it. That is the
// drift this guard exists for: the set of things you have turned off changes
// when you upgrade, and nothing announces it.
var disabledPluginVerdicts = map[string]disabledPluginVerdict{
	"NamespaceLifecycle": {
		enforces: "rejects writes into a namespace that does not exist or is terminating, and " +
			"protects the system namespaces from deletion",
		verdict: "GAP, tracked by #72 and #86. IPAM accepts objects into namespaces that do not " +
			"exist, where stock Kubernetes refuses. It is not simply delegable: this server " +
			"has no informer on namespaces, and #86 has to settle where a project's " +
			"namespaces are authoritative before the check can be written at all.",
		gap: true,
	},
	"MutatingAdmissionPolicy": {
		enforces: "applies cluster-authored CEL mutation policies to incoming objects",
		verdict: "Deferred to the main kube-apiserver. A policy targeting ipam.miloapis.com " +
			"resources would not be enforced here, which is a real limitation of aggregation " +
			"and not specific to this server; the plugin also needs informers that block " +
			"readyz without a fully wired CoreAPI client.",
	},
	"MutatingAdmissionWebhook": {
		enforces: "calls registered mutating webhooks",
		verdict: "Deferred to the main kube-apiserver, same reasoning as the policy plugin. " +
			"Note IPAM's own defaulting is in the registry strategies, so nothing IPAM " +
			"itself needs depends on this.",
	},
	"ValidatingAdmissionPolicy": {
		enforces: "evaluates cluster-authored CEL validation policies",
		verdict: "Deferred. IPAM's own validation is in the registry strategies and is " +
			"unconditional, so turning this off removes no IPAM-authored rule.",
	},
	"ValidatingAdmissionWebhook": {
		enforces: "calls registered validating webhooks",
		verdict:  "Deferred, same as the mutating case.",
	},
}

// TestEveryDisabledUpstreamPluginIsClassified reads the upstream list and
// requires a verdict for each.
func TestEveryDisabledUpstreamPluginIsClassified(t *testing.T) {
	upstream := options.NewAdmissionOptions().RecommendedPluginOrder
	if len(upstream) == 0 {
		t.Fatal("upstream RecommendedPluginOrder is empty; the probe is broken and this test " +
			"would pass against any configuration")
	}

	for _, name := range upstream {
		if _, ok := disabledPluginVerdicts[name]; !ok {
			t.Errorf("upstream admission plugin %q is enabled by default in this Kubernetes "+
				"version and IPAM turns it off, with no recorded verdict.\n"+
				"Say what it enforces and why running without it is acceptable — or mark it a "+
				"gap and file the task.\n"+
				"This fires on a Kubernetes bump that adds a plugin, which is the only moment "+
				"the set of things you have switched off changes without anyone deciding to.",
				name)
		}
	}
	for name := range disabledPluginVerdicts {
		if !contains(upstream, name) {
			t.Errorf("verdict recorded for %q, which upstream no longer enables by default; "+
				"a stale entry makes this guard look more complete than it is", name)
		}
	}
}

// TestDisableAllAdmissionReallyDisablesThemAll verifies the premise the verdicts
// above rest on.
//
// Every verdict says "IPAM turns this off". If that stopped being true — a
// plugin left enabled by a refactor — the verdicts would describe a server that
// no longer exists, and a reader would trust a table that had quietly inverted.
func TestDisableAllAdmissionReallyDisablesThemAll(t *testing.T) {
	o := &IPAMServerOptions{RecommendedOptions: options.NewRecommendedOptions("", nil)}
	disableAllAdmission(o)

	if got := o.RecommendedOptions.Admission.RecommendedPluginOrder; len(got) != 0 {
		t.Errorf("RecommendedPluginOrder = %v, want empty", got)
	}
	if got := o.RecommendedOptions.Admission.Plugins.Registered(); len(got) != 0 {
		t.Errorf("registered plugins = %v, want none", got)
	}
}

// TestQuotaAdmissionEnablesOnlyIPAMsOwnPlugins pins the other configuration.
//
// With --enable-quota the plugin set is replaced rather than added to, so the
// upstream five stay off in BOTH configurations. Without this, a reader could
// reasonably assume quota mode restores the recommended set, and the verdicts
// above would only be true of half the deployments.
func TestQuotaAdmissionEnablesOnlyIPAMsOwnPlugins(t *testing.T) {
	o := &IPAMServerOptions{RecommendedOptions: options.NewRecommendedOptions("", nil)}
	registerQuotaAdmission(o)

	registered := o.RecommendedOptions.Admission.Plugins.Registered()
	for _, upstreamName := range options.NewAdmissionOptions().RecommendedPluginOrder {
		if contains(registered, upstreamName) {
			t.Errorf("upstream plugin %q is registered under --enable-quota; the verdicts in "+
				"disabledPluginVerdicts claim it is off in every configuration", upstreamName)
		}
	}
	if !contains(registered, platformConsumerGuardName) {
		t.Errorf("the platform-consumer guard is not registered under --enable-quota; "+
			"registered = %v", registered)
	}
}

// contains is a local helper; slices.Contains would do, but this file is read
// by people auditing a security population and a stdlib import list that stays
// short keeps the reading cheap.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// C. The other source of conditional behaviour: the environment
// ---------------------------------------------------------------------------

// Populations A and B cover behaviour gated on a FLAG and behaviour inherited
// from upstream. Neither would have caught a behaviour gated on an environment
// variable, and #85 hid one level up from where anyone was looking — so the
// question is worth asking of every configuration source, not just the one that
// produced the last defect.
//
// The claim this asserts is strong and currently true: **nothing in the serving
// path reads the environment.** Every knob is a flag, which means every knob is
// discoverable with --help, appears in the Deployment's args, and is visible to
// anyone auditing a running server. An environment read is none of those things
// — it does not appear in --help, and on this project the dev overlay sets
// environment variables (ENABLE_QUOTA among them) that a reader could easily
// assume are read directly rather than passed through to flags.

type envReadClassification struct {
	// where names the file, so the claim can be checked in one jump.
	where string
	// why explains what the variable does and why reading it is not a
	// conditional behaviour in the serving path.
	why string
}

var envReadClassifications = map[string]envReadClassification{
	"POSTGRES_DSN": {
		where: "cmd/ipam/migrate.go and cmd/ipam/reclaim.go",
		why: "a fallback source for a required connection string on two ADMIN subcommands, " +
			"neither of which serves traffic. It selects no behaviour: with or without it the " +
			"command does the same thing, and absent both it refuses rather than defaulting.",
	},
	"IPAM_TEST_POSTGRES_DSN": {
		where: "internal/testdb/testdb.go",
		why: "test harness only. The package exists to hand tests a database and is not " +
			"reachable from the server binary.",
	},
}

// environmentReads scans the serving path for os.Getenv / os.LookupEnv and
// returns the variable names read.
//
// Directories rather than packages: this is a completeness guard, so it must
// over-approximate. See installedFilterNames for why that direction is the safe
// one.
func environmentReads(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}

	for _, root := range []string{".", "../../internal"} {
		err := filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "os" {
					return true
				}
				if sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
					return true
				}
				if len(call.Args) == 0 {
					// A non-literal argument is worse than an unclassified one:
					// nothing can enumerate what it reads. Recorded under a name
					// that cannot be classified away.
					found["<non-literal>"] = path
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					found["<non-literal>"] = path
					return true
				}
				found[strings.Trim(lit.Value, `"`)] = path
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return found
}

// TestEveryEnvironmentReadIsClassified fails when the serving path grows a
// dependency on the environment that nobody has justified.
//
// cmd/milo-ipam is deliberately out of scope: it is the CLI, it runs on an
// operator's workstation, and NO_COLOR and CI are conventions it SHOULD honour.
// The claim here is about the server.
func TestEveryEnvironmentReadIsClassified(t *testing.T) {
	reads := environmentReads(t)

	// Positive control. The scan finding nothing is indistinguishable from a
	// scan that is broken, and this file's whole subject is signals that are
	// absent for the wrong reason.
	if len(reads) == 0 {
		t.Fatal("the environment scan found no reads at all, including the ones known to " +
			"exist in migrate.go and testdb.go; the scan is broken and would pass against " +
			"any amount of new environment-gated behaviour")
	}

	for name, where := range reads {
		if _, ok := envReadClassifications[name]; !ok {
			t.Errorf("%s reads environment variable %q, which is not classified.\n"+
				"Prefer a flag: a flag appears in --help, in the Deployment's args, and to "+
				"anyone auditing a running server, and an environment read does none of "+
				"those. If it must be an environment read, say what it does and why it "+
				"selects no behaviour in the serving path.", where, name)
		}
	}
	for name, class := range envReadClassifications {
		if _, ok := reads[name]; !ok {
			t.Errorf("%q is classified (%s) but no longer read; a stale entry makes this "+
				"guard look more complete than it is", name, class.where)
		}
	}
}
