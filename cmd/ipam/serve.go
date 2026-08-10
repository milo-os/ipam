package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/options"
	etcdfeature "k8s.io/apiserver/pkg/storage/feature"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	tracingbase "k8s.io/component-base/tracing"
	"k8s.io/klog/v2"
	openapicommon "k8s.io/kube-openapi/pkg/common"

	quotaadmission "go.miloapis.com/milo/pkg/quota/admission"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocator"
	ipamapiserver "go.miloapis.com/ipam/internal/apiserver"
	"go.miloapis.com/ipam/internal/metrics"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/version"
	generatedopenapi "go.miloapis.com/ipam/pkg/generated/openapi"

	// Register JSON logging format.
	_ "k8s.io/component-base/logs/json/register"
)

// pgxpoolStatsInterval is how often the background sampler reads
// (*pgxpool.Pool).Stat() and republishes the four ipam_pgxpool_* gauges.
// Stat() is cheap (atomic reads of pool counters) so 15s is comfortably
// within Prometheus' default scrape interval without adding meaningful
// overhead.
const pgxpoolStatsInterval = 15 * time.Second

const (
	// defaultLeaseSweepInterval is how often retained allocations are checked
	// against their lease. Minutes rather than seconds: a lease is measured in
	// days, so the sweep only needs to be frequent enough that an expiry is acted
	// on promptly relative to its own duration, and each pass takes a short lock
	// on every pool holding retained allocations.
	defaultLeaseSweepInterval = 5 * time.Minute

	// minLeaseSweepInterval floors the flag. Every pass takes a row lock on each
	// pool holding retained allocations, and claims against those pools wait
	// behind it — so a sweep tuned down to milliseconds would turn a background
	// job into a source of allocation latency. One second is fast enough for a
	// test that needs to observe the grace window inside a minute, and slow
	// enough that it cannot be the reason a pool is contended.
	minLeaseSweepInterval = time.Second
)

// allocatorPoolRetrySchedule controls the back-off between attempts to open
// the allocator pgxpool at startup. With the postgres component installed
// in the same overlay, the IPAM apiserver pod may start before the
// PostgreSQL StatefulSet is Ready; failing the whole pod start in that
// window forces a CrashLoopBackOff that delays first-readiness by the
// kubelet's restart back-off. Three attempts at 2s/4s/8s gives ~14s of
// tolerance before failing — enough for the standard postgres bring-up,
// short enough that a genuinely-broken DSN still surfaces quickly.
var allocatorPoolRetrySchedule = []time.Duration{
	0,               // first attempt is immediate
	2 * time.Second, // 2s before the second
	4 * time.Second, // 4s before the third
	8 * time.Second, // 8s before giving up (only used when len > 3)
}

// newAllocatorPoolWithRetry opens the pgxpool with bounded exponential
// back-off. Distinguishes "DSN parses but server is unreachable" (retried)
// from "DSN itself is malformed" (returned immediately) — the latter is
// surfaced by pgxpool.NewWithConfig synchronously and won't be fixed by
// waiting.
func newAllocatorPoolWithRetry(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	var lastErr error
	for i, wait := range allocatorPoolRetrySchedule {
		if wait > 0 {
			klog.V(2).InfoS("allocator pgxpool: backing off before retry", "attempt", i+1, "wait", wait, "lastErr", lastErr)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			lastErr = err
			continue
		}
		// NewWithConfig returns a pool object even when the server is
		// unreachable; only Ping confirms a live connection. Without this
		// the readyz check would be the first place we notice DB-down.
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			if i > 0 {
				klog.InfoS("allocator pgxpool: connected", "attempt", i+1)
			}
			return pool, nil
		}
		pool.Close()
		lastErr = err
	}
	return nil, fmt.Errorf("allocator pgxpool: exhausted %d retries: %w", len(allocatorPoolRetrySchedule), lastErr)
}

