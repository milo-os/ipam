package allocator

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/migrations"
)

// setDeadline fixes a retained row's deadline relative to its retention; a
// nil offset means no lease.
func setDeadline(t *testing.T, db *pgxpool.Pool, allocationKey string, after *time.Duration) {
	t.Helper()
	var err error
	if after == nil {
		_, err = db.Exec(context.Background(),
			`UPDATE ipam_cidr_allocations SET expires_at = 'infinity' WHERE allocation_key = $1`, allocationKey)
	} else {
		_, err = db.Exec(context.Background(),
			`UPDATE ipam_cidr_allocations SET expires_at = ipam_ceil_second(retained_at + $2::bigint * interval '1 microsecond')
			  WHERE allocation_key = $1`, allocationKey, after.Microseconds())
	}
	if err != nil {
		t.Fatalf("set deadline of %s: %v", allocationKey, err)
	}
}

func tighten(t *testing.T, db *pgxpool.Pool, poolKey string) int {
	t.Helper()
	var moved int
	inTx(t, context.Background(), db.Begin, func(tx pgx.Tx) error {
		var err error
		moved, err = TightenRetention(context.Background(), tx, poolKey)
		return err
	})
	return moved
}

// Lowering a pool's cap brings every retained deadline the cap cuts short
// forward to it, including rows that had no lease; a deadline already inside
// the cap stays. Raising or removing the cap moves nothing back.
func TestAPoolCapTightensRetainedDeadlinesAndNeverLoosensThem(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "sweep-cap", "10.76.0.0/16")
	retain(t, db, poolKey, "claim-long", "long")
	retain(t, db, poolKey, "claim-none", "none")
	retain(t, db, poolKey, "claim-short", "short")
	setDeadline(t, db, "long", ptr(10*time.Hour))
	setDeadline(t, db, "none", nil)
	setDeadline(t, db, "short", ptr(30*time.Minute))
	lease := func(key string) time.Duration {
		r := retainedRetention(t, db, key)
		if r.Lease != LeaseFinite {
			return -1
		}
		return leaseOf(r)
	}

	setPoolCap(t, db, poolKey, dur(time.Hour))
	if moved := tighten(t, db, poolKey); moved != 2 {
		t.Errorf("tightening to 1h moved %d deadlines, want 2", moved)
	}
	for key, want := range map[string]time.Duration{"long": time.Hour, "none": time.Hour, "short": 30 * time.Minute} {
		if got := lease(key); got != want {
			t.Errorf("%s: lease %s after a 1h cap, want %s", key, got, want)
		}
	}
	if a := readAllocationObject(t, db, "none"); a.Status.ExpiresAt == nil {
		t.Error("the IPAllocation that gained a deadline does not show it")
	}

	if moved := tighten(t, db, poolKey); moved != 0 {
		t.Errorf("tightening again to the same cap moved %d, want 0", moved)
	}
	setPoolCap(t, db, poolKey, dur(5*time.Hour))
	if moved := tighten(t, db, poolKey); moved != 0 {
		t.Errorf("loosening the cap moved %d deadlines, want 0", moved)
	}
	setPoolCap(t, db, poolKey, nil)
	if moved := tighten(t, db, poolKey); moved != 0 {
		t.Errorf("removing the cap moved %d deadlines, want 0", moved)
	}
	for key, want := range map[string]time.Duration{"long": time.Hour, "none": time.Hour, "short": 30 * time.Minute} {
		if got := lease(key); got != want {
			t.Errorf("%s: lease %s after loosening, want it kept at %s", key, got, want)
		}
	}
}

// Rolling migration 008 back while a pool's cap is being applied waits for the
// tightening rather than deadlocking with it: Down takes the allocation table
// first, the order the running service takes it in.
func TestMigrationDownWaitsForATighteningInsteadOfDeadlocking(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(10))
	ctx := context.Background()
	poolKey := seedPool(t, db, "down-race", "10.99.0.0/16")
	seedLeaseClass(t, db, dur(10*time.Hour))
	retain(t, db, poolKey, "claim-a", "alloc-a")
	sweep(t, db, SweepObserve)
	setPoolCap(t, db, poolKey, dur(time.Hour))

	// Hold the IPAllocation the tightening will republish, so it stops there
	// holding the pool and the allocation rows.
	h, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = h.Rollback(ctx) }()
	if _, err := h.Exec(ctx, `SELECT 1 FROM ipam_objects WHERE key = 'alloc-a' FOR UPDATE`); err != nil {
		t.Fatalf("hold the IPAllocation: %v", err)
	}
	tightenErr := make(chan error, 1)
	go func() {
		tx, err := db.Begin(ctx)
		if err != nil {
			tightenErr <- err
			return
		}
		if _, err = TightenRetention(ctx, tx, poolKey); err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		tightenErr <- err
	}()
	testdb.WaitForLockWaiters(t, db, 1)

	sqlDB, err := sql.Open("pgx", db.Config().ConnString())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	downErr := make(chan error, 1)
	go func() { downErr <- goose.DownTo(sqlDB, ".", 7) }()
	testdb.WaitForLockWaiters(t, db, 2)
	_ = h.Rollback(ctx)

	if err := <-tightenErr; err != nil {
		t.Errorf("tightening racing a Down: %v", err)
	}
	if err := <-downErr; err != nil {
		t.Errorf("Down racing a tightening: %v", err)
	}
}
