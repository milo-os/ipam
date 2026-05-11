-- +goose Up
-- Helper function used by field-selector expression indexes.
--
-- convert_from is STABLE (encoding-aware) and cannot appear in an index
-- expression, which requires IMMUTABLE. Since ipam_objects.data is always
-- UTF-8 encoded JSON, we can safely declare this wrapper IMMUTABLE — the
-- result is deterministic for any given input byte sequence.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ipam_data_to_jsonb(data bytea) RETURNS jsonb AS $$
    SELECT convert_from(data, 'UTF8')::jsonb
$$ LANGUAGE sql IMMUTABLE;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS ipam_data_to_jsonb(bytea);