// startPgxpoolStatsSampler launches a goroutine that periodically copies
// pool.Stat() into the metrics package's pgxpool gauges. The goroutine
// exits when ctx is cancelled.
func startPgxpoolStatsSampler(ctx context.Context, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	// Publish once immediately so the gauges have non-zero values from the
	// first scrape rather than staying at the metrics-package default of 0
	// for up to one full interval.
	metrics.ObservePgxpoolStat(pool.Stat())
	// Heartbeat: stamp the sampler's last successful run timestamp so the
	// IPAMPgxpoolMetricsStale alert (time() - heartbeat > 90s) can detect a
	// dead sampler goroutine. Prometheus' built-in `timestamp(<gauge>)` is
	// not a reliable signal here — it returns the evaluation time of the
	// gauge sample, not the sampler's last write.
	metrics.PgxpoolSamplerLastRunSeconds.Set(float64(time.Now().Unix()))

	go func() {
		ticker := time.NewTicker(pgxpoolStatsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				metrics.ObservePgxpoolStat(pool.Stat())
				metrics.PgxpoolSamplerLastRunSeconds.Set(float64(time.Now().Unix()))
			}
		}
	}()
}

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	_ = utilfeature.DefaultMutableFeatureGate.Set("LoggingBetaOptions=true")
	_ = utilfeature.DefaultMutableFeatureGate.Set("RemoteRequestHeaderUID=true")
	// MutatingAdmissionPolicy is a 1.34+ resource. The kind dev cluster runs
	// 1.32 and doesn't register it, so the informer fails readyz indefinitely.
	_ = utilfeature.DefaultMutableFeatureGate.Set("MutatingAdmissionPolicy=false")
}

// IPAMServerOptions contains configuration for the IPAM server.
type IPAMServerOptions struct {
	RecommendedOptions *options.RecommendedOptions
	Logs               *logsapi.LoggingConfiguration

	// PostgresDSN is the PostgreSQL connection string. Required — postgres is
	// the only supported storage backend.
	PostgresDSN string

	// PlatformProject is the project the platform's own address space lives in:
	// the class catalog, and the operator-authored pools that back it.
	//
	// Required, and deliberately not defaulted. There is no unprefixed keyspace
	// any more — every object belongs to a project, and the platform's is a
	// project like any other, which tenants consume out of. Two candidate
	// defaults exist and both are wrong: "" reinstates the unprefixed keyspace
	// this removes, and any concrete name would silently make some tenant's
	// project the platform on a cluster that happened to use that name.
	//
	// It is also the value tenant.Identity.IsPlatform compares against, so it
	// decides who clears the class-visibility gate and the class-consumption
	// SAR. That makes it security-relevant configuration, which is a second
	// reason it must be stated rather than inferred.
	PlatformProject string

	// EnableQuota gates Milo quota enforcement. Default true (production
	// enforces). Environments without a Milo quota backend (kind/dev, e2e)
	// must set --enable-quota=false: the quota plugin's ClaimCreationPolicy /
	// resource-type informers cannot sync against a cluster that lacks the
	// quota.miloapis.com CRDs, which would otherwise block readyz forever.
	EnableQuota bool

	// LeaseSweepInterval is how often the retention-lease sweeper runs. Zero
	// disables it.
	//
	// Configurable rather than fixed because a faithful test of the lease
	// lifecycle needs a lease longer than the sweep interval — otherwise the
	// first pass finds the allocation already past lease plus grace and releases
	// it in one step, reporting that release works while never observing the
	// warning window that two-phase expiry exists to provide. At the default of
	// five minutes that test takes a quarter of an hour; at a few seconds it
	// takes a minute, which is the difference between the lifecycle being
	// covered in CI and not.
	LeaseSweepInterval time.Duration
}

