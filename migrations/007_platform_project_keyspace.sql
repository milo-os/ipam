-- 007: there is no unprefixed key space.
--
-- Every object IPAM stores belongs to a project — the platform's own included.
-- The IPClass catalog and the operator-authored pools that back it now live
-- under "project/<platform>/", where <platform> is the apiserver's
-- --platform-project, exactly like a tenant's objects live under their own
-- project. Keys of the shape "/ipam.miloapis.com/..." are no longer read or
-- written by any code path.
--
-- # Why this refuses rather than rewriting
--
-- The rewrite itself is trivial and is spelled out below. What this migration
-- cannot do is know the platform project's NAME. That value is deployment
-- configuration — a serve flag — and the schema has never seen it. Guessing it
-- would move every platform object into a project chosen by a migration, which
-- is a worse outcome than stopping.
--
-- Note how this differs from 005 and 006, which also refused. Those refused
-- because a backfill was *impossible*: a scope digest is a SHA-256 over a
-- string the schema does not store, so a stored digest can be neither
-- recomputed nor recognised. Here the transformation is a deterministic,
-- reversible string operation and the only missing input is one name the
-- operator already has. So this refusal comes with the exact command, not with
-- an instruction to delete everything.
--
-- # The rewrite
--
-- Run this with your --platform-project value substituted for :platform, with
-- the apiserver stopped:
--
--   BEGIN;
--   ALTER TABLE ipam_pool_class_offer DROP CONSTRAINT ipam_pool_class_offer_pool_key_fkey;
--   UPDATE ipam_objects
--      SET key = 'project/' || :'platform' || '/' || ltrim(key, '/')
--    WHERE key LIKE '/ipam.miloapis.com/%';
--   UPDATE ipam_pool_class_offer
--      SET pool_key = 'project/' || :'platform' || '/' || ltrim(pool_key, '/')
--    WHERE pool_key LIKE '/ipam.miloapis.com/%';
--   ALTER TABLE ipam_pool_class_offer
--     ADD CONSTRAINT ipam_pool_class_offer_pool_key_fkey
--     FOREIGN KEY (pool_key) REFERENCES ipam_objects (key) ON DELETE CASCADE;
--   COMMIT;
--
-- # Why the constraint has to come off, and not just be updated alongside
--
-- The obvious version of this is one UPDATE on ipam_objects, with the
-- referencing tables updated in the same transaction. It does not work, and it
-- fails on the first statement rather than at COMMIT:
--
--   ERROR:  update or delete on table "ipam_objects" violates foreign key
--           constraint "ipam_pool_class_offer_pool_key_fkey"
--   DETAIL: Key (key)=(/ipam.miloapis.com/ippools/perf-block-v4-root) is still
--           referenced from table "ipam_pool_class_offer".
--
-- That is verbatim from running it. The constraints REFERENCING
-- ipam_objects(key) — ipam_pool_class_offer, ipam_cidr_allocations and
-- ipam_asn_allocations on pool_key — are neither ON UPDATE CASCADE nor
-- DEFERRABLE, so the parent row cannot move while a child references it and
-- there is no ordering of plain UPDATEs that gets around it. Dropping and
-- restoring the one constraint inside the transaction is the whole trick. If
-- the allocation counts below are non-zero, do the same for those two.
--
-- Three things make the rewrite itself safe, and are worth re-checking before
-- trusting the shape of it anywhere else:
--
--   * The mapping is total and unambiguous. Under the old model an unprefixed
--     key meant "platform-owned" by construction — nothing else could write
--     one — so every such key belongs to the platform project and no other.
--
--   * It is a pure string transformation, so it is reversible and can be
--     rehearsed on a copy. Nothing is hashed and nothing is regenerated.
--
--   * No object is renamed, only re-keyed. A cascade-provisioned pool's NAME
--     embeds its scope digest, and that digest folds in the owning tenant — so
--     if any cascade pools exist for the PLATFORM tenant, their names were
--     derived under the old empty-tenant identity and will not be re-derivable.
--     The guard counts ipam_pool_identity separately for that reason: those
--     rows cannot be re-keyed correctly and must be deleted with their pools.
--
-- # status.scopeDigest heals itself; you do not need to do anything
--
-- A re-keyed pool keeps the digest it was created with: the pool digest of its
-- scope under a tenant named "" (migration 005's default, 6139457f… for the
-- empty scope). This migration cannot recompute it — a digest is a SHA-256 over
-- a canonical form the schema never stores, and it folds in the project name
-- this migration is refusing for want of. That is the same wall 005 and 006 hit.
--
-- It no longer matters, because IPPool's PrepareForUpdate re-derives the field
-- on every spec write. A re-homed pool corrects itself the next time anything
-- touches it, and the recompute is a no-op on a pool that was already right.
-- Nothing has to be done by hand; if you want to force it, any spec-level write
-- will do, including adding an annotation.
--
-- Until that write happens the stale value is cosmetic. Nothing server-side
-- reads it: `status.scopeDigest` is not a selectable field, and no field label
-- conversion is registered for any IPAM resource, so a query filtering on it is
-- rejected outright rather than silently matching nothing —
--
--   Error from server (BadRequest): "status.scopeDigest" is not a known field
--   selector: only "metadata.name", "metadata.namespace"
--
-- — which means the index idx_ipam_ippool_scope_digest is unreachable through
-- the API and the only consumer of the field anywhere is the CLI detail view.
-- A stale value can mislead a human reading `pool show`, and nothing else.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    n_objects   bigint;
    n_offers    bigint;
    n_allocs    bigint;
    n_asn       bigint;
    n_identity  bigint;
