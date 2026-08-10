-- +goose Up
--
-- Accept 'PoolCarve' as a third allocation purpose, and reclassify the rows
-- that were already carves wearing the 'Reservation' label.
--
-- WHY THIS IS 003 AND NOT AN EDIT TO 002
-- --------------------------------------
-- 002 has only ever run on disposable clusters, so editing it is tempting and
-- was the first suggestion. It does not work, for a reason that has nothing to
-- do with how disposable the data is: **goose records applied versions**. A
-- database already at version 2 will never re-run 002, so widening the CHECK
-- there is a no-op everywhere it is already applied. The file would say one
-- thing and every existing database would do another, and `migrate up` would
-- report nothing to do while every write of a PoolCarve row failed.
--
-- That is the same silent divergence between a stated schema and the real one
-- that this migration series has been cleaning up, and it is why 001 was kept
-- byte-identical. A migration that has been applied anywhere is immutable; the
-- fix for an applied migration is always another migration.
--
-- Concretely: there is a live cluster at version 2 right now. This file
-- unblocks it without anyone having to recreate a database first.
--
-- WHY THREE VALUES WHEN THE SEARCH ONLY DISTINGUISHES TWO
-- ------------------------------------------------------
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
-- is worth stating because a third enum value the hot path ignores looks like
-- something to simplify back to two. It is not. Before this value existed the
-- two were told apart by an allocation-key naming convention
-- (`<poolKey>#reservation/<n>`) — semantics carried in a string prefix, which
-- works only until a pool key contains a '#'.
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
-- On a fresh database this updates nothing. It exists so 003 is honestly
-- re-runnable against a populated one rather than only against a database
-- someone remembered to recreate — the delete guard reads this column, so a
-- database that skipped the backfill would keep the undeletable-pool bug with
-- the constraint looking correct.
UPDATE ipam_cidr_allocations a
   SET purpose = 'PoolCarve'
 WHERE a.purpose = 'Reservation'
   AND EXISTS (
         SELECT 1 FROM ipam_objects o
          WHERE o.key = a.allocation_key
            AND o.kind = 'IPPool'
       );

-- +goose Down
--
-- Restores the two-value constraint. Carves fold back to 'Reservation', which
-- is exactly how they were represented before this migration, so no row is
-- deleted — unlike 002's Down, which must drop retained allocations because
-- 001's schema cannot represent them at all.
--
-- The order matters: the rows must stop saying 'PoolCarve' before a constraint
-- that forbids it is added, or the ADD CONSTRAINT fails on its own data.
--
-- Down/up is value-preserving for every carve whose child pool object still
-- exists, but the property comes from the backfill rather than from Down:
-- re-applying Up re-derives 'PoolCarve' by looking for that object. A carve
-- whose IPPool object is gone folds to 'Reservation' and stays there, so the
-- round trip is not a pure inverse.
--
-- READ THIS BEFORE RELYING ON THAT PREMISE. An earlier version of this comment
-- said an orphaned carve "cannot happen to real data" because a carve *is* a
-- child pool's block. That is a claim about the API paths, not about the
-- schema, and **nothing in the schema enforces it**: the foreign key on
-- pool_key protects a pool against losing the allocations held *in* it, while
-- allocation_key — which is what names the child pool — carries no constraint
-- at all.
--
-- The premise held only because every path that deletes a pool released its
-- carve first, and one path did not. AllocatingIPPoolREST overrode Delete but
-- not DeleteCollection, so `kubectl delete ippool --all` dispatched to the
-- embedded Store and skipped the override entirely: no blocking count, no
-- carve release, reservations left behind. Orphaned carves were reachable
-- through a supported kubectl invocation. Fixed by routing DeleteCollection
-- through Delete.
--
-- So the durable statement is: this backfill assumes every carve's child pool
-- object still exists, that invariant is maintained entirely by the registry's
-- delete paths, and a new delete path that skips them breaks the backfill
-- silently. TestNoCarveOutlivesTheChildPoolItNames asserts the invariant
-- directly and is what catches the next one.
--
-- Deliberately NOT enforced with a foreign key, and the reason is about scope
-- rather than about avoiding coupling.
--
-- Coupling between the generic Store and this table already exists: 001's
-- pool_key FK is ON DELETE RESTRICT, so the Store is already blocked from
-- deleting a pool object while allocations are held in it. That is deliberate
-- and well-scoped — it protects exactly the invariant it is about.
--
-- An allocation_key FK would not be. There is no partial foreign key in
-- Postgres, so a constraint on that column applies to claims and reservations
-- as well as carves, and the generic Store deletes ipam_objects rows
-- (internal/storage/postgres/store.go) in its own transaction with no
-- knowledge of this table. It would therefore block ordinary object deletion
-- whenever any allocation row still referenced it — extending the existing
-- coupling to two purposes that do not need it, on the most generic path in
-- the service, to protect an invariant that matters for one. DEFERRABLE does
-- not help: it buys ordering freedom within a transaction, and these are
-- separate transactions.
--
-- The test is the better enforcement: exactly scoped to the invariant, and it
-- fails loudly rather than by blocking unrelated deletes.

UPDATE ipam_cidr_allocations SET purpose = 'Reservation' WHERE purpose = 'PoolCarve';

ALTER TABLE ipam_cidr_allocations DROP CONSTRAINT IF EXISTS ipam_cidr_alloc_purpose_check;
ALTER TABLE ipam_cidr_allocations
    ADD CONSTRAINT ipam_cidr_alloc_purpose_check
    CHECK (purpose IN ('Claim', 'Reservation'));
