// Package apiserver wires the IPAM aggregated API server. It assembles
// generic apiserver configuration with the IPAM-specific REST storages for
// all 9 resources under ipam.miloapis.com/v1alpha1.
//
// Postgres is the only supported storage backend. Claim creates run
// synchronously inside a Postgres transaction so the response body includes
// the allocated CIDR / IP / ASN.
package apiserver

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2"

	_ "go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/asnclaim"
	"go.miloapis.com/ipam/internal/registry/ipam/asnpool"
	"go.miloapis.com/ipam/internal/registry/ipam/ipaddress"
	"go.miloapis.com/ipam/internal/registry/ipam/ipaddressclaim"
	"go.miloapis.com/ipam/internal/registry/ipam/ipprefix"
	"go.miloapis.com/ipam/internal/registry/ipam/ipprefixclaim"
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
	// IPPrefixClaim and IPAddressClaim creates. Required.
	PrefixAllocator allocator.PrefixAllocator
	// ASNAllocator drives synchronous ASN allocation for ASNClaim creates.
	// Required.
	ASNAllocator allocator.ASNAllocator
	// AllocatorPool is the pgx pool the allocators commit against. The claim
	// REST handlers open transactions on this pool. Required.
	AllocatorPool *pgxpool.Pool
	// PoolChecker authorises cross-project IPPrefixClaim creates via
	// SubjectAccessReview. nil bypasses the check (e.g. when no authorizer
	// is configured).
	PoolChecker access.PoolAccessChecker
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
	// RESTOptionsGetter for the *claim resources (ipprefixclaims,
	// ipaddressclaims, asnclaims). At first glance the AllocatingREST
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

	// IPPrefixClass — cluster-scoped, no status subresource.
	prefixClassStore, err := ipprefix.NewClassStorage(Scheme, c.GenericConfig.RESTOptionsGetter)
	if err != nil {
		return nil, fmt.Errorf("create IPPrefixClass storage: %w", err)
	}
	v1alpha1Storage["ipprefixclasses"] = prefixClassStore

	// IPPrefix — cluster-scoped, with status subresource, and (when allocator
	// pool is configured) deletion protection that rejects deletes for prefixes
	// with active allocations.
	prefixStore, prefixStatusStore, err := ipprefix.NewPrefixStorage(
		Scheme,
		c.GenericConfig.RESTOptionsGetter,
		c.ExtraConfig.AllocatorPool,
	)
	if err != nil {
		return nil, fmt.Errorf("create IPPrefix storage: %w", err)
	}
	v1alpha1Storage["ipprefixes"] = prefixStore
	v1alpha1Storage["ipprefixes/status"] = prefixStatusStore

	// IPPrefixClaim — namespaced, with status subresource.
	prefixClaimStore, prefixClaimStatusStore, err := ipprefixclaim.NewAllocatingStorage(
		Scheme,
		c.GenericConfig.RESTOptionsGetter,
		c.ExtraConfig.PrefixAllocator,
		c.ExtraConfig.AllocatorPool,
		allocCodec,
		c.ExtraConfig.PoolChecker,
	)
	if err != nil {
		return nil, fmt.Errorf("create IPPrefixClaim storage: %w", err)
	}
	v1alpha1Storage["ipprefixclaims"] = prefixClaimStore
	v1alpha1Storage["ipprefixclaims/status"] = prefixClaimStatusStore

	// IPAddress — namespaced, with status subresource.
	addrStore, addrStatusStore, err := ipaddress.NewStorage(Scheme, c.GenericConfig.RESTOptionsGetter)
	if err != nil {
		return nil, fmt.Errorf("create IPAddress storage: %w", err)
	}
	v1alpha1Storage["ipaddresses"] = addrStore
	v1alpha1Storage["ipaddresses/status"] = addrStatusStore

	// IPAddressClaim — namespaced, with status subresource. poolChecker
	// is passed so cross-project allocation (prefixSelector.projectRef
	// targeting another project) goes through the same SAR + visibility
	// gate as IPPrefixClaim.
	addrClaimStore, addrClaimStatusStore, err := ipaddressclaim.NewAllocatingStorage(
		Scheme,
		c.GenericConfig.RESTOptionsGetter,
		c.ExtraConfig.PrefixAllocator,
		c.ExtraConfig.AllocatorPool,
		allocCodec,
		c.ExtraConfig.PoolChecker,
	)
	if err != nil {
		return nil, fmt.Errorf("create IPAddressClaim storage: %w", err)
	}
	v1alpha1Storage["ipaddressclaims"] = addrClaimStore
	v1alpha1Storage["ipaddressclaims/status"] = addrClaimStatusStore

	// ASNPoolClass — cluster-scoped, no status subresource.
	asnPoolClassStore, err := asnpool.NewClassStorage(Scheme, c.GenericConfig.RESTOptionsGetter)
	if err != nil {
		return nil, fmt.Errorf("create ASNPoolClass storage: %w", err)
	}
	v1alpha1Storage["asnpoolclasses"] = asnPoolClassStore

	// ASNPool — cluster-scoped, with status subresource. Delete-protection
	// requires the allocator pool so it can count active allocations.
	asnPoolStore, asnPoolStatusStore, err := asnpool.NewPoolStorage(Scheme, c.GenericConfig.RESTOptionsGetter, c.ExtraConfig.AllocatorPool)
	if err != nil {
		return nil, fmt.Errorf("create ASNPool storage: %w", err)
	}
	v1alpha1Storage["asnpools"] = asnPoolStore
	v1alpha1Storage["asnpools/status"] = asnPoolStatusStore

	// ASNClaim — namespaced, with status subresource.
	asnClaimStore, asnClaimStatusStore, err := asnclaim.NewAllocatingStorage(
		Scheme,
		c.GenericConfig.RESTOptionsGetter,
		c.ExtraConfig.ASNAllocator,
		c.ExtraConfig.AllocatorPool,
		allocCodec,
	)
	if err != nil {
		return nil, fmt.Errorf("create ASNClaim storage: %w", err)
	}
	v1alpha1Storage["asnclaims"] = asnClaimStore
	v1alpha1Storage["asnclaims/status"] = asnClaimStatusStore

	apiGroupInfo.VersionedResourcesStorageMap["v1alpha1"] = v1alpha1Storage

	if err := s.GenericAPIServer.InstallAPIGroup(&apiGroupInfo); err != nil {
		return nil, err
	}

	klog.Info("IPAM server initialized successfully")
	return s, nil
}

// Run starts the server and blocks until the context is cancelled.
func (s *IPAMServer) Run(ctx context.Context) error {
	return s.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}
