-- +goose Up
-- Multi-tenant scoping for allocation tracking.
--
-- The storage layer prepends "project/<name>/" to every object key so
-- per-tenant reads and writes are isolated by storage key. The allocation
-- tracking tables live outside ipam_objects, so they need their own explicit
-- tenant column to support per-project capacity queries. Existing rows
-- default to "" (platform scope).

ALTER TABLE ipam_prefix_allocations ADD COLUMN IF NOT EXISTS owner_project TEXT NOT NULL DEFAULT '';
ALTER TABLE ipam_asn_allocations    ADD COLUMN IF NOT EXISTS owner_project TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_ipam_prefix_alloc_project ON ipam_prefix_allocations (owner_project);
CREATE INDEX IF NOT EXISTS idx_ipam_asn_alloc_project    ON ipam_asn_allocations (owner_project);

-- +goose Down
DROP INDEX IF EXISTS idx_ipam_prefix_alloc_project;
DROP INDEX IF EXISTS idx_ipam_asn_alloc_project;

ALTER TABLE ipam_prefix_allocations DROP COLUMN IF EXISTS owner_project;
ALTER TABLE ipam_asn_allocations    DROP COLUMN IF EXISTS owner_project;
