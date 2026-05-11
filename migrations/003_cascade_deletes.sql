-- +goose Up
-- ON DELETE CASCADE on the allocation tables.
--
-- Adds database-level FK constraints so orphan allocation rows can never
-- persist even if the application-side delete guards (which check for active
-- allocations and return HTTP 409) are bypassed by a bug or manual SQL.
--
-- Defensive cleanup: drop any existing orphans before adding the FK so
-- ALTER TABLE doesn't fail on out-of-spec data.

DELETE FROM ipam_prefix_allocations
 WHERE pool_key NOT IN (SELECT key FROM ipam_objects);

DELETE FROM ipam_asn_allocations
 WHERE pool_key NOT IN (SELECT key FROM ipam_objects);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'ipam_prefix_allocations_pool_key_fk'
    ) THEN
        ALTER TABLE ipam_prefix_allocations
        ADD CONSTRAINT ipam_prefix_allocations_pool_key_fk
        FOREIGN KEY (pool_key) REFERENCES ipam_objects (key)
        ON DELETE CASCADE;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'ipam_asn_allocations_pool_key_fk'
    ) THEN
        ALTER TABLE ipam_asn_allocations
        ADD CONSTRAINT ipam_asn_allocations_pool_key_fk
        FOREIGN KEY (pool_key) REFERENCES ipam_objects (key)
        ON DELETE CASCADE;
    END IF;
END$$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE ipam_prefix_allocations DROP CONSTRAINT IF EXISTS ipam_prefix_allocations_pool_key_fk;
ALTER TABLE ipam_asn_allocations    DROP CONSTRAINT IF EXISTS ipam_asn_allocations_pool_key_fk;
