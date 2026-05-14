// Package postgres implements k8s.io/apiserver/pkg/storage.Interface backed by PostgreSQL.
//
// Objects are stored in a ipam_objects table as JSON-encoded byte arrays.
// Watch support is provided via a ipam_changelog table that records all mutations.
package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Postgres driver (pgx)
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/tenant"
	pgwatch "go.miloapis.com/ipam/internal/watch"
)

// Store implements storage.Interface against PostgreSQL.
type Store struct {
	db        *sql.DB
	codec     runtime.Codec
	versioner storage.Versioner
	// newFunc creates a zero-value object of the stored type, used for bookmark events.
	newFunc func() runtime.Object
	// watcher provides watch functionality via the changelog table.
	watcher *pgwatch.PostgresWatcher
	// readyErr is set if the store failed to initialize. Always wrapped in
	// *readyErrHolder so every Store carries the same concrete type — bare
	// error-interface values whose underlying types differ across calls
	// would trigger atomic.Value's "store of inconsistently typed value"
	// panic on the second Store.
	readyErr atomic.Value
}

// readyErrHolder is the only type ever stored into Store.readyErr. Wrapping
// the error this way means atomic.Value's first-Store-locks-the-type rule
// never trips even if future call sites Store concrete error types that
// differ from each other.
type readyErrHolder struct {
	err error
}

// NewWithWatchExclusions creates a Postgres-backed storage.Interface whose
// polled watcher skips emitting events for keys matching any of the supplied
// prefixes. Used by the postgres-native AllocatingREST claim layer to stop
// the polled watcher from duplicating work on claim rows. Pass a nil
// excludedKeyPrefixes for no exclusions.
//
// dsn is passed through to the watcher so it can open a dedicated
// LISTEN/NOTIFY connection separate from the pooled *sql.DB used for
// queries. If dsn is empty the watcher falls back to polling-only.
func NewWithWatchExclusions(db *sql.DB, codec runtime.Codec, dsn string, excludedKeyPrefixes []string) *Store {
	return &Store{
		db:        db,
		codec:     codec,
		versioner: storage.APIObjectVersioner{},
		watcher:   pgwatch.NewWithExclusions(db, codec, dsn, excludedKeyPrefixes),
	}
}

// Versioner returns the storage versioner.
func (s *Store) Versioner() storage.Versioner {
	return s.versioner
}

// Create inserts a new object. It fails if the key already exists.
func (s *Store) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	if err := s.validateKey(key); err != nil {
		return err
	}
	key = tenant.FromContext(ctx).ApplyPrefix(key)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to begin transaction: %w", err))
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			klog.ErrorS(err, "Failed to rollback transaction", "key", key)
		}
	}()

	// Prepare version (inside transaction to ensure serialization)
	rv, err := s.nextResourceVersion(ctx, tx)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to get next resource version: %w", err))
	}
	if err := s.versioner.UpdateObject(obj, uint64(rv)); err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to set resource version: %w", err))
	}

	data, err := runtime.Encode(s.codec, obj)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to encode object: %w", err))
	}

	kind, namespace, name := extractMetadata(obj)
	labels := labelsJSON(obj)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO ipam_objects (key, resource_version, kind, namespace, name, data, labels, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())`,
		key, rv, kind, namespace, name, data, labels,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return storage.NewKeyExistsError(key, 0)
		}
		return storage.NewInternalError(fmt.Errorf("failed to insert object: %w", err))
	}

	// Write changelog entry
	if err := writeChangelog(ctx, tx, key, rv, "ADDED", data); err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to write changelog: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to commit transaction: %w", err))
	}

	return decode(s.codec, data, out, rv)
}

// Delete removes the object at the given key.
func (s *Store) Delete(ctx context.Context, key string, out runtime.Object, preconditions *storage.Preconditions, validateDeletion storage.ValidateObjectFunc, cachedExistingObject runtime.Object, opts storage.DeleteOptions) error {
	if err := s.validateKey(key); err != nil {
		return err
	}
	key = tenant.FromContext(ctx).ApplyPrefix(key)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to begin transaction: %w", err))
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			klog.ErrorS(err, "Failed to rollback transaction", "key", key)
		}
	}()

	// Fetch existing object for precondition check and return value
	var existingData []byte
	var existingRV int64
	err = tx.QueryRowContext(ctx,
		`SELECT data, resource_version FROM ipam_objects WHERE key = $1 FOR UPDATE`,
		key,
	).Scan(&existingData, &existingRV)
	if err != nil {
		if err == sql.ErrNoRows {
			return storage.NewKeyNotFoundError(key, 0)
		}
		return storage.NewInternalError(fmt.Errorf("failed to read existing object: %w", err))
	}

	// Decode existing object for precondition and validation checks
	existing, err := decodeToObject(s.codec, existingData)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to decode existing object: %w", err))
	}

	if preconditions != nil {
		if err := checkPreconditions(key, preconditions, existing); err != nil {
			return err
		}
	}

	if validateDeletion != nil {
		if err := validateDeletion(ctx, existing); err != nil {
			return err
		}
	}

	rv, err := s.nextResourceVersion(ctx, tx)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to get next resource version: %w", err))
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM ipam_objects WHERE key = $1`, key)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to delete object: %w", err))
	}

	// Write changelog entry (data is nil for deletes to save space, or we can store last known state)
	if err := writeChangelog(ctx, tx, key, rv, "DELETED", existingData); err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to write changelog: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to commit transaction: %w", err))
	}

	return decode(s.codec, existingData, out, existingRV)
}

