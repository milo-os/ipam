// Package apiserver wires the IPAM aggregated API server. It assembles
// generic apiserver configuration with the IPAM-specific REST storages for
// IP prefix and address resources under ipam.miloapis.com/v1alpha1.
//
// Postgres is the only supported storage backend. Claim creates run
// synchronously inside a Postgres transaction so the response body includes
// the allocated CIDR / IP.
package apiserver

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocator"
	_ "go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/registry/ipam/ipallocation"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclaim"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclass"
	"go.miloapis.com/ipam/internal/registry/ipam/ippool"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam/install"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

var (
	// Scheme defines the runtime type system for API object serialization.
	Scheme = runtime.NewScheme()
	// Codecs provides serializers for API objects.
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	install.Install(Scheme)

	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})

	// Register unversioned meta types required by the API machinery.
	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	Scheme.AddUnversionedTypes(unversioned,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)
}

// ExtraConfig extends the generic apiserver configuration with IPAM-specific
// settings.
type ExtraConfig struct {
	// PrefixAllocator drives synchronous CIDR/single-address allocation for
	// IPClaim creates. Required.
	PrefixAllocator allocator.PrefixAllocator
	// AllocatorPool is the pgx pool the allocators commit against. The claim
	// REST handlers open transactions on this pool. Required.
	AllocatorPool *pgxpool.Pool
	// LeaseSweepInterval is how often the retention-lease sweeper runs. Zero
	// disables it entirely.
	//
	// Disabled and enabled are both safe defaults here, because expiry is
	// already off unless a class or pool states a lease — a running sweeper with
	// no leases configured examines the retained set and releases nothing. The
	// switch exists so an operator can stop it without editing every class, and
	// so a deployment that wants no background work in its apiservers can say so.
	LeaseSweepInterval time.Duration

	// PlatformProject is the project the platform's own address space lives in:
	// the IPClass catalog and the operator-authored pools backing it.
	//
	// Carried here for the background sweeper, which is not a request and so
	// never passes through the handler chain that puts this on a request
	// context. Without it every sweep pass fails to load a class and retention
	// silently stops being enforced — the sweeper caches a not-found class as
	// nil and reads that as "no lease".
	PlatformProject string

	// ClassChecker authorises class consumption via SubjectAccessReview. It
	// replaced PoolChecker: a claim names a class and never a pool, so the class
	// name is the only authorization boundary a claim crosses.
	//
	// nil is a denial for every project-scoped claim, not a bypass. Wiring it to
	// nothing removes the boundary rather than removing the requirement, and the
	// design is explicit that this check must fail closed.
	ClassChecker access.ClassAccessChecker

	// NamespaceChecker refuses a claim into a namespace that cannot collect
	// what would be bound into it — Terminating, or absent (#86, #72).
	//
	// nil DISABLES the check rather than denying every claim, which is the
	// OPPOSITE of ClassChecker above and is deliberate. That one is an
	// authorization boundary and must fail closed; this one is a liveness hint
	// and must fail open, because putting another service's availability in the
	// hot path of every allocation turns a partial outage into a total one.
	NamespaceChecker access.NamespaceChecker
}

// Config combines generic and IPAM-specific configuration.
type Config struct {
	GenericConfig *genericapiserver.RecommendedConfig
	ExtraConfig   ExtraConfig
}

// IPAMServer is the IPAM service apiserver.
type IPAMServer struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   *ExtraConfig
}

// CompletedConfig prevents incomplete configuration from being used.
type CompletedConfig struct {
	*completedConfig
}

// Complete validates and fills default values for the configuration.
func (cfg *Config) Complete() CompletedConfig {
	c := completedConfig{
		cfg.GenericConfig.Complete(),
		&cfg.ExtraConfig,
	}
	return CompletedConfig{&c}
}

