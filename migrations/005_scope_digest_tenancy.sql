-- 005 — the scope digest gains a tenant (internal/scope canonical form v2).
--
-- WHAT CHANGED IN GO
--
-- A scope digest identifies an address space, and an address space belongs to a
-- tenant. It did not carry one. Two projects that each have a network named
-- `default` — the name a platform that creates one network per project produces
-- for every project it has — derived the same digest, and both places that key
-- address-space identity on it failed:
--
--   * ipam_pool_identity's primary key is (class_name, scope_digest). The
--     second project to claim lost the ON CONFLICT and was handed the first
--     project's pool_key, in the first project's key space, then allocated
--     through it — bypassing the tenant prefixing the storage layer applies to
--     every other path.
--
--   * the allocation search is `pool_key = $1 AND (purpose <> 'Claim' OR
--     scope_digest = $2)` and the exclusion constraint below is
--     (pool_key, scope_digest, allocated_cidr). Neither mentions an owner, so
--     within one shared platform pool two projects' same-named networks were
--     ONE address space and could not hold the same address — defeating
--     `uniqueWithin: [network]` for the shared tenant IPv4 case it exists for.
--
-- internal/scope now emits the tenant as the second field of the canonical
-- form, so the digest is a function of (tenant, scope). Both defects are fixed
-- by that one change, and the second is fixed in the RIGHT direction: the two
-- projects become distinct spaces and may now hold the same address.
--
-- Folding tenancy into the digest rather than adding an owner column to the two
-- tables is what makes the exclusion constraint correct for free. Re-keying it
-- would mean dropping and rebuilding a GiST exclusion constraint over every
-- allocation in the service, and leaving it alone while adding an owner column
-- elsewhere would have left the schema still enforcing one space across
-- tenants — enforcement being the half that fails silently.
--
-- WHY THERE IS NO BACKFILL
--
-- The canonical form is version-tagged (`ipam.scope.v2`) precisely so a v2
-- digest cannot collide with a v1 one. That is not the same as being able to
-- TELL THEM APART: both are 64 lowercase hex characters and nothing in a row
-- records which encoding produced it. A digest also cannot be recomputed in
-- SQL, because it is a SHA-256 over a string this schema does not store.
--
-- So there is no backfill — only a reset. A v1 row left in place is not
-- inert. Its consequences are:
--
--   * ipam_pool_identity: a scope's existing pool is never found again, so the
--     next claim provisions a second one and the scope is renumbered. The model
--     promises subnets "appear on first use and are never renumbered".
--   * ipam_cidr_allocations: a v1 Claim row stops matching the v2 digest of the
--     space it belongs to, so it no longer blocks anything and no longer
--     participates in the exclusion constraint for that space. That is two
--     holders of one address, silently — the worst shape a bug in this service
--     can take.
--
-- This migration therefore REFUSES a database that already holds allocations or
-- pool identities, rather than migrating it into either of those states. Prod is
-- empty and disposable, so the refusal costs nothing there; anywhere it does
-- fire, it fires before the damage instead of after.
--
-- An operator who accepts the reset opts in explicitly, by emptying the two
-- tables before running this migration:
--
--   -- Inspect what would be lost first. Every row here is an address a holder
--   -- believes it has.
--   SELECT pool_key, allocated_cidr, claim_key, owner_project
--     FROM ipam_cidr_allocations ORDER BY pool_key, allocated_cidr;
--
--   DELETE FROM ipam_pool_identity;
--   DELETE FROM ipam_cidr_allocations;
--
-- The order matters wherever the objects go too, and getting it wrong looks
-- like this migration being at fault rather than the sequence: an allocation
-- references its pool ON DELETE RESTRICT, so deleting pools before the claims
-- that hold addresses in them returns 409s. Claims first, wait for them to go,
-- then pools. `task load:cleanup` and `task load:cascade-cleanup` do exactly
-- that for the k6 fixtures, which is the likeliest source of leftover rows: k6
-- skips teardown when a run is killed, so an interrupted load test is the
-- normal way a database arrives here non-empty.
--
-- Note what that leaves behind: the IPClaim, IPAllocation and IPPool objects in
-- ipam_objects survive, naming addresses nothing holds any more. They have to
-- be deleted too, and through the API rather than in SQL, so their watchers see
-- it. The reset is a re-addressing exercise, which is exactly why it is not
-- done silently by a migration.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    n_alloc  BIGINT;
    n_ident  BIGINT;
BEGIN
    SELECT count(*) INTO n_alloc FROM ipam_cidr_allocations;
    SELECT count(*) INTO n_ident FROM ipam_pool_identity;

    IF n_alloc > 0 OR n_ident > 0 THEN
        RAISE EXCEPTION
            'refusing to migrate: % allocation row(s) and % pool identity row(s) carry v1 scope digests',
            n_alloc, n_ident
        USING HINT =
            'A v1 digest cannot be recomputed or recognised, so there is no backfill. '
            'Left in place, identity rows renumber their scope and allocation rows stop '
            'blocking the space they belong to (two holders, one address). Empty both '
            'tables and delete the corresponding IPClaim/IPAllocation/IPPool objects '
            'through the API, then re-run. Claims BEFORE pools: allocations reference '
            'their pool ON DELETE RESTRICT, so deleting pools first returns 409s that '
            'read like this guard being wrong. If the rows came from the k6 load suite '
            '(a run killed before its teardown is the usual cause), `task load:cleanup` '
            'and `task load:cascade-cleanup` do it in that order. '
            'See the header of 005_scope_digest_tenancy.sql.';
    END IF;
END
$$;
-- +goose StatementEnd

-- The default for a writer that does not supply a digest.
--
-- 002 set this to the v1 digest of the empty scope and explained the choice:
-- a writer that forgets lands in the STRICTEST space, so the failure is a
-- spurious conflict someone notices rather than a silent double-allocation
-- nobody does. That reasoning is unchanged; only the value moves, to
-- scope.EmptyDigest("") — the v2 digest of the empty scope for the platform
-- tenant, canonical form "13:ipam.scope.v20:1:0".
--
-- Leaving the v1 value here would have been the quietest possible version of
-- the bug this migration exists for: every defaulted row would sit in a space
-- no live code can name, blocking nothing and blocked by nothing.
--
-- Pinned by TestEmptyDigestMatchesMigrationDefault in internal/scope.
ALTER TABLE ipam_cidr_allocations
    ALTER COLUMN scope_digest
    SET DEFAULT '6139457f3fc41de42d41d373bf75cc032c63fbedb7def334f08f8b40803793d9';

-- +goose Down

-- Restores 002's default. It does not and cannot restore v1 digests to rows
-- written under v2 — see the header. Rolling back the schema without also
-- resetting the data leaves v2 rows in a v1 service, which fails in exactly the
-- two ways described there, with the roles reversed.
ALTER TABLE ipam_cidr_allocations
    ALTER COLUMN scope_digest
    SET DEFAULT 'e3c2bb77ee53dba0fd2bfae23530b5e487f017115ec74806bb60cc3f09daf3fa';
