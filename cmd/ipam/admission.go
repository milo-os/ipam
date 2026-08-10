package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	quotaadmission "go.miloapis.com/milo/pkg/quota/admission"
	milorequest "go.miloapis.com/milo/pkg/request"

	ipamapiserver "go.miloapis.com/ipam/internal/apiserver"
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

// objectConvertorSetter is the duck-typed interface satisfied by milo's
// ResourceQuotaEnforcementPlugin via its exported SetObjectConvertor method.
type objectConvertorSetter interface {
	SetObjectConvertor(runtime.ObjectConvertor)
}

// objectConvertorInitializer injects an ObjectConvertor into the quota plugin.
//
// IPAM is an aggregated apiserver: admission decodes its objects as INTERNAL Go
// types (ipam.IPClaim), whose ObjectMeta is inlined without a "metadata" JSON
// wrapper. milo's quota plugin renders the per-resource ResourceClaim name from
// a CEL template ("trigger.metadata.name"); evaluated against an internal type
// run through ToUnstructured, that map has no "metadata" key and the claim
// create fails with `no such key: metadata`. milo handles this when given an
// ObjectConvertor — it first converts the internal object to its external
// versioned form (which carries metadata) before building the CEL trigger. The
// scheme registers the internal⇄v1alpha1 conversions, so it is exactly the
// convertor milo needs. Without this initializer objectConvertor stays nil and
// every quota-enforced create is denied with an internal error.
type objectConvertorInitializer struct {
	convertor runtime.ObjectConvertor
}

var _ admission.PluginInitializer = objectConvertorInitializer{}

func (i objectConvertorInitializer) Initialize(plugin admission.Interface) {
	if setter, ok := plugin.(objectConvertorSetter); ok {
		setter.SetObjectConvertor(i.convertor)
	}
}

// disableAllAdmission installs an empty admission plugin set — the baseline for
// a delegating aggregated apiserver. The recommended plugins (NamespaceLifecycle,
// webhooks, ValidatingAdmissionPolicy, …) run informers that block readyz
// without a wired CoreAPI client, and IPAM defers those concerns to the main
// kube-apiserver. registerQuotaAdmission overrides this with the quota plugin
// when --enable-quota is set.
func disableAllAdmission(o *IPAMServerOptions) {
	o.RecommendedOptions.Admission.Plugins = admission.NewPlugins()
	o.RecommendedOptions.Admission.RecommendedPluginOrder = []string{}
	o.RecommendedOptions.Admission.DefaultOffPlugins = nil
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
			// IPAM's scheme converts internal types to their versioned form so
			// the quota plugin's CEL trigger carries metadata (see initializer).
			objectConvertorInitializer{convertor: ipamapiserver.Scheme},
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
			// The request carries no project and no organization, so nothing is
			// set on the context.
			//
			// This is NOT "platform-scoped", which is what this comment used to
			// say. The platform is an ordinary project named by
			// --platform-project and a platform caller lands in the first case
			// above like any other tenant. Reaching here means the request named
			// no tenant at all.
			//
			// For writes it is also unreachable: isUntenantedWrite above already
			// refused this request. The platform-consumer guard remains as the
			// fail-closed backstop for the quota path, since milo's quota plugin
			// falls back to a loopback-root client rather than denying when it
			// finds no consumer on the context.
		}

		apiHandler.ServeHTTP(w, req.WithContext(ctx))
	})
}

// installRequestFilters installs every API-handler wrapper, in order, and is
// the single place that decides which of them are conditional.
//
// It exists because that decision was previously spread across an `if
// o.EnableQuota` block in Config(), and the untenanted-write gate was inside
// it. The dev overlay sets ENABLE_QUOTA=false, so the gate was inert on every
// dev cluster and in every e2e and load run — measured after it had supposedly
// landed: an unimpersonated kubectl created an IPPool at an unprefixed key.
//
// Collecting them here makes "which filters are unconditional" a property of
// one function that a test can assert, rather than a fact spread across a
// branch. TestTenancyFiltersAreInstalledWithoutQuota pins it.
//
// Order matters and is the reverse of installation: the first installed runs
// first. Platform project before the write gate, so the request is fully
// described before anything judges it; the quota consumer context last,
// immediately before dispatch.
func installRequestFilters(genericConfig *genericapiserver.RecommendedConfig, platformProject string, enableQuota bool) {
	// Unconditional: every request needs to know which project is the platform,
	// whether or not quota is enforced.
	installPlatformProjectFilter(genericConfig, platformProject)

	// Unconditional, and for a stronger reason: tenancy is not a quota concern.
	// Quota decides whether a project may have another address; this decides
	// whether there is a project at all. A deployment that turns quota off is
	// not asking to accept writes from nobody.
	installUntenantedWriteFilter(genericConfig)

	// Conditional: mirroring the tenant onto milo's request keys is meaningless
	// without the quota plugin reading them.
	if enableQuota {
		installConsumerContextFilter(genericConfig)
	}
}

