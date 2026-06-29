-- +goose Up
-- Reconcile databases provisioned before migration 001 was consolidated, which
-- renamed ipam_prefix_allocations -> ipam_cidr_allocations. goose records 001
-- as applied by version, so the rewritten 001 never re-runs on an existing
-- database — leaving the old table name and breaking claim allocation with
-- "relation \"ipam_cidr_allocations\" does not exist". Rename idempotently:
-- pre-consolidation databases pick up the new name here; fresh databases (where
-- 001 already created ipam_cidr_allocations) skip via IF EXISTS.
ALTER TABLE IF EXISTS ipam_prefix_allocations RENAME TO ipam_cidr_allocations;

-- +goose Down
ALTER TABLE IF EXISTS ipam_cidr_allocations RENAME TO ipam_prefix_allocations;