// Watch starts a watch on the given key with the given options.
//
// For project-scoped requests the watcher is given the tenant-prefixed key so
// the changelog stream is filtered to events whose key starts with that
// prefix. Platform requests pass the bare key through and see the global view.
func (s *Store) Watch(ctx context.Context, key string, opts storage.ListOptions) (watch.Interface, error) {
	return s.watcher.Watch(ctx, tenant.FromContext(ctx).ApplyPrefix(key), opts, s.newFunc)
}

// SetNewFunc sets the factory function for creating zero-value objects.
// This is used to construct bookmark events during watch.
func (s *Store) SetNewFunc(f func() runtime.Object) {
	s.newFunc = f
}

// Stop signals the embedded PostgresWatcher's LISTEN/NOTIFY listener and
// changelog cleanup goroutine to terminate. Wired into the storage
// decorator's DestroyFunc so the watcher exits cleanly when the apiserver
// shuts down a resource's cacher — without it the LISTEN connection leaks
// across rolling restarts and the cleanup loop keeps deleting changelog
// rows after the apiserver thinks it has stopped.
func (s *Store) Stop() {
	if s.watcher != nil {
		s.watcher.Stop()
	}
}

// Get retrieves an object by key.
func (s *Store) Get(ctx context.Context, key string, opts storage.GetOptions, objPtr runtime.Object) error {
	if err := s.validateKey(key); err != nil {
		return err
	}
	key = tenant.FromContext(ctx).ApplyPrefix(key)

	var data []byte
	var rv int64
	err := s.db.QueryRowContext(ctx,
		`SELECT data, resource_version FROM ipam_objects WHERE key = $1`,
		key,
	).Scan(&data, &rv)
	if err != nil {
		if err == sql.ErrNoRows {
			if opts.IgnoreNotFound {
				return runtime.SetZeroValue(objPtr)
			}
			return storage.NewKeyNotFoundError(key, 0)
		}
		return storage.NewInternalError(fmt.Errorf("failed to get object: %w", err))
	}

	return decode(s.codec, data, objPtr, rv)
}

