package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"k8s.io/apiserver/pkg/admission"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	ipamapiserver "go.miloapis.com/ipam/internal/apiserver"
)

// withEnv makes NewIPAMServerOptions read env instead of the process
// environment for the rest of the test.
func withEnv(t *testing.T, env map[string]string) {
	t.Helper()
	prev := lookupEnv
	lookupEnv = func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	t.Cleanup(func() { lookupEnv = prev })
}

// The limit comes from the default, then the environment, then the flag, and
// Validate refuses a value that would turn the limiter off or that client-go
// would refuse.
func TestKubeAPILimitFlagsAndEnv(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       map[string]string
		args      []string
		wantQPS   float32
		wantBurst int
		wantErr   string
	}{
		{name: "defaults", wantQPS: 100, wantBurst: 200},
		{name: "empty env keeps the defaults", env: map[string]string{"KUBE_API_QPS": "", "KUBE_API_BURST": ""}, wantQPS: 100, wantBurst: 200},
		{name: "env", env: map[string]string{"KUBE_API_QPS": "50", "KUBE_API_BURST": "75"}, wantQPS: 50, wantBurst: 75},
		{name: "fractional env", env: map[string]string{"KUBE_API_QPS": "2.5"}, wantQPS: 2.5, wantBurst: 200},
		{name: "flag beats env", env: map[string]string{"KUBE_API_QPS": "50", "KUBE_API_BURST": "75"}, args: []string{"--kube-api-qps=300", "--kube-api-burst=600"}, wantQPS: 300, wantBurst: 600},
		{name: "flags", args: []string{"--kube-api-qps=20", "--kube-api-burst=40"}, wantQPS: 20, wantBurst: 40},
		{name: "zero QPS", args: []string{"--kube-api-qps=0"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "unlimited QPS", args: []string{"--kube-api-qps=-1"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "unlimited QPS from env", env: map[string]string{"KUBE_API_QPS": "-1"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "zero burst", args: []string{"--kube-api-burst=0"}, wantErr: "--kube-api-burst must be at least 1"},
		{name: "NaN QPS flag", args: []string{"--kube-api-qps=NaN"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "+Inf QPS flag", args: []string{"--kube-api-qps=+Inf"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "-Inf QPS flag", args: []string{"--kube-api-qps=-Inf"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "NaN QPS env", env: map[string]string{"KUBE_API_QPS": "NaN"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "+Inf QPS env", env: map[string]string{"KUBE_API_QPS": "+Inf"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "-Inf QPS env", env: map[string]string{"KUBE_API_QPS": "-Inf"}, wantErr: "--kube-api-qps must be a finite number greater than 0"},
		{name: "malformed QPS env", env: map[string]string{"KUBE_API_QPS": "fast"}, wantErr: `KUBE_API_QPS="fast"`},
		{name: "malformed burst env", env: map[string]string{"KUBE_API_BURST": "1.5"}, wantErr: `KUBE_API_BURST="1.5"`},
		{name: "malformed env with a flag still fails", env: map[string]string{"KUBE_API_BURST": "x"}, args: []string{"--kube-api-burst=10"}, wantErr: "KUBE_API_BURST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, tc.env)
			o := NewIPAMServerOptions()
			fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
			o.AddFlags(fs)
			if err := fs.Parse(append([]string{"--postgres-dsn=postgres://x"}, tc.args...)); err != nil {
				t.Fatalf("parse: %v", err)
			}
			err := o.Validate()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate = %v, want an error mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if o.KubeAPIQPS != tc.wantQPS || o.KubeAPIBurst != tc.wantBurst {
				t.Errorf("limit = %v/%d, want %v/%d", o.KubeAPIQPS, o.KubeAPIBurst, tc.wantQPS, tc.wantBurst)
			}
		})
	}
}

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: milo
  cluster:
    server: https://milo.invalid:6443
users:
- name: ipam
  user:
    token: t
contexts:
- name: milo
  context: {cluster: milo, user: ipam}
current-context: milo
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loopbackRecorder stands in for the quota plugin: it keeps the config the
// loopback initializer hands it.
type loopbackRecorder struct {
	*admission.Handler
	cfg *rest.Config
}

func (r *loopbackRecorder) SetLoopbackConfig(cfg *rest.Config) { r.cfg = cfg }

// Every client IPAM builds from --kubeconfig carries the limit: the config the
// generic apiserver builds the root admission clients and the namespace check
// from, the copy the quota plugin builds its per-project clients from, and the
// clients themselves, each with a bucket of its own.
func TestKubeAPILimitsReachEveryClient(t *testing.T) {
	withEnv(t, nil)
	o := NewIPAMServerOptions()
	fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	o.AddFlags(fs)
	if err := fs.Parse([]string{
		"--postgres-dsn=postgres://x",
		"--kubeconfig=" + writeKubeconfig(t),
		"--kube-api-qps=150",
		"--kube-api-burst=300",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	c := genericapiserver.NewRecommendedConfig(ipamapiserver.Codecs)
	if err := o.applyCoreAPI(c); err != nil {
		t.Fatalf("applyCoreAPI: %v", err)
	}
	if o.RecommendedOptions.CoreAPI != nil {
		t.Error("CoreAPI options left set; RecommendedOptions.ApplyTo would rebuild the client config without the limit")
	}
	if c.SharedInformerFactory == nil {
		t.Error("SharedInformerFactory not set")
	}
	cfg := c.ClientConfig
	if cfg == nil {
		t.Fatal("ClientConfig not set")
	}
	if cfg.Host != "https://milo.invalid:6443" {
		t.Errorf("ClientConfig.Host = %q, want the kubeconfig's server", cfg.Host)
	}
	if cfg.QPS != 150 || cfg.Burst != 300 || cfg.RateLimiter != nil {
		t.Errorf("ClientConfig limit = %v/%d (shared limiter %v), want 150/300 and none shared", cfg.QPS, cfg.Burst, cfg.RateLimiter)
	}

	// RecommendedOptions.ApplyTo builds the root clients from ClientConfig.
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if l := kube.CoreV1().RESTClient().GetRateLimiter(); l == nil || l.QPS() != 150 {
		t.Errorf("root client limiter = %v, want 150 QPS", l)
	}

	o.EnableQuota = true
	wireAdmissionInitializers(o)
	inits, err := o.RecommendedOptions.ExtraAdmissionInitializers(c)
	if err != nil {
		t.Fatalf("ExtraAdmissionInitializers: %v", err)
	}
	plugin := &loopbackRecorder{Handler: admission.NewHandler(admission.Create)}
	for _, i := range inits {
		i.Initialize(plugin)
	}
	if plugin.cfg == nil {
		t.Fatal("quota plugin got no loopback config")
	}
	if plugin.cfg.QPS != 150 || plugin.cfg.Burst != 300 {
		t.Errorf("loopback limit = %v/%d, want 150/300", plugin.cfg.QPS, plugin.cfg.Burst)
	}

	// The quota plugin copies the loopback config per project, as here. Its
	// clients are dynamic; the typed client reads the same rest.Config fields.
	projectLimiter := func(project string) flowcontrol.RateLimiter {
		pc := rest.CopyConfig(plugin.cfg)
		pc.Host += "/apis/resourcemanager.miloapis.com/v1alpha1/projects/" + project + "/control-plane"
		if _, err := dynamic.NewForConfig(pc); err != nil {
			t.Fatal(err)
		}
		cl, err := kubernetes.NewForConfig(pc)
		if err != nil {
			t.Fatal(err)
		}
		return cl.CoreV1().RESTClient().GetRateLimiter()
	}
	a, b := projectLimiter("p1"), projectLimiter("p2")
	if a.QPS() != 150 {
		t.Errorf("project client limiter = %v QPS, want 150", a.QPS())
	}
	if a == b {
		t.Error("two project clients share one limiter, want one each")
	}
}