func NewIPAMServerOptions() *IPAMServerOptions {
	opts := &IPAMServerOptions{
		RecommendedOptions: options.NewRecommendedOptions(
			"/registry/ipam.miloapis.com",
			ipamapiserver.Codecs.LegacyCodec(ipamapiserver.Scheme.PrioritizedVersionsAllGroups()...),
		),
		Logs:               logsapi.NewLoggingConfiguration(),
		EnableQuota:        true,
		LeaseSweepInterval: defaultLeaseSweepInterval,
	}

	// IPAM is a delegating aggregated apiserver — admission webhooks, policies,
	// and namespace lifecycle are all enforced by the main kube-apiserver before
	// requests are forwarded here, so the recommended plugins (Namespace,
	// WebhookConfiguration, ValidatingAdmissionPolicy, …) are disabled by
	// default — their informers silently block readyz without a wired-up CoreAPI
	// client. The one admission concern IPAM owns is quota enforcement, which
	// Config() layers on top (replacing this empty set with the quota plugins)
	// only when --enable-quota is set, since the quota plugin needs flags parsed
	// first and a reachable Milo quota backend.
	disableAllAdmission(opts)

	return opts
}

// AddFlags registers command-line flags for all options.
func (o *IPAMServerOptions) AddFlags(fs *pflag.FlagSet) {
	o.RecommendedOptions.AddFlags(fs)

	fs.StringVar(&o.PostgresDSN, "postgres-dsn", o.PostgresDSN,
		"PostgreSQL connection string (required)")

	fs.StringVar(&o.PlatformProject, "platform-project", o.PlatformProject,
		"The project the platform's own address space lives in: the IPClass catalog and the "+
			"operator-authored pools that back it (required). Everything IPAM stores is scoped to a "+
			"project, including the platform's own objects, and tenants consume out of this one. This "+
			"is also the project whose callers are treated as the platform for class-consumption "+
			"authorization, so it must name a project only operators can act as.")

	fs.DurationVar(&o.LeaseSweepInterval, "lease-sweep-interval", o.LeaseSweepInterval,
		"How often retained allocations are checked against their retention lease. "+
			"0 disables the sweeper entirely; below "+minLeaseSweepInterval.String()+" is rejected, because each "+
			"pass takes a row lock on every pool holding retained allocations and claims "+
			"against those pools wait behind it. Lower it in tests that need to observe "+
			"the Expiring grace window without waiting for a production-length interval.")

	fs.BoolVar(&o.EnableQuota, "enable-quota", o.EnableQuota,
		"Enforce Milo quota on resource creation (default true). Requires a reachable "+
			"Milo quota backend via --kubeconfig; set false in environments without one "+
			"(e.g. kind/dev, e2e) to avoid readyz blocking on quota informers.")
}

// platformProjectFilter puts the configured platform project on every request
// context, so tenant.FromContext can answer IsPlatform and the allocator's
// class lookups know which project's keyspace to read.
//
// A filter rather than a package-level variable in internal/tenant. The value
// is process-wide immutable configuration, which is exactly what tempts one
// into a global — but IsPlatform decides an authorization bypass, and a global
// means a test that forgets to set it gets a confidently wrong answer instead
// of a failure. Carried on the context, every way of getting it wrong fails
// closed: an unconfigured server, a request that somehow bypassed this filter,
// and a tenant.Identity built as a literal all report "not the platform".
//
// It is installed unconditionally, unlike consumerContextFilter, which is
// wired only with --enable-quota. Tenant scoping is not optional.
func platformProjectFilter(platformProject string, apiHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := tenant.WithPlatformProject(req.Context(), platformProject)
		apiHandler.ServeHTTP(w, req.WithContext(ctx))
	})
}

