-- +goose Up
--
-- Record WHEN an allocation was retained, separately from when it was
-- allocated, and measure the retention lease from it.
--
-- THE BUG THIS FIXES
-- ------------------
-- 002 built the retained-set index on allocated_at, the only timestamp the
-- table had:
--
--   CREATE INDEX idx_ipam_cidr_alloc_retained
--       ON ipam_cidr_allocations (allocated_at)
--       WHERE claim_key IS NULL AND purpose = 'Claim';
--
-- allocated_at is set when the address was handed out. A lease measured from
-- it expires from the moment of *allocation*, not of retention — so an address
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
-- --------------------------------------------
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

-- +goose Down
--
-- Restores the 003-era state. retained_at is dropped, so retention timestamps
-- are lost — there is nowhere to put them, and the index goes back to
-- allocated_at, which is the bug. Down is for rolling back a deployment, not
-- for running on a cluster with a lease configured.

DROP TRIGGER IF EXISTS ipam_cidr_alloc_retained_at ON ipam_cidr_allocations;
DROP FUNCTION IF EXISTS ipam_set_retained_at();

DROP INDEX IF EXISTS idx_ipam_cidr_alloc_retained;

ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS retained_at;

CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_retained
    ON ipam_cidr_allocations (allocated_at)
    WHERE claim_key IS NULL AND purpose = 'Claim';
