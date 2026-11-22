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

-- Both indexes are built in migrate's transaction, which holds a SHARE lock
-- on ipam_cidr_allocations until it commits, so allocations and releases wait
-- for the build. Each is partial over retained claim rows, but building it
-- still reads the whole table: seconds per million rows. goose can run a
-- migration outside a transaction (its NO TRANSACTION annotation) for CREATE
-- INDEX CONCURRENTLY; that is the change to make if the table outgrows a
-- maintenance window, not a requirement at today's sizes.
--
-- The due set. Every term of the predicate is the sweeper's own, so the index
-- holds exactly the rows a lease applies to: purpose = 'Claim' keeps out
-- reservations and pool carves, which have no claim either and must never
-- expire, and expires_at IS NOT NULL keeps out what is not yet evaluated.
-- 'infinity' rows are in it and sort last, so a range scan up to now() never
-- reaches them however many there are. allocation_key breaks ties, so the
-- sweeper can page through the set by key without missing or repeating rows.
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_expiry
    ON ipam_cidr_allocations (expires_at, allocation_key)
    WHERE claim_key IS NULL AND purpose = 'Claim' AND expires_at IS NOT NULL;

-- The rows still to evaluate. Empty in steady state, so the sweeper's
-- evaluation reads nothing rather than walking the whole retained set.
CREATE INDEX IF NOT EXISTS idx_ipam_cidr_alloc_unevaluated
    ON ipam_cidr_allocations (retained_at, allocation_key)
    WHERE claim_key IS NULL AND purpose = 'Claim' AND retained_at IS NOT NULL AND expires_at IS NULL;

-- A deadline is stored rounded up to the whole second. The API reports times
-- to the second, so status.expiresAt is then exactly the instant the sweeper
-- acts on, and never earlier than retention plus the lease.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ipam_ceil_second(t TIMESTAMPTZ) RETURNS TIMESTAMPTZ AS $$
    SELECT CASE WHEN date_trunc('second', t) = t THEN t
                ELSE date_trunc('second', t) + interval '1 second' END;
$$ LANGUAGE sql STABLE;
-- +goose StatementEnd

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

-- The deadlines an observing sweeper has counted. In observe mode nothing is
-- released, so a due row stays due; counting the due set on every pass would
-- count one row once a minute for as long as it is held. A deadline is counted
-- once, in the transaction that records it here, so the count and the record
-- cannot disagree, and a deadline that falls due by any route (a retention
-- committing late, an evaluation, a tightened cap) is counted the first time a
-- pass sees it due. The deadline is part of the key: a row retained again
-- under a new deadline is a new would-be release. Entries for rows that are no
-- longer retained under that deadline are pruned by the sweeper.
CREATE TABLE IF NOT EXISTS ipam_retention_observed (
    allocation_key TEXT NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    observed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (allocation_key, expires_at)
);

COMMENT ON TABLE ipam_retention_observed IS
    'Deadlines the retention sweeper has counted as would-be releases in observe mode.';

-- +goose Down

-- Locks are taken in the order the running service takes them, allocations
-- before the sweeper's ledger, so a Down run against a live apiserver waits
-- for in-flight sweeps and claims rather than deadlocking with them.
LOCK TABLE ipam_cidr_allocations IN ACCESS EXCLUSIVE MODE;

DROP TABLE IF EXISTS ipam_retention_observed;

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

-- Dropping expires_at forgets every fixed deadline. Up again leaves them NULL,
-- and the sweeper re-evaluates them against the classes and pools as they are
-- then, so a lease whose class or pool was loosened in between comes back
-- longer than it was.
DROP INDEX IF EXISTS idx_ipam_cidr_alloc_unevaluated;
DROP INDEX IF EXISTS idx_ipam_cidr_alloc_expiry;
ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS expires_at;
DROP FUNCTION IF EXISTS ipam_ceil_second(TIMESTAMPTZ);
