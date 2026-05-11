-- +goose Up
-- Dedicated labels column for GIN-indexed label-selector filtering.
--
-- Keeps data BYTEA unchanged; extracts metadata.labels into a separate jsonb
-- column so containment checks (labels @> $required::jsonb) can use the GIN
-- index instead of loading every row and filtering in Go.

ALTER TABLE ipam_objects
    ADD COLUMN IF NOT EXISTS labels jsonb NOT NULL DEFAULT '{}';

UPDATE ipam_objects
   SET labels = COALESCE(
           convert_from(data, 'UTF8')::jsonb -> 'metadata' -> 'labels',
           '{}'::jsonb
       );

-- jsonb_path_ops is smaller and faster than jsonb_ops for @> (containment)
-- checks. It does not support the ? (key-exists) operator, but we only need
-- containment for label-selector pushdown.
CREATE INDEX IF NOT EXISTS idx_ipam_objects_labels
    ON ipam_objects USING gin(labels jsonb_path_ops);

-- +goose Down
DROP INDEX IF EXISTS idx_ipam_objects_labels;
ALTER TABLE ipam_objects DROP COLUMN IF EXISTS labels;
