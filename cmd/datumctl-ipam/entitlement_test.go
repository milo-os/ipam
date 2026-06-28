package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEnsureIPAMEntitlementNoopAtPlatformScope(t *testing.T) {
	// project == "" (platform scope): the preflight must not touch the network.
	var out bytes.Buffer
	err := EnsureIPAMEntitlement(context.Background(), datumEnv{apiHost: "api.example.test", credHelper: "/x"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("EnsureIPAMEntitlement at platform scope = %v, want nil", err)
	}
	if out.Len() != 0 {
		t.Errorf("platform scope wrote %q, want no output", out.String())
	}
}

func TestEnsureEntitlementSkippedInKubeconfigMode(t *testing.T) {
	// With an explicit --kubeconfig (dev/e2e path), the entitlement check must
	// never run, even with a project set: dev clusters are not project-entitled.
	a := newApp(IOStreams{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
		&globalOptions{output: outputTable, color: "never", kubeconfig: "/tmp/kc", project: "proj-1"})

	called := false
	a.entitlementCheck = func(env datumEnv, in io.Reader, out io.Writer) error { called = true; return nil }

	if err := a.ensureEntitlement(strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("ensureEntitlement (kubeconfig mode) = %v, want nil", err)
	}
	if called {
		t.Error("entitlement check ran in kubeconfig mode; it must be skipped")
	}
}

func TestEnsureEntitlementRunsInDatumMode(t *testing.T) {
	helper := writeFakeHelper(t, "tok")
	setDatumEnv(t, "api.example.test", "acme", "proj-1", helper)

	a := newApp(IOStreams{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
		&globalOptions{output: outputTable, color: "never"})

	var gotEnv datumEnv
	called := false
	a.entitlementCheck = func(env datumEnv, in io.Reader, out io.Writer) error {
		called = true
		gotEnv = env
		return nil
	}

	if err := a.ensureEntitlement(strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("ensureEntitlement (datum mode) = %v, want nil", err)
	}
	if !called {
		t.Fatal("entitlement check did not run in datum mode")
	}
	if gotEnv.project != "proj-1" || gotEnv.org != "acme" {
		t.Errorf("entitlement check got env %+v, want project=proj-1 org=acme", gotEnv)
	}
}

func TestSkipEntitlementCheck(t *testing.T) {
	for _, name := range []string{"version", "completion", "help", "__complete"} {
		cmd := &cobra.Command{Use: name}
		cmd.Flags().Bool("help", false, "")
		if !skipEntitlementCheck(cmd) {
			t.Errorf("skipEntitlementCheck(%q) = false, want true", name)
		}
	}

	gated := &cobra.Command{Use: "claim"}
	gated.Flags().Bool("help", false, "")
	if skipEntitlementCheck(gated) {
		t.Error("skipEntitlementCheck(claim) = true, want false (API command must be gated)")
	}

	helped := &cobra.Command{Use: "claim"}
	helped.Flags().Bool("help", false, "")
	_ = helped.Flags().Set("help", "true")
	if !skipEntitlementCheck(helped) {
		t.Error("skipEntitlementCheck(claim --help) = false, want true")
	}
}
