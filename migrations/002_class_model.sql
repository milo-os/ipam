-- 002: the class model.
--
-- A claim names a CLASS and carries a SCOPE. The allocator resolves which pool
-- serves it, provisioning one down a parent chain the first time a scope is
-- claimed into. This migration adds everything that model needs on top of 001.
--
-- WHAT IT ADDS
--
--   ipam_pool_identity      one pool per (class, scope digest), so a herd of
--                           simultaneous first claims produces one pool and
--                           every loser reads the winner's
--   ipam_pool_class_offer   which classes a pool offers itself to
--   allocation_key          the identity of an allocation row, NOT NULL UNIQUE,
--                           because a retained allocation has no claim
--   purpose                 Claim, Reservation or PoolCarve. Non-Claim rows are
--                           withheld from every address space, which is what a
--                           reservation and a child pool's carve have in common
--   scope_digest            the address space an allocation must be unique in
--   an EXCLUDE constraint   (pool_key, scope_digest, allocated_cidr &&), so two
--                           allocations cannot overlap within one space
--   an ordered index        (pool_key, allocated_cidr), for a bounded search
--   a search floor          the lowest address a pool's space is known free
--                           from, per (pool, address space)
--   a consumption total     addresses spoken for, per pool, so capacity is not
--                           remeasured over every allocation on each write
--
-- Everything here is additive: 001 is left byte-identical, and two allocations
-- may hold the same address exactly when their scopes differ. Down is a no-op
-- (see the note above it).
--
-- POSTGRESQL FLOOR: 13. Two independent things need it — 001 uses
-- pg_current_xact_id() / pg_snapshot_xmin(), and 002 needs btree_gist
-- installable by a non-superuser, which requires trusted extensions. Neither
-- file uses 14+ syntax. Verified on 13.23, as a non-superuser role owning its
-- database, exclusion constraint and all. Deployed clusters run 17.10.
--
-- The guard below is redundant with 001, which fails first below 13 with
-- `pg_current_xact_id() does not exist` (confirmed on 12.22). It covers a
-- database restored from a 001-era dump onto an older server.
--
-- Chain depth and cycle rejection happen in Go — internal/allocator/class.go
-- and internal/registry/ipam/ipclass/storage.go — not here. If you raise this
-- floor, name the feature that needs it.

-- +goose Up

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

-- 1. Expression indexes
--
-- A JSON expression index over a path the model stopped writing does not
-- error. It stops matching, every query that relied on it degrades to a
-- sequential scan over ipam_objects, and nothing says so. Only paths the new
-- model does not write are dropped here.
--
-- IMPORTANT: these indexes are also declared in Go, in each resource's
-- strategy.go, and `ipam migrate up` runs fieldindex.SyncIndexes immediately
-- after goose. An index dropped here whose Go declaration still exists is
-- recreated seconds later. The corresponding FieldIndex entries in
-- internal/registry/ipam/{ipclaim,ipallocation,ippool}/strategy.go must be
-- updated to match this migration.

-- A claim names a class, not a pool, so nothing reads this index. The allocator
-- resolves the pool and records it in status.poolRef.
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
-- The query needs three things: this index, a `status.scopeDigest` entry in
-- IPPool's SelectableFields, and a field-label conversion on the scheme. Drop
-- either of the other two and the API rejects the selector outright, leaving
-- this index maintained on every write and never read. All three are in place,
-- and
-- test/e2e/field-selectors issues the query against a live apiserver so it
-- cannot stop working without a test failing.
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