// installPlatformProjectFilter makes the platform-project filter the innermost
// wrapper of the API handler, so the value is on the context before the REST
// and admission path reads it.
//
// Ordering against consumerContextFilter does not matter: that filter reads
// only Project() and Org(), neither of which consults the platform project.
// What does matter is that both sit inside the generic chain, after
// authentication has populated UserInfo.
func installPlatformProjectFilter(genericConfig *genericapiserver.RecommendedConfig, platformProject string) {
	prev := genericConfig.BuildHandlerChainFunc
	if prev == nil {
		prev = genericapiserver.DefaultBuildHandlerChain
	}
	genericConfig.BuildHandlerChainFunc = func(apiHandler http.Handler, c *genericapiserver.Config) http.Handler {
		return prev(platformProjectFilter(platformProject, apiHandler), c)
	}
}

func (o *IPAMServerOptions) Complete() error { return nil }

func (o *IPAMServerOptions) Validate() error {
	if o.PostgresDSN == "" {
		return fmt.Errorf("--postgres-dsn is required")
	}
	// Rejected at startup rather than per-request. Both fail closed, but an
	// unset flag is a deployment mistake and not a runtime condition: without it
	// the class catalog is read from a keyspace nothing writes to, so every
	// claim fails with "class not found" and nothing points at the cause. One
	// clear message before the first request is strictly better than that
	// message on every request forever.
	if o.PlatformProject == "" {
		return fmt.Errorf(
			"--platform-project is required: the IPClass catalog and the platform's own pools " +
				"live in a project like everything else, and there is no unprefixed keyspace to fall back to")
	}
	// The value becomes a storage key prefix ("project/<name>/") and is compared
	// against the parent-name extra a real request carries, so it has to be a
	// project name and nothing more. A slash in particular would let it address
	// a keyspace other than its own.
	if errs := validation.IsDNS1123Subdomain(o.PlatformProject); len(errs) > 0 {
		return fmt.Errorf("--platform-project %q must be a valid project name: %s",
			o.PlatformProject, strings.Join(errs, "; "))
	}
	// Zero is a real setting — it disables the sweeper — so the floor applies
	// only to positive values. Rejected rather than silently clamped: an
	// operator who asked for 10ms and got 1s would be running something other
	// than what they configured, and would find out from a latency graph.
	if o.LeaseSweepInterval < 0 {
		return fmt.Errorf("--lease-sweep-interval must not be negative; 0 disables the sweeper")
	}
	if o.LeaseSweepInterval > 0 && o.LeaseSweepInterval < minLeaseSweepInterval {
		return fmt.Errorf(
			"--lease-sweep-interval is %s, below the %s minimum: each pass takes a row lock on every pool holding retained allocations, and claims against those pools wait behind it",
			o.LeaseSweepInterval, minLeaseSweepInterval)
	}
	return nil
}

