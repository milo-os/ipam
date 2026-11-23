package main

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	genericapiserver "k8s.io/apiserver/pkg/server"
	clientgoinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/tracing"
)

// Client-side rate limit for every client IPAM builds from --kubeconfig: the
// quota plugin's root and per-project clients, the namespace check's
// per-project clients, and the informers. Each client gets its own token
// bucket of this size.
//
// client-go's default is 5 QPS with a burst of 10. Under quota admission each
// claim create makes a ResourceClaim POST and watch, plus a namespace lookup,
// against its project's control plane, so at that default creates queued
// behind the limiter at about 10 per second. 100 and 200 are what Milo's own
// controller-manager gives its client to the same API server. The limit stays
// on: Milo's API Priority and Fairness protects the server, and this keeps a
// runaway IPAM from spending that budget all at once.
const (
	defaultKubeAPIQPS   float32 = 100
	defaultKubeAPIBurst int     = 200

	kubeAPIQPSEnv   = "KUBE_API_QPS"
	kubeAPIBurstEnv = "KUBE_API_BURST"
)

// coreAPIResync matches k8s.io/apiserver's CoreAPIOptions.ApplyTo.
const coreAPIResync = 10 * time.Minute

// kubeAPILimitsFromEnv reads the flag defaults from the environment so a
// deployment can set them without adding arguments. A flag given on the
// command line still wins.
func kubeAPILimitsFromEnv(lookup func(string) (string, bool)) (float32, int, error) {
	qps, burst := defaultKubeAPIQPS, defaultKubeAPIBurst
	if v, ok := lookup(kubeAPIQPSEnv); ok && v != "" {
		f, err := strconv.ParseFloat(v, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("%s=%q: not a number", kubeAPIQPSEnv, v)
		}
		qps = float32(f)
	}
	if v, ok := lookup(kubeAPIBurstEnv); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, 0, fmt.Errorf("%s=%q: not an integer", kubeAPIBurstEnv, v)
		}
		burst = n
	}
	return qps, burst, nil
}

// validateKubeAPILimits refuses a limit that turns rate limiting off (QPS of
// zero falls back to client-go's default and a negative one disables the
// limiter), one that is not a finite number, or one client-go would refuse
// when it builds the client.
func validateKubeAPILimits(qps float32, burst int) error {
	if !(qps > 0) || math.IsInf(float64(qps), 0) {
		return fmt.Errorf("--kube-api-qps must be a finite number greater than 0, got %v", qps)
	}
	if burst < 1 {
		return fmt.Errorf("--kube-api-burst must be at least 1, got %d", burst)
	}
	return nil
}

// withKubeAPILimits sets the rate limit on cfg. RateLimiter is cleared so each
// client built from cfg, or from a copy of it, gets its own bucket instead of
// sharing one.
func withKubeAPILimits(cfg *rest.Config, qps float32, burst int) {
	cfg.QPS = qps
	cfg.Burst = burst
	cfg.RateLimiter = nil
}

// applyCoreAPI does what k8s.io/apiserver's CoreAPIOptions.ApplyTo does, with
// the rate limit set before any client is built. RecommendedOptions.ApplyTo
// builds the admission plugins' root clients straight after CoreAPI.ApplyTo
// with no hook in between, and the quota plugin sends an org-scoped claim's
// ResourceClaim through that root client, so setting the limit afterwards
// would miss it. Call this before RecommendedOptions.ApplyTo; it clears
// RecommendedOptions.CoreAPI so ApplyTo leaves the result alone.
func (o *IPAMServerOptions) applyCoreAPI(c *genericapiserver.RecommendedConfig) error {
	core := o.RecommendedOptions.CoreAPI
	if core == nil {
		return nil
	}

	var cfg *rest.Config
	var err error
	if path := core.CoreAPIKubeconfigPath; path != "" {
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: path}, &clientcmd.ConfigOverrides{})
		if cfg, err = loader.ClientConfig(); err != nil {
			return fmt.Errorf("load kubeconfig at %q: %w", path, err)
		}
	} else if cfg, err = rest.InClusterConfig(); err != nil {
		return err
	}
	withKubeAPILimits(cfg, o.KubeAPIQPS, o.KubeAPIBurst)

	// The tracer provider is set later, by the tracing options inside
	// RecommendedOptions.ApplyTo, so read it when each client builds its
	// transport rather than now. The informers' clientset below is built
	// before that and so goes untraced; the admission clients are built after
	// it and are traced.
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return tracing.WrapperFor(c.TracerProvider)(rt)
	})

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create Kubernetes clientset: %w", err)
	}
	c.ClientConfig = cfg
	c.SharedInformerFactory = clientgoinformers.NewSharedInformerFactory(cs, coreAPIResync)
	o.RecommendedOptions.CoreAPI = nil
	return nil
}

// lookupEnv is the environment lookup NewIPAMServerOptions uses; tests swap it.
var lookupEnv = os.LookupEnv
