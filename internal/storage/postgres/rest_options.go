package postgres

import (
	"database/sql"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/storagebackend/factory"
	"k8s.io/client-go/tools/cache"
)

// RESTOptionsGetter implements generic.RESTOptionsGetter for Postgres-backed storage.
type RESTOptionsGetter struct {
	db    *sql.DB
	dsn   string
	codec runtime.Codec
	// watchExcludedKeyPrefixes is forwarded to every Store the decorator
	// creates, so the polled watcher skips events for those keys. Used
	// when the postgres-native QuotaClaim REST layer is active.
	watchExcludedKeyPrefixes []string
}

// NewRESTOptionsGetter creates a RESTOptionsGetter that produces Postgres-backed stores.
//
// Connection pool sizing matters: the database/sql default MaxOpenConns is
// unlimited, which causes new physical connections under contention and wastes
// time on TCP+TLS+auth handshakes. We cap it explicitly so concurrent
// goroutines share a small set of warm connections. The pgx/stdlib driver
// caches prepared statements per physical connection automatically
// (StatementCacheModePrepare), so a small warm pool is ideal.
//
// The dsn is also stored for the watcher's dedicated LISTEN/NOTIFY connection,
// which uses a separate physical connection from the pooled *sql.DB.
func NewRESTOptionsGetter(dsn string) (*RESTOptionsGetter, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres connection: %w", err)
	}
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(25)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping postgres: %w", err)
	}
	return &RESTOptionsGetter{db: db, dsn: dsn}, nil
}

// GetRESTOptions returns the REST options for a given resource. Its
// StorageDecorator serves reads and writes straight from the Postgres store
// and watches through the LISTEN/NOTIFY watcher, deliberately skipping the
// apiserver's in-memory cacher: the cacher seeds itself under a tenant-less
// context and so can't serve project-scoped lists, which is wrong for this
// service's per-project key layout. Reads hit Postgres directly, which the
// connection pool is sized for.
func (r *RESTOptionsGetter) GetRESTOptions(resource schema.GroupResource, example runtime.Object) (generic.RESTOptions, error) {
	ret := generic.RESTOptions{
		ResourcePrefix: resource.Group + "/" + resource.Resource,
		StorageConfig: &storagebackend.ConfigForResource{
			Config: storagebackend.Config{
				// The type isn't used by our decorator but must be non-empty
				// to satisfy validation in upstream code.
				Type:  "postgres",
				Codec: r.codec,
				// EventsHistoryWindow must be >= 1m15s (the cacher's
				// DefaultEventFreshDuration). It's the minimum duration of
				// changelog events the storage promises to keep — clients
				// resuming a watch within this window can replay missed
				// events. Our changelog retention is 24h, well above this.
				EventsHistoryWindow: 75 * time.Second,
			},
			GroupResource: resource,
		},
		Decorator: func(
			config *storagebackend.ConfigForResource,
			resourcePrefix string,
			keyFunc func(obj runtime.Object) (string, error),
			newFunc func() runtime.Object,
			newListFunc func() runtime.Object,
			getAttrsFunc storage.AttrFunc,
			trigger storage.IndexerFuncs,
			indexers *cache.Indexers,
		) (storage.Interface, factory.DestroyFunc, error) {
			rawStore := NewWithWatchExclusions(r.db, r.codec, r.dsn, r.watchExcludedKeyPrefixes)
			rawStore.SetNewFunc(newFunc)

			// Serve reads and writes straight from Postgres — the apiserver's
			// in-memory cacher is intentionally not used. The cacher seeds
			// itself under the apiserver's tenant-less context, so it never
			// sees project-scoped objects and would make project-scoped lists
			// come back empty even when the objects exist. Going to the store
			// keeps each request's own tenant scope; WATCH uses the polled
			// PostgresWatcher. Destroy stops its LISTEN connection and changelog
			// cleanup goroutine so the connection doesn't leak across restarts.
			return rawStore, rawStore.Stop, nil
		},
	}
	return ret, nil
}

// SetCodec sets the codec used for encoding/decoding objects.
// This must be called before the RESTOptionsGetter is used.
func (r *RESTOptionsGetter) SetCodec(codec runtime.Codec) {
	r.codec = codec
}

// SetWatchExcludedKeyPrefixes configures the polled watcher to skip events
// for keys matching any of the supplied prefixes. Must be called before the
// RESTOptionsGetter produces stores, because each decorator invocation
// captures the current slice.
func (r *RESTOptionsGetter) SetWatchExcludedKeyPrefixes(excludedKeyPrefixes []string) {
	r.watchExcludedKeyPrefixes = append([]string(nil), excludedKeyPrefixes...)
}

// DB exposes the underlying *sql.DB so components like the synchronous
// allocator can share the same connection pool as the storage layer.
func (r *RESTOptionsGetter) DB() *sql.DB {
	return r.db
}

// Codec returns the runtime codec configured on the getter.
func (r *RESTOptionsGetter) Codec() runtime.Codec {
	return r.codec
}
