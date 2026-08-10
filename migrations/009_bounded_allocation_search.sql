-- 009: make the allocation search bounded — an ordered index, and a floor.
--
-- THE PROBLEM THIS SOLVES
-- ----------------------
-- Every allocation loaded every allocation already in the pool. Measured on a
-- fresh schema with sequential /48s (internal/allocator/allocation_scaling_postgres_test.go):
--
--     occupancy   ms/alloc   KiB/alloc   tuples/alloc
--           500      3.938      1226.8          550.5
--          4000     10.563     10105.7         4050.5
--
-- tuples/alloc is occupancy + 50.5 at every checkpoint. That is the database's
-- own statement that each call reads the whole set — linear per allocation,
-- quadratic over a pool's life, and it gets worse on its own as pools fill.
--
-- Two things are needed to stop that, and neither works without the other.
--
--
-- +goose Up

-- 1. AN INDEX THAT CAN BE READ IN ADDRESS ORDER
-- ---------------------------------------------
-- The bounded search (internal/allocation.Scan) consumes allocations in
-- ascending address order and stops at the first gap that fits. It therefore
-- needs rows delivered in that order without a sort.
--
-- idx_ipam_cidr_alloc_pool_scope from 002 cannot do it. Its key is
-- (pool_key, scope_digest) and allocated_cidr is an INCLUDE column — INCLUDE
-- payload is not part of the key, so it can satisfy a fetch but never an
-- ORDER BY. That is why the old query had an explicit Sort node above the scan
-- (298 kB of quicksort at 2000 rows) and why sorting was never the fix: a
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
-- ------------------------------------------------------------
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
-- ----------------------------------------------------------------
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
-- WHY A TABLE RATHER THAN A COLUMN ON THE POOL
-- ---------------------------------------------
-- The floor is per (pool, address space), not per pool. A shared pool serves
-- many spaces at once — that is the entire point of uniqueWithin — and they
-- fill independently, so one floor per pool would be pinned to the least-full
-- space and buy nothing. Storing a map inside the pool's JSON would grow
-- without bound in the same object the allocator rewrites under lock on every
-- claim.
CREATE TABLE IF NOT EXISTS ipam_pool_search_floor (
    -- ON DELETE CASCADE, because a floor for a pool that no longer exists is
    -- not merely useless: pool names are a pure function of scope, so the next
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
-- ---------------------------------------------------------------
-- Three code paths delete an allocation row today — Release's delete branch,
-- ForceRelease, and ReleasePoolReservations — and the lease sweep reaches two
-- of them. Patching each is how docs/verification-conventions.md rule 4
-- happened three times in this service: IPPool's Delete was fixed and
-- DeleteCollection was not, then IPAllocation had neither. The miss was never
-- the condition, it was the SET of entry points.
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


-- 4. HOW THE FLOOR IS RAISED, AND WHY IT IS A COMPARE-AND-SET
-- ------------------------------------------------------------
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
-- DO NOT READ THE CAS AS AVOIDING A LOCK. An earlier version of this comment
-- said "the CAS needs no lock at all", which is false and was the most
-- dangerous sentence in this file: an UPDATE takes a row lock for the rest of
-- the transaction whether or not its WHERE clause is a compare-and-set. The CAS
-- solves the LOST-UPDATE problem. It does nothing about lock ordering.
--
-- Lock ordering is handled where it has to be, in the code. AllocatePrefix
-- takes the pool row lock first and touches the floor second. Every path that
-- DELETES an allocation — and so fires the trigger above — now takes the pool
-- row lock before deleting, so it acquires the same two locks in the same
-- order. Release does this for every pool it will touch, sorted, because a
-- claim can span pools and two releases could otherwise take the same pair in
-- opposite orders.
--
-- Get that wrong and the symptom is not slowness: Postgres detects the cycle
-- and kills one transaction with SQLSTATE 40P01, which reaches the caller as a
-- 500 on the service's core operation, intermittently and under exactly the
-- concurrency the design targets.

-- +goose Down

-- Deliberately a no-op.
--
-- The mechanical inverse would DROP both objects, and both are pure cache: the
-- index only makes an existing query faster, and the floor table only makes an
-- existing search start later. Rolling back past this point leaves code that
-- does not read either, so dropping them buys nothing and a DROP TABLE in a
-- Down section is exactly the shape that once created the whole schema, ran the
-- Down, dropped every table and exited 0 (migrations/README.md).
--
-- If an operator genuinely wants the space back, DROP INDEX
-- idx_ipam_cidr_alloc_pool_addr and DROP TABLE ipam_pool_search_floor are safe
-- to run by hand at any version, which is the honest way to offer a destructive
-- operation: on request, not on rollback.