// untenantedWriteFilter refuses a write from a caller carrying no project.
//
// # Why this is its own filter rather than part of consumerContextFilter
//
// It was part of it, and that made the gate inert wherever it matters most.
// consumerContextFilter is installed only under --enable-quota, because
// mirroring the tenant onto milo's request keys is meaningless without the
// quota plugin reading them. The dev overlay sets ENABLE_QUOTA=false, so every
// Chainsaw suite and every k6 run drove a server with no gate — and an
// unimpersonated kubectl could still create an IPPool at an unprefixed key,
// measured on the dev cluster after the fix had supposedly landed.
//
// The bitter part is that isUntenantedWrite's own comment identified this
// hazard — "admission only installs alongside the quota plugin, so a server
// without --enable-quota had no refusal at all" — as the reason NOT to put the
// gate in admission. Moving it to the handler chain fixed that, and then
// installing it inside the same `if o.EnableQuota` reintroduced it one level up.
// Two lines earlier, installPlatformProjectFilter is unconditional with the
// comment "every request needs to know which project is the platform, whether
// or not quota is enforced". The same sentence applies to tenancy, and this
// filter now gets the same treatment.
//
// Tenancy is not a quota concern. Quota decides whether a project may have
// another address; tenancy decides whether there is a project at all. A
// deployment that turns quota off is not asking to accept writes from nobody.
func untenantedWriteFilter(apiHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if isUntenantedWrite(req, tenant.FromContext(req.Context())) {
			rejectUntenantedWrite(w, req)
			return
		}
		apiHandler.ServeHTTP(w, req)
	})
}

// installUntenantedWriteFilter installs the write gate as an API-handler
// wrapper. It must be installed UNCONDITIONALLY — see untenantedWriteFilter.
//
// Install it after installPlatformProjectFilter so the platform project is on
// the context first; the gate does not need it today, but a filter that runs
// before the request is fully described is a trap for whoever extends it.
func installUntenantedWriteFilter(genericConfig *genericapiserver.RecommendedConfig) {
	prev := genericConfig.BuildHandlerChainFunc
	if prev == nil {
		prev = genericapiserver.DefaultBuildHandlerChain
	}
	genericConfig.BuildHandlerChainFunc = func(apiHandler http.Handler, c *genericapiserver.Config) http.Handler {
		return prev(untenantedWriteFilter(apiHandler), c)
	}
}

// ipamAPIPrefix is the path prefix every request for an IPAM resource carries.
// Discovery, healthz, readyz, metrics and openapi live outside it and are
// deliberately untouched by the write gate — they are reads, and a monitoring
// scrape has no project.
const ipamAPIPrefix = "/apis/ipam.miloapis.com/"