BEGIN
    SELECT count(*) INTO n_objects
      FROM ipam_objects
     WHERE key LIKE '/ipam.miloapis.com/%';

    IF n_objects = 0 THEN
        RETURN;
    END IF;

    -- Counted rather than merely mentioned: the referencing rows decide whether
    -- the rewrite is one statement or four, and an operator who runs the one
    -- statement with rows present gets a foreign-key error instead of an
    -- answer.
    SELECT count(*) INTO n_offers
      FROM ipam_pool_class_offer WHERE pool_key LIKE '/ipam.miloapis.com/%';
    SELECT count(*) INTO n_allocs
      FROM ipam_cidr_allocations WHERE pool_key LIKE '/ipam.miloapis.com/%';
    SELECT count(*) INTO n_asn
      FROM ipam_asn_allocations WHERE pool_key LIKE '/ipam.miloapis.com/%';
    SELECT count(*) INTO n_identity
      FROM ipam_pool_identity WHERE pool_key LIKE '/ipam.miloapis.com/%';

    RAISE EXCEPTION
        'refusing to migrate: % object(s) live in the unprefixed key space, which no code path reads any more (% offer, % cidr-allocation, % asn-allocation, % pool-identity row(s) reference them)',
        n_objects, n_offers, n_allocs, n_asn, n_identity
    USING HINT =
        'The platform is a project now, and this migration cannot know its name — '
        '--platform-project is a serve flag the schema has never seen, so guessing '
        'it would move every platform object into a project chosen by a migration. '
        'Stop the apiserver and, in ONE transaction: drop the foreign key '
        'ipam_pool_class_offer_pool_key_fkey, rewrite both key columns with '
        'key = ''project/<platform>/'' || ltrim(key, ''/''), then restore the constraint. '
        'The foreign keys onto ipam_objects(key) are neither ON UPDATE CASCADE nor '
        'DEFERRABLE, so a plain UPDATE of ipam_objects fails immediately while a child row '
        'references it — dropping the constraint is not optional and no ordering avoids it. '
        'Do the same for ipam_cidr_allocations and ipam_asn_allocations if their counts '
        'above are non-zero. Pool-identity rows are the exception: a cascade pool NAME '
        'embeds a digest derived under the old empty-tenant identity, so those rows and '
        'their pools must be deleted through the API rather than re-keyed. '
        'The exact statements are in the header of 007_platform_project_keyspace.sql.';
END
$$;
-- +goose StatementEnd

-- +goose Down

-- Nothing to undo. This migration changes no schema; it refuses to proceed
-- while data sits in a key space the running code cannot address. Rolling it
-- back cannot un-rewrite keys an operator rewrote by hand, and should not try:
-- the reverse transformation is in the header if it is ever wanted.
SELECT 1;
