package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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

	// EnableQuota gates Milo quota enforcement. Default true (production
	// enforces). Environments without a Milo quota backend (kind/dev, e2e)
	// must set --enable-quota=false: the quota plugin's ClaimCreationPolicy /
	// resource-type informers cannot sync against a cluster that lacks the
	// quota.miloapis.com CRDs, which would otherwise block readyz forever.
	EnableQuota bool
}

func NewIPAMServerOptions() *IPAMServerOptions {
	opts := &IPAMServerOptions{
		RecommendedOptions: options.NewRecommendedOptions(
			"/registry/ipam.miloapis.com",
			ipamapiserver.Codecs.LegacyCodec(ipamapiserver.Scheme.PrioritizedVersionsAllGroups()...),
		),
		Logs:        logsapi.NewLoggingConfiguration(),
		EnableQuota: true,
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

	fs.BoolVar(&o.EnableQuota, "enable-quota", o.EnableQuota,
		"Enforce Milo quota on resource creation (default true). Requires a reachable "+
			"Milo quota backend via --kubeconfig; set false in environments without one "+
			"(e.g. kind/dev, e2e) to avoid readyz blocking on quota informers.")
}

func (o *IPAMServerOptions) Complete() error { return nil }

func (o *IPAMServerOptions) Validate() error {
	if o.PostgresDSN == "" {
		return fmt.Errorf("--postgres-dsn is required")
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
	if o.EnableQuota {
		registerQuotaAdmission(o)
		wireAdmissionInitializers(o)
		installConsumerContextFilter(genericConfig)
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

	var poolChecker access.PoolAccessChecker
	var classChecker access.ClassAccessChecker
	if genericConfig.Authorization.Authorizer != nil {
		poolChecker = access.NewPoolAccessChecker(genericConfig.Authorization.Authorizer)
		classChecker = access.NewClassAccessChecker(genericConfig.Authorization.Authorizer)
	}

	// ClientConfig comes from --kubeconfig via CoreAPI.ApplyTo, which runs
	// whether or not quota is enabled, so this check is live in dev and e2e too.
	// With no kubeconfig it disables itself.
	namespaceChecker := access.NewNamespaceChecker(genericConfig.ClientConfig)
	if namespaceChecker == nil {
		klog.InfoS("no client config: IPClaim namespace-liveness checking is disabled")
	}

	return &ipamapiserver.Config{
		GenericConfig: genericConfig,
		ExtraConfig: ipamapiserver.ExtraConfig{
			PrefixAllocator:  prefixAllocator,
			AllocatorPool:    allocatorPool,
			PoolChecker:      poolChecker,
			ClassChecker:     classChecker,
			NamespaceChecker: namespaceChecker,
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
