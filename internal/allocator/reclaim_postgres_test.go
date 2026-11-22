package allocator

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// inTx runs fn in a transaction and commits it, failing the test on any error.
func inTx(t *testing.T, ctx context.Context, begin func(context.Context) (pgx.Tx, error), fn func(pgx.Tx) error) {
	t.Helper()
	tx, err := begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// A reclaim binds only a row that is unbound, records the replacement's policy,
// and ends the retention the trigger stamped.
func TestReclaimAllocationBindsOnlyAnUnboundRow(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	a := NewPostgresPrefixAllocator()
	poolKey := seedPool(t, db, "reclaim-cas", "10.56.0.0/16")
	cidr := allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)

	// Bound: refused, and the binding is untouched.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	err = a.ReclaimAllocation(ctx, tx, "alloc-a", "claim-b", ipamv1alpha1.ReclaimDelete)
	_ = tx.Rollback(ctx)
	var bound *AllocationBoundError
	if !errors.As(err, &bound) || bound.ClaimKey != "claim-a" {
		t.Fatalf("reclaiming a bound row returned %v, want an AllocationBoundError naming claim-a", err)
	}

	releaseReturningRetained(t, db, "claim-a")

	inTx(t, ctx, db.Begin, func(tx pgx.Tx) error {
		row, err := a.LockClaimAllocation(ctx, tx, "alloc-a")
		if err != nil {
			return err
		}
		if row == nil || row.ClaimKey != "" || row.CIDR != cidr || row.PoolKey != poolKey ||
			row.ReclaimPolicy != ipamv1alpha1.ReclaimRetain || row.ClassName != "standard" {
			t.Errorf("LockClaimAllocation = %+v, want the unbound Retain row holding %s", row, cidr)
		}
		return a.ReclaimAllocation(ctx, tx, "alloc-a", "claim-a2", ipamv1alpha1.ReclaimDelete)
	})

	var (
		claimKey, policy string
		retainedAt       *string
	)
	if err := db.QueryRow(ctx,
		`SELECT claim_key, reclaim_policy, retained_at::text FROM ipam_cidr_allocations WHERE allocation_key = 'alloc-a'`,
	).Scan(&claimKey, &policy, &retainedAt); err != nil {
		t.Fatalf("read reclaimed row: %v", err)
	}
	if claimKey != "claim-a2" || policy != "Delete" || retainedAt != nil {
		t.Errorf("reclaimed row = (%s, %s, retained_at %v), want (claim-a2, Delete, NULL)", claimKey, policy, retainedAt)
	}
}

// No row under the key, and a row that is not a claim's, are both nothing to
// reclaim.
func TestLockClaimAllocationFindsOnlyClaimRows(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	a := NewPostgresPrefixAllocator()
	poolKey := seedPool(t, db, "reclaim-lookup", "10.57.0.0/16")
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose)
		 VALUES ($1, '10.57.0.0/32', NULL, 'reservation-0', 'IPv4', 'Reservation')`, poolKey); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}

	inTx(t, ctx, db.Begin, func(tx pgx.Tx) error {
		for _, key := range []string{"alloc-missing", "reservation-0"} {
			row, err := a.LockClaimAllocation(ctx, tx, key)
			if err != nil {
				return err
			}
			if row != nil {
				t.Errorf("LockClaimAllocation(%q) = %+v, want nil", key, row)
			}
		}
		return nil
	})
}
