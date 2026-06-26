package main

import (
	"context"
	"fmt"
	"io"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	quotaadmission "go.miloapis.com/milo/pkg/quota/admission"
	milorequest "go.miloapis.com/milo/pkg/request"

	"go.miloapis.com/ipam/internal/tenant"
)

// loopbackConfigSetter is the duck-typed interface satisfied by milo's
// ResourceQuotaEnforcementPlugin via its exported SetLoopbackConfig method.
// Milo's own LoopbackInitializer is internal and not importable, so IPAM
// supplies its own initializer that targets this method. Defining the
// interface locally (rather than importing the plugin type) keeps the
// initializer decoupled from milo's concrete plugin struct.
type loopbackConfigSetter interface {
	SetLoopbackConfig(*rest.Config)
}

// loopbackConfigInitializer injects a base *rest.Config into any admission
// plugin that wants one via SetLoopbackConfig. The quota plugin copies this
// config and rewrites its Host to the per-project control-plane path
// (…/apis/resourcemanager.miloapis.com/v1alpha1/projects/<id>/control-plane)
// when a project-scoped request arrives.
type loopbackConfigInitializer struct {
	config *rest.Config
}

var _ admission.PluginInitializer = loopbackConfigInitializer{}

func (i loopbackConfigInitializer) Initialize(plugin admission.Interface) {
	if setter, ok := plugin.(loopbackConfigSetter); ok {
		setter.SetLoopbackConfig(i.config)
	}
}

// registerQuotaAdmission replaces the (deliberately empty) admission plugin set
// with ONLY milo's ResourceQuotaEnforcement plugin. The other recommended
// plugins (NamespaceLifecycle, ValidatingAdmissionPolicy, webhooks, …) remain
// disabled: IPAM is a delegating aggregated apiserver and those informers block
// readyz without a fully-wired CoreAPI client. See NewIPAMServerOptions.
func registerQuotaAdmission(o *IPAMServerOptions) {
	plugins := admission.NewPlugins()
	quotaadmission.Register(plugins)
	registerPlatformConsumerGuard(plugins)

	o.RecommendedOptions.Admission.Plugins = plugins
	// Order: the platform-consumer guard runs first so a platform-scoped create
	// with no derivable consumer is rejected before the quota plugin would
	// otherwise fall back to the loopback-root client.
	o.RecommendedOptions.Admission.RecommendedPluginOrder = []string{
		platformConsumerGuardName,
		quotaadmission.PluginName,
	}
	o.RecommendedOptions.Admission.EnablePlugins = []string{
		platformConsumerGuardName,
		quotaadmission.PluginName,
	}
	o.RecommendedOptions.Admission.DefaultOffPlugins = nil
}

// wireAdmissionInitializers installs the ExtraAdmissionInitializers hook so the
// loopback *rest.Config reaches the quota plugin. RecommendedOptions.ApplyTo
// calls ExtraAdmissionInitializers AFTER CoreAPI.ApplyTo has populated
// config.ClientConfig from --kubeconfig (which points at milo-apiserver), so we
// reuse that exact config as the loopback base — no dedicated flag is needed.
// The same ClientConfig is what ApplyTo turns into the plugin's root dynamic
// client (via initializer.WantsDynamicClient), so the dynamic client and the
// loopback config are guaranteed to point at the same milo control plane.
func wireAdmissionInitializers(o *IPAMServerOptions) {
	o.RecommendedOptions.ExtraAdmissionInitializers = func(c *genericapiserver.RecommendedConfig) ([]admission.PluginInitializer, error) {
		if c.ClientConfig == nil {
			// No --kubeconfig and no in-cluster config. The quota plugin's
			// per-project routing needs a base config; without one project
			// clients cannot be built. Surface this clearly rather than
			// letting it fail later inside getProjectClient.
			return nil, fmt.Errorf("quota admission requires a loopback config: set --kubeconfig to the milo-apiserver kubeconfig")
		}
		klog.V(2).InfoS("wiring loopback config for quota admission", "host", c.ClientConfig.Host)
		return []admission.PluginInitializer{
			loopbackConfigInitializer{config: rest.CopyConfig(c.ClientConfig)},
		}, nil
	}
}