// Config builds the complete server configuration from options.
func (o *IPAMServerOptions) Config() (*ipamapiserver.Config, error) {
	if err := o.RecommendedOptions.SecureServing.MaybeDefaultWithSelfSignedCerts(
		"localhost", nil, nil); err != nil {
		return nil, fmt.Errorf("create self-signed certificates: %w", err)
	}

	genericConfig := genericapiserver.NewRecommendedConfig(ipamapiserver.Codecs)
	genericConfig.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("1.36", "", "")

	// Definition names come from the generated OpenAPIModelName() accessors so
	// they stay in sync with Scheme.ToOpenAPIDefinitionName(); server-side apply
	// resolves its managed-fields type converter against that keying. Keep the
	// default GetDefinitionName — overriding it desyncs names from their $refs
	// and silently breaks SSA.
	namer := openapinamer.NewDefinitionNamer(ipamapiserver.Scheme)
	getDefs := func(ref openapicommon.ReferenceCallback) map[string]openapicommon.OpenAPIDefinition {
		return generatedopenapi.GetOpenAPIDefinitions(ref)
	}
	genericConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(getDefs, namer)
	genericConfig.OpenAPIV3Config.Info.Title = "IPAM"
	genericConfig.OpenAPIV3Config.Info.Version = version.Version

	genericConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(getDefs, namer)
	genericConfig.OpenAPIConfig.Info.Title = "IPAM"
	genericConfig.OpenAPIConfig.Info.Version = version.Version

	// Postgres is the only storage backend; disable the recommended-options
	// etcd path so the apiserver does not try to dial etcd or register etcd
	// healthchecks.
	o.RecommendedOptions.Etcd = nil

	// Quota enforcement is wired here (not in NewIPAMServerOptions) because it
	// depends on parsed flags (--enable-quota) and a reachable Milo quota
	// backend. When enabled, replace the empty admission set with the quota
	// plugins, supply the loopback config, and mirror the tenant scope onto the
	// request context (via keys milo's quota plugin reads) — the filter must run
	// after authentication and before admission, which installing it as the
	// innermost API-handler wrapper guarantees.
	// Unconditional: every request needs to know which project is the platform,
	// whether or not quota is enforced.
	installRequestFilters(genericConfig, o.PlatformProject, o.EnableQuota)

	if o.EnableQuota {
		registerQuotaAdmission(o)
		wireAdmissionInitializers(o)
	}

	if err := o.RecommendedOptions.ApplyTo(genericConfig); err != nil {
		return nil, fmt.Errorf("apply recommended options: %w", err)
	}

	// IPAM's own spans come from the global OpenTelemetry provider, but the
	// apiserver only sets its provider on the server config, not globally — so
	// publish it here. Without this, domain spans would export nothing and would
	// not nest under the per-request span. With tracing off this is a no-op
	// provider, so it stays safe either way.
	if genericConfig.TracerProvider != nil {
		otel.SetTracerProvider(genericConfig.TracerProvider)
	}
	otel.SetTextMapPropagator(tracingbase.Propagators())

	if o.EnableQuota {
		// Gate readiness on the quota plugin's caches syncing so IPAM does not
		// serve (and silently bypass quota) before the ClaimCreationPolicy /
		// resource-type informers are warm. APF's FlowSchema /
		// PriorityLevelConfiguration informers sync via the same CoreAPI client
		// (see #38), so FlowControl is left enabled.
		genericConfig.AddReadyzChecks(quotaadmission.ReadinessCheck())
	}

	codec := ipamapiserver.Codecs.LegacyCodec(ipamapiserver.Scheme.PrioritizedVersionsAllGroups()...)

	pgGetter, err := pgstore.NewRESTOptionsGetter(o.PostgresDSN)
	if err != nil {
		return nil, fmt.Errorf("create postgres RESTOptionsGetter: %w", err)
	}
	pgGetter.SetCodec(codec)
	genericConfig.RESTOptionsGetter = pgGetter

	// pgx pool for the synchronous allocators. Sized similarly to the
	// database/sql pool inside the storage RESTOptionsGetter so the two
	// access paths don't compete.
	//
	// MaxConns is capped at 10 as a mitigation for an intermittent heap
	// corruption seen under sustained ~4-8k req/s load. The crash is
	// inside Go's stdlib `context.(*cancelCtx).propagateCancel` map
	// assignment — so far we have not identified an unsynchronised map
	// in IPAM code, and the suspicion is concurrency-induced runtime
	// state corruption that surfaces only when many request goroutines
	// overlap. Reducing the DB pool reduces concurrent allocator
	// goroutines and so reduces request fan-out.
	//
	// Capacity implication: the quota-service postgres-first ADR
	// measured ~37 sustained CIDR allocations / second per held DB
	// connection under SELECT … FOR UPDATE on the pool row. With
	// MaxConns=10 that puts a soft ceiling of ~370 synchronous
	// allocations / second on this apiserver before goroutines start
	// queueing on the pool — i.e. before allocation latency starts
	// climbing. That is well above current production traffic but
	// below the 4-8k req/s load profile the heap-corruption work was
	// chasing, so anyone running the load suite at the higher tier
	// should expect throughput to plateau here, not continue to scale.
	//
	// MaxConns is intentionally hardcoded rather than wired to an env
	// var (e.g. IPAM_PG_MAX_CONNS) — the cap exists specifically to
	// bound goroutine fan-out under the unresolved heap-corruption
	// failure mode, and exposing a knob would invite operators to lift
	// it before the root cause is fixed and resurface that crash. Once
	// the root cause is identified and the cap is no longer load-
	// bearing, raise it (or expose IPAM_PG_MAX_CONNS) — flag both this
	// cap and the watch-exclusion question in apiserver.go for revisit.
	poolCfg, err := pgxpool.ParseConfig(o.PostgresDSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	poolCfg.MaxConns = 10
	// Trace every database call on this pool so a claim's trace shows lock-wait
	// and query latency without instrumenting the allocator. The SQL text is
	// static and carries no user data, so it is safe to record; bound parameters
	// are deliberately excluded because they carry tenant-scoped values.
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithTrimSQLInSpanName(),
	)
	allocatorPool, err := newAllocatorPoolWithRetry(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	prefixAllocator := allocator.NewPostgresPrefixAllocator()

	// Wire postgres + pgxpool readiness into /readyz so the load balancer
	// drains the pod when either path can no longer serve requests. The
	// generic apiserver registers /healthz, /readyz, /livez automatically
	// but those only cover its own internal state — they do NOT probe the
	// storage backend.
	genericConfig.AddReadyzChecks(
		healthz.NamedCheck("postgres-storage", func(_ *http.Request) error {
			return pgGetter.DB().Ping()
		}),
		healthz.NamedCheck("postgres-allocator-pool", func(req *http.Request) error {
			pingCtx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
			defer cancel()
			return allocatorPool.Ping(pingCtx)
		}),
	)
	// PreShutdownHook is registered on the GenericAPIServer post-build —
	// see Run() below; it closes the allocator pgxpool AFTER the
	// apiserver stops accepting new requests so in-flight transactions
	// commit cleanly or roll back rather than getting torn down.

	// Replace the etcd-specific feature support checker (still wired into the
	// k8s.io/apiserver cacher even with no etcd backend) with one that
	// advertises RequestWatchProgress as supported. The cacher uses this
	// signal to enable ConsistentListFromCache, which lets default kubectl
	// reads be served from the in-memory cache instead of round-tripping to
	// Postgres on every request. Without this override the cacher disables
	// the fast path and per-request fixed overhead (auth + DB round-trip +
	// decode) dominates read latency — observed as GET p95 ≈ list p95 with
	// both ~3× the SLO.
	etcdfeature.DefaultFeatureSupportChecker = pgstore.NewFeatureSupportChecker()

	// A nil checker denies every project-scoped claim rather than allowing one,
	// so an apiserver started without an authorizer serves platform callers only.
	var classChecker access.ClassAccessChecker
	if genericConfig.Authorization.Authorizer != nil {
		classChecker = access.NewClassAccessChecker(genericConfig.Authorization.Authorizer)
	}

	// The namespace-liveness checker reuses the client config --kubeconfig
	// produced, rewriting Host per project the way milo's quota plugin does.
	//
	// Wired here rather than as an admission plugin ON PURPOSE. Admission
	// installs only under --enable-quota and the dev overlay sets
	// ENABLE_QUOTA=false, so an admission-based check would be inert on every
	// dev cluster and in every e2e and load run — #85 exactly, measured after
	// that fix had supposedly landed.
	//
	// GATED ON --kubeconfig BEING SET, not on ClientConfig being non-nil.
	//
	// That distinction is the whole safety of this wiring and it is not
	// obvious. CoreAPIOptions.ApplyTo falls back to rest.InClusterConfig() when
	// --kubeconfig is empty, and in a pod that SUCCEEDS — so ClientConfig is
	// non-nil in every in-cluster deployment whether or not Milo is reachable.
	// Inferring "there is a project control plane to ask" from it would build a
	// checker pointed at the ROOT apiserver, rewrite the Host to a
	// /projects/<id>/control-plane path that does not exist there, get a 404,
	// read it as "namespace missing", and REFUSE EVERY PROJECT-SCOPED CLAIM.
	//
	// Measured on the dev cluster: KUBECONFIG is empty, CoreAPI is never
	// disabled, so ClientConfig is the kind cluster's own API server.
	//
	// --kubeconfig is the signal that actually means what is needed: the
	// deployment's own comment says it MUST point at milo-apiserver for a real
	// control plane. Empty means there is no Milo to ask, so the check is off
	// and IPAM behaves as it did before #86 — which is the documented fail-open
	// state, not a silent hole, because this check is liveness rather than
	// authorization.
	var nsChecker access.NamespaceChecker
	if o.RecommendedOptions.CoreAPI != nil && o.RecommendedOptions.CoreAPI.CoreAPIKubeconfigPath != "" {
		nsChecker = access.NewNamespaceChecker(genericConfig.ClientConfig)
	}
	// Say which it is at startup. Whether a correctness check is running must
	// not be something an operator has to infer from a flag two layers away —
	// that inference is what made #85 survive as long as it did.
	klog.InfoS("namespace liveness check", "enabled", nsChecker != nil,
		"reason", "requires --kubeconfig pointing at the Milo control plane")

	return &ipamapiserver.Config{
		GenericConfig: genericConfig,
		ExtraConfig: ipamapiserver.ExtraConfig{
			PrefixAllocator:  prefixAllocator,
			AllocatorPool:    allocatorPool,
			ClassChecker:     classChecker,
			NamespaceChecker: nsChecker,
			// The sweeper is not a request, so it cannot pick this up from the
			// handler chain the way every REST path does.
			PlatformProject: o.PlatformProject,
			// Enabled by default and harmless when no class or pool states a
			// lease: the sweeper examines the retained set and releases nothing.
			LeaseSweepInterval: o.LeaseSweepInterval,
		},
	}, nil
}

// NewServeCommand creates the serve subcommand that starts the API server.
func NewServeCommand() *cobra.Command {
	o := NewIPAMServerOptions()

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the IPAM API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.Complete(); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			return Run(o, cmd.Context())
		},
	}

	flags := cmd.Flags()
	o.AddFlags(flags)
	logsapi.AddFlags(o.Logs, flags)
	return cmd
}

