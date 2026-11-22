package allocator

import (
	"context"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
)

// Deleting a child pool frees its reservations and then its place in its
// parent. Call by call that locks the child before the parent, which is the
// reverse of the order a scope-range release takes, ancestor first. With the
// child's lock held elsewhere, the child-pool release queues on it first and
// the scope-range release, behind it, takes the parent: when the child's lock
// is let go, the old order had each holding what the other wanted, and
// Postgres killed one with 40P01. ReleaseChildPool locks the parent first, so
// the scope-range release queues behind it instead.
func TestReleasingAChildPoolRacingAScopeRangeReleaseDoesNotDeadlock(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(8))
	ctx := context.Background()
	a := NewPostgresPrefixAllocator()

	// Q is the source, R a range carved from it and held by a claim, P a pool
	// carved from R that holds a reservation of its own.
	q := seedPool(t, db, "lineage-q", "10.62.0.0/16")
	keyR := tenant.Identity{Name: testProject}.ResourceKey("ippools", "lineage-r")
	keyP := tenant.Identity{Name: testProject}.ResourceKey("ippools", "lineage-p")
	carve := func(src, child string, plen int) *net.IPNet {
		t.Helper()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		block, err := carveFromPool(ctx, tx, src, plen, poolCarve{
			AllocationKey: child, ClassName: "standard", IPFamily: "IPv4", OwnerProject: testProject,
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("carve %s from %s: %v", child, src, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit carve: %v", err)
		}
		return block
	}
	blockR := carve(q, keyR, 20)
	seedPool(t, db, "lineage-r", blockR.String())
	blockP := carve(keyR, keyP, 24)
	seedPool(t, db, "lineage-p", blockP.String())

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if ok, err := insertReservation(ctx, tx, keyP, net.IPNet{IP: blockP.IP, Mask: net.CIDRMask(32, 32)}, "IPv4", "standard"); err != nil || !ok {
		t.Fatalf("reserve in P: %v %v", ok, err)
	}
	if _, err := BindScopeRange(ctx, tx, keyR, "range-claim"); err != nil {
		t.Fatalf("bind R: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	// Hold P so both releases have to queue.
	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := lockPoolRow(ctx, holder, keyP); err != nil {
		t.Fatalf("hold P: %v", err)
	}

	run := func(f func(pgx.Tx) error) <-chan error {
		done := make(chan error, 1)
		go func() {
			tx, err := db.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			if err := f(tx); err != nil {
				_ = tx.Rollback(ctx)
				done <- err
				return
			}
			done <- tx.Commit(ctx)
		}()
		return done
	}

	childDone := run(func(tx pgx.Tx) error { return ReleaseChildPool(ctx, tx, a, keyP) })
	testdb.WaitForLockWaiters(t, db, 1)
	rangeDone := run(func(tx pgx.Tx) error { return ReleaseScopeRange(ctx, tx, a, keyR) })
	testdb.WaitForLockWaiters(t, db, 2)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release P: %v", err)
	}
	if err := <-childDone; err != nil {
		t.Errorf("child-pool release: %v", err)
	}
	if err := <-rangeDone; err != nil {
		t.Errorf("scope-range release: %v", err)
	}
}
