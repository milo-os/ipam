-- +goose Up
-- IPAM service initial schema.
--
-- Provisions the four core tables, LISTEN/NOTIFY plumbing, and the
-- xmin-horizon column the watcher uses to order events by commit time.
-- Requires PostgreSQL 13+ for pg_current_xact_id() / pg_snapshot_xmin().

CREATE SEQUENCE IF NOT EXISTS ipam_resource_version_seq;

CREATE TABLE IF NOT EXISTS ipam_objects (
    key              TEXT PRIMARY KEY,
    resource_version BIGINT NOT NULL DEFAULT nextval('ipam_resource_version_seq'),
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL DEFAULT '',
    name             TEXT NOT NULL,
    data             BYTEA NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ipam_objects_kind ON ipam_objects (kind);
CREATE INDEX IF NOT EXISTS idx_ipam_objects_namespace ON ipam_objects (namespace);
CREATE INDEX IF NOT EXISTS idx_ipam_objects_kind_ns ON ipam_objects (kind, namespace);
CREATE INDEX IF NOT EXISTS idx_ipam_objects_key_prefix ON ipam_objects (key text_pattern_ops);

CREATE TABLE IF NOT EXISTS ipam_prefix_allocations (
    id             BIGSERIAL PRIMARY KEY,
    pool_key       TEXT NOT NULL,
    allocated_cidr CIDR NOT NULL,
    claim_key      TEXT NOT NULL UNIQUE,
    ip_family      TEXT NOT NULL CHECK (ip_family IN ('IPv4', 'IPv6')),
    is_child_pool  BOOLEAN NOT NULL DEFAULT FALSE,
    reclaim_policy TEXT NOT NULL DEFAULT 'Delete',
    allocated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ipam_prefix_alloc_pool
    ON ipam_prefix_allocations (pool_key);

CREATE TABLE IF NOT EXISTS ipam_asn_allocations (
    id             BIGSERIAL PRIMARY KEY,
    pool_key       TEXT NOT NULL,
    asn            BIGINT NOT NULL,
    claim_key      TEXT NOT NULL UNIQUE,
    reclaim_policy TEXT NOT NULL DEFAULT 'Delete',
    allocated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pool_key, asn)
);

CREATE TABLE IF NOT EXISTS ipam_changelog (
    id               BIGSERIAL PRIMARY KEY,
    key              TEXT NOT NULL,
    resource_version BIGINT NOT NULL,
    event_type       TEXT NOT NULL CHECK (event_type IN ('ADDED', 'MODIFIED', 'DELETED')),
    data             BYTEA,
    commit_xid       BIGINT NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ipam_changelog_rv ON ipam_changelog (resource_version);
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_key ON ipam_changelog (key);
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_rv_key ON ipam_changelog (resource_version, key);
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_created_at ON ipam_changelog (created_at);
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_commit_xid_id
    ON ipam_changelog (commit_xid, id);

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
DROP SEQUENCE IF EXISTS ipam_resource_version_seq;
