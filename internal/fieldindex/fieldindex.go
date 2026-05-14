package fieldindex

import (
	"context"
	"database/sql"
	"fmt"
)

// FieldIndex describes a single SQL expression index that backs a field
// selector declared by a resource's SelectableFields function. Declaring
// indexes alongside SelectableFields keeps intent co-located with the code
// that uses it; SyncIndexes applies them idempotently at startup.
type FieldIndex struct {
	// IndexName is the Postgres index name (must be unique across all tables).
	IndexName string
	// Expression is the full CREATE INDEX body after "ON ipam_objects":
	//   ((convert_from(data, 'UTF8')::jsonb -> 'spec' ->> 'ipFamily'))
	//   WHERE kind = 'IPPrefixClaim'
	Expression string
}

// SyncIndexes creates each index if it does not already exist. It uses
// CREATE INDEX IF NOT EXISTS so it is safe to call on every startup without
// holding a schema lock longer than necessary. Each index is created in its
// own statement so a single failure does not block the others.
func SyncIndexes(ctx context.Context, db *sql.DB, indexes []FieldIndex) error {
	for _, idx := range indexes {
		stmt := fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON ipam_objects %s`,
			idx.IndexName, idx.Expression,
		)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sync index %q: %w", idx.IndexName, err)
		}
	}
	return nil
}
