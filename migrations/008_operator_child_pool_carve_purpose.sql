-- 008: reclassify operator-created child pools' carves, which 003 missed.
--
-- WHAT 003 FIXED AND WHAT IT DID NOT
-- ----------------------------------
-- 003 introduced the 'PoolCarve' purpose and backfilled the carves that were
-- wearing the 'Reservation' label. There were carves wearing a second wrong
-- label and nobody had found them yet: the ones written by the IPPool
-- registry's child-pool path.
--
-- Two routes create a child pool. The cascade's provisionPool called
-- carveFromPool, which recorded 'PoolCarve' — those are the rows 003 was
-- written against. AllocatingIPPoolREST.Create, reached when an operator sets
-- spec.parentPoolRef, called AllocatePrefix instead, which hardcodes 'Claim'.
-- Same operation, same table, two labels, and 003's backfill only looked at
-- one of the wrong ones.
--
-- WHY THE LABEL MATTERED
-- ----------------------
-- The allocator's search asks `purpose <> 'Claim' OR scope_digest = $2`. A
-- carve is space that has really left the pool, so it must be withheld from
-- every address space carved from that pool — which is what `purpose <>
-- 'Claim'` says. Labelled 'Claim', it was instead compared by digest, so it
-- only withheld its block from claims that happened to share its digest and
-- the parent could hand out addresses inside a live child pool's range.
--
-- The Go fix is in internal/registry/ipam/ippool/storage.go, which now calls
-- PrefixAllocator.CarveChildPool for this — the same entry point the cascade
-- uses, which additionally searches the pool's whole allocation set rather
-- than one address space's. This migration is for the rows already written.
--
-- THE DISCRIMINATOR IS 003's, VERBATIM AND FOR THE SAME REASON
-- -----------------------------------------------------------
-- A carve's allocation_key *is* the child pool's object key, so a row whose
-- allocation_key names an ipam_objects row of kind 'IPPool' is a carve and
-- nothing else can be. A real claim's allocation_key names an IPAllocation.
-- Object keys carry their resource in the key, so the two cannot collide.
--
-- 003's backfill assumes every carve's child pool object still exists, and
-- that assumption is unenforced — see 003's Down section and
-- TestNoCarveOutlivesTheChildPoolItNames. This inherits it. A carve orphaned
-- by a delete path that skipped the release is invisible to both migrations
-- and stays mislabelled.
--
-- scope_digest is deliberately left alone. It no longer decides anything for
-- these rows once purpose is right, and the value the Go code now writes — the
-- universal address space digest — is computed in Go and cannot be derived in
-- SQL, so writing a literal here would be a copy that drifts.

-- +goose Up

UPDATE ipam_cidr_allocations a
   SET purpose = 'PoolCarve'
 WHERE a.purpose = 'Claim'
   AND EXISTS (
         SELECT 1 FROM ipam_objects o
          WHERE o.key = a.allocation_key
            AND o.kind = 'IPPool'
       );

-- +goose Down

-- Deliberately a no-op, rather than the mechanical inverse.
--
-- The inverse is `SET purpose = 'Claim'` on the same rows, and it would put
-- back the defect this migration exists to remove: a child pool's block
-- withheld from one address space instead of all of them. Nothing about the
-- schema requires it either — 003 widened the CHECK to accept 'PoolCarve', and
-- 003 is the migration a rollback past this point would have to undo, so the
-- value stays legal at every version this can roll back to.
--
-- So down/up is not a pure inverse here, in the same way and for the same
-- reason 003's is not: re-applying Up re-derives the correct purpose from the
-- child pool object, and there is no state a rollback needs to restore.
SELECT 1;
