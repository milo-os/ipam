package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	clientset "go.miloapis.com/ipam/pkg/client/clientset/versioned"
)

// globalOptions holds the flags shared by every subcommand.
type globalOptions struct {
	kubeconfig string
	namespace  string
	output     string
	quiet      bool
	color      string // auto | always | never
	verbose    bool
	assumeYes  bool // --yes / --force

	// Overridable context (flags > env > config).
	org     string
	project string
}

// app is the runtime context threaded through commands. It resolves the
// transport and clientset lazily so that read-only, no-API paths (help,
// --plugin-manifest, completion) never touch credentials or the network.
type app struct {
	io   IOStreams
	opts *globalOptions

	// clientFactory builds the clientset and resolves the effective namespace.
	// It is a field so tests can inject a fake clientset without a real cluster.
	clientFactory func() (clientset.Interface, string, error)

	// entitlementCheck verifies the active project is entitled to IPAM. It is a
	// field so tests can stub it (the real one talks to the control plane). It
	// receives the effective datum scope and the prompt I/O streams.
	entitlementCheck func(env datumEnv, in io.Reader, out io.Writer) error

	// The --org/--project values as the user typed them, captured before
	// resolveDatum folds the datumctl-injected context into opts. Without the
	// snapshot there is no way to tell a flag the user set from a value the
	// environment supplied, and only the former is worth warning about.
	flagOrg, flagProject string
	scopeFlagsCaptured   bool

	// scopeWarned makes the ignored-scope warning fire once per invocation
	// rather than once per client build.
	scopeWarned bool

	// Memoized result of clientFactory so a command that fetches the client more
	// than once does not rebuild config, re-fetch a token, or re-log diagnostics.
	clientResolved bool
	cachedCS       clientset.Interface
	cachedNS       string
	cachedErr      error

	color colorState
}

// newApp wires the default, real transport-backed client factory and the real
// service-entitlement preflight.
func newApp(streams IOStreams, opts *globalOptions) *app {
	a := &app{io: streams, opts: opts}
	a.clientFactory = a.defaultClientFactory
	a.entitlementCheck = func(env datumEnv, in io.Reader, out io.Writer) error {
		return EnsureIPAMEntitlement(context.Background(), env, in, out)
	}
	return a
}

// ensureEntitlement runs the IPAM service-entitlement preflight for the active
// project. It is a no-op outside the datum path (kubeconfig/in-cluster dev and
// e2e clusters are reached directly and are not project-entitled) and a no-op at
// platform scope (no project). Prompts go to in/out, which the root wires to the
// command's stdin and stderr so the structured stdout contract stays clean.
func (a *app) ensureEntitlement(in io.Reader, out io.Writer) error {
	env, mode := a.resolveDatum()
	if mode != modeDatum {
		return nil
	}
	if a.entitlementCheck == nil {
		return nil
	}
	return a.entitlementCheck(env, in, out)
}

// resolveDatum reads the datumctl-injected context and reconciles it with the
// explicit --org/--project overrides, returning the effective scope and the
// chosen transport mode. It is shared by the client factory and the entitlement
// preflight so both agree on the active project and whether we're on the datum
// path (vs kubeconfig/in-cluster, which is not project-entitled).
func (a *app) resolveDatum() (datumEnv, transportMode) {
	env := readDatumEnv()
	// Snapshot the flags before the sync below overwrites them with the injected
	// context. Only on this first call are opts.org/opts.project still purely
	// what the user typed.
	if !a.scopeFlagsCaptured {
		a.flagOrg, a.flagProject = a.opts.org, a.opts.project
		a.scopeFlagsCaptured = true
	}
	// Effective scope: explicit --org/--project flags override the context
	// datumctl injects. The scope is carried by the control-plane URL path, so
	// keep env and opts in sync for both the transport and verbose diagnostics.
	if a.opts.org == "" {
		a.opts.org = env.org
	}
	if a.opts.project == "" {
		a.opts.project = env.project
	}
	env.org = a.opts.org
	env.project = a.opts.project
	return env, chooseMode(a.opts.kubeconfig, env)
}

func (a *app) defaultClientFactory() (clientset.Interface, string, error) {
	env, mode := a.resolveDatum()
	cfg, ns, err := restConfigFor(mode, a.opts.kubeconfig, env)
	if err != nil {
		return nil, "", err
	}
	// Namespace applies only to namespaced resources (claims/allocations); the
	// project/org scope lives in the URL, not the namespace. Only an explicit
	// --namespace overrides the transport default ("default").
	if a.opts.namespace != "" {
		ns = a.opts.namespace
	}
	// Whether or not --verbose is set: an --org/--project that does not reach the
	// server has to say so. See warnIgnoredScopeFlags.
	a.warnIgnoredScopeFlags(mode)

	// --verbose: surface the resolved scope, transport, and API host on stderr,
	// and wrap the transport so every API call (method + path) is logged. stdout
	// (the json/yaml data contract) is untouched.
	if a.opts.verbose {
		a.vlogf("resolved scope: %s", a.verboseScopeLine(mode, ns))
		a.vlogf("transport: %s", mode)
		a.vlogf("API host: %s", cfg.Host)
		cfg.Wrap(verboseTransport(a.io.ErrOut))
	}
	cs, err := newClientset(cfg)
	if err != nil {
		return nil, "", err
	}
	return cs, ns, nil
}

