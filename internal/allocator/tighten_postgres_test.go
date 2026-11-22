package allocator

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/testdb"
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
			`UPDATE ipam_cidr_allocations SET expires_at = date_trunc('second', retained_at) + $2::bigint * interval '1 microsecond'
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
		moved, _, err = TightenRetention(context.Background(), tx, poolKey)
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
