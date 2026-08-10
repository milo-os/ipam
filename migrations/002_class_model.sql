-- +goose Up
--
-- Class model: scope-based pool identity, class offers, and allocation
-- uniqueness scoped by address space.
--
-- 001 modelled one claim drawing from one named pool. The class model inverts
-- that: a claim names a class and carries a scope, the allocator resolves (or
-- provisions) a pool from it, and two allocations may hold the same address
-- exactly when their scopes differ. Three things follow, and this migration
-- provides them:
--
--   * an identity table concurrent pool provisioning contends on, cheaply,
--   * a class-offer table so class health does not scan every pool's JSON,
--   * a scope digest on every allocation, and an exclusion constraint that
--     enforces non-overlap *within* an address space rather than across all
--     of them.
--
-- 001 is deliberately left byte-identical. Everything here is additive, and
-- Down restores 001-era state exactly (see the note on retained allocations).
--
-- PostgreSQL floor
-- ----------------
-- 13, and 001's header comment is right. Two independent things need it:
-- 001 uses pg_current_xact_id() / pg_snapshot_xmin(), added in 13, and 002
-- needs btree_gist installable by a non-superuser, which requires the trusted
-- extension mechanism, also added in 13. Nothing in either file uses 14+
-- syntax. Deployed clusters run 17.10.
--
-- Verified: 001 and 002 both apply cleanly on 13.23, as a non-superuser role
-- owning its database, exclusion constraint and all.
--
-- The guard below is belt-and-braces, not the primary enforcement. Below 13 the
-- schema already fails at 001 — `pg_current_xact_id() does not exist` — before
-- this ever runs, confirmed on 12.22. It earns its place only for a database
-- restored from a 001-era dump onto an older server, and as a statement of the
-- floor that executes rather than one that rots.
--
-- (An earlier revision of this file asserted a floor of 14, justified by a
-- recursive CTE with CYCLE. No such query exists anywhere in the repo — chain
-- depth and cycle rejection are done in Go, in internal/allocator/class.go and
-- internal/registry/ipam/ipclass/storage.go. The claim was inherited from a
-- task brief and repeated without checking. If you raise this floor, name the
-- feature that needs it and make sure it is one this repo actually uses.)

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('server_version_num')::int < 130000 THEN
        RAISE EXCEPTION
            'IPAM requires PostgreSQL 13 or newer (pg_snapshot_xmin, trusted extensions); this server is %',
            current_setting('server_version');
    END IF;
END
$$;
-- +goose StatementEnd

-- btree_gist supplies the `=` operator classes that let a GiST exclusion
-- constraint mix equality columns with the `&&` overlap operator on cidr. It
-- is a trusted extension from PostgreSQL 13 on, so the database owner can
-- install it without superuser; the deployed role owns its database.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- ---------------------------------------------------------------------------
-- 1. Expression indexes
-- ---------------------------------------------------------------------------
--
-- A JSON expression index over a path the model stopped writing does not
-- error. It stops matching, every query that relied on it degrades to a
-- sequential scan over ipam_objects, and nothing says so. Only paths the new
-- model no longer writes are dropped here.
--
-- IMPORTANT: these indexes are also declared in Go, in each resource's
-- strategy.go, and `ipam migrate up` runs fieldindex.SyncIndexes immediately
-- after goose. An index dropped here whose Go declaration still exists is
-- recreated seconds later. The corresponding FieldIndex entries in
-- internal/registry/ipam/{ipclaim,ipallocation,ippool}/strategy.go must be
-- updated to match this migration.

-- IPClaim.spec.poolRef is gone: a claim no longer names a pool. The allocator
-- resolves one and records it in status.poolRef.
DROP INDEX IF EXISTS idx_ipam_ipclaim_pool_ref_name;

-- IPClaim.spec.ipFamily survives as a field, but only to select the default
-- class when className is empty. Nothing lists claims by family; claims are
-- listed by class. The two remaining ipFamily indexes (IPPool, IPAllocation)
-- are still queried and are left alone.
DROP INDEX IF EXISTS idx_ipam_ipclaim_ip_family;

