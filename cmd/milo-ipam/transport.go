package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.datum.net/datumctl/plugin"
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
		_, _ = fmt.Fprintf(l.w, "API call: %s %s -> error: %v\n", req.Method, req.URL.Path, err)
		return resp, err
	}
	_, _ = fmt.Fprintf(l.w, "API call: %s %s -> %s\n", req.Method, req.URL.Path, resp.Status)
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
//
// The fields are sourced from the datumctl plugin SDK (go.datum.net/datumctl/
// plugin), which reads the same DATUM_* environment variables. We keep this as a
// local struct (rather than passing plugin.PluginContext around) so the
// control-plane URL construction and --org/--project overrides stay testable
// without the SDK and without exporting the SDK type through the codebase.
type datumEnv struct {
	org        string
	project    string
	apiHost    string
	credHelper string
}

// readDatumEnv reads the datumctl-injected context via the plugin SDK. The SDK
// resolves the same DATUM_* variables the plugin used to read by hand; routing
// through it keeps the plugin aligned with the host's contract (including future
// additions like the session-scoped token).
func readDatumEnv() datumEnv {
	ctx := plugin.Context()
	return datumEnv{
		org:        ctx.Org,
		project:    ctx.Project,
		apiHost:    ctx.APIHost,
		credHelper: ctx.CredentialsHelper,
	}
}

// usable reports whether the Datum transport has everything it needs. We
// require both an API host and a credentials helper; org/project may be empty,
// which makes the transport usable for the commands that do not address a
// project's objects.
//
// An empty project is NOT "platform scope". Every IPAM object belongs to a
// project, the platform's own included, and a request carrying no
// iam.miloapis.com/parent-* extras addresses no keyspace: it lists nothing and
// is refused on write. If a command needs to reach IPAM objects, it needs a
// project — the platform's, if that is what is meant.
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
	// The SDK execs DATUM_CREDENTIALS_HELPER (the datumctl binary) to mint a fresh,
	// short-lived token, honoring the active session. The plugin never persists it.
	token, err := plugin.Token()
	if err != nil {
		return nil, "", newCLIError(exitUnavailable, fmt.Sprintf("failed to obtain an access token: %v", err)).
			withFix("re-run `datumctl login` and try again.").withCause(err)
	}
	cfg := &rest.Config{
		Host:        controlPlaneHost(env),
		BearerToken: token,
	}
	cfg.UserAgent = userAgent()
	// The active project/org scope is encoded in the control-plane URL path (see
	// controlPlaneHost), so within that control plane namespaced resources live
	// in "default" — the same namespace datumctl itself targets. An explicit
	// --namespace (applied by the caller) overrides this.
	return cfg, "default", nil
}

// controlPlaneHost builds the fully-qualified IPAM API base URL for the active
// Datum scope. datumctl injects DATUM_API_HOST as a bare hostname (e.g.
// "api.datum.net") and conveys scope via DATUM_PROJECT/DATUM_ORG; a project's
// (or org's) Milo control plane is addressed by a path prefix off that host —
// the same construction datumctl and the compute plugin use. With neither set,
// the bare platform root is used, for cluster-scoped, operator-level calls.
func controlPlaneHost(env datumEnv) string {
	base := strings.TrimRight(ensureScheme(env.apiHost), "/")
	switch {
	case env.project != "":
		return fmt.Sprintf("%s/apis/resourcemanager.miloapis.com/v1alpha1/projects/%s/control-plane", base, env.project)
	case env.org != "":
		return fmt.Sprintf("%s/apis/resourcemanager.miloapis.com/v1alpha1/organizations/%s/control-plane", base, env.org)
	default:
		return base
	}
}

// ensureScheme prepends https:// when host has no scheme. datumctl provides
// DATUM_API_HOST without one; client-go needs an absolute URL or it routes the
// request to an HTML-serving endpoint, surfacing as "serializer for text/html".
func ensureScheme(host string) string {
	if host == "" || strings.Contains(host, "://") {
		return host
	}
	return "https://" + host
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
	return fmt.Sprintf("milo-ipam/%s", pluginVersion)
}

// newClientset builds the generated IPAM clientset from a rest.Config.
func newClientset(cfg *rest.Config) (clientset.Interface, error) {
	cs, err := clientset.NewForConfig(cfg)
	if err != nil {
		return nil, newCLIError(exitUnavailable, fmt.Sprintf("failed to build IPAM client: %v", err)).withCause(err)
	}
	return cs, nil
}