// GetList retrieves a list of objects matching the key prefix and options.
func (s *Store) GetList(ctx context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
	listPtr, err := meta.GetItemsPtr(listObj)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to get items pointer: %w", err))
	}

	v, err := conversion.EnforcePtr(listPtr)
	if err != nil || v.Kind() != reflect.Slice {
		return storage.NewInternalError(fmt.Errorf("need ptr to slice: %w", err))
	}

	id := tenant.FromContext(ctx)

	var rows *sql.Rows
	if opts.Recursive {
		// Prefix match: normalize key to ensure it ends with / for prefix matching.
		// Apply tenant scoping: project requests see only their own prefix range.
		keyPrefix := key
		if !strings.HasSuffix(keyPrefix, "/") {
			keyPrefix += "/"
		}
		rows, err = s.db.QueryContext(ctx,
			`SELECT data, resource_version FROM ipam_objects WHERE key LIKE $1 ORDER BY key`,
			id.ApplyPrefix(keyPrefix)+"%",
		)
	} else {
		// Exact match on key
		rows, err = s.db.QueryContext(ctx,
			`SELECT data, resource_version FROM ipam_objects WHERE key = $1`,
			id.ApplyPrefix(key),
		)
	}
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to list objects: %w", err))
	}
	defer rows.Close()

	for rows.Next() {
		var data []byte
		var rv int64
		if err := rows.Scan(&data, &rv); err != nil {
			return storage.NewInternalError(fmt.Errorf("failed to scan row: %w", err))
		}

		elem := reflect.New(v.Type().Elem())
		obj := elem.Interface().(runtime.Object)
		if err := decode(s.codec, data, obj, rv); err != nil {
			return storage.NewInternalError(fmt.Errorf("failed to decode list item: %w", err))
		}

		// Apply predicate filtering (label selectors, field selectors).
		// Predicate errors (e.g. malformed selector expressions) propagate
		// up as a 5xx — they should never reach here because the
		// apiserver's selector parser rejects them, but if one does we
		// want it visible rather than silently swallowed as no-match.
		if !opts.Predicate.Empty() {
			matches, err := matchesPredicate(obj, opts.Predicate)
			if err != nil {
				return storage.NewInternalError(fmt.Errorf("predicate match: %w", err))
			}
			if !matches {
				continue
			}
		}
		v.Set(reflect.Append(v, elem.Elem()))
	}
	if err := rows.Err(); err != nil {
		return storage.NewInternalError(fmt.Errorf("error iterating rows: %w", err))
	}

	// The Postgres backend does not support point-in-time reads: the SELECT
	// above returns whatever is live in ipam_objects right now, not a
	// snapshot pinned to any particular resource version. Sourcing
	// list.ResourceVersion from max(returned-row.rv) produces false positives
	// in the apiserver cacher's DetectCacheInconsistency check: under churn
	// a deletion of a non-max row leaves max(rv) unchanged, so the value can
	// coincidentally equal the cacher's RV and trigger a digest comparison
	// between two states captured at different moments in time.
	//
	// Using currentResourceVersion() (the xmin-horizon-filtered max from
	// ipam_changelog) makes the list RV advance monotonically with commit
	// order and essentially always differ from the cacher's RV at comparison
	// time, which cleanly short-circuits the consistency check instead of
	// logging a spurious "Cache consistency check failed" event. The real
	// fix — point-in-time changelog replay — is tracked as a follow-up.
	listRV, err := s.currentResourceVersion(ctx)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to get current resource version: %w", err))
	}

	if listAccessor, err := meta.ListAccessor(listObj); err == nil {
		listAccessor.SetResourceVersion(fmt.Sprintf("%d", listRV))
	}

	return nil
}

// guaranteedUpdateMaxBackoff caps the per-attempt back-off wait. With 10ms
// initial and doubling on each retry, attempt 7 is at 640ms; capping there
// keeps a long ctx (e.g. an apiserver request with no deadline) from
// inflating each successive attempt past a reasonable timer.
const guaranteedUpdateMaxBackoff = 640 * time.Millisecond

// GuaranteedUpdate performs a read-modify-write cycle with optimistic
// locking. Conflict retries are deadline-bounded rather than
// attempt-bounded: under sustained burst contention on a single key (e.g.
// an IPPrefix pool's status update during rapid allocation) an
// attempt-bounded loop surfaces a Conflict the caller has to handle even
// when ample time remains. Bounding by ctx.Deadline() lets the apiserver's
// own request timeout govern total wall-clock instead, with exponential
// back-off (10ms, 20ms, 40ms…, capped at guaranteedUpdateMaxBackoff)
// between attempts.
//
// When ctx has no deadline (rare for apiserver requests but possible for
// internal callers) the loop falls back to a hard 10-attempt cap. Either
// way the function always returns either a successful result or the most
// recent storage error.
func (s *Store) GuaranteedUpdate(ctx context.Context, key string, destination runtime.Object, ignoreNotFound bool, preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc, cachedExistingObject runtime.Object) error {
	if err := s.validateKey(key); err != nil {
		return err
	}
	key = tenant.FromContext(ctx).ApplyPrefix(key)

	const fallbackMaxAttempts = 10
	deadline, hasDeadline := ctx.Deadline()
	backoff := 10 * time.Millisecond

	for attempt := 0; ; attempt++ {
		data, rv, err := s.guaranteedUpdateOnce(ctx, key, destination, ignoreNotFound, preconditions, tryUpdate)
		if err == nil {
			return decode(s.codec, data, destination, rv)
		}
		if !storage.IsConflict(err) {
			return err
		}

		// Decide whether to retry.
		if hasDeadline {
			remaining := time.Until(deadline)
			// Leave 100ms head-room for the next attempt to actually run.
			if remaining < backoff+100*time.Millisecond {
				return err
			}
		} else if attempt >= fallbackMaxAttempts {
			return err
		}

		// Exponential back-off with the configured cap. Use a timer so a
		// cancelled context aborts the wait promptly.
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		if next := backoff * 2; next < guaranteedUpdateMaxBackoff {
			backoff = next
		} else {
			backoff = guaranteedUpdateMaxBackoff
		}
	}
}