func Run(o *IPAMServerOptions, ctx context.Context) error {
	if err := logsapi.ValidateAndApply(o.Logs, utilfeature.DefaultMutableFeatureGate); err != nil {
		return fmt.Errorf("apply logging configuration: %w", err)
	}

	cfg, err := o.Config()
	if err != nil {
		return err
	}

	server, err := cfg.Complete().New()
	if err != nil {
		return err
	}

	defer logs.FlushLogs()

	// Close the allocator pgxpool AFTER the apiserver stops accepting new
	// requests but BEFORE the process exits. PreShutdownHooks run after the
	// HTTP server has drained, so any in-flight allocation transaction
	// either commits or rolls back via context cancellation cleanly. Without
	// this hook the pool got torn down on process exit alongside in-flight
	// transactions, surfacing as `tx_error` in allocation_failures_total.
	if err := server.GenericAPIServer.AddPreShutdownHook("close-allocator-pool", func() error {
		klog.InfoS("PreShutdown: closing allocator pgxpool")
		cfg.ExtraConfig.AllocatorPool.Close()
		return nil
	}); err != nil {
		return fmt.Errorf("register pgxpool shutdown hook: %w", err)
	}

	// Background sampler that publishes pgxpool.Stat() into the
	// ipam_pgxpool_* gauges.
	startPgxpoolStatsSampler(ctx, cfg.ExtraConfig.AllocatorPool)

	klog.InfoS("starting IPAM server", "storageBackend", "postgres")
	return server.Run(ctx)
}
