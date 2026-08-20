-- 004: repair the kind column and stop the offer trigger depending on it.
--
-- WHAT WAS WRONG
--
-- ipam_objects.kind was written from the request object's TypeMeta. Server-side
-- apply converts to the internal version on its way through the field manager,
-- and that conversion clears TypeMeta, so every object a GitOps deployment
-- wrote stored an empty kind while its own document said IPPool.
--
-- Two things read that column and so saw nothing: the trigger that publishes a
-- pool to the classes it names, which is the only writer of
-- ipam_pool_class_offer, and the allocator's lookup of the pools offering a
-- class. With no offer rows, every claim was refused with "no pool offers this
-- class" in any environment deployed by apply.
--
-- The storage layer now takes the kind from the encoded document. This
-- migration repairs the rows written before that and removes the trigger's
-- dependence on the column, so a wrong column can never again empty the offer
-- table.
--
-- WHY THE BACKFILL IS TWO STATEMENTS
--
-- The trigger fires AFTER INSERT OR UPDATE OF data. A kind-only UPDATE is
-- therefore invisible to it and republishes nothing. Rather than smuggle the
-- column into a data write, the offers are resynced explicitly: that also
-- repairs a pool whose kind was right all along but whose offers are missing or
-- stale, which a data-touching UPDATE scoped to broken rows would skip.

-- +goose Up

-- 1. THE TRIGGER READS THE DOCUMENT
--
-- Same guard, same scope, sourced from the bytes it already parses for
-- spec.classNames.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ipam_pool_class_offers_from_data() RETURNS TRIGGER AS $$
BEGIN
    IF ipam_data_to_jsonb(NEW.data) ->> 'kind' IS DISTINCT FROM 'IPPool' THEN
        RETURN NEW;
    END IF;
    PERFORM ipam_sync_pool_class_offers(
        NEW.key,
        ARRAY(SELECT jsonb_array_elements_text(
            COALESCE(ipam_data_to_jsonb(NEW.data) -> 'spec' -> 'classNames', '[]'::jsonb)))
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- 2. BACKFILL THE COLUMN FROM THE DOCUMENT
--
-- The column stays: fourteen partial indexes and the remaining kind-scoped
-- queries are declared on it, and a jsonb extraction in their place would make
-- every one of them a wider scan. It is a denormalisation of the document, and
-- is now written as one.
UPDATE ipam_objects
   SET kind = ipam_data_to_jsonb(data) ->> 'kind'
 WHERE kind IS DISTINCT FROM ipam_data_to_jsonb(data) ->> 'kind';

-- 3. REPUBLISH EVERY POOL'S OFFERS
--
-- Independent of the trigger and of what the column used to say, so it converges
-- whatever state a database is in. ipam_sync_pool_class_offers is idempotent: it
-- deletes the offers a pool no longer names and inserts the rest ON CONFLICT DO
-- NOTHING, so a database whose offers were already correct is left unchanged.
-- +goose StatementBegin
DO $$
DECLARE
    pools BIGINT := 0;
BEGIN
    PERFORM ipam_sync_pool_class_offers(
        o.key,
        ARRAY(SELECT jsonb_array_elements_text(
            COALESCE(ipam_data_to_jsonb(o.data) -> 'spec' -> 'classNames', '[]'::jsonb))))
      FROM ipam_objects o
     WHERE ipam_data_to_jsonb(o.data) ->> 'kind' = 'IPPool';

    GET DIAGNOSTICS pools = ROW_COUNT;
    RAISE NOTICE '004_kind_from_document: resynced offers for % pool(s)', pools;
END
$$;
-- +goose StatementEnd

-- Down restores the trigger's kind-column guard. It deliberately does not
-- re-empty the column or the offer table: both are now correct, and a rollback
-- to the previous binary keeps working with them because that binary reads the
-- column it stopped being able to write, not the other way round.

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ipam_pool_class_offers_from_data() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.kind <> 'IPPool' THEN
        RETURN NEW;
    END IF;
    PERFORM ipam_sync_pool_class_offers(
        NEW.key,
        ARRAY(SELECT jsonb_array_elements_text(
            COALESCE(ipam_data_to_jsonb(NEW.data) -> 'spec' -> 'classNames', '[]'::jsonb)))
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