// guaranteedUpdateOnce performs a single attempt of the read-modify-write cycle.
// It returns the encoded data and resource version on success.
func (s *Store) guaranteedUpdateOnce(ctx context.Context, key string, destination runtime.Object, ignoreNotFound bool, preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc) ([]byte, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, storage.NewInternalError(fmt.Errorf("failed to begin transaction: %w", err))
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			klog.ErrorS(err, "Failed to rollback transaction", "key", key)
		}
	}()

	// Read current state with row lock
	var existingData []byte
	var existingRV int64
	err = tx.QueryRowContext(ctx,
		`SELECT data, resource_version FROM ipam_objects WHERE key = $1 FOR UPDATE`,
		key,
	).Scan(&existingData, &existingRV)

	var existing runtime.Object
	if err == sql.ErrNoRows {
		if !ignoreNotFound {
			return nil, 0, storage.NewKeyNotFoundError(key, 0)
		}
		// Create a zero-value object of the destination type
		existing = reflect.New(reflect.TypeOf(destination).Elem()).Interface().(runtime.Object)
	} else if err != nil {
		return nil, 0, storage.NewInternalError(fmt.Errorf("failed to read existing object: %w", err))
	} else {
		existing, err = decodeToObject(s.codec, existingData)
		if err != nil {
			return nil, 0, storage.NewInternalError(fmt.Errorf("failed to decode existing object: %w", err))
		}
		if err := s.versioner.UpdateObject(existing, uint64(existingRV)); err != nil {
			return nil, 0, storage.NewInternalError(fmt.Errorf("failed to set resource version on existing: %w", err))
		}
	}

	if preconditions != nil {
		if err := checkPreconditions(key, preconditions, existing); err != nil {
			return nil, 0, err
		}
	}

	// Run the update function
	res := existing.DeepCopyObject()
	ret, _, err := tryUpdate(res, storage.ResponseMeta{ResourceVersion: uint64(existingRV)})
	if err != nil {
		return nil, 0, err
	}

	// Check for no-op update: if the serialized contents are unchanged, skip the write.
	// Per the storage.Interface contract, if tryUpdate returns output identical to input,
	// no write should be performed.
	newData, err := runtime.Encode(s.codec, ret)
	if err != nil {
		return nil, 0, storage.NewInternalError(fmt.Errorf("failed to encode updated object: %w", err))
	}
	if existingData != nil && bytes.Equal(existingData, newData) {
		// No-op: return existing data with existing RV, no write needed.
		return existingData, existingRV, nil
	}

	// Get next resource version (inside transaction for serialization)
	rv, err := s.nextResourceVersion(ctx, tx)
	if err != nil {
		return nil, 0, storage.NewInternalError(fmt.Errorf("failed to get next resource version: %w", err))
	}
	if err := s.versioner.UpdateObject(ret, uint64(rv)); err != nil {
		return nil, 0, storage.NewInternalError(fmt.Errorf("failed to set resource version: %w", err))
	}

	data, err := runtime.Encode(s.codec, ret)
	if err != nil {
		return nil, 0, storage.NewInternalError(fmt.Errorf("failed to encode updated object: %w", err))
	}

	kind, namespace, name := extractMetadata(ret)
	labels := labelsJSON(ret)

	if existingData == nil {
		// Object didn't exist, insert it
		_, err = tx.ExecContext(ctx,
			`INSERT INTO ipam_objects (key, resource_version, kind, namespace, name, data, labels, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())`,
			key, rv, kind, namespace, name, data, labels,
		)
		if err != nil {
			return nil, 0, storage.NewInternalError(fmt.Errorf("failed to insert object: %w", err))
		}
		if err := writeChangelog(ctx, tx, key, rv, "ADDED", data); err != nil {
			return nil, 0, storage.NewInternalError(fmt.Errorf("failed to write changelog: %w", err))
		}
	} else {
		// Object exists, update it
		_, err = tx.ExecContext(ctx,
			`UPDATE ipam_objects SET resource_version = $1, kind = $2, namespace = $3, name = $4, data = $5, labels = $6, updated_at = NOW()
			 WHERE key = $7`,
			rv, kind, namespace, name, data, labels, key,
		)
		if err != nil {
			return nil, 0, storage.NewInternalError(fmt.Errorf("failed to update object: %w", err))
		}
		if err := writeChangelog(ctx, tx, key, rv, "MODIFIED", data); err != nil {
			return nil, 0, storage.NewInternalError(fmt.Errorf("failed to write changelog: %w", err))
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, storage.NewInternalError(fmt.Errorf("failed to commit transaction: %w", err))
	}

	return data, rv, nil
}