// New creates and initializes the IPAMServer with storage and API groups.
func (c completedConfig) New() (*IPAMServer, error) {
	genericServer, err := c.GenericConfig.New("ipam-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	s := &IPAMServer{GenericAPIServer: genericServer}

	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(v1alpha1.GroupName, Scheme, metav1.ParameterCodec, Codecs)

	// Versioned codec for the synchronous-allocation REST stores. They write
	// directly into ipam_objects bypassing the standard storage layer, so they
	// need a codec that converts internal → v1alpha1 → JSON the same way the
	// standard storage path does. Reads are serviced by the standard path,
	// which uses the same codec — keeping the wire format symmetric.
	allocCodec := Codecs.LegacyCodec(v1alpha1.SchemeGroupVersion)

	// Watch exclusions are intentionally NOT configured on the postgres
	// RESTOptionsGetter for the *claim resources (ipclaims, asnclaims).
	// At first glance the AllocatingREST
	// pattern looks like it might double-emit watch events — Create writes
	// the claim row + ADDED changelog entry directly via
	// allocator.InsertObject (bypassing the embedded Store.Create), and
	// Delete writes the Releasing-phase MODIFIED entry plus the DELETED
	// entry the same way. But each of those writes is the SOLE write for
	// its event: the embedded *genericregistry.Store's Create/Delete is
	// never reached, so there is no second writer to deduplicate against.
	// The cacher's WATCH (served by the polled internal/watch.PostgresWatcher)
	// picks up exactly those single changelog entries and dispatches them to
	// subscribers — that is the watch path for claims. Status subresource
	// updates flow through the generic Store's Update, which writes one row
	// + one MODIFIED entry; AllocatingREST never writes status outside of
	// Create / Delete, so there is no double-write there either.
	//
	// Adding the claim key prefixes to SetWatchExcludedKeyPrefixes would
	// make the polled watcher SKIP those changelog rows entirely, which
	// would silently break Watch for all three claim resources (no ADDED,
	// no MODIFIED, no DELETED events ever reach clients). The exclusion
	// hook exists in pgstore for the quota-service shape where a separate
	// per-handler LISTEN connection serves claim watches; IPAM does not
	// have that — there is no Watch override on AllocatingREST, and the
	// cacher → PostgresWatcher pipeline is the only watch path.

	v1alpha1Storage := map[string]rest.Storage{}

	// IPClass — cluster-scoped, with status subresource. A class is policy and
	// nothing about writing one allocates anything; the pool is here only so
	// reads can resolve status.offeringPools, which is derived from every
	// pool's spec.classNames and so cannot be known by any write to a class.
	// It is registered before IPPool because
	// pools offer themselves to classes; nothing enforces that order, but it is
	// the order an operator authors them in.
	ipClassStore, ipClassStatusStore, err := ipclass.NewIPClassStorage(
		Scheme,
		c.GenericConfig.RESTOptionsGetter,
		c.ExtraConfig.AllocatorPool,
	)
	if err != nil {
		return nil, fmt.Errorf("create IPClass storage: %w", err)
	}
	v1alpha1Storage["ipclasses"] = ipClassStore
	v1alpha1Storage["ipclasses/status"] = ipClassStatusStore

	// IPPool — cluster-scoped, with status subresource. Root pools persist
	// directly; child pools (with spec.parentPoolRef) allocate a sub-prefix
	// from the parent pool synchronously inside Create.
	ipPoolStore, ipPoolStatusStore, err := ippool.NewIPPoolStorage(
		Scheme,
		c.GenericConfig.RESTOptionsGetter,
		c.ExtraConfig.PrefixAllocator,
		c.ExtraConfig.AllocatorPool,
		allocCodec,
	)
	if err != nil {
		return nil, fmt.Errorf("create IPPool storage: %w", err)
	}
	v1alpha1Storage["ippools"] = ipPoolStore
	v1alpha1Storage["ippools/status"] = ipPoolStatusStore

	// IPAllocation — namespaced. Rows are system-created by the IPClaim Create
	// handler inside the allocation transaction, but deletion is this storage's
	// own: the address lives in a row the object does not own, and removing the
	// object through the generic path left that row holding an address nothing
	// named. It therefore needs the allocator and the pool.
	ipAllocStore, ipAllocStatusStore, err := ipallocation.NewAllocationStorage(
		Scheme,
		c.GenericConfig.RESTOptionsGetter,
		c.ExtraConfig.PrefixAllocator,
		c.ExtraConfig.AllocatorPool,
	)
	if err != nil {
		return nil, fmt.Errorf("create IPAllocation storage: %w", err)
	}
	v1alpha1Storage["ipallocations"] = ipAllocStore
	v1alpha1Storage["ipallocations/status"] = ipAllocStatusStore

	// IPClaim — namespaced, with status subresource. Synchronous allocation
	// against an IPPool; produces an IPAllocation in the same transaction.
	ipClaimStore, ipClaimStatusStore, err := ipclaim.NewAllocatingStorage(
		Scheme,
		c.GenericConfig.RESTOptionsGetter,
		c.ExtraConfig.PrefixAllocator,
		c.ExtraConfig.AllocatorPool,
		allocCodec,
		c.ExtraConfig.ClassChecker,
		c.ExtraConfig.NamespaceChecker,
	)
	if err != nil {
		return nil, fmt.Errorf("create IPClaim storage: %w", err)
	}
	v1alpha1Storage["ipclaims"] = ipClaimStore
	v1alpha1Storage["ipclaims/status"] = ipClaimStatusStore

	apiGroupInfo.VersionedResourcesStorageMap["v1alpha1"] = v1alpha1Storage

	if err := s.GenericAPIServer.InstallAPIGroup(&apiGroupInfo); err != nil {
		return nil, err
	}

	if c.ExtraConfig.LeaseSweepInterval > 0 {
		if err := s.addLeaseSweeper(c.ExtraConfig); err != nil {
			return nil, err
		}
	}

	klog.Info("IPAM server initialized successfully")
	return s, nil
}

// addLeaseSweeper starts the retention-lease sweeper as a post-start hook.
//
// In-process rather than a separate deployable, and with no leader election.
// Both are deliberate:
//
//   - The apiserver already runs background work of this shape — the watcher's
//     changelog cleanup — so this is not a new kind of thing for the deployment.
//   - Replicas sweeping concurrently is safe because each pool is swept under
//     that pool's row lock, which is the same lock the allocation path takes. Two
//     sweepers produce one winner and one no-op, and neither can race a claim
//     reclaiming its address. Leader election would add a failure mode (a stalled
//     leader stops all sweeping) to buy a property the lock already provides.
//
// The hook's context is cancelled at shutdown, which ends the loop between
// passes. A pass in flight finishes its current pool transaction or rolls back
// with it; nothing is left half-swept, because each pool is one transaction.
func (s *IPAMServer) addLeaseSweeper(cfg *ExtraConfig) error {
	sweeper, ok := cfg.PrefixAllocator.(interface {
		SweepExpiredLeases(context.Context, allocator.TxBeginner, allocator.SweepOptions) (allocator.SweepResult, error)
	})
	if !ok {
		return fmt.Errorf("configured allocator does not support lease sweeping")
	}
	interval := cfg.LeaseSweepInterval
	pool := cfg.AllocatorPool
	platformProject := cfg.PlatformProject

	return s.GenericAPIServer.AddPostStartHook("ipam-lease-sweeper", func(hookCtx genericapiserver.PostStartHookContext) error {
		go func() {
			// The sweeper reads the class catalog, which lives in the platform
			// project, so its context needs the same value the request filter
			// puts on a request context. This is the one caller that cannot get
			// it from the handler chain.
			ctx := tenant.WithPlatformProject(hookCtx.Context, platformProject)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			klog.InfoS("retention lease sweeper started", "interval", interval.String())
			for {
				select {
				case <-ctx.Done():
					klog.InfoS("retention lease sweeper stopped")
					return
				case <-ticker.C:
					if _, err := sweeper.SweepExpiredLeases(ctx, pool, allocator.SweepOptions{}); err != nil {
						// Logged rather than fatal: a sweep that cannot run is a
						// capacity problem, not a serving one, and the apiserver
						// must keep answering claims either way.
						klog.ErrorS(err, "retention lease sweep failed")
					}
				}
			}
		}()
		return nil
	})
}

// Run starts the server and blocks until the context is cancelled.
func (s *IPAMServer) Run(ctx context.Context) error {
	return s.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}
