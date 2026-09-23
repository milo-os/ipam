-- 005: stamp the creation timestamp and UID on objects written outside the
-- apiserver's create path.
--
-- WHAT WAS WRONG
--
-- Two kinds of object are written straight into ipam_objects rather than being
-- POSTed: the pools a class provisions as a claim cascades, and the
-- IPAllocation a claim materialises. Neither passed through
-- rest.FillObjectMetaSystemFields, which is what stamps metadata.uid and
-- metadata.creationTimestamp on everything the apiserver creates, so both were
-- stored with neither. `kubectl get ippools` reported 0001-01-01T00:00:00Z for
-- every provisioned pool while the two hand-created root pools showed a real
-- time, and nothing could take an owner reference to a provisioned pool,
-- because an owner reference names a UID.
--
-- The two write paths now stamp both fields. This migration repairs the rows
-- written before that.
--
-- WHERE THE TIMESTAMP COMES FROM
--
-- ipam_objects.created_at, which the insert has always set from NOW(). It is
-- the same instant the object came into being, so the repaired documents say
-- what they would have said had the field been stamped at the time. The UID has
-- no such source and is generated: a UID is only required to be unique and
-- stable from here on, and nothing yet references these, precisely because they
-- had none to reference.
--
-- WHY THE ROWS ARE REVERSIONED
--
-- Bumping resource_version and writing a MODIFIED changelog row makes the
-- repair an ordinary update as far as every watcher is concerned. Without it a
-- client that established a watch before the migration would keep serving the
-- unstamped object from its cache until something else happened to touch it.

-- +goose Up

-- +goose StatementBegin
WITH repaired AS (
    UPDATE ipam_objects
       SET data = convert_to(
             jsonb_set(
               jsonb_set(
                 ipam_data_to_jsonb(data),
                 '{metadata,creationTimestamp}',
                 CASE
                   WHEN ipam_data_to_jsonb(data) -> 'metadata' ->> 'creationTimestamp' IS NULL
                   THEN to_jsonb(to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
                   ELSE ipam_data_to_jsonb(data) -> 'metadata' -> 'creationTimestamp'
                 END),
               '{metadata,uid}',
               CASE
                 WHEN ipam_data_to_jsonb(data) -> 'metadata' ->> 'uid' IS NULL
                 THEN to_jsonb(gen_random_uuid()::text)
                 ELSE ipam_data_to_jsonb(data) -> 'metadata' -> 'uid'
               END)
             ::text, 'UTF8'),
           resource_version = nextval('ipam_resource_version_seq'),
           updated_at = NOW()
     -- ->> yields SQL NULL both for an absent key and for the JSON null that a
     -- zero metav1.Time marshals to, which is what these documents actually
     -- carry.
     WHERE ipam_data_to_jsonb(data) -> 'metadata' ->> 'creationTimestamp' IS NULL
        OR ipam_data_to_jsonb(data) -> 'metadata' ->> 'uid' IS NULL
    RETURNING key, resource_version, data
)
INSERT INTO ipam_changelog (key, resource_version, event_type, data)
SELECT key, resource_version, 'MODIFIED', data FROM repaired;
-- +goose StatementEnd

-- Down does nothing. It cannot: the migration does not record which documents
-- were missing which field, and a rollback that stripped every UID and creation
-- timestamp would break the objects that always had them. The previous binary
-- reads both fields happily — it just never wrote them — so leaving the repair
-- in place is also what a rollback wants.

-- +goose Down

SELECT 1;