// Stats returns storage statistics.
func (s *Store) Stats(ctx context.Context) (storage.Stats, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ipam_objects`,
	).Scan(&count)
	if err != nil {
		return storage.Stats{}, storage.NewInternalError(fmt.Errorf("failed to count objects: %w", err))
	}
	return storage.Stats{ObjectCount: count}, nil
}

// GetCurrentResourceVersion returns the current resource version from the sequence.
func (s *Store) GetCurrentResourceVersion(ctx context.Context) (uint64, error) {
	rv, err := s.currentResourceVersion(ctx)
	if err != nil {
		return 0, storage.NewInternalError(fmt.Errorf("failed to get current resource version: %w", err))
	}
	return uint64(rv), nil
}

// EnableResourceSizeEstimation is a no-op for the Postgres backend.
func (s *Store) EnableResourceSizeEstimation(storage.KeysFunc) error {
	return nil
}

// CompactRevision returns the latest observed compacted revision.
// The Postgres backend does not support compaction, so this always returns 0.
func (s *Store) CompactRevision() int64 {
	return 0
}

// ReadinessCheck verifies the Postgres connection is healthy.
func (s *Store) ReadinessCheck() error {
	if v := s.readyErr.Load(); v != nil {
		return v.(*readyErrHolder).err
	}
	return s.db.Ping()
}

// RequestWatchProgress requests that the storage emit a progress notification
// (bookmark event) to all current watchers, advanced to the latest committed
// resource version. The k8s.io/apiserver cacher uses this to drive the
// ConsistentListFromCache feature: when a client issues a default kubectl
// `get` (no resource version), the cacher calls RequestWatchProgress, waits
// for the bookmark to arrive on the watch stream, and then serves the read
// from its in-memory store at memory-speed instead of round-tripping to the
// underlying database.
//
// For Postgres, "the latest committed resource version" is the current value
// of ipam_resource_version_seq. We push it to the watcher, which translates
// it into a bookmark event on every active watch.
func (s *Store) RequestWatchProgress(ctx context.Context) error {
	rv, err := s.currentResourceVersion(ctx)
	if err != nil {
		return fmt.Errorf("postgres: failed to read current resource version: %w", err)
	}
	s.watcher.NotifyProgress(uint64(rv))
	return nil
}

// nextResourceVersion returns the next resource version from the sequence.
// It accepts either a *sql.DB or *sql.Tx so that the sequence fetch can be
// performed within the same transaction as the mutation.
func (s *Store) nextResourceVersion(ctx context.Context, querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (int64, error) {
	var rv int64
	err := querier.QueryRowContext(ctx, `SELECT nextval('ipam_resource_version_seq')`).Scan(&rv)
	return rv, err
}

// currentResourceVersion returns the highest resource version that is known
// to be durably committed. It intentionally does NOT return the sequence's
// last_value, because nextval() is visible before its owning transaction
// commits — returning that value would let RequestWatchProgress advertise a
// bookmark RV whose underlying row is not yet in the changelog, regressing
// the commit-ordering guarantee the watcher's xmin-horizon cursor provides.
//
// The max(resource_version) in ipam_changelog filtered by commit_xid below
// the snapshot horizon is the highest RV every future snapshot will see.
// On a freshly bootstrapped database the changelog is empty; we return 1
// rather than 0 because the apiserver storage layer rejects list responses
// with resource version 0 ("illegal resource version from storage: 0"),
// which deadlocks informers on first start.
func (s *Store) currentResourceVersion(ctx context.Context) (int64, error) {
	var rv int64
	err := s.db.QueryRowContext(ctx,
		`SELECT GREATEST(COALESCE(MAX(resource_version), 0), 1)
		   FROM ipam_changelog
		  WHERE commit_xid < pg_snapshot_xmin(pg_current_snapshot())::text::bigint`,
	).Scan(&rv)
	return rv, err
}

// validateKey checks that the key is not empty.
func (s *Store) validateKey(key string) error {
	if key == "" {
		return storage.NewInternalError(fmt.Errorf("key must not be empty"))
	}
	return nil
}