-- 2. Pool identity — what concurrent provisioning contends on
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
-- The foreign key is DEFERRABLE INITIALLY DEFERRED so the allocator can claim
-- the identity row *before* the pool object exists:
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
-- The one-round-trip alternative settles win-or-lose without the follow-up
-- SELECT:
--
--   ON CONFLICT (class_name, scope_digest)
--   DO UPDATE SET pool_key = ipam_pool_identity.pool_key
--   RETURNING pool_key, (xmax = 0) AS won
--
-- It is correct, and it does not scale: DO UPDATE makes every loser take a row
-- lock on the winner's tuple and write a new version, so the losers serialise
-- against each other on the single row the whole herd is contending for. DO
-- NOTHING takes no lock and writes nothing.
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
    -- The claim that it is a backstop is false twice over:
    --
    --   * pool_key mixed the calling project in (via tenant.Identity.ResourceKey)
    --     while scope_digest did not, so the derivation stopped being a function
    --     of the primary key, which is the precondition that would make the
    --     constraint reachable.
    --   * and it did not become reachable, because the constraint never runs on
    --     that path. Two tenants proposing different pool_keys for one
    --     (class_name, scope_digest) conflict on the PRIMARY KEY first; DO
    --     NOTHING suppresses the insert, so the second tenant writes no row and
    --     this index is never consulted. It sees one row, is satisfied, and the
    --     divergence continues silently — with the second tenant reading and
    --     allocating from the first tenant's pool.
    --
    -- So a pool_key that disagrees with the digest is invisible here by
    -- construction. The derivation puts the tenant inside the
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

-- 3. Class offers — class health without scanning every pool
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
-- migrations/README.md. It is not a cache: allocator.DiscoverPool
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

-- The offer table is maintained by the database, not by the registry.
--
-- A pool publishes itself to classes through spec.classNames, and that spec
-- arrives by several routes: the IPPool registry's own Create, the generic
-- store's Create and Update, and the cascade when it provisions a pool. A hook
-- on one of them is a hook missing from the others, and a pool whose offers
-- were never written is invisible to discovery while looking perfectly correct
-- in the API.
--
-- Deletes need no trigger: ipam_pool_class_offer.pool_key cascades.
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

DROP TRIGGER IF EXISTS ipam_objects_pool_class_offers ON ipam_objects;
CREATE TRIGGER ipam_objects_pool_class_offers
    AFTER INSERT OR UPDATE OF data ON ipam_objects
    FOR EACH ROW EXECUTE FUNCTION ipam_pool_class_offers_from_data();
-- +goose StatementEnd

-- 4. Allocations outlive their claims
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
-- claim-bound: 001 declares claim_key NOT NULL.
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
    ADD CONSTRAINT ipam_cidr_alloc_purpose_check
    CHECK (purpose IN ('Claim', 'Reservation', 'PoolCarve'));

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

-- 5. Overlap exclusion
--
-- This constraint is what prevents two allocations from overlapping. The Go
-- allocator prevents it too, and is not the enforcement: a bug there, or any
-- writer that does not go through it, is refused here.
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
--   scope_digest  Carries the owning tenant as well as the scope — see
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
-- participate: a held address is capacity nobody else can use.
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

--
-- Accept 'PoolCarve' as a third allocation purpose, and reclassify the rows
-- that were already carves wearing the 'Reservation' label.
--
-- THREE VALUES, THOUGH THE SEARCH DISTINGUISHES ONLY TWO
-- The allocator's free-space search must treat a gateway reservation and a
-- child pool's carve identically — both are space that is spoken for and
-- neither belongs to a claim — so its predicate is `purpose <> 'Claim'` and
-- stays that way. Both non-Claim values are scope-independent.
--
-- It is the **pool delete guard** that has to tell them apart. A pool's own
-- edge reservations are not claims against it, and counting them as such made
-- any pool with `reservations` set permanently undeletable, with an error
-- telling the operator to release claims that did not exist.
--
-- So the distinction is real but narrow: exactly one reader acts on it. That
-- matters because a third enum value the hot path ignores looks like something
-- to simplify back to two. It is not. The alternative is to carry the
-- distinction in an allocation-key naming convention, and semantics in a string
-- prefix work only until a pool key contains that character.
--
-- Do not add an index on `purpose`. The hot path still only asks "is this a
-- Claim in my scope", which idx_ipam_cidr_alloc_pool_scope already covers; the
-- delete guard runs once per pool deletion.

ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_alloc_purpose_check;
ALTER TABLE ipam_cidr_allocations
    ADD CONSTRAINT ipam_cidr_alloc_purpose_check
    CHECK (purpose IN ('Claim', 'Reservation', 'PoolCarve'));

