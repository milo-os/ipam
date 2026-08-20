package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage"
)

// decode decodes data into an object and sets its resource version.
func decode(codec runtime.Codec, data []byte, into runtime.Object, rv int64) error {
	_, _, err := codec.Decode(data, nil, into)
	if err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to decode object: %w", err))
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(into, uint64(rv)); err != nil {
		return storage.NewInternalError(fmt.Errorf("failed to set resource version: %w", err))
	}
	return nil
}

// decodeToObject decodes data into a new runtime.Object.
func decodeToObject(codec runtime.Codec, data []byte) (runtime.Object, error) {
	obj, _, err := codec.Decode(data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decode object: %w", err)
	}
	return obj, nil
}

// labelsJSON returns the object's metadata.labels encoded as a JSON object
// suitable for insertion into the labels jsonb column. Returns []byte("{}") if
// the object has no labels or the accessor call fails — the column is NOT NULL
// so we always need a valid jsonb value.
func labelsJSON(obj runtime.Object) []byte {
	accessor, err := meta.Accessor(obj)
	if err != nil || len(accessor.GetLabels()) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(accessor.GetLabels())
	if err != nil {
		return []byte("{}")
	}
	return b
}

// extractMetadata extracts kind, namespace, and name for the object's row.
//
// data is the encoded document about to be written. The kind is taken from it
// rather than from the object's TypeMeta: the encoder writes the kind from the
// scheme, whereas TypeMeta is cleared by the conversion to the internal version
// that every server-side apply goes through, so an apply-written row would
// otherwise record no kind at all. Taking both from the same bytes also means
// the column and the document can never disagree.
func extractMetadata(obj runtime.Object, data []byte) (kind, namespace, name string) {
	accessor, err := meta.Accessor(obj)
	if err == nil {
		namespace = accessor.GetNamespace()
		name = accessor.GetName()
	}
	return kindFromData(data, obj), namespace, name
}

// kindFromData reads the kind out of an encoded API object, falling back to the
// object's TypeMeta if the document does not carry one.
func kindFromData(data []byte, obj runtime.Object) string {
	var doc struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &doc); err == nil && doc.Kind != "" {
		return doc.Kind
	}
	return obj.GetObjectKind().GroupVersionKind().Kind
}

// isUniqueViolation checks if the error is a Postgres unique constraint violation.
func isUniqueViolation(err error) bool {
	// SQLSTATE 23505 = unique_violation. Prefer the typed pgx error path and
	// fall back to string matching so the check remains robust even if the
	// error arrives wrapped in a context we don't control.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}
	return strings.Contains(err.Error(), "duplicate key value violates unique constraint") ||
		strings.Contains(err.Error(), "23505")
}

// checkPreconditions validates storage preconditions against the existing object.
func checkPreconditions(key string, preconditions *storage.Preconditions, existing runtime.Object) error {
	if preconditions == nil {
		return nil
	}
	return preconditions.Check(key, existing)
}

// matchesPredicate checks if an object matches a storage selection predicate.
// Predicate errors (e.g. a malformed selector that slips past upstream
// validation) are propagated to the caller so they surface as a 4xx
// response instead of being silently swallowed as "this object does not
// match" — the latter would let a typo in a label selector quietly return
// an empty list with no indication anything was wrong.
func matchesPredicate(obj runtime.Object, predicate storage.SelectionPredicate) (bool, error) {
	return predicate.Matches(obj)
}

// writeChangelog inserts a row into ipam_changelog within the given transaction.
func writeChangelog(ctx context.Context, tx *sql.Tx, key string, rv int64, eventType string, data []byte) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO ipam_changelog (key, resource_version, event_type, data, created_at)
		 VALUES ($1, $2, $3, $4, NOW())`,
		key, rv, eventType, data,
	)
	return err
}
