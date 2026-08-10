-- 010: rewrite persisted capacity counts as decimal strings.
--
-- THE PROBLEM THIS SOLVES
-- ----------------------
-- `feat!: report capacity exactly and utilization as a float` changed
-- PoolCapacity.{total,allocated,available} from int64 to string, so an IPv6
-- pool could report a real address count instead of a saturated MaxInt64. The
-- type changed; the rows already in the table did not.
--
-- Stored objects are JSON blobs decoded into the current Go types on read, so
-- every pre-existing IPPool became undecodable the moment the new binary
-- started:
--
--     Internal error occurred: failed to decode list item: failed to decode
--     object: json: cannot unmarshal number into Go struct field
--     PoolCapacity.status.capacity.total of type string
--
-- This is not a degraded read of one field. LIST decodes every item, so one
-- stale row fails the whole collection: `GET /ippools` returns 500 and the
-- pool list, the console, and the CLI are all down until it is fixed. Upgrading
-- any non-empty deployment without this migration is a full outage of the
-- resource.
--
-- WHY BOTH TABLES
-- ---------------
-- `ipam_changelog` carries a snapshot of the object in each event, and Watch
-- decodes it with the same types. Converting only `ipam_objects` leaves LIST
-- working while any watcher establishing from an old cursor still breaks — the
-- harder failure to attribute, because it depends on how far back the client
-- resumes from.
--
-- WHY THIS IS SAFE TO RE-RUN
-- --------------------------
-- The WHERE clause tests `jsonb_typeof(...) = 'number'`, so a converted row is
-- not matched twice and a row written by the new binary is never touched. The
-- values are exact: `to_jsonb(x #>> '{}')` renders the stored number as its own
-- text, and every count that existed under the old type was an int64, so there
-- is no float notation to mangle.
--
-- WHAT IS DELIBERATELY NOT CONVERTED
-- ----------------------------------
-- `status.largestFreePrefix`, removed by the preceding commit, is left in place
-- on old rows. Kubernetes JSON decoding ignores unknown fields, so a stale key
-- costs one dead entry per row and nothing else; rewriting every object to drop
-- it would touch far more rows than the outage requires. It disappears the next
-- time each pool's status is written.
--
-- `status.utilizationPercent` also changed, int64 to float, and needs no
-- conversion in either direction: JSON has one number type and an integral
-- value decodes into a float field unchanged.

-- +goose Up

-- +goose StatementBegin
UPDATE ipam_objects
SET data = convert_to(
      jsonb_set(
        jsonb_set(
          jsonb_set(
            convert_from(data, 'UTF8')::jsonb,
            '{status,capacity,total}',
            to_jsonb((convert_from(data, 'UTF8')::jsonb #> '{status,capacity,total}') #>> '{}')
          ),
          '{status,capacity,allocated}',
          to_jsonb((convert_from(data, 'UTF8')::jsonb #> '{status,capacity,allocated}') #>> '{}')
        ),
        '{status,capacity,available}',
        to_jsonb((convert_from(data, 'UTF8')::jsonb #> '{status,capacity,available}') #>> '{}')
      )::text,
      'UTF8')
WHERE kind = 'IPPool'
  AND jsonb_typeof(convert_from(data, 'UTF8')::jsonb #> '{status,capacity,total}') = 'number';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE ipam_changelog
SET data = convert_to(
      jsonb_set(
        jsonb_set(
          jsonb_set(
            convert_from(data, 'UTF8')::jsonb,
            '{status,capacity,total}',
            to_jsonb((convert_from(data, 'UTF8')::jsonb #> '{status,capacity,total}') #>> '{}')
          ),
          '{status,capacity,allocated}',
          to_jsonb((convert_from(data, 'UTF8')::jsonb #> '{status,capacity,allocated}') #>> '{}')
        ),
        '{status,capacity,available}',
        to_jsonb((convert_from(data, 'UTF8')::jsonb #> '{status,capacity,available}') #>> '{}')
      )::text,
      'UTF8')
WHERE data IS NOT NULL
  AND jsonb_typeof(convert_from(data, 'UTF8')::jsonb #> '{status,capacity,total}') = 'number';
-- +goose StatementEnd

-- +goose Down

-- Down converts back to numbers, so a rollback to the previous binary can read
-- its own rows. It is lossy by construction and cannot be otherwise: a count
-- that does not fit in an int64 — the entire reason for the string type — has
-- no faithful numeric form. Such values are clamped to MaxInt64, which is
-- exactly what the old code stored for them anyway.

-- +goose StatementBegin
UPDATE ipam_objects
SET data = convert_to(
      jsonb_set(
        jsonb_set(
          jsonb_set(
            convert_from(data, 'UTF8')::jsonb,
            '{status,capacity,total}',
            to_jsonb(LEAST((convert_from(data, 'UTF8')::jsonb #>> '{status,capacity,total}')::numeric,
                           9223372036854775807::numeric)::bigint)
          ),
          '{status,capacity,allocated}',
          to_jsonb(LEAST((convert_from(data, 'UTF8')::jsonb #>> '{status,capacity,allocated}')::numeric,
                         9223372036854775807::numeric)::bigint)
        ),
        '{status,capacity,available}',
        to_jsonb(LEAST((convert_from(data, 'UTF8')::jsonb #>> '{status,capacity,available}')::numeric,
                       9223372036854775807::numeric)::bigint)
      )::text,
      'UTF8')
WHERE kind = 'IPPool'
  AND jsonb_typeof(convert_from(data, 'UTF8')::jsonb #> '{status,capacity,total}') = 'string';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE ipam_changelog
SET data = convert_to(
      jsonb_set(
        jsonb_set(
          jsonb_set(
            convert_from(data, 'UTF8')::jsonb,
            '{status,capacity,total}',
            to_jsonb(LEAST((convert_from(data, 'UTF8')::jsonb #>> '{status,capacity,total}')::numeric,
                           9223372036854775807::numeric)::bigint)
          ),
          '{status,capacity,allocated}',
          to_jsonb(LEAST((convert_from(data, 'UTF8')::jsonb #>> '{status,capacity,allocated}')::numeric,
                         9223372036854775807::numeric)::bigint)
        ),
        '{status,capacity,available}',
        to_jsonb(LEAST((convert_from(data, 'UTF8')::jsonb #>> '{status,capacity,available}')::numeric,
                       9223372036854775807::numeric)::bigint)
      )::text,
      'UTF8')
WHERE data IS NOT NULL
  AND jsonb_typeof(convert_from(data, 'UTF8')::jsonb #> '{status,capacity,total}') = 'string';
-- +goose StatementEnd
