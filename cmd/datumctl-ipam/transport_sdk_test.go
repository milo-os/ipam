package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeFakeHelper writes an executable that mimics the datumctl credentials
// helper: `<helper> auth get-token` prints a token to stdout. Returns its path.
func writeFakeHelper(t *testing.T, token string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake credentials helper script is POSIX-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-helper")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"auth\" ] && [ \"$2\" = \"get-token\" ]; then\n" +
		"  printf '%s' '" + token + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake helper: %v", err)
	}
	return path
}

// setDatumEnv points the plugin SDK at a fake datum context for the duration of
// the test. The SDK (and readDatumEnv) read these same variables.
func setDatumEnv(t *testing.T, host, org, project, helper string) {
	t.Helper()
	t.Setenv("DATUM_API_HOST", host)
	t.Setenv("DATUM_ORG", org)
	t.Setenv("DATUM_PROJECT", project)
	t.Setenv("DATUM_CREDENTIALS_HELPER", helper)
	t.Setenv("DATUM_SESSION", "")
}

func TestReadDatumEnvViaSDK(t *testing.T) {
	setDatumEnv(t, "api.example.test", "acme", "proj-1", "/usr/bin/datumctl")
	env := readDatumEnv()
	if env.apiHost != "api.example.test" || env.org != "acme" ||
		env.project != "proj-1" || env.credHelper != "/usr/bin/datumctl" {
		t.Fatalf("readDatumEnv() = %+v, did not reflect DATUM_* env via SDK", env)
	}
}

func TestChooseModeAndUsable(t *testing.T) {
	cases := []struct {
		name           string
		kubeconfigFlag string
		env            datumEnv
		want           transportMode
	}{
		{
			name: "datum env complete selects datum mode",
			env:  datumEnv{apiHost: "api.example.test", credHelper: "/x/datumctl", project: "p"},
			want: modeDatum,
		},
		{
			name: "missing helper falls back to kubeconfig",
			env:  datumEnv{apiHost: "api.example.test"},
			want: modeKubeconfig,
		},
		{
			name: "missing host falls back to kubeconfig",
			env:  datumEnv{credHelper: "/x/datumctl"},
			want: modeKubeconfig,
		},
		{
			name:           "explicit --kubeconfig forces kubeconfig even with datum env",
			kubeconfigFlag: "/tmp/kubeconfig",
			env:            datumEnv{apiHost: "api.example.test", credHelper: "/x/datumctl"},
			want:           modeKubeconfig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseMode(tc.kubeconfigFlag, tc.env); got != tc.want {
				t.Errorf("chooseMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDatumRestConfigUsesSDKToken(t *testing.T) {
	helper := writeFakeHelper(t, "sdk-token-abc")
	setDatumEnv(t, "api.example.test", "acme", "proj-1", helper)

	env := readDatumEnv()
	cfg, ns, err := datumRestConfig(env)
	if err != nil {
		t.Fatalf("datumRestConfig() error = %v", err)
	}
	if cfg.BearerToken != "sdk-token-abc" {
		t.Errorf("BearerToken = %q, want token from credentials helper", cfg.BearerToken)
	}
	wantHost := "https://api.example.test/apis/resourcemanager.miloapis.com/v1alpha1/projects/proj-1/control-plane"
	if cfg.Host != wantHost {
		t.Errorf("Host = %q, want %q", cfg.Host, wantHost)
	}
	if ns != "default" {
		t.Errorf("namespace = %q, want default", ns)
	}
}

func TestDatumRestConfigRequiresUsableEnv(t *testing.T) {
	// No host/helper: datum config must refuse with an unavailable CLI error.
	_, _, err := datumRestConfig(datumEnv{})
	if err == nil {
		t.Fatal("datumRestConfig() with empty env: expected error, got nil")
	}
	ce, ok := err.(*cliError)
	if !ok || ce.code != exitUnavailable {
		t.Fatalf("expected *cliError exitUnavailable, got %T %v", err, err)
	}
}

func TestKubeconfigRestConfigPreserved(t *testing.T) {
	// The kubeconfig path must keep working independently of the SDK/datum env.
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "config")
	const content = `apiVersion: v1
kind: Config
clusters:
- name: dev
  cluster:
    server: https://kube.example.test:6443
contexts:
- name: dev
  context:
    cluster: dev
    user: dev
    namespace: ipam-dev
current-context: dev
users:
- name: dev
  user:
    token: kubeconfig-token
`
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}

	cfg, ns, err := kubeconfigRestConfig(kubeconfig)
	if err != nil {
		t.Fatalf("kubeconfigRestConfig() error = %v", err)
	}
	if cfg.Host != "https://kube.example.test:6443" {
		t.Errorf("Host = %q, want kubeconfig server", cfg.Host)
	}
	if cfg.BearerToken != "kubeconfig-token" {
		t.Errorf("BearerToken = %q, want kubeconfig token", cfg.BearerToken)
	}
	if ns != "ipam-dev" {
		t.Errorf("namespace = %q, want ipam-dev from context", ns)
	}
}
