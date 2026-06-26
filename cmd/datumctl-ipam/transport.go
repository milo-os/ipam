package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"

	clientset "go.miloapis.com/ipam/pkg/client/clientset/versioned"
)

// verboseTransport returns a transport wrapper that logs each API call's method
// and path to w. Used under --verbose so the user can see the exact calls made
// ("why did I get this result?") without polluting stdout.
func verboseTransport(w io.Writer) transport.WrapperFunc {
	return func(rt http.RoundTripper) http.RoundTripper {
		return &loggingRoundTripper{inner: rt, w: w}
	}
}

type loggingRoundTripper struct {
	inner http.RoundTripper
	w     io.Writer
}

func (l *loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.inner.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(l.w, "API call: %s %s -> error: %v\n", req.Method, req.URL.Path, err)
		return resp, err
	}
	fmt.Fprintf(l.w, "API call: %s %s -> %s\n", req.Method, req.URL.Path, resp.Status)
	return resp, err
}

// transportMode names how the plugin reaches the IPAM API.
type transportMode string

const (
	// modeDatum uses the datumctl-injected context: a short-lived bearer token
	// from the credentials helper against DATUM_API_HOST, scoped to the active
	// org/project. This is the production path.
	modeDatum transportMode = "datum"
	// modeKubeconfig uses a standard kubeconfig (KUBECONFIG / --kubeconfig /
	// in-cluster). This is what the e2e/dev path uses, since the kind cluster has
	// no Datum front door.
	modeKubeconfig transportMode = "kubeconfig"
)

// datumEnv captures the environment contract datumctl uses to dispatch to a
// plugin. The plugin never holds a long-lived credential: it asks the helper for
// a fresh token immediately before building a client.
type datumEnv struct {
	org        string
	project    string
	apiHost    string
	credHelper string
}

func readDatumEnv() datumEnv {
	return datumEnv{
		org:        os.Getenv("DATUM_ORG"),
		project:    os.Getenv("DATUM_PROJECT"),
		apiHost:    os.Getenv("DATUM_API_HOST"),
		credHelper: os.Getenv("DATUM_CREDENTIALS_HELPER"),
	}
}

// usable reports whether the Datum transport has everything it needs. We require
// both an API host and a credentials helper; org/project may be empty for
// platform-scoped callers.
func (d datumEnv) usable() bool {
	return d.apiHost != "" && d.credHelper != ""
}

// chooseMode decides the transport. An explicit --kubeconfig always forces
// kubeconfig mode (so verification against the dev cluster is unambiguous).
// Otherwise, if datumctl injected a usable context, use it; else fall back to
// kubeconfig/in-cluster.
func chooseMode(kubeconfigFlag string, env datumEnv) transportMode {
	if kubeconfigFlag != "" {
		return modeKubeconfig
	}
	if env.usable() {
		return modeDatum
	}
	return modeKubeconfig
}

// restConfigFor builds a rest.Config for the chosen mode and returns the default
// namespace to operate in. For Datum mode the namespace defaults to the active
// project; for kubeconfig mode it comes from the current context.
func restConfigFor(mode transportMode, kubeconfigFlag string, env datumEnv) (*rest.Config, string, error) {
	switch mode {
	case modeDatum:
		return datumRestConfig(env)
	default:
		return kubeconfigRestConfig(kubeconfigFlag)
	}
}

// datumRestConfig fetches a fresh token from the credentials helper and builds a
// rest.Config pointed at the Datum API host.
func datumRestConfig(env datumEnv) (*rest.Config, string, error) {
	if !env.usable() {
		return nil, "", newCLIError(exitUnavailable,
			"Datum transport selected but DATUM_API_HOST/DATUM_CREDENTIALS_HELPER are not set").
			withFix("run via `datumctl ipam ...`, or use --kubeconfig / KUBECONFIG to target a cluster directly.")
	}
	token, err := fetchToken(env.credHelper)
	if err != nil {
		return nil, "", newCLIError(exitUnavailable, fmt.Sprintf("failed to obtain an access token: %v", err)).
			withFix("re-run `datumctl login` and try again.").withCause(err)
	}
	cfg := &rest.Config{
		Host:        env.apiHost,
		BearerToken: token,
	}
	cfg.UserAgent = userAgent()
	ns := env.project
	if ns == "" {
		ns = "default"
	}
	return cfg, ns, nil
}

// fetchToken invokes `<helper> auth get-token` and returns the trimmed token. The
// helper handles refresh transparently; the plugin only ever sees a short-lived
// bearer token.
func fetchToken(helper string) (string, error) {
	cmd := exec.Command(helper, "auth", "get-token")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("credentials helper %q returned an empty token", helper)
	}
	return token, nil
}

// kubeconfigRestConfig loads a standard kubeconfig, honoring --kubeconfig and
// KUBECONFIG, and falls back to in-cluster config when no kubeconfig is found.
func kubeconfigRestConfig(kubeconfigFlag string) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigFlag != "" {
		rules.ExplicitPath = kubeconfigFlag
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})

	cfg, err := cc.ClientConfig()
	if err != nil {
		// Last resort: in-cluster (running inside a pod).
		if inCluster, icErr := rest.InClusterConfig(); icErr == nil {
			inCluster.UserAgent = userAgent()
			return inCluster, "default", nil
		}
		return nil, "", newCLIError(exitUnavailable, fmt.Sprintf("no usable kubeconfig: %v", err)).
			withFix("set KUBECONFIG or pass --kubeconfig, or run inside the cluster.").withCause(err)
	}
	cfg.UserAgent = userAgent()

	ns, _, nsErr := cc.Namespace()
	if nsErr != nil || ns == "" {
		ns = "default"
	}
	return cfg, ns, nil
}

func userAgent() string {
	return fmt.Sprintf("datumctl-ipam/%s", pluginVersion)
}

// newClientset builds the generated IPAM clientset from a rest.Config.
func newClientset(cfg *rest.Config) (clientset.Interface, error) {
	cs, err := clientset.NewForConfig(cfg)
	if err != nil {
		return nil, newCLIError(exitUnavailable, fmt.Sprintf("failed to build IPAM client: %v", err)).withCause(err)
	}
	return cs, nil
}