-- Reclassify carves that already exist as 'Reservation'.
--
-- The discriminator is exact rather than a heuristic: a carve's allocation_key
-- *is* the child pool's object key, so a row whose allocation_key names an
-- ipam_objects row of kind 'IPPool' is a carve and nothing else can be. A
-- claim's allocation_key names an IPAllocation, and a genuine edge reservation
-- names no pool object at all.
--
-- This updates nothing on any database reachable from 001: every row there
-- takes the 'Claim' default, so none is a 'Reservation' naming an IPPool. It is
-- kept as a no-op guard for a database populated by a build that wrote carves
-- under the old label, because the delete guard reads this column and a pool
-- whose carve is mislabelled cannot be deleted.
UPDATE ipam_cidr_allocations a
   SET purpose = 'PoolCarve'
 WHERE a.purpose = 'Reservation'
   AND EXISTS (
         SELECT 1 FROM ipam_objects o
          WHERE o.key = a.allocation_key
            AND o.kind = 'IPPool'
       );

--
-- Record WHEN an allocation was retained, separately from when it was
-- allocated, and measure the retention lease from it.
--
-- THE INDEX MUST BE ON retained_at, NOT allocated_at
--
-- allocated_at is set when the address is handed out. A lease measured from
-- it expires from the moment of allocation, not of retention — so an address
-- allocated a year ago and retained yesterday is already a year past a 30-day
-- lease and is released on the sweeper's first pass. Retention would survive
-- until the next tick and then evaporate, worst for exactly the long-lived
-- addresses retention exists to protect, and it would present as "the sweeper
-- is too aggressive" while the actual cause is a column name.
--
-- allocated_at has no other consumer: nothing in Go reads it, and this index
-- was its only reference in the schema. So it is left exactly as it is —
-- "when was this address handed out" is a true and useful fact, it is simply
-- not the one a lease needs.

ALTER TABLE ipam_cidr_allocations ADD COLUMN IF NOT EXISTS retained_at TIMESTAMPTZ;

COMMENT ON COLUMN ipam_cidr_allocations.retained_at IS
    'When this allocation entered the retained state (claim_key cleared). NULL '
    'means not retained, or retained before the lease feature existed; either '
    'way no lease applies. Never measure a lease from allocated_at.';

-- NO BACKFILL, AND THE REASON IS THE BUG ABOVE
-- There are already-retained rows in the wild with no retained_at. The
-- tempting fills are allocated_at and NOW(). Both reproduce the defect this
-- migration exists to remove:
--
--   allocated_at  is the original bug verbatim — a lease measured from when
--                 the address was handed out.
--
--   NOW()         looks better and is the same failure displaced. A lease is
--                 off by default, so an operator may enable one months after
--                 this migration runs. Every backfilled row is then already
--                 months into its lease and is swept on the first pass — the
--                 same "instantly expired" behaviour, just with a different
--                 reference point.
--
-- The general form: ANY backfill invents a retention moment in the past, and a
-- lease measured from an invented past moment expires against time that has
-- already elapsed. NULL is the only value that does not encode a false
-- history.
--
-- NULL therefore means "no lease applies". These rows are held until something
-- releases them deliberately, which is the pre-lease behaviour and a strictly
-- safer failure than releasing an address someone is using.
--
-- The cost is honest and must be stated: those rows are permanently exempt
-- from expiry, because nothing will re-retain an allocation that is already
-- retained. That is a real capacity leak, and it is *visible* rather than
-- silent — this finds it:
--
--   SELECT allocation_key, pool_key, allocated_cidr, owner_project, allocated_at
--     FROM ipam_cidr_allocations
--    WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NULL;
--
-- An operator who wants those swept opts in explicitly, choosing the moment
-- the clock starts rather than inheriting a guess:
--
--   UPDATE ipam_cidr_allocations SET retained_at = NOW()
--    WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NULL;

