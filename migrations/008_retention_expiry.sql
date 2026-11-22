-- 008: store when a retained allocation's lease runs out.
--
-- WHY A STORED DEADLINE
--
-- A retained allocation's lease is the shorter of its class's retentionLease
-- and its pool's maxRetentionLease, fixed when the claim is deleted. Storing
-- the deadline it gives, rather than deriving it on every sweep, makes the due
-- set one range scan over an index, whatever the size of the retained set and
-- however many retained rows have no lease at all. It also means the deadline
-- is the same fact the IPAllocation shows as status.expiresAt, so the warning
-- an owner reads and the instant the sweeper acts on cannot disagree.
--
-- THREE STATES, ONE COLUMN
--
--   NULL        not yet evaluated. Retention writes the deadline in the
--               transaction that unbinds the claim; it is NULL only when that
--               evaluation failed (the class could not be read), and the
--               sweeper evaluates it on its next pass.
--   'infinity'  evaluated: neither the class nor the pool sets a lease, so the
--               allocation is held until it is released or reclaimed.
--   finite      due for release at or after this instant.
--
-- A row whose retained_at is NULL is never evaluated and never expires: it was
-- retained before retention was timestamped (see 002), and any deadline given
-- it would be measured from an invented moment.
--
-- NO BACKFILL
--
-- Rows retained before this migration keep expires_at NULL and are evaluated
-- by the sweeper, against the class and pool as they are then. That is the
-- only source of a lease those rows have; the values in force when they were
-- retained were never recorded.

-- +goose Up

ALTER TABLE ipam_cidr_allocations ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

COMMENT ON COLUMN ipam_cidr_allocations.expires_at IS
    'When a retained allocation''s lease runs out. NULL: not yet evaluated; '
    '''infinity'': no lease; otherwise due for release at or after this instant. '
    'Cleared whenever the row is bound to a claim.';

-- The due set. Every term of the predicate is the sweeper's own, so the index
-- holds exactly the rows a lease applies to: purpose = 'Claim' keeps out
-- reservations and pool carves, which have no claim either and must never
-- expire, and expires_at IS NOT NULL keeps out what is not yet evaluated.
-- 'infinity' rows are in it and sort last, so a range scan up to now() never
-- reaches them however many there are.
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_expiry
    ON ipam_cidr_allocations (expires_at)
    WHERE claim_key IS NULL AND purpose = 'Claim' AND expires_at IS NOT NULL;

-- The rows still to evaluate. Empty in steady state, so the sweeper's
-- reconcile reads nothing rather than walking the whole retained set.
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_unevaluated
    ON ipam_cidr_allocations (retained_at)
    WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NOT NULL AND expires_at IS NULL;

-- Rebinding ends retention, and with it the lease: a later retention starts a
-- fresh one. This covers a reclaim by a recreated claim and any other path
-- that sets claim_key, so no writer has to remember it.
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
        NEW.expires_at := NULL;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- How far the observing sweeper has counted. In observe mode nothing is
-- released, so a due row stays due; counting the due set on every pass would
-- count one row once a minute for as long as it is held. The sweeper counts a
-- deadline once, as it passes, and records here the last one it counted. One
-- row, written only by the replica holding the sweep's advisory lock.
CREATE TABLE IF NOT EXISTS ipam_retention_sweep_state (
    id                    SMALLINT PRIMARY KEY CHECK (id = 1),
    mode                  TEXT NOT NULL,
    counted_through       TIMESTAMPTZ NOT NULL,
    counted_through_key   TEXT NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE ipam_retention_sweep_state IS
    'Single row: the retention sweeper''s mode and the last (expires_at, '
    'allocation_key) it counted as a would-be release in observe mode.';

-- +goose Down

DROP TABLE IF EXISTS ipam_retention_sweep_state;

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

DROP INDEX IF EXISTS idx_ipam_cidr_alloc_unevaluated;
DROP INDEX IF EXISTS idx_ipam_cidr_alloc_expiry;
ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS expires_at;