// client resolves the clientset and effective namespace once, memoizing the
// result so repeat callers within a command don't rebuild config, re-fetch a
// token, or re-emit verbose diagnostics.
func (a *app) client() (clientset.Interface, string, error) {
	if !a.clientResolved {
		a.cachedCS, a.cachedNS, a.cachedErr = a.clientFactory()
		a.clientResolved = true
	}
	return a.cachedCS, a.cachedNS, a.cachedErr
}

// resolveColor computes the color decision for this invocation against the data
// stream, the requested output format, and the --color mode.
func (a *app) resolveColor() {
	a.color = resolveColor(a.opts.color, a.io.Out, a.opts.output)
}

// scopeLine returns a short descriptor of the scope the call actually used, for
// success output and verbose diagnostics.
//
// # Why this is transport-aware
//
// It used to print a.opts.project unconditionally, which is only the truth on
// the Datum transport. There the project is carried in the control-plane URL
// path (see controlPlaneHost) and Milo's front gate turns that path into the
// caller's tenant identity, so --project genuinely re-targets the call.
//
// On the kubeconfig transport nothing on this side can stamp the tenant extras:
// the request goes wherever the kubeconfig points, and the server derives the
// project from the caller's identity. So `--project ipam-cli-a --verbose`
// printed `resolved scope: ipam-cli-a` while reading ipam-cli-b — the tool an
// operator reaches for to debug a scoping problem answering with the scope they
// asked for rather than the one they got. The same value reached the `claim
// create` success line and `allocation show`, so a claim was reported as landing
// in a project it had nothing to do with.
//
// Returning the namespace here is not a fallback: on this transport the
// namespace is the only tenancy boundary this side knows, and claiming to know
// the project would be inventing it again in quieter words.
func (a *app) scopeLine(namespace string) string {
	if _, mode := a.resolveDatum(); mode != modeDatum {
		return namespace
	}
	switch {
	case a.opts.org != "" && a.opts.project != "":
		return fmt.Sprintf("%s / %s", a.opts.org, a.opts.project)
	case a.opts.project != "":
		return a.opts.project
	default:
		return namespace
	}
}

// verboseScopeLine is the --verbose form of scopeLine: the same honest value,
// with the reason attached.
//
// The diagnostic gets the longer form because of what it is for. Somebody runs
// --verbose when a call went somewhere they did not expect, and a bare
// namespace on the kubeconfig transport would leave them to work out for
// themselves why the project they passed is not shown.
func (a *app) verboseScopeLine(mode transportMode, namespace string) string {
	if mode == modeDatum {
		return a.scopeLine(namespace)
	}
	return fmt.Sprintf("namespace %s — this transport carries no project; the server "+
		"derives the tenant from the caller's identity", namespace)
}

// warnIgnoredScopeFlags tells the user, once, that the --org/--project they
// passed does not reach the server on this transport.
//
// It is not gated on --verbose. A silently ignored scoping flag is the hazard
// itself: the operator believes they re-targeted the call, the call goes
// somewhere else, and every subsequent output is read against the wrong tenant.
// Making the correction visible only under a debugging flag means it is absent
// exactly when nobody suspects a problem.
//
// It goes to stderr, so the -o json|yaml stdout contract is untouched.
func (a *app) warnIgnoredScopeFlags(mode transportMode) {
	if a.scopeWarned || mode == modeDatum {
		return
	}
	if a.flagOrg == "" && a.flagProject == "" {
		return
	}
	a.scopeWarned = true

	var flags []string
	if a.flagOrg != "" {
		flags = append(flags, "--org "+a.flagOrg)
	}
	if a.flagProject != "" {
		flags = append(flags, "--project "+a.flagProject)
	}
	_, _ = fmt.Fprintf(a.io.ErrOut,
		"warning: %s ignored on the kubeconfig transport. This kubeconfig reaches one\n"+
			"         control plane directly, and the server derives the tenant from the\n"+
			"         caller's identity — nothing here can re-scope the request. To read\n"+
			"         another tenant's space, point --kubeconfig at that control plane.\n",
		strings.Join(flags, " and "))
}

// vlogf writes a diagnostic line to stderr only when --verbose is set.
func (a *app) vlogf(format string, args ...any) {
	if a.opts.verbose {
		_, _ = fmt.Fprintf(a.io.ErrOut, format+"\n", args...)
	}
}