-- Maintained by the database, not by convention.
--
-- retained_at has to be set exactly when claim_key is cleared. Leaving that to
-- every caller is how the carve release came to be gated on a field that was
-- never set, and how three delete paths ended up disagreeing: an invariant
-- maintained only by the writers that currently exist survives until someone
-- adds a writer. The transition is observable in the row itself, so the
-- database can maintain it.
--
-- The trigger only fills a gap it finds — an explicit retained_at written by
-- the application wins, so this constrains nothing and rescues the case where
-- a new release path forgets. It fires only on the non-NULL -> NULL transition
-- of claim_key, which is the release path and not a hot one.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ipam_set_retained_at() RETURNS trigger AS $$
BEGIN
    IF OLD.claim_key IS NOT NULL AND NEW.claim_key IS NULL AND NEW.retained_at IS NULL THEN
        NEW.retained_at := NOW();
    END IF;
    -- Re-binding a retained allocation to a claim ends its retention, so the
    -- clock must not keep running: a later release starts a fresh lease.
    IF NEW.claim_key IS NOT NULL THEN
        NEW.retained_at := NULL;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS ipam_cidr_alloc_retained_at ON ipam_cidr_allocations;
CREATE TRIGGER ipam_cidr_alloc_retained_at
    BEFORE UPDATE ON ipam_cidr_allocations
    FOR EACH ROW EXECUTE FUNCTION ipam_set_retained_at();

-- Rebuild the retained-set index on the column a sweeper actually orders by.
--
-- EVERY TERM IN THIS PREDICATE IS LOAD-BEARING. Do not simplify it to
-- `claim_key IS NULL`, which is the obvious-looking reduction and the
-- catastrophic one:
--
--   claim_key IS NULL   selects rows with no claim — which is retained
--                       allocations *and* pool reservations *and* pool carves,
--                       because none of those has a claim either.
--
--   purpose = 'Claim'   is what narrows that to retained allocations. Without
--                       it the sweeper expires gateway reservations and the
--                       carves that child pools occupy. **A gateway
--                       reservation must never expire**; releasing one hands a
--                       subnet's gateway address to the next claim, and
--                       releasing a carve frees the block a live child pool is
--                       allocating from. That is the worst outcome this
--                       feature could produce, and it is one word away.
--
--   retained_at IS NOT NULL  keeps the index to rows a lease can apply to, and
--                       makes the "no lease applies" rows genuinely absent
--                       from the scan rather than sorted to one end of it
--                       where a careless ORDER BY would pick them up first.
--
-- The sweep filters on the partial predicate first and only orders within that
-- set, so there is no reason to index retained_at on its own.
DROP INDEX IF EXISTS idx_ipam_cidr_alloc_retained;
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_retained
    ON ipam_cidr_allocations (retained_at)
    WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NOT NULL;

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
-- compares a reservation against every `uniqueWithin: []` claim in the pool,
-- whoever made it. That is the point of reserved rows participating in the
-- constraint — a held address is capacity nobody else can use. Give a
-- reservation the owner's pool digest instead and it stops participating for
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
-- The choice: a writer that
-- forgets lands in the STRICTEST space, so the failure is a spurious conflict
-- someone notices rather than a silent double-allocation nobody does. Only the
-- value moves, to scope.EmptyAddressSpaceDigest() — canonical form
-- "13:ipam.scope.v31:0".
--
-- It is the address-space value rather than the pool one because the rows that
-- can arrive without a digest are allocations, and the strictest space for an
-- allocation is the tenant-independent empty one. Under a tenant-qualified default such
-- a row would sit in the platform's pool space, which no claim from any project
-- shares — blocking nothing.
--
-- Pinned by TestEmptyDigestMatchesMigrationDefault in internal/scope.
ALTER TABLE ipam_cidr_allocations
    ALTER COLUMN scope_digest
    SET DEFAULT 'c86bbfc3761caa942844f05f5a8379f15cdd300f512a9d5b5baaa787c4695c42';

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

    -- Counted rather than mentioned: the referencing rows decide whether
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

