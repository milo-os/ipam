# Runbook: IPAMRetentionRowsStuck

**Alert:** `IPAMRetentionRowsStuck`
**Severity:** warning
**SLO:** none (capacity hygiene)

## What this alert means

The retention sweeper keeps completing passes, but for 30 minutes it has also
kept counting retained allocations it could not act on. Each one is undone
on its own and retried on every pass, so the rest of the retained set is
swept normally. These allocations, though, stay as they are:

- `ipam_retention_sweep_row_errors_total`: a release, a lease write or a cap
  that failed for one allocation, usually because its IPAllocation object
  cannot be written or deleted. The row keeps its old deadline, or stays
  unevaluated, so its status never disagrees with it and it is never
  released early.
- `ipam_retention_lease_unevaluable_total`: a lease whose class cannot be
  read. The row stays unevaluated and held.
- `ipam_retention_cap_apply_failures_total`: an IPPool update whose
  `maxRetentionLease` could not be applied straight away. The sweeper applies
  it on its next pass, so this counter only keeps rising when the sweeper
  cannot apply it either.

`IPAMRetentionSweeperStalled` does not fire for any of this, because the
passes complete.

## Diagnose

1. **Which counter is rising?** Use the provider dashboard's Retention row
   ("Sweeper: pass p95, failures, skipped pools"), or query each counter's
   `increase(...[30m])`.
2. **Which allocations?** The apiserver logs name them:
   - `Could not release a retained allocation whose lease ran out`
   - `Could not fix a retained allocation's lease and publish it`
   - `Could not apply a pool's cap to a retained allocation and publish it`
   - `Could not evaluate a retained allocation's lease` (an unreadable class)
3. **Is the IPAllocation object readable?**
   ```sh
   kubectl get ipallocation -n <namespace> <name> -o yaml
   ```
   A decode error here is the cause for row errors. For unreadable classes,
   find the rows and their class:
   ```sql
   SELECT allocation_key, pool_key, class_name, class_project
     FROM ipam_cidr_allocations
    WHERE claim_key IS NULL AND purpose = 'Claim'
      AND retained_at IS NOT NULL AND expires_at IS NULL;
   ```

## Mitigate

- Repair or delete the class that cannot be read. The next pass evaluates its
  rows.
- For an IPAllocation that cannot be written, release it if it is not
  needed: `datumctl ipam allocation release <name>`. Otherwise restore the
  object from the row; this needs an engineer. Until then the address is
  held, which is the safe failure.
