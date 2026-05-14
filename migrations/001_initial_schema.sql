-- +goose Up
-- IPAM service initial schema.
--
-- Provisions all core tables, indexes, helper functions, and LISTEN/NOTIFY
-- plumbing in a single migration. Requires PostgreSQL 13+ for
-- pg_current_xact_id() / pg_snapshot_xmin().
--
-- FK constraints on the allocation tables use ON DELETE RESTRICT so that a
-- pool object cannot be deleted while claims against it still exist. The
-- application layer already returns HTTP 409 for this case; RESTRICT is the
-- database-level backstop.

CREATE SEQUENCE IF NOT EXISTS ipam_resource_version_seq;

-- +goose StatementBegin
-- ipam_data_to_jsonb wraps convert_from so it can appear in IMMUTABLE index
-- expressions. convert_from is STABLE (encoding-aware); since ipam_objects.data
-- is always UTF-8 JSON the result is deterministic for any given byte sequence.
CREATE OR REPLACE FUNCTION ipam_data_to_jsonb(data bytea) RETURNS jsonb AS $$
    SELECT convert_from(data, 'UTF8')::jsonb
$$ LANGUAGE sql IMMUTABLE;
-- +goose StatementEnd

CREATE TABLE IF NOT EXISTS ipam_objects (
    key              TEXT PRIMARY KEY,
    resource_version BIGINT NOT NULL DEFAULT nextval('ipam_resource_version_seq'),
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL DEFAULT '',
    name             TEXT NOT NULL,
    data             BYTEA NOT NULL,
    labels           jsonb NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ipam_objects_kind       ON ipam_objects (kind);
CREATE INDEX IF NOT EXISTS idx_ipam_objects_namespace  ON ipam_objects (namespace);
CREATE INDEX IF NOT EXISTS idx_ipam_objects_kind_ns    ON ipam_objects (kind, namespace);
CREATE INDEX IF NOT EXISTS idx_ipam_objects_key_prefix ON ipam_objects (key text_pattern_ops);
-- jsonb_path_ops is smaller and faster than jsonb_ops for @> (containment)
-- checks used in label-selector pushdown.
CREATE INDEX IF NOT EXISTS idx_ipam_objects_labels     ON ipam_objects USING gin(labels jsonb_path_ops);

CREATE TABLE IF NOT EXISTS ipam_prefix_allocations (
    id             BIGSERIAL PRIMARY KEY,
    pool_key       TEXT NOT NULL REFERENCES ipam_objects (key) ON DELETE RESTRICT,
    allocated_cidr CIDR NOT NULL,
    claim_key      TEXT NOT NULL UNIQUE,
    ip_family      TEXT NOT NULL CHECK (ip_family IN ('IPv4', 'IPv6')),
    is_child_pool  BOOLEAN NOT NULL DEFAULT FALSE,
    reclaim_policy TEXT NOT NULL DEFAULT 'Delete',
    owner_project  TEXT NOT NULL DEFAULT '',
    allocated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ipam_prefix_alloc_pool    ON ipam_prefix_allocations (pool_key);
CREATE INDEX IF NOT EXISTS idx_ipam_prefix_alloc_project ON ipam_prefix_allocations (owner_project);

CREATE TABLE IF NOT EXISTS ipam_asn_allocations (
    id             BIGSERIAL PRIMARY KEY,
    pool_key       TEXT NOT NULL REFERENCES ipam_objects (key) ON DELETE RESTRICT,
    asn            BIGINT NOT NULL,
    claim_key      TEXT NOT NULL UNIQUE,
    reclaim_policy TEXT NOT NULL DEFAULT 'Delete',
    owner_project  TEXT NOT NULL DEFAULT '',
    allocated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pool_key, asn)
);

CREATE INDEX IF NOT EXISTS idx_ipam_asn_alloc_project ON ipam_asn_allocations (owner_project);

CREATE TABLE IF NOT EXISTS ipam_changelog (
    id               BIGSERIAL PRIMARY KEY,
    key              TEXT NOT NULL,
    resource_version BIGINT NOT NULL,
    event_type       TEXT NOT NULL CHECK (event_type IN ('ADDED', 'MODIFIED', 'DELETED')),
    data             BYTEA,
    commit_xid       BIGINT NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ipam_changelog_rv             ON ipam_changelog (resource_version);
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_key            ON ipam_changelog (key);
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_rv_key         ON ipam_changelog (resource_version, key);
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_created_at     ON ipam_changelog (created_at);
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_commit_xid_id  ON ipam_changelog (commit_xid, id);
-- Covering index for currentResourceVersion(): makes MAX(resource_version)
-- WHERE commit_xid < xmin an index-only scan.
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_rv_desc_xid    ON ipam_changelog (resource_version DESC, commit_xid);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ipam_notify_changelog() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('ipam_changes', NEW.resource_version::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS ipam_changelog_notify ON ipam_changelog;
CREATE TRIGGER ipam_changelog_notify
    AFTER INSERT ON ipam_changelog
    FOR EACH ROW EXECUTE FUNCTION ipam_notify_changelog();

-- +goose Down
DROP TRIGGER IF EXISTS ipam_changelog_notify ON ipam_changelog;
DROP FUNCTION IF EXISTS ipam_notify_changelog();
DROP TABLE IF EXISTS ipam_changelog;
DROP TABLE IF EXISTS ipam_asn_allocations;
DROP TABLE IF EXISTS ipam_prefix_allocations;
DROP TABLE IF EXISTS ipam_objects;
DROP FUNCTION IF EXISTS ipam_data_to_jsonb(bytea);
DROP SEQUENCE IF EXISTS ipam_resource_version_seq;