-- NOT dropped, contrary to the original plan for this migration:
--   idx_ipam_ippool_parent_pool_ref_name  — IPPoolSpec.ParentPoolRef is still
--       a live field (pools nest so a continent summarises as one route), and
--       the index answers "what is carved from this pool" on pool delete.
--   idx_ipam_ipallocation_pool_ref_name   — IPAllocationSpec.PoolRef is still
--       a live field and is how a pool's inventory is listed.

-- IPClass is new in this model; 001 has no indexes for it at all.

-- Query: resolving a claim that names no class to the default class for its
-- family, and listing the catalog by family.
CREATE INDEX IF NOT EXISTS idx_ipam_ipclass_ip_family
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily'))
    WHERE kind = 'IPClass';

-- Query: "which classes carve from this one" — walked at class-write time to
-- cap chain depth and reject cycles, and again before a class is deleted or
-- its parent changed.
CREATE INDEX IF NOT EXISTS idx_ipam_ipclass_parent_class_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'parentClassName'))
    WHERE kind = 'IPClass';

-- Query: "which pools offer class X", the parent resolution for a class with
-- no parentClassName. classNames is an array, so this is a containment index:
--   WHERE kind = 'IPPool'
--     AND ipam_data_to_jsonb(data) -> 'spec' -> 'classNames' @> '["public-unicast-ipv4"]'
-- ipam_pool_class_offer below answers the same question with a cheap count;
-- this index is what makes that table rebuildable and auditable from the
-- objects themselves, which is worth having for a table the application
-- maintains by hand.
CREATE INDEX IF NOT EXISTS idx_ipam_ippool_class_names
    ON ipam_objects USING gin ((ipam_data_to_jsonb(data) -> 'spec' -> 'classNames') jsonb_path_ops)
    WHERE kind = 'IPPool';

-- Query: `kubectl get ippool --field-selector status.scopeDigest=...`, and the
-- operator question "which pool serves this network in this location". The
-- authoritative index on that mapping is ipam_pool_identity; this one exists
-- so the answer is reachable from the object store as well, which is what an
-- on-call engineer has.
--
-- That query was aspirational for a long time and is now real. It needed three
-- things and had one: this index, a `status.scopeDigest` entry in IPPool's
-- SelectableFields, and a field-label conversion on the scheme. Only the index
-- existed, so the API rejected the selector outright and this index was
-- maintained on every write and never read. All three are in place now, and
-- test/e2e/field-selectors issues the query against a live apiserver so it
-- cannot quietly stop working again.
CREATE INDEX IF NOT EXISTS idx_ipam_ippool_scope_digest
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'status' ->> 'scopeDigest'))
    WHERE kind = 'IPPool';

-- Query: "every claim of class X" — draining a class onto new space, and
-- rejecting the deletion of a class that still has claims.
CREATE INDEX IF NOT EXISTS idx_ipam_ipclaim_class_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'className'))
    WHERE kind = 'IPClaim';

-- Query: "every claim drawing from this pool" — the replacement for the
-- dropped spec.poolRef index. Backs the 409 on deleting a pool with live
-- claims, and pool drain.
CREATE INDEX IF NOT EXISTS idx_ipam_ipclaim_status_pool_ref_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'status' -> 'poolRef' ->> 'name'))
    WHERE kind = 'IPClaim';

-- Query: "everything class X has handed out" — the class inventory, and the
-- unit quota attributes usage to.
CREATE INDEX IF NOT EXISTS idx_ipam_ipallocation_class_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'className'))
    WHERE kind = 'IPAllocation';

-- Query: "which allocation is this claim bound to" on release, and — with the
-- expression IS NULL — "which allocations are retained with no claim", the
-- list a lease expiry sweep walks.
CREATE INDEX IF NOT EXISTS idx_ipam_ipallocation_claim_ref_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' -> 'claimRef' ->> 'name'))
    WHERE kind = 'IPAllocation';

