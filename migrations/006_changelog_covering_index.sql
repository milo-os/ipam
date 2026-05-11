-- +goose Up
-- Covering index for currentResourceVersion(): scans backward from the highest
-- resource_version, stops at the first row whose commit_xid is below the
-- snapshot horizon. Makes MAX(resource_version) WHERE commit_xid < xmin an
-- index-only scan rather than a heap-fetching aggregate over the whole table.
CREATE INDEX IF NOT EXISTS idx_ipam_changelog_rv_desc_xid
    ON ipam_changelog (resource_version DESC, commit_xid);

-- +goose Down
DROP INDEX IF EXISTS idx_ipam_changelog_rv_desc_xid;
