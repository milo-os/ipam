-- 003: reset cascade-provisioned pools for the v4 pool digest.
--
-- WHAT CHANGED IN THE CODE
--
-- A cascade pool is identified by (class_name, scope_digest), and the digest
-- folded in one project: the one holding the class DEFINITION. For a platform
-- class offered to every project that value is the same for every caller, so
-- two consumers whose claims carried the same scope reached one pool — two
-- tenants' networks both named `default` backed by one prefix, with no error
-- and nothing to see. See docs/enhancements/114-pool-identity-per-consumer-project.md.
--
-- internal/scope now takes a PoolTenancy of (owner, consumer) rather than one
-- tenant string, and IPClass.spec.poolPer gained two reserved roles: `project`,
-- which makes the consumer part of the identity, and `allProjects`, which says
-- it deliberately is not. A class that provisions pools must name one of them,
-- so no class can go on sharing without having said so. The canonical form went
-- from ipam.scope.v2 to ipam.scope.v4.
--
-- WHY THIS IS A RESET AND NOT A BACKFILL
--
-- A digest is a SHA-256 over a string the schema does not store. There is no
-- expression that turns a v2 digest into a v4 one, in SQL or anywhere else, and
-- the digest is embedded in four places at once: the pool's NAME, the identity
-- row, IPPool.status.scopeDigest, and the scope_digest of the PoolCarve row the
-- pool left against its parent. Leaving the old rows in place would not be
-- half-migrated, it would be inert: every new claim derives a v4 digest, misses
-- every identity row, and provisions a parallel chain beside the stale one,
-- which is worse than deleting them because the stale chain still holds space
-- out of its parent.
--
-- So the provisioned chain is deleted and rebuilt on first use. That is
-- affordable exactly once, and only because nothing has allocated yet.
--
-- WHAT IT DOES NOT TOUCH
--
--   * Operator-authored pools. They are identified by their own names and are
--     never provisioned by a class, so they never appear in ipam_pool_identity.
--     Their space is returned to them by the carve rows this deletes.
--   * IPClass objects. Every class that provisions pools must now name
--     `project` or `allProjects` in spec.poolPer, and no default here can make
--     that decision: shared is correct for announceable public space and wrong
--     for a per-tenant prefix. spec.poolPer is immutable, so a class written
--     before the rule is replaced rather than edited, and until it is, its
--     claims are refused rather than quietly sharing. This migration deletes
--     the pools such a class provisioned, so replacing it costs nothing here —
--     which is the whole reason the rule can be this strict.
--   * IPClaim and IPAllocation objects. If any existed, this migration would
--     have aborted.

-- +goose Up

-- 1. REFUSE IF ANY REAL ADDRESS HAS BEEN HANDED OUT
--
-- purpose = 'Claim' is the discriminator: a Reservation and a PoolCarve are the
-- provisioned chain's own bookkeeping and are rebuilt with it, but a Claim row
-- is an address a tenant holds. Deleting the pool beneath it would renumber a
-- live allocation, which is the one thing the model forbids, so this stops and
-- makes a human decide instead of doing it quietly.
--
-- Scoped to pools named by ipam_pool_identity. A claim against an
-- operator-authored pool is unaffected by any of this and must not block it.
-- +goose StatementBegin
DO $$
DECLARE
    live_claims BIGINT;
BEGIN
    SELECT count(*) INTO live_claims
      FROM ipam_cidr_allocations a
      JOIN ipam_pool_identity i ON i.pool_key = a.pool_key
     WHERE a.purpose = 'Claim';

    IF live_claims > 0 THEN
        RAISE EXCEPTION USING
            ERRCODE = 'raise_exception',
            MESSAGE = format(
                '003_pool_identity_consumer: refusing to reset %s live allocation(s) held out of cascade-provisioned pools',
                live_claims),
            DETAIL  = 'This migration deletes every class-provisioned pool so it is rebuilt under the v4 pool digest. '
                      'Those pools currently back real addresses, and deleting them would renumber allocations that are '
                      'promised never to be renumbered.',
            HINT    = 'Decide per allocation before re-running: release it, or migrate the address space by hand. '
                      'SELECT a.allocation_key, a.pool_key, a.allocated_cidr, a.owner_project '
                      'FROM ipam_cidr_allocations a JOIN ipam_pool_identity i ON i.pool_key = a.pool_key '
                      'WHERE a.purpose = ''Claim'';';
    END IF;
END
$$;
-- +goose StatementEnd

-- 2. RECORD THE DELETIONS ON THE CHANGELOG BEFORE THEY HAPPEN
--
-- A watcher that misses these keeps a provisioned pool in its cache forever:
-- the object is gone and no event ever says so. Written before the DELETE so
-- the pool rows are still readable, and with a fresh resource_version from the
-- same sequence every write uses, so the watch cursor advances in order.
--
-- The DELETE below does not write these itself. The changelog is maintained by
-- the storage layer in Go, not by a trigger, and this migration writes at the
-- table level.
INSERT INTO ipam_changelog (key, resource_version, event_type, data)
SELECT o.key, nextval('ipam_resource_version_seq'), 'DELETED', o.data
  FROM ipam_objects o
 WHERE o.key IN (SELECT pool_key FROM ipam_pool_identity);

-- 3. RELEASE THE SPACE THE PROVISIONED CHAIN HELD
--
-- Both directions, in one statement, because a nested chain is both at once: a
-- pool's own rows (its edge Reservations, and the PoolCarve backing its child)
-- have pool_key IN the set, and the PoolCarve that backs it sits in its PARENT
-- and has allocation_key IN the set. The parent may be an operator-authored
-- pool, which is exactly how its space comes back.
--
-- Deleting these first is not tidiness: ipam_cidr_allocations.pool_key is
-- ON DELETE RESTRICT, so step 4 fails outright while any of them remain. The
-- AFTER DELETE trigger lowers each affected search floor on the way through;
-- the floors themselves are removed with their pools in step 4.
DELETE FROM ipam_cidr_allocations
 WHERE pool_key       IN (SELECT pool_key FROM ipam_pool_identity)
    OR allocation_key IN (SELECT pool_key FROM ipam_pool_identity);

-- 4. DELETE THE POOLS THEMSELVES
--
-- ipam_pool_identity, ipam_pool_class_offer, ipam_pool_search_floor and
-- ipam_pool_consumption all reference ipam_objects(key) ON DELETE CASCADE, so
-- the identity rows, the offers, the floors and the consumption totals go with
-- the pools. That cascade is why the key set is materialised into an array
-- first: deleting the pools removes the very rows the WHERE clause reads.
-- +goose StatementBegin
DO $$
DECLARE
    provisioned TEXT[];
BEGIN
    SELECT coalesce(array_agg(pool_key), '{}') INTO provisioned FROM ipam_pool_identity;

    DELETE FROM ipam_objects WHERE key = ANY(provisioned);

    RAISE NOTICE '003_pool_identity_consumer: reset % cascade-provisioned pool(s); they are rebuilt on first claim',
        coalesce(array_length(provisioned, 1), 0);
END
$$;
-- +goose StatementEnd

-- Down is a no-op, for the same reason 002's is.
--
-- The pools this deleted cannot be recreated by reversing anything: their names
-- and identities were functions of a digest whose inputs no table stores, and
-- re-deriving them would need the v2 encoding this migration exists to retire.
-- Roll back by restoring a dump taken before it, and only alongside a binary
-- that still computes v2.

-- +goose Down

-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