-- Query: "every allocation in this address space" — the inventory view behind
-- `datumctl ipam address show`, and the cross-check that the exclusion
-- constraint below and the object store agree.
--
-- Same history as the IPPool index above: declared and indexed but not
-- selectable and not converted, so unreachable through the API until the
-- field-label conversions were registered.
CREATE INDEX IF NOT EXISTS idx_ipam_ipallocation_scope_digest
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'status' ->> 'scopeDigest'))
    WHERE kind = 'IPAllocation';

-- ---------------------------------------------------------------------------
-- 2. Pool identity — what concurrent provisioning contends on
-- ---------------------------------------------------------------------------
--
-- A claim for a network and location that has never been used cascades: it
-- creates the location's subnet pool, and possibly the network's prefix pool
-- above it. A herd of first claims all observe no pool and all try to create
-- one. Exactly one must win and the rest must read the winner's pool rather
-- than fail.
--
-- The primary key is the contention point: (class_name, scope_digest), where
-- scope_digest is the canonical digest (internal/scope) of the triggering
-- claim's scope projected onto the provisioning class's poolPer. class_name is
-- in the key because two classes may have identical poolPer and still
-- provision different pools. The digest is used rather than columns because
-- the set of roles varies by class — there is no fixed column list to index.
--
-- The foreign key is DEFERRABLE INITIALLY DEFERRED, and that is the whole
-- design. The allocator claims the identity row *before* the pool object
-- exists:
--
--   INSERT INTO ipam_pool_identity (class_name, scope_digest, pool_key)
--   VALUES ($1, $2, $3)
--   ON CONFLICT (class_name, scope_digest) DO NOTHING
--   RETURNING pool_key;
--
--   -- rows returned  -> this transaction won; create the pool object under
--   --                   pool_key and commit. The deferred FK is checked at
--   --                   COMMIT, by which time the object exists.
--   -- zero rows      -> someone else won. The INSERT blocked on their
--   --                   speculative index entry until they committed, so a
--   --                   plain SELECT now reads their pool_key. No error was
--   --                   raised, so no savepoint is needed and the
--   --                   transaction is still usable.
--
-- Losers block on a single narrow index tuple while holding no pool lock and
-- no ipam_objects lock. A herd costs one index wait, not a queue behind
-- SELECT ... FOR UPDATE on a row that does not exist yet.
--
-- DO NOTHING vs DO UPDATE, measured
-- ---------------------------------
-- The one-round-trip alternative settles win-or-lose without the follow-up
-- SELECT:
--
--   ON CONFLICT (class_name, scope_digest)
--   DO UPDATE SET pool_key = ipam_pool_identity.pool_key
--   RETURNING pool_key, (xmax = 0) AS won
--
-- It is correct, and it was the first thing implemented. But DO UPDATE makes
-- every loser take a row lock on the winner's tuple and write a new version,
-- so the losers serialise against each other on the single row the whole herd
-- is contending for. DO NOTHING takes no lock and writes nothing, so once the
-- winner commits the losers proceed in parallel.
--
-- Measured on PostgreSQL 17.10, median wall time for the whole herd, 5 rounds:
--
--     herd    DO NOTHING    DO UPDATE     p95 loser latency (DN / DU)
--       24           8ms         15ms       7.5ms /  13.5ms
--      100          17ms         79ms      15.4ms /  76.1ms
--      200          26ms        227ms      22.8ms / 216.7ms
--
-- DO NOTHING grows roughly linearly with herd size; DO UPDATE does not. The
-- extra round trip a loser pays under DO NOTHING is one RTT, paid in parallel
-- and constant in herd size, which is why it loses at 24 and wins from there
-- on. Herds this size are realistic: a placement scaling into a location its
-- network has never used turns every simultaneous first claim into a
-- contender for the same identity row.
--
-- Do not "optimise" this into a single-statement CTE:
--
--   WITH ins AS (INSERT ... ON CONFLICT DO NOTHING RETURNING pool_key)
--   SELECT ... FROM ins
--   UNION ALL SELECT ... FROM ipam_pool_identity WHERE NOT EXISTS (SELECT 1 FROM ins)
--
-- It looks like one round trip with no row lock, and it is unsafe. The
-- fallback branch reads the statement's snapshot, taken before the winner
-- committed, so it finds nothing: measured, 456 of 480 concurrent losers
-- (95%) received no row at all rather than the winner's pool_key.
--
-- Only cascade-provisioned pools appear here. Operator-authored pools are
-- identified by their own name and are never provisioned by a class.
CREATE TABLE IF NOT EXISTS ipam_pool_identity (
    class_name   TEXT NOT NULL,
    scope_digest TEXT NOT NULL,
    pool_key     TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (class_name, scope_digest),

    -- One identity per pool, in the other direction: a pool cannot be claimed
    -- as the answer for two different scopes.
    --
    -- Note what that means for the ON CONFLICT above. It arbitrates on
    -- (class_name, scope_digest), so a pool_key colliding across two different
    -- scopes would raise rather than upsert. It does not, because pool_key is
    -- derived from the same inputs the digest is taken over — see poolNameFor
    -- and PlanCascade in internal/allocator/cascade.go. That is an invariant in
    -- Go, not in this schema, and it is a *hash* argument rather than an
    -- algebraic one: the digest determines the derivation inputs only in the
    -- sense that two different inputs producing one digest would be a SHA-256
    -- collision.
    --
    -- DO NOT READ THIS CONSTRAINT AS A BACKSTOP AGAINST KEY-DERIVATION BUGS.
    -- It said so until 005, and the claim had already been falsified twice over:
    --
    --   * pool_key mixed the calling project in (via tenant.Identity.ResourceKey)
    --     while scope_digest did not, so the derivation stopped being a function
    --     of the primary key the moment tenancy was added to keys — the precise
    --     precondition the old comment said would make the constraint reachable.
    --   * and it did not become reachable, because the constraint never runs on
    --     that path. Two tenants proposing different pool_keys for one
    --     (class_name, scope_digest) conflict on the PRIMARY KEY first; DO
    --     NOTHING suppresses the insert, so the second tenant writes no row and
    --     this index is never consulted. It sees one row, is satisfied, and the
    --     divergence continues silently — with the second tenant reading and
    --     allocating from the first tenant's pool.
    --
    -- So a pool_key that disagrees with the digest is invisible here by
    -- construction. 005 fixed the derivation by putting the tenant inside the
    -- digest; what catches a regression is TestPlanCascadeSeparatesProjects and
    -- the class-tenant-collision e2e suite, not this constraint. What this
    -- constraint does catch is the same pool_key inserted for two DIFFERENT
    -- (class_name, scope_digest) rows, where no ON CONFLICT arbiter suppresses
    -- it.
    --
    -- DEFERRABLE is not optional here, and the reason is not obvious. ON
    -- CONFLICT (class_name, scope_digest) suppresses conflicts only on the
    -- arbiter index it names — with DO NOTHING or DO UPDATE alike. A conflict
    -- on any *other* unique index
    -- still raises 23505 — and the loser of a provisioning race inserts a row
    -- that duplicates both the primary key and this one. Whether it raises
    -- depends on which index Postgres reaches before the arbiter kills the
    -- speculative tuple, so an immediate constraint here turns roughly one
    -- concurrent first-claim in twenty into a hard error. Deferring the check
    -- to COMMIT skips it during the statement; by then the speculative tuple
    -- is gone and there is nothing to violate. A genuine duplicate still fails,
    -- just at COMMIT.
    --
    -- This was found by the herd test in cascade_concurrency_test.go, not by
    -- reading the code. Do not make it immediate.
    CONSTRAINT ipam_pool_identity_pool_key_key UNIQUE (pool_key)
        DEFERRABLE INITIALLY DEFERRED,

    -- Deferred so the identity row may be inserted before the pool object it
    -- names. ON DELETE CASCADE because this row is a pointer, not a claim on
    -- the pool — deleting the pool retires the identity and the next claim
    -- for that scope provisions a fresh one. (Allocations use RESTRICT for
    -- the opposite reason: they are a claim on the pool.)
    CONSTRAINT ipam_pool_identity_pool_fk
        FOREIGN KEY (pool_key) REFERENCES ipam_objects (key)
        ON DELETE CASCADE
        DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX IF NOT EXISTS idx_ipam_pool_identity_class ON ipam_pool_identity (class_name);

-- ---------------------------------------------------------------------------
-- 3. Class offers — class health without scanning every pool
-- ---------------------------------------------------------------------------
--
-- IPClass.status.offeringPools must be answerable cheaply: zero means every
-- claim naming the class fails, which is worth surfacing before a consumer
-- discovers it. Reading it from JSON means a scan of every IPPool row.
--
--   SELECT count(*) FROM ipam_pool_class_offer WHERE class_name = $1;
--
-- This is a projection of IPPool.spec.classNames, and it must be maintained on
-- pool *spec* writes only. Status writes happen on every allocation; rewriting
-- offer rows there would make one contended row per class of every pool,
-- serialising claims that the per-pool locking deliberately keeps independent
-- — the same mistake as a class-level utilization counter.
--
-- ipam_sync_pool_class_offers below is the safety belt: it writes nothing and
-- takes no row locks when the offered set is unchanged, so calling it on a
-- write that did not touch spec is inefficient rather than harmful.
--
-- It can be rebuilt at any time from ipam_objects; see the rebuild query in
-- migrations/README.md. It is NOT merely a cache any more: allocator.DiscoverPool
-- joins it to find the pools offering a class, so a pool missing its offer rows
-- is a pool nothing can allocate from.
--
-- A rebuild is still safe, and deliberately so. This table carries no policy —
-- it records that a pool volunteered itself, never that a class consented to be
-- backed by that pool's project. Consent lives on the class
-- (IPClass.spec.backingProjects) and is applied above the join, so reconstructing
-- offers from spec.classNames cannot promote an unconsented offer into a live
-- one.
CREATE TABLE IF NOT EXISTS ipam_pool_class_offer (
    pool_key   TEXT NOT NULL REFERENCES ipam_objects (key) ON DELETE CASCADE,
    class_name TEXT NOT NULL,
    PRIMARY KEY (pool_key, class_name)
);

-- The lookup direction the class-health query uses.
CREATE INDEX IF NOT EXISTS idx_ipam_pool_class_offer_class ON ipam_pool_class_offer (class_name);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ipam_sync_pool_class_offers(p_pool_key TEXT, p_class_names TEXT[])
RETURNS void AS $$
BEGIN
    DELETE FROM ipam_pool_class_offer
     WHERE pool_key = p_pool_key
       AND NOT (class_name = ANY (COALESCE(p_class_names, ARRAY[]::TEXT[])));

    INSERT INTO ipam_pool_class_offer (pool_key, class_name)
    SELECT p_pool_key, c
      FROM unnest(COALESCE(p_class_names, ARRAY[]::TEXT[])) AS c
        ON CONFLICT DO NOTHING;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 4. Allocations outlive their claims
-- ---------------------------------------------------------------------------
--
-- A claim released under reclaimPolicy Retain leaves its allocation in place,
-- still held and still counted against its holder, until something releases it
-- explicitly. claim_key therefore cannot be NOT NULL and cannot be the row's
-- identity.
ALTER TABLE ipam_cidr_allocations ALTER COLUMN claim_key DROP NOT NULL;

-- The UNIQUE constraint from 001 is kept as-is and still does its job. In
-- PostgreSQL a unique index is NULLS DISTINCT by default, so any number of
-- retained (claim_key IS NULL) rows coexist while a non-NULL claim_key is
-- still unique — one live claim can hold at most one allocation, and no two
-- live claims can name the same one. Do not "fix" this with NULLS NOT
-- DISTINCT: that would allow exactly one retained allocation in the entire
-- table.

-- The allocation's own object key takes over the identity role claim_key gave
-- up: always present, one row per IPAllocation object.
--
-- NOT NULL rather than nullable, deliberately. A nullable identity column is
-- not an identity column — it would let a row exist that no API object
-- corresponds to, which is precisely the state retention and reservations are
-- meant to make impossible. Every allocation row, bound or retained or
-- reserved, backs exactly one IPAllocation.
--
-- It is added in three steps so this migration applies to a database with rows
-- in it rather than requiring a reset: add nullable, backfill, then constrain.
-- The backfill uses claim_key because every row predating this migration is
-- claim-bound — claim_key could not be NULL before this file made it so.
ALTER TABLE ipam_cidr_allocations ADD COLUMN IF NOT EXISTS allocation_key TEXT;

UPDATE ipam_cidr_allocations SET allocation_key = claim_key WHERE allocation_key IS NULL;

ALTER TABLE ipam_cidr_allocations ALTER COLUMN allocation_key SET NOT NULL;

ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_alloc_allocation_key_key;
ALTER TABLE ipam_cidr_allocations
    ADD CONSTRAINT ipam_cidr_alloc_allocation_key_key UNIQUE (allocation_key);

-- The address space this allocation belongs to: the digest of the claim's
-- scope projected onto its class's uniqueWithin.
--
-- The default is the digest of the empty scope — "one space platform-wide",
-- which is the strictest space and the one an allocation with no scope
-- genuinely occupies. Keeping the default is deliberate: a writer that forgets
-- the digest lands in the strictest space, so the failure is a spurious
-- conflict someone notices, not a silent double-allocation nobody does.
-- The value is scope.Digest(nil) in internal/scope, pinned by a test there.
ALTER TABLE ipam_cidr_allocations
    ADD COLUMN IF NOT EXISTS scope_digest TEXT NOT NULL
    DEFAULT 'e3c2bb77ee53dba0fd2bfae23530b5e487f017115ec74806bb60cc3f09daf3fa';

-- The class the address was handed out under, recorded on the row because the
-- allocation outlives the claim that chose it. Empty on reservations, which
-- belong to a pool rather than to a class.
ALTER TABLE ipam_cidr_allocations ADD COLUMN IF NOT EXISTS class_name TEXT NOT NULL DEFAULT '';

-- A reserved position is a real allocation held by the pool, so reserved space
-- has an owner, appears in utilization, and can be programmed. It has no claim
-- and is never handed out, which is why claim_key being nullable was a
-- prerequisite for reservations as well as for retention.
ALTER TABLE ipam_cidr_allocations
    ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'Claim';

ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_alloc_purpose_check;
ALTER TABLE ipam_cidr_allocations
    ADD CONSTRAINT ipam_cidr_alloc_purpose_check CHECK (purpose IN ('Claim', 'Reservation'));

-- The hot path of every allocation: read the addresses already taken in this
-- pool, in this address space, and hand the set to FindFirstAvailableBlock.
-- INCLUDE keeps it index-only.
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_pool_scope
    ON ipam_cidr_allocations (pool_key, scope_digest) INCLUDE (allocated_cidr);

-- Class inventory and quota attribution.
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_class ON ipam_cidr_allocations (class_name);

-- The lease-expiry sweep: retained allocations, oldest first.
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_retained
    ON ipam_cidr_allocations (allocated_at)
    WHERE claim_key IS NULL AND purpose = 'Claim';

-- ---------------------------------------------------------------------------
-- 5. Overlap exclusion
-- ---------------------------------------------------------------------------
--
-- CLAUDE.md describes a GiST overlap index on (pool_key, allocated_cidr) as an
-- existing secondary check. It never existed: 001 creates a plain btree on
-- pool_key alone, which cannot answer an overlap question at all. The Go
-- allocator has been the only thing preventing overlapping allocations.
--
-- An exclusion constraint fails at creation if any overlapping pair is already
-- present, so it can be added now and possibly never again. Adding it while
-- the tables are still disposable is the point.
--
-- Both equality columns are load-bearing, and getting either wrong breaks a
-- case the design requires:
--
--   pool_key      Pools nest — 10.128.0.0/12 is carved from 10.128.0.0/9 — and
--                 a child pool's carve-out is recorded as an allocation
--                 against its parent. Allocations at different levels of a
--                 chain therefore overlap by construction. Constraining on
--                 scope_digest alone would reject every nested pool.
--
--   scope_digest  Carries the owning tenant as well as the scope — see 005 and
--                 internal/scope. So this constraint is already per-tenant and
--                 must not grow an owner_project column: it would have to be
--                 dropped and rebuilt over every allocation in the service to
--                 say something the digest already says.
--
--                 uniqueWithin explicitly permits two allocations to hold the
--                 same address when their scopes differ. tenant-endpoint-ipv4
--                 is uniqueWithin [network] over a /20 every network in the
--                 location shares: two networks both holding 10.128.0.2 out of
--                 one pool is the intended behaviour, not a bug. Constraining
--                 on pool_key alone would reject it, and IPv4 tenant space
--                 would exhaust at ~4000 addresses per location in total
--                 instead of per network.
--
-- Retained (claim_key IS NULL) and reserved (purpose = 'Reservation') rows
-- participate. That is the point of both: a held address is capacity nobody
-- else can use, and it must behave that way here.
ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_alloc_no_overlap;
ALTER TABLE ipam_cidr_allocations
    ADD CONSTRAINT ipam_cidr_alloc_no_overlap
    EXCLUDE USING gist (
        pool_key     WITH =,
        scope_digest WITH =,
        allocated_cidr inet_ops WITH &&
    );

-- A pool must not be offered to two classes whose uniqueWithin differs. The
-- constraint above compares allocations only within one digest, so a pool
-- serving both a uniqueWithin [] class and a uniqueWithin [network] class
-- would let the second hand out addresses that overlap the first's. Nothing in
-- the schema can express that rule; it belongs in IPClass/IPPool validation.

-- +goose Down
--
-- Restores 001-era state. One thing cannot be restored: 001's schema has no
-- way to represent an allocation without a claim, so retained and reserved
-- allocations are deleted rather than migrated. That is a real loss of data
-- and the reason this file says so out loud rather than failing on a NOT NULL
-- violation halfway through.

ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_alloc_no_overlap;
ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_alloc_purpose_check;
ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_alloc_allocation_key_key;

DROP INDEX IF EXISTS idx_ipam_cidr_alloc_retained;
DROP INDEX IF EXISTS idx_ipam_cidr_alloc_class;
DROP INDEX IF EXISTS idx_ipam_cidr_alloc_pool_scope;

DELETE FROM ipam_cidr_allocations WHERE claim_key IS NULL;

ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS purpose;
ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS class_name;
ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS scope_digest;
ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS allocation_key;
ALTER TABLE ipam_cidr_allocations ALTER COLUMN claim_key SET NOT NULL;

DROP FUNCTION IF EXISTS ipam_sync_pool_class_offers(TEXT, TEXT[]);
DROP TABLE IF EXISTS ipam_pool_class_offer;
DROP TABLE IF EXISTS ipam_pool_identity;

DROP INDEX IF EXISTS idx_ipam_ipallocation_scope_digest;
DROP INDEX IF EXISTS idx_ipam_ipallocation_claim_ref_name;
DROP INDEX IF EXISTS idx_ipam_ipallocation_class_name;
DROP INDEX IF EXISTS idx_ipam_ipclaim_status_pool_ref_name;
DROP INDEX IF EXISTS idx_ipam_ipclaim_class_name;
DROP INDEX IF EXISTS idx_ipam_ippool_scope_digest;
DROP INDEX IF EXISTS idx_ipam_ippool_class_names;
DROP INDEX IF EXISTS idx_ipam_ipclass_parent_class_name;
DROP INDEX IF EXISTS idx_ipam_ipclass_ip_family;

-- Recreate the two indexes Up dropped, exactly as 001 declares them.
CREATE INDEX IF NOT EXISTS idx_ipam_ipclaim_ip_family
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' ->> 'ipFamily'))
    WHERE kind = 'IPClaim';

CREATE INDEX IF NOT EXISTS idx_ipam_ipclaim_pool_ref_name
    ON ipam_objects ((ipam_data_to_jsonb(data) -> 'spec' -> 'poolRef' ->> 'name'))
    WHERE kind = 'IPClaim';

-- btree_gist is deliberately left installed. Dropping an extension that
-- another object in the database may depend on is not a reversal of anything
-- this migration did, and an unused extension costs nothing.