// consumerContextFilter wraps the API handler so that, AFTER authentication has
// populated the request's UserInfo (and thus the iam.miloapis.com/parent-*
// extras) and BEFORE admission runs (admission runs inside the wrapped REST
// handler), the tenant scope is mirrored onto the request context using the
// keys milo's quota plugin reads (milorequest.WithProject / WithOrganization).
//
// Handler-chain ordering: genericapiserver builds the chain by wrapping the API
// handler with DefaultBuildHandlerChain. We wrap the API handler FIRST (here),
// then hand the result to the default chain, so this filter becomes the
// innermost wrapper — it runs LAST among the chain filters, i.e. after
// WithAuthentication, and immediately before the API handler dispatches to the
// REST/admission path. That guarantees tenant.FromContext can see the
// authenticated user's extras and that the project/org context is set before
// the quota plugin's Validate reads it.
func consumerContextFilter(apiHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		id := tenant.FromContext(ctx)

		switch {
		case id.Project() != "":
			ctx = milorequest.WithProject(ctx, id.Project())
		case id.Org() != "":
			ctx = milorequest.WithOrganization(ctx, id.Org())
		default:
			// Platform-scoped (or unscoped) request: no project/org is set on
			// the context. The platform-consumer guard admission plugin will
			// fail-closed on quota-protected creates that reach this state.
		}

		apiHandler.ServeHTTP(w, req.WithContext(ctx))
	})
}

// installConsumerContextFilter sets BuildHandlerChainFunc so the consumer
// context filter is the innermost wrapper of the API handler.
func installConsumerContextFilter(genericConfig *genericapiserver.RecommendedConfig) {
	prev := genericConfig.BuildHandlerChainFunc
	if prev == nil {
		prev = genericapiserver.DefaultBuildHandlerChain
	}
	genericConfig.BuildHandlerChainFunc = func(apiHandler http.Handler, c *genericapiserver.Config) http.Handler {
		return prev(consumerContextFilter(apiHandler), c)
	}
}

const platformConsumerGuardName = "IPAMPlatformConsumerGuard"

// quotaProtectedResources is the set of IPAM resources whose creation is
// subject to quota enforcement. A platform-scoped create of one of these with
// no derivable consumer is rejected fail-closed (see platformConsumerGuard).
//
// Only claims consume quota: pools are infrastructure objects and allocations
// are system-created inside the claim transaction. Keeping this list explicit
// (rather than deriving it from milo's ClaimCreationPolicy, which the quota
// plugin already does) avoids duplicating policy lookup while still letting the
// guard fail closed before the quota plugin's loopback-root fallback.
var quotaProtectedResources = map[schema.GroupResource]struct{}{
	{Group: "ipam.miloapis.com", Resource: "ipclaims"}: {},
}

// platformConsumerGuard is an IPAM-local validating admission plugin ordered
// before the quota plugin. It enforces the platform "explicit consumer
// required" rule: a CREATE of a quota-protected resource that carries no
// project/org context (i.e. a platform-scoped request) is denied with 403.
//
// Why a guard instead of context mutation: milo's quota plugin reads the
// consumer from request context ONLY and, when none is present, falls back to a
// loopback-root client rather than denying. Admission plugins cannot mutate the
// context seen by sibling plugins, so the only way to fail closed without
// modifying the shared plugin is a sibling plugin (ordered first) that rejects
// the request outright. The consumer context filter has already run by this
// point, so the absence of a project/org on the context is authoritative.
type platformConsumerGuard struct {
	*admission.Handler
}

var _ admission.ValidationInterface = &platformConsumerGuard{}

func newPlatformConsumerGuard() *platformConsumerGuard {
	return &platformConsumerGuard{Handler: admission.NewHandler(admission.Create)}
}

func registerPlatformConsumerGuard(plugins *admission.Plugins) {
	plugins.Register(platformConsumerGuardName, func(_ io.Reader) (admission.Interface, error) {
		return newPlatformConsumerGuard(), nil
	})
}

func (g *platformConsumerGuard) Validate(ctx context.Context, attrs admission.Attributes, _ admission.ObjectInterfaces) error {
	if attrs.GetOperation() != admission.Create {
		return nil
	}
	gr := schema.GroupResource{
		Group:    attrs.GetResource().Group,
		Resource: attrs.GetResource().Resource,
	}
	if _, protected := quotaProtectedResources[gr]; !protected {
		return nil
	}

	if hasDerivableConsumer(ctx) {
		return nil
	}

	return apierrors.NewForbidden(
		gr,
		attrs.GetName(),
		fmt.Errorf("explicit consumer required for quota-protected resource %q: scope the request to a project (or organization) — platform-scoped creates with no consumer are denied", gr.Resource),
	)
}

// hasDerivableConsumer reports whether the request context carries a consumer
// the quota plugin can use to route and attribute quota. It mirrors exactly the
// keys the consumer context filter sets and the quota plugin reads.
func hasDerivableConsumer(ctx context.Context) bool {
	if id, ok := milorequest.ProjectID(ctx); ok && id != "" {
		return true
	}
	if id, ok := milorequest.OrganizationID(ctx); ok && id != "" {
		return true
	}
	return false
}