UPDATE ipam_cidr_allocations a
   SET purpose = 'PoolCarve'
 WHERE a.purpose = 'Claim'
   AND EXISTS (
         SELECT 1 FROM ipam_objects o
          WHERE o.key = a.allocation_key
            AND o.kind = 'IPPool'
       );

-- 1. AN INDEX THAT CAN BE READ IN ADDRESS ORDER
-- The bounded search (internal/allocation.Scan) consumes allocations in
-- ascending address order and stops at the first gap that fits. It therefore
-- needs rows delivered in that order without a sort.
--
-- idx_ipam_cidr_alloc_pool_scope from 002 cannot do it. Its key is
-- (pool_key, scope_digest) and allocated_cidr is an INCLUDE column — INCLUDE
-- payload is not part of the key, so it can satisfy a fetch but never an
-- ORDER BY. A search over that index needs an explicit Sort above the scan —
-- 298 kB of quicksort at 2000 rows — and sorting is not the fix, because a
-- sorted whole-set read is still a whole-set read.
--
-- The new index keys on (pool_key, allocated_cidr), which is exactly the
-- ordering the scan walks. purpose and scope_digest ride along as INCLUDE
-- columns so the search's filter — `purpose <> 'Claim' OR scope_digest = $2` —
-- is evaluated from the index without touching the heap.
--
-- That filter is deliberately NOT in the key. It is a disjunction, so no
-- composite key can serve it as a range, and putting scope_digest ahead of
-- allocated_cidr would order rows by address WITHIN a scope while the search
-- needs one address order across the scopes it must respect. The cost is that
-- a pool shared by many address spaces has the scan step over rows belonging to
-- other spaces. That is a constant factor in the number of spaces, not a return
-- of the exponent, and for cascade-provisioned pools — one space each — it is
-- exactly zero.
--
-- The 002 index is KEPT. It still serves loadAllocationsInScope and the
-- capacity recompute, which want the whole set for one pool and do not care
-- about order. Dropping it would trade a bounded search for an unbounded
-- capacity read.
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_pool_addr
    ON ipam_cidr_allocations (pool_key, allocated_cidr)
    INCLUDE (purpose, scope_digest);

-- 2. A FLOOR, WITHOUT WHICH THE BOUNDED SEARCH IS STILL LINEAR
-- This is the half that is easy to leave out, because the search looks bounded
-- without it. It is not. A pool filled sequentially — which is what first-fit
-- does, and what every load run and every real subnet allocation produces — has
-- its first free block at the END. A scan starting at the pool's base examines
-- every allocation to reach it. The index removes the sort and the row
-- transfer; the exponent survives untouched.
--
-- The floor is the lowest address in this pool, in this address space, that the
-- service believes could still be free. The search starts there instead of at
-- the pool's base, and a sequentially filled pool then decides after one page.
--
-- THE INVARIANT IS ONE-DIRECTIONAL, AND EVERYTHING DEPENDS ON THAT
-- A floor that is too LOW is harmless: the scan starts earlier than it needed
-- to, walks addresses it will find taken, and returns the same answer more
-- slowly. A missing row means "start at the base", which is the safest value
-- there is.
--
-- A floor that is too HIGH is a correctness bug, and a silent one. The scan
-- never looks below it, so free space below the floor is invisible — no error,
-- no exhaustion, just addresses nobody will ever be handed, and a pool that
-- reports full while holding room.
--
-- So every writer must be able to justify only ever moving a floor up to a
-- point it has PROVED is fully allocated, and must move it down on any event
-- that frees an address. Releases lower it; nothing else may raise it except a
-- completed search that walked the ground it is skipping.
--
-- It is a cache, not a source of truth. Deleting every row in this table
-- changes no answer the service gives — only how long it takes to give it.
-- That is the property to preserve when changing anything here: if a change
-- makes a wrong floor produce a wrong ADDRESS rather than a slow search, it is
-- the wrong change.
--
-- A TABLE RATHER THAN A COLUMN ON THE POOL
-- The floor is per (pool, address space), not per pool. A shared pool serves
-- many spaces at once — that is the entire point of uniqueWithin — and they
-- fill independently, so one floor per pool would be pinned to the least-full
-- space and buy nothing. Storing a map inside the pool's JSON would grow
-- without bound in the same object the allocator rewrites under lock on every
-- claim.
CREATE TABLE IF NOT EXISTS ipam_pool_search_floor (
    -- ON DELETE CASCADE, because a floor for a pool that no longer exists is
    -- worse than useless: pool names are a pure function of scope, so the next
    -- pool to take this key would inherit a floor describing a different pool's
    -- occupancy — a too-high floor, which is the one direction that loses
    -- addresses.
    pool_key     TEXT NOT NULL REFERENCES ipam_objects(key) ON DELETE CASCADE,

    -- The address space, matching ipam_cidr_allocations.scope_digest. Not a
    -- foreign key anywhere: a digest is a value, not a row.
    scope_digest TEXT NOT NULL,

    -- The lowest address that could still be free. INET rather than TEXT so
    -- LEAST() and the comparisons in the release paths use address order
    -- rather than lexical order — '10.0.0.9' sorts after '10.0.0.10' as text,
    -- and a floor compared that way moves the wrong way.
    floor        INET NOT NULL,

    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (pool_key, scope_digest)
);

