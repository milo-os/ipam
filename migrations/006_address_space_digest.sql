-- 006 — the address-space digest stops carrying a tenant field (canonical
-- form v3). The POOL digest is unchanged and stays v2.
--
-- WHAT CHANGED IN GO
--
-- 005 folded the tenant into the one digest this service had. That was right
-- for pools and wrong for allocations, and the wrongness was on the success
-- path:
--
--   A class with `uniqueWithin: []` says nothing separates two allocations, so
--   its pool is ONE address space and no two claims may hold the same block.
--   It is the strictest setting the class model has, and a public-unicast IPv4
--   class is spelled exactly that way. With a tenant folded in unconditionally
--   it became "one space PER TENANT" — very nearly the loosest. Measured on a
--   live cluster: a platform-authored class with uniqueWithin: [] over a
--   platform-authored 10.202.0.0/24 handed 10.202.0.0/32 to project-alpha AND
--   to project-beta. Both Bound, one pool, nothing logged.
--
-- internal/scope now has two digests instead of one, because there are two
-- questions and they need the tenant in different places:
--
--   * PoolDigest (v2, UNCHANGED) — the identity of a pool a tenant owns. The
--     tenant is its own field and is always present. It has to be: a pool is an
--     object in a tenant-prefixed key space, and a provisioning class may
--     legitimately declare no poolPer at all (nothing in the IPClass registry
--     requires one), so for those classes the tenant field is the only thing
--     keeping two tenants' pools apart. Without it the second tenant loses
--     ipam_pool_identity's ON CONFLICT and allocates through the first
--     tenant's pool_key — the defect 005 was written to fix.
--
--   * AddressSpaceDigest (v3, NEW) — the space an allocation must be unique
--     in. There is no tenant field; the tenant qualifies each REF instead. A
--     network named `default` in project A is a different NETWORK from
--     `default` in project B — a property of the ref, not of the space. So
--     `uniqueWithin: [network]` still separates the two projects (005's
--     property, preserved) while `uniqueWithin: []`, which has no refs to
--     qualify, is one space for everyone (006's fix).
--
-- WHICH ROWS THIS AFFECTS, AND WHICH IT DELIBERATELY DOES NOT
--
-- ipam_cidr_allocations.scope_digest holds both kinds of value, told apart by
-- the row's purpose. They are never compared to each other: the allocator's
-- search is `pool_key = $1 AND (purpose <> 'Claim' OR scope_digest = $2)`, so a
-- non-Claim row is matched by purpose and its digest is never read.
--
--   purpose = 'Claim'       — an address-space digest. THE ENCODING CHANGED.
--   purpose = 'PoolCarve'   — a pool digest (the child pool's identity). v2,
--                             unchanged, left alone.
--   purpose = 'Reservation' — rewritten below, see the backfill.
--
-- ipam_pool_identity is untouched for the same reason: it is keyed entirely on
-- pool digests, which did not change. That is not a convenience, it is the
-- point. A provisioned pool's NAME embeds the first eight characters of its
-- digest, so re-tagging the pool form would rename every cascade-provisioned
-- pool, miss every identity lookup, provision a duplicate, and renumber a scope
-- the model promises never to renumber. Pinned from both directions by
-- TestPoolDigestEncodingIsUnchanged and TestEmptyDigestMatchesMigrationDefault
-- in internal/scope.
--
-- WHY CLAIM ROWS ARE REFUSED RATHER THAN MIGRATED
--
-- A digest cannot be recognised — every one is 64 hex characters and nothing
-- records which encoding produced it — and it cannot be recomputed in SQL,
-- being a SHA-256 over a string this schema does not store.
--
-- It could in principle be recomputed in Go for SOME rows, from the IPClaim's
-- spec.scope and the IPClass's uniqueWithin, both of which are still in
-- ipam_objects. That is worth stating precisely, because "impossible" would be
-- too strong and would not survive someone checking:
--
--   * A bound row (claim_key IS NOT NULL) could be recomputed today.
--   * A RETAINED row (claim_key IS NULL) could not, ever. Retention outlives
--     the claim: the IPClaim that carried the scope is gone, and the class may
--     have been edited or deleted since. Those rows hold addresses precisely so
--     a replacement can reclaim them, and they are the ones a wrong answer
--     hurts most.
--
-- So a general backfill does not exist, and a partial one is worse than none:
-- it would leave the retained set in a space no live code can name, blocking
-- nothing and blocked by nothing, while looking like a completed migration.
-- Left in place, a stale Claim digest stops matching the space it belongs to —
-- two holders of one address, silently, which is the worst shape a bug in this
-- service can take.
--
-- This migration therefore refuses a database holding Claim rows, and ONLY
-- Claim rows. That is the difference from 005, which had to refuse on the whole
-- table because v1 → v2 changed every digest. Scoping the refusal to the rows
-- whose encoding actually moved means a database with provisioned pools, carves
-- and reservations but no live claims migrates cleanly — which is the normal
-- state after a load-test teardown.
--
-- An operator who accepts the reset opts in explicitly:
--
--   -- Inspect what would be lost first. Every row here is an address a holder
--   -- believes it has.
--   SELECT pool_key, allocated_cidr, claim_key, owner_project
--     FROM ipam_cidr_allocations WHERE purpose = 'Claim'
--    ORDER BY pool_key, allocated_cidr;
--
--   DELETE FROM ipam_cidr_allocations WHERE purpose = 'Claim';
--
-- The IPClaim and IPAllocation objects in ipam_objects survive that DELETE and
-- name addresses nothing holds any more. Delete them through the API, not in
-- SQL, so their watchers see it. Claims before pools: an allocation references
-- its pool ON DELETE RESTRICT, so removing pools first returns 409s that read
-- like this guard being wrong. `task load:cleanup` and
-- `task load:cascade-cleanup` do it in that order, and an interrupted k6 run —
-- which skips its teardown — is the usual reason a database arrives here
-- non-empty.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    n_claim BIGINT;
BEGIN
    SELECT count(*) INTO n_claim
      FROM ipam_cidr_allocations WHERE purpose = 'Claim';

    IF n_claim > 0 THEN
        RAISE EXCEPTION
            'refusing to migrate: % claim allocation row(s) carry v2 address-space digests', n_claim
        USING HINT =
            'A stored digest cannot be recognised or recomputed in SQL, and retained rows '
            '(claim_key IS NULL) cannot be recomputed at all — the claim that carried the scope '
            'is gone. Left in place they stop matching the space they belong to, which is two '
            'holders of one address. Delete the Claim rows and the corresponding '
            'IPClaim/IPAllocation objects through the API, then re-run. Pool identities, carves '
            'and reservations are NOT affected and must be left alone. '
            'See the header of 006_address_space_digest.sql.';
    END IF;
END
$$;
-- +goose StatementEnd

-- Reservations move to the address-space form, and this one IS a real backfill:
-- constant to constant, with no history invented and nothing to recompute.
--
-- A reservation belongs to no tenant's address space — it is the pool's own,
-- held against everyone — so the empty address-space digest is the honest value
-- for it, and the owning tenant's pool digest never was.
--
-- The search does not read it either way (`purpose <> 'Claim'` excludes
-- reservations from every space). What changes is the exclusion constraint,
-- which does NOT filter by purpose: (pool_key, scope_digest, allocated_cidr)
-- now compares a reservation against every `uniqueWithin: []` claim in the
-- pool, whoever made it. 002's header says reserved rows participate in that
-- constraint precisely so that a held address is capacity nobody else can use;
-- carrying the owner's pool digest is what stopped them participating for
-- anyone but the owner.
--
-- It cannot wrongly refuse a claim: the search already withholds every
-- reservation from every space, so no legitimate allocation overlaps one. A
-- violation here means a bug upstream handed out a block it had been shown.
UPDATE ipam_cidr_allocations
   SET scope_digest = 'c86bbfc3761caa942844f05f5a8379f15cdd300f512a9d5b5baaa787c4695c42'
 WHERE purpose = 'Reservation';

-- The default for a writer that does not supply a digest.
--
-- 002 and 005 both explained the choice and it is unchanged: a writer that
-- forgets lands in the STRICTEST space, so the failure is a spurious conflict
-- someone notices rather than a silent double-allocation nobody does. Only the
-- value moves, to scope.EmptyAddressSpaceDigest() — canonical form
-- "13:ipam.scope.v31:0".
--
-- It is the address-space value rather than the pool one because the rows that
-- can arrive without a digest are allocations, and the strictest space for an
-- allocation is now the tenant-independent empty one. Under 005's default such
-- a row would sit in the platform's pool space, which no claim from any project
-- shares — blocking nothing.
--
-- Pinned by TestEmptyDigestMatchesMigrationDefault in internal/scope.
ALTER TABLE ipam_cidr_allocations
    ALTER COLUMN scope_digest
    SET DEFAULT 'c86bbfc3761caa942844f05f5a8379f15cdd300f512a9d5b5baaa787c4695c42';

-- +goose Down

-- Restores 005's default and puts reservations back in their owner's pool
-- space.
--
-- The reservation half round-trips only for platform-owned pools. A reservation
-- on a project-owned pool was written under EmptyPoolDigest(<project>), and
-- this cannot recover the project — the column does not record which pool
-- digest it once held, and owner_project is the pool's owner rather than the
-- digest's tenant. Rolling back therefore puts project-owned pools'
-- reservations in the platform's pool space.
--
-- That is survivable in exactly the way the forward direction is not, and it is
-- worth being explicit about why: nothing reads a reservation's digest. The
-- search excludes reservations by purpose, so the only consumer is the
-- exclusion constraint, and a reservation in the wrong space constrains less
-- than it should rather than blocking something it should not. It cannot cause
-- a double allocation on its own, because the search still withholds the block
-- from every space.
--
-- It does NOT restore v2 address-space digests to Claim rows written under v3 —
-- see the header. Rolling the schema back without also deleting those rows
-- leaves v3 digests in a v2 service, which fails the same way the forward
-- direction does with the roles reversed.
UPDATE ipam_cidr_allocations
   SET scope_digest = '6139457f3fc41de42d41d373bf75cc032c63fbedb7def334f08f8b40803793d9'
 WHERE purpose = 'Reservation';

ALTER TABLE ipam_cidr_allocations
    ALTER COLUMN scope_digest
    SET DEFAULT '6139457f3fc41de42d41d373bf75cc032c63fbedb7def334f08f8b40803793d9';
