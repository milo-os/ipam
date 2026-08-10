package main

// The scope the CLI reports must be the scope the call used.
//
// --project re-targets on the Datum transport, because the project is carried
// in the control-plane URL path and Milo's front gate turns that path into the
// caller's tenant identity. On the kubeconfig transport nothing on this side
// can stamp those extras: the request goes wherever the kubeconfig points and
// the server derives the tenant from the caller's identity.
//
// The CLI reported the flag's value on both, so `--project ipam-cli-a
// --verbose` printed `resolved scope: ipam-cli-a` while reading ipam-cli-b.
// That is the worst of the three available behaviours — the diagnostic an
// operator reaches for to find a scoping problem asserting the wrong answer.
//
// scopeLine feeds three user-facing surfaces (verbose diagnostics, the `claim
// create` success line, `allocation show`), so these pin the helper rather than
// one command's output.

import (
	"strings"
	"testing"
)

func TestScopeLineIgnoresProjectOnKubeconfigTransport(t *testing.T) {
	// No DATUM_* in the environment, so chooseMode lands on kubeconfig.
	t.Setenv("DATUM_API_HOST", "")
	t.Setenv("DATUM_CREDENTIALS_HELPER", "")

	ta := newTestApp(newFakeClientset(), &globalOptions{
		output: outputTable, color: "never",
		org: "acme", project: "ipam-cli-a",
	})

	got := ta.app.scopeLine("team-ns")
	if got != "team-ns" {
		t.Errorf("scopeLine = %q; on the kubeconfig transport it must report the "+
			"namespace, not the --project value the server never saw", got)
	}
	if strings.Contains(got, "ipam-cli-a") || strings.Contains(got, "acme") {
		t.Errorf("scopeLine leaks the ignored scope flags: %q", got)
	}
}

func TestScopeLineReportsProjectOnDatumTransport(t *testing.T) {
	t.Setenv("DATUM_API_HOST", "api.datum.test")
	t.Setenv("DATUM_CREDENTIALS_HELPER", "/usr/bin/true")

	ta := newTestApp(newFakeClientset(), &globalOptions{
		output: outputTable, color: "never",
		org: "acme", project: "ipam-cli-a",
	})

	got := ta.app.scopeLine("team-ns")
	if got != "acme / ipam-cli-a" {
		t.Errorf("scopeLine = %q, want %q — --project really does re-target here",
			got, "acme / ipam-cli-a")
	}
}

func TestVerboseScopeLineSaysWhyThereIsNoProject(t *testing.T) {
	t.Setenv("DATUM_API_HOST", "")
	t.Setenv("DATUM_CREDENTIALS_HELPER", "")

	ta := newTestApp(newFakeClientset(), nil)

	got := ta.app.verboseScopeLine(modeKubeconfig, "team-ns")
	if !strings.Contains(got, "team-ns") {
		t.Errorf("verbose scope line should name the namespace it used: %q", got)
	}
	// A bare namespace would leave the reader to work out why the project they
	// passed is not shown — which is the question that made them type --verbose.
	if !strings.Contains(got, "identity") {
		t.Errorf("verbose scope line should say where the tenant comes from: %q", got)
	}
}

func TestIgnoredScopeFlagsWarnWithoutVerbose(t *testing.T) {
	t.Setenv("DATUM_API_HOST", "")
	t.Setenv("DATUM_CREDENTIALS_HELPER", "")

	ta := newTestApp(newFakeClientset(), &globalOptions{
		output: outputTable, color: "never", project: "ipam-cli-a",
	})
	// Prime the flag snapshot the way a real invocation does.
	_, mode := ta.app.resolveDatum()
	ta.app.warnIgnoredScopeFlags(mode)

	warning := ta.err.String()
	if !strings.Contains(warning, "--project ipam-cli-a") {
		t.Errorf("warning should quote the flag that was ignored:\n%s", warning)
	}
	if !strings.Contains(warning, "ignored") {
		t.Errorf("warning should say plainly that the flag was ignored:\n%s", warning)
	}
	// The warning must not be gated on --verbose: opts.verbose is false here and
	// a silently ignored scoping flag is the hazard itself.
	if warning == "" {
		t.Fatal("no warning emitted without --verbose")
	}
	// Data contract: diagnostics never touch stdout.
	if ta.out.String() != "" {
		t.Errorf("warning leaked onto stdout: %q", ta.out.String())
	}

	// Once per invocation, not once per client build.
	ta.err.Reset()
	ta.app.warnIgnoredScopeFlags(mode)
	if ta.err.String() != "" {
		t.Errorf("warning repeated:\n%s", ta.err.String())
	}
}

func TestNoScopeWarningWhenNoScopeFlagsGiven(t *testing.T) {
	t.Setenv("DATUM_API_HOST", "")
	t.Setenv("DATUM_CREDENTIALS_HELPER", "")

	ta := newTestApp(newFakeClientset(), nil)
	_, mode := ta.app.resolveDatum()
	ta.app.warnIgnoredScopeFlags(mode)

	if ta.err.String() != "" {
		t.Errorf("warned about flags nobody passed:\n%s", ta.err.String())
	}
}

func TestNoScopeWarningOnDatumTransport(t *testing.T) {
	t.Setenv("DATUM_API_HOST", "api.datum.test")
	t.Setenv("DATUM_CREDENTIALS_HELPER", "/usr/bin/true")

	ta := newTestApp(newFakeClientset(), &globalOptions{
		output: outputTable, color: "never", project: "ipam-cli-a",
	})
	_, mode := ta.app.resolveDatum()
	ta.app.warnIgnoredScopeFlags(mode)

	if ta.err.String() != "" {
		t.Errorf("--project is honoured on this transport; warning is wrong:\n%s", ta.err.String())
	}
}

// The snapshot must distinguish a flag the user typed from a value datumctl
// injected: warning about the latter would fire on every ordinary invocation.
func TestScopeFlagSnapshotExcludesInjectedContext(t *testing.T) {
	t.Setenv("DATUM_API_HOST", "")
	t.Setenv("DATUM_CREDENTIALS_HELPER", "")
	t.Setenv("DATUM_PROJECT", "injected-project")

	ta := newTestApp(newFakeClientset(), nil)
	_, mode := ta.app.resolveDatum()

	if ta.app.flagProject != "" {
		t.Errorf("flagProject = %q; an injected project is not a flag the user passed", ta.app.flagProject)
	}
	ta.app.warnIgnoredScopeFlags(mode)
	if ta.err.String() != "" {
		t.Errorf("warned about an injected context rather than a typed flag:\n%s", ta.err.String())
	}
}