-- Lowering the floor on release needs every space's row for a pool when the
-- released block is one that blocked all of them (a reservation, or a carve
-- backing a child pool). The primary key's leading column already serves that
-- prefix scan, so no second index is added; this comment exists so the next
-- person does not add one after reading the release path.

-- 3. THE FLOOR IS LOWERED BY A TRIGGER, NOT BY THE RELEASE PATHS
-- Three code paths delete an allocation row today — Release's delete branch,
-- ForceRelease, and ReleasePoolReservations — and the lease sweep reaches two
-- of them. Patching each entry point is the failure docs/verification-conventions.md
-- rule 4 describes: the set of entry points is what a fix misses, not the
-- condition inside any one of them.
--
-- A trigger has no set. Every delete lowers the floor, including from paths
-- nobody has written yet, and including a DELETE typed by hand into psql during
-- an incident — which is precisely when a stale floor would otherwise be
-- installed and never noticed.
--
-- Only DELETE fires it. Retention (Release with reclaimPolicy Retain) sets
-- claim_key to NULL and leaves the row, so the address is still held and still
-- blocks: the floor must NOT move. Re-binding a retained allocation likewise
-- frees nothing. Those are the two cases that look like a release and are not.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ipam_lower_search_floor() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.purpose = 'Claim' THEN
        -- A claim's block belonged to ONE address space and blocked only that
        -- one, so only that space's floor may move.
        UPDATE ipam_pool_search_floor
           SET floor = LEAST(floor, host(OLD.allocated_cidr)::inet),
               updated_at = NOW()
         WHERE pool_key = OLD.pool_key
           AND scope_digest = OLD.scope_digest;
    ELSE
        -- A reservation or a carve was withheld from EVERY address space in the
        -- pool — that is what `purpose <> 'Claim'` means to the search — so
        -- releasing one returns space to all of them. Lowering only its own
        -- digest's floor would leave every other space unable to see the
        -- freed block, which is the silent lost-address failure.
        UPDATE ipam_pool_search_floor
           SET floor = LEAST(floor, host(OLD.allocated_cidr)::inet),
               updated_at = NOW()
         WHERE pool_key = OLD.pool_key;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS ipam_cidr_alloc_lower_floor ON ipam_cidr_allocations;
CREATE TRIGGER ipam_cidr_alloc_lower_floor
    AFTER DELETE ON ipam_cidr_allocations
    FOR EACH ROW EXECUTE FUNCTION ipam_lower_search_floor();