// isUntenantedWrite reports whether this request mutates an IPAM resource while
// carrying no project.
//
// Every object this service stores belongs to a project, the platform's own
// included. A caller with no project therefore has nowhere legitimate to write,
// and before this gate the write succeeded — landing at an unprefixed key in a
// keyspace no read path consults, and answering 201 Created for an object
// nothing would ever find.
//
// The gate is here, in the handler chain, rather than in admission or in each
// REST implementation. Admission only installs alongside the quota plugin, so a
// server without --enable-quota had no refusal at all; and the four REST
// implementations expose eleven separate write entrypoints between them, which
// is a list someone has to remember to extend. This filter already runs after
// authentication and before dispatch, and it sees every verb on every resource.
//
// Reads are intentionally not gated. An untenanted read already returns
// nothing, which is the tenancy model working rather than failing — and it is
// the documented answer to "why does my kubectl show zero pools".
//
// # Collection deletes are not gated either, and that is not a loophole
//
// Refusing them broke namespace deletion for the whole cluster. Kubernetes'
// namespace controller must DELETE-collection every namespaced type before it
// can drop the `kubernetes` finalizer, and it is a cluster-internal controller
// carrying no project extras. Two refusals — ipclaims and ipallocations —
// produced `NamespaceDeletionContentFailure: "Failed to delete all resource
// types, 2 remaining: unknown, unknown"`, where "unknown" is the body of this
// gate's own 403. Every namespace deletion hung forever, including namespaces
// containing no IPAM objects at all, because the controller sweeps every type
// regardless of contents.
//
// The exemption is narrow and rests on the same argument the read exemption
// does: a caller with no project has no keyspace, so a collection delete
// targets nothing and removes nothing. The 403 was pure cost — it prevented no
// write, because there was no write to prevent. What #78 was actually about is
// a NAMED write creating or altering an object at an unprefixed key, and that
// stays refused.
//
// Deliberately keyed on RequestInfo rather than on the URL shape. The apiserver
// has already parsed the request by the time this filter runs, so asking it is
// exact; inferring "no name segment therefore a collection" from the path is
// the kind of reimplementation that drifts from the thing it mirrors.
func isUntenantedWrite(req *http.Request, id tenant.Identity) bool {
	if id.Name != "" {
		return false
	}
	if !strings.HasPrefix(req.URL.Path, ipamAPIPrefix) {
		return false
	}
	if isCollectionDelete(req) {
		return false
	}
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// isCollectionDelete reports whether this is a DELETE against a collection
// rather than a named object — the verb the namespace controller issues when
// tearing a namespace down.
//
// Falls back to the path when RequestInfo is absent, which happens only if this
// filter is ever installed outside the apiserver's chain. The fallback errs
// toward treating a delete as NAMED, so an unparseable request stays refused
// rather than silently exempt.
func isCollectionDelete(req *http.Request) bool {
	if req.Method != http.MethodDelete {
		return false
	}
	if info, ok := genericapirequest.RequestInfoFrom(req.Context()); ok {
		return info.IsResourceRequest && info.Verb == "deletecollection"
	}
	// No RequestInfo: a collection delete has no name segment after the
	// resource. Trailing slashes are tolerated; anything else is named.
	trimmed := strings.TrimSuffix(req.URL.Path, "/")
	parts := strings.Split(strings.TrimPrefix(trimmed, ipamAPIPrefix), "/")
	// <version>/<resource> or <version>/namespaces/<ns>/<resource>
	return len(parts) == 2 || len(parts) == 4
}

// rejectUntenantedWrite writes the 403 for a write that carries no project.
//
// It is a 403 rather than a 400 because this is an authorization statement: the
// caller authenticated successfully and simply has no project to write into.
// The message names the extra that carries the project, because the usual cause
// is a kubeconfig that talks to IPAM directly instead of through Milo's front
// gate — which is exactly how every pre-cutover e2e suite came to author its
// catalog into a keyspace production would never have.
func rejectUntenantedWrite(w http.ResponseWriter, req *http.Request) {
	klog.V(2).InfoS("rejecting untenanted write",
		"path", req.URL.Path, "verb", req.Method)

	status := apierrors.NewForbidden(
		schema.GroupResource{Group: "ipam.miloapis.com"}, "",
		fmt.Errorf("writes must be scoped to a project: this request carries no %s "+
			"extra, so it has no keyspace to write into. Route the request through "+
			"Milo's front gate, or use the platform project's credentials for "+
			"platform-owned objects", tenant.ExtraParentName),
	).ErrStatus

	body, err := json.Marshal(status)
	if err != nil {
		http.Error(w, "forbidden: writes must be scoped to a project", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
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
// subject to quota enforcement. A create of one of these carrying no derivable
// consumer is rejected fail-closed (see platformConsumerGuard).
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
// before the quota plugin. It enforces the "explicit consumer required" rule: a
// CREATE of a quota-protected resource that carries no project/org context is
// denied with 403.
//
// "No project/org context" does not mean "the platform". A platform caller is
// scoped to the project named by --platform-project and carries it like any
// tenant; reaching this guard means the request named no tenant at all. The
// name predates platform-as-a-project and is kept only because renaming an
// admission plugin changes a string operators may have configured.
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
