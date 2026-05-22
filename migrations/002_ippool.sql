-- +goose Up
--
-- Schema migration for the IPPool/IPClaim/IPAllocation rename:
--
--   IPPrefixClass  → removed (visibility moved into IPPool.spec.visibility)
--   IPPrefix       → IPAllocation (namespaced leaf, system-created)
--   IPPrefixClaim  → IPClaim
--   IPPool         → new cluster-scoped pool kind
--
-- All affected resources keep the same ipam_objects table; only their
-- kind-scoped expression indexes change.

-- IPPool — new cluster-scoped pool kind.
CREATE INDEX IF NOT EXISTS idx_ipam_ippool_ip_family
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily'))
    WHERE kind = 'IPPool';

CREATE INDEX IF NOT EXISTS idx_ipam_ippool_parent_pool_ref_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' -> 'parentPoolRef' ->> 'name'))
    WHERE kind = 'IPPool';

-- IPAllocation — replaces the IPPrefix indexes. spec.classRef is gone;
-- spec.poolRef takes its place.
DROP INDEX IF EXISTS idx_ipam_ipprefix_ip_family;
DROP INDEX IF EXISTS idx_ipam_ipprefix_class_ref_name;

CREATE INDEX IF NOT EXISTS idx_ipam_ipallocation_ip_family
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily'))
    WHERE kind = 'IPAllocation';

CREATE INDEX IF NOT EXISTS idx_ipam_ipallocation_pool_ref_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' -> 'poolRef' ->> 'name'))
    WHERE kind = 'IPAllocation';

-- IPClaim — replaces the IPPrefixClaim indexes. spec.prefixRef → spec.poolRef.
DROP INDEX IF EXISTS idx_ipam_ipprefixclaim_ip_family;
DROP INDEX IF EXISTS idx_ipam_ipprefixclaim_prefix_ref_name;

CREATE INDEX IF NOT EXISTS idx_ipam_ipclaim_ip_family
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily'))
    WHERE kind = 'IPClaim';

CREATE INDEX IF NOT EXISTS idx_ipam_ipclaim_pool_ref_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' -> 'poolRef' ->> 'name'))
    WHERE kind = 'IPClaim';

-- +goose Down
DROP INDEX IF EXISTS idx_ipam_ippool_ip_family;
DROP INDEX IF EXISTS idx_ipam_ippool_parent_pool_ref_name;
DROP INDEX IF EXISTS idx_ipam_ipallocation_ip_family;
DROP INDEX IF EXISTS idx_ipam_ipallocation_pool_ref_name;
DROP INDEX IF EXISTS idx_ipam_ipclaim_ip_family;
DROP INDEX IF EXISTS idx_ipam_ipclaim_pool_ref_name;

CREATE INDEX IF NOT EXISTS idx_ipam_ipprefix_ip_family
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily'))
    WHERE kind = 'IPPrefix';
CREATE INDEX IF NOT EXISTS idx_ipam_ipprefix_class_ref_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' -> 'classRef' ->> 'name'))
    WHERE kind = 'IPPrefix';
CREATE INDEX IF NOT EXISTS idx_ipam_ipprefixclaim_ip_family
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily'))
    WHERE kind = 'IPPrefixClaim';
CREATE INDEX IF NOT EXISTS idx_ipam_ipprefixclaim_prefix_ref_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' -> 'prefixRef' ->> 'name'))
    WHERE kind = 'IPPrefixClaim';