-- 3. A RUNNING CONSUMPTION TOTAL, SO CAPACITY STOPS WALKING THE POOL
-- Pool capacity would otherwise be answered by measuring every allocation, on
-- the write path, inside the allocation transaction: handing out one address
-- would cost more the more the pool already held.
--
-- Maintaining a total needs only the allocations that OVERLAP the block being
-- added or removed, because every other allocation contributes the same amount
-- before and after and cancels. That set comes from the GiST index the EXCLUDE
-- constraint above already maintains.
--
-- The total counts each ADDRESS once, not each allocation. Allocations may
-- legitimately overlap across address spaces, so summing allocation sizes
-- reports a /28 shared by eight networks as eight times its occupancy.
--
-- ONE ROW PER POOL, unlike the search floor above. The floor is per address
-- space because spaces fill independently. Consumption is not: it answers how
-- much of the pool's address range is spoken for, and an address held in two
-- spaces is one address.
--
-- No backfill. A pool with no row here has its total computed once from the
-- full set on its next write, and maintained incrementally from then on.
-- Computing it here would mean a second implementation of the free-region
-- arithmetic, in PL/pgSQL, free to disagree with the Go one that maintains it.
CREATE TABLE IF NOT EXISTS ipam_pool_consumption (
    -- ON DELETE CASCADE, for the same reason as the floor: pool names are a
    -- function of scope, so the next pool to take this key would inherit the
    -- total. Here the inherited value is too HIGH, and the pool would report
    -- space it has as consumed.
    pool_key TEXT PRIMARY KEY REFERENCES ipam_objects(key) ON DELETE CASCADE,

    -- NUMERIC because a /20 of IPv6 holds 2^108 addresses, past every integer
    -- type.
    consumed NUMERIC NOT NULL CHECK (consumed >= 0),

    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);


-- 4. HOW THE FLOOR IS RAISED, AND WHY IT IS A COMPARE-AND-SET
-- The allocator raises the floor after a search, to the lowest free address
-- that search actually walked to. The write is a compare-and-set against the
-- value the search STARTED from (see raiseSearchFloor in
-- internal/allocator/prefix.go), and that is not defensive tidiness.
--
-- Without it: a release commits while a search is in flight, the trigger above
-- lowers the floor to the freed address, and the search then overwrites it with
-- a higher value justified only by ground it walked BEFORE the release. The
-- freed address is below the new floor, so no later search looks there, and it
-- is gone — no error, no exhaustion, just an address that stops existing.
--
-- With the CAS the losing writer is the one that would have raised it: the
-- floor no longer equals what the search observed, the update matches no row,
-- and the lower value survives. That costs a slower scan next time and loses
-- nothing, which is the direction this whole mechanism is built to fail in.
--
-- DO NOT READ THE CAS AS AVOIDING A LOCK. An UPDATE takes a row lock for the
-- rest of the transaction whether or not its WHERE clause is a compare-and-set.
-- The CAS solves the LOST-UPDATE problem and does nothing about lock ordering.
--
-- Lock ordering is handled where it has to be, in the code. AllocatePrefix
-- takes the pool row lock first and touches the floor second. Every path that
-- DELETES an allocation — and so fires the trigger above — takes the pool
-- row lock before deleting, so it acquires the same two locks in the same
-- order. Release does this for every pool it will touch, sorted, because a
-- claim can span pools and two releases could otherwise take the same pair in
-- opposite orders.
--
-- Get that wrong and the symptom is not slowness: Postgres detects the cycle
-- and kills one transaction with SQLSTATE 40P01, which reaches the caller as a
-- 500 on the service's core operation, intermittently and under exactly the
-- concurrency the design targets.

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

-- Down is deliberately a no-op rather than a reversal.
--
-- Reversing would drop scope_digest, purpose and allocation_key, and with them
-- the only record of which address space each allocation belongs to. A retained
-- allocation has no claim to rebuild that from, so the drop is not recoverable
-- by re-applying Up. The 001-era schema is also unable to represent two
-- allocations holding the same address, which this schema permits, so a
-- reversal would have to choose one of them to delete.
--
-- Roll back by restoring a dump taken before this migration.

-- +goose Down

-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
