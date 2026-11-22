-- 006: give classes created before admission stamped their status the status
-- admission stamps now.
--
-- WHAT WAS WRONG
--
-- A class used to be stored with status {phase: Pending} and nothing ever
-- advanced it. Since #121 (PR #123), PrepareForCreate stamps phase Ready and an
-- Accepted condition at create, because a stored class has already passed every
-- check there is. That fixed new classes only: an update copies the old status
-- forward and nothing recomputes it, so every class created before the change
-- still reads Pending. The class works; only the display is wrong. Recreating
-- the class is not an option once child pools and live claims hang off it.
--
-- WHAT IS REPAIRED
--
-- Exactly the shape the old binary wrote: phase Pending or empty, and no
-- conditions. A class in any other state, including Pending with conditions,
-- was written by something else and is left alone. Only status is replaced;
-- spec and metadata are untouched. The status is the one
-- ipclass.AcceptedStatus builds, with the transition time set to now; the
-- migration test compares the two.
--
-- WHY THE ROWS ARE REVERSIONED
--
-- As in 005: bumping resource_version and writing a MODIFIED changelog row makes
-- the repair an ordinary update to every watcher, so a client that was already
-- watching sees Ready without waiting for something else to touch the class.
--
-- WHY A MIGRATION
--
-- The repair is a one-time correction of rows an old binary wrote, which is what
-- 004 and 005 already are. Two replicas running it at once write each class
-- once: the second UPDATE waits on the first one's row locks, then re-checks the
-- WHERE clause against the repaired row and skips it.

-- +goose Up

-- +goose StatementBegin
WITH repaired AS (
    UPDATE ipam_objects
       SET data = convert_to(
             jsonb_set(
               ipam_data_to_jsonb(data),
               '{status}',
               jsonb_build_object(
                 'phase', 'Ready',
                 'conditions', jsonb_build_array(jsonb_build_object(
                   'type', 'Accepted',
                   'status', 'True',
                   'reason', 'ClassAccepted',
                   'message', 'IPClass passed validation and is available to claims',
                   'lastTransitionTime',
                   to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))))
             ::text, 'UTF8'),
           resource_version = nextval('ipam_resource_version_seq'),
           updated_at = NOW()
     WHERE kind = 'IPClass'
       AND COALESCE(ipam_data_to_jsonb(data) -> 'status' ->> 'phase', '') IN ('', 'Pending')
       -- Absent, JSON null and [] all mean no conditions.
       AND COALESCE(jsonb_array_length(
             CASE WHEN jsonb_typeof(ipam_data_to_jsonb(data) -> 'status' -> 'conditions') = 'array'
                  THEN ipam_data_to_jsonb(data) -> 'status' -> 'conditions'
             END), 0) = 0
    RETURNING key, resource_version, data
)
INSERT INTO ipam_changelog (key, resource_version, event_type, data)
SELECT key, resource_version, 'MODIFIED', data FROM repaired;
-- +goose StatementEnd

-- Down does nothing. The migration does not record which classes it repaired,
-- and putting Pending back would restore the wrong display it exists to fix.
-- The previous binary reads a Ready status without complaint.

-- +goose Down

SELECT 1;
