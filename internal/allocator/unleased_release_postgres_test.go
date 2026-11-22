package allocator

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// allowUnleased is the guard of a caller allowed to release a retained row
// with no lease deadline.
func allowUnleased(context.Context) (bool, error) { return true, nil }

// retainWithDeadline leaves alloc-a retained in a fresh pool with expires_at
// set to the given SQL expression, and returns the pool's key.
func retainWithDeadline(t *testing.T, db *pgxpool.Pool, name, cidr, expires string) string {
	t.Helper()
	poolKey := seedPool(t, db, name, cidr)
	allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)
	releaseReturningRetained(t, db, "claim-a")
	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_cidr_allocations SET expires_at = `+expires+` WHERE allocation_key = 'alloc-a'`); err != nil {
		t.Fatalf("set deadline %s: %v", expires, err)
	}
	return poolKey
}

// releaseWith runs ReleaseRetainedAllocation for alloc-a under guard and
// commits it if it succeeds.
func releaseWith(t *testing.T, db *pgxpool.Pool, guard UnleasedReleaseGuard) error {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := NewPostgresPrefixAllocator().ReleaseRetainedAllocation(ctx, tx, "alloc-a", guard); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return nil
}

// A retained row is released without the guard only if it has a finite
// deadline. NULL (not yet evaluated) and 'infinity' (no lease) need the guard
// to allow it; refused, the row stays.
func TestReleaseRetainedAllocationGuardsARowWithNoDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expires  string
		unleased bool
	}{
		{"not evaluated", "NULL", true},
		{"no lease", "'infinity'", true},
		{"running lease", "now() + interval '1 hour'", false},
		{"overdue lease", "now() - interval '1 minute'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Pool(t)
			retainWithDeadline(t, db, "unleased-guard", "10.61.0.0/16", tc.expires)

			calls := 0
			deny := func(context.Context) (bool, error) { calls++; return false, nil }

			if !tc.unleased {
				if err := releaseWith(t, db, deny); err != nil {
					t.Fatalf("release of a row with a finite deadline: %v", err)
				}
				if calls != 0 {
					t.Errorf("the guard was asked %d times about a row with a finite deadline", calls)
				}
				if rowExists(t, db, "alloc-a") {
					t.Error("the release left the row")
				}
				return
			}

			for label, guard := range map[string]UnleasedReleaseGuard{"no guard": nil, "denying guard": deny} {
				var unleased *UnleasedReleaseError
				if err := releaseWith(t, db, guard); !errors.As(err, &unleased) {
					t.Fatalf("%s: release returned %v, want *UnleasedReleaseError", label, err)
				}
				if !rowExists(t, db, "alloc-a") {
					t.Fatalf("%s: the refused release freed the row", label)
				}
			}
			if calls != 1 {
				t.Errorf("denying guard asked %d times, want 1", calls)
			}
			if err := releaseWith(t, db, allowUnleased); err != nil {
				t.Fatalf("allowed release: %v", err)
			}
			if rowExists(t, db, "alloc-a") {
				t.Error("the allowed release left the row")
			}
		})
	}
}

// The guard is asked while the release holds the pool and the row, so the
// deadline it was asked about cannot change before the delete.
func TestTheUnleasedGuardRunsUnderThePoolAndRowLocks(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(4))
	poolKey := retainWithDeadline(t, db, "unleased-locks", "10.65.0.0/16", "'infinity'")

	nowait := func(ctx context.Context, sql string, arg string) error {
		tx, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, sql, arg)
		return err
	}
	locked := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "55P03"
	}

	asked := false
	guard := func(ctx context.Context) (bool, error) {
		asked = true
		if err := nowait(ctx, `SELECT 1 FROM ipam_objects WHERE key = $1 FOR UPDATE NOWAIT`, poolKey); !locked(err) {
			t.Errorf("pool row lockable while the guard runs (err %v); the release does not hold it", err)
		}
		if err := nowait(ctx, `SELECT 1 FROM ipam_cidr_allocations WHERE allocation_key = $1 FOR UPDATE NOWAIT`, "alloc-a"); !locked(err) {
			t.Errorf("allocation row lockable while the guard runs (err %v); the release does not hold it", err)
		}
		return true, nil
	}
	if err := releaseWith(t, db, guard); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !asked {
		t.Fatal("the guard was not asked about a row with no deadline")
	}
}

// A guard that cannot decide fails the release, and frees nothing.
func TestAnUnleasedGuardErrorFreesNothing(t *testing.T) {
	db := testdb.Pool(t)
	retainWithDeadline(t, db, "unleased-error", "10.66.0.0/16", "'infinity'")
	boom := errors.New("authorizer unavailable")

	err := releaseWith(t, db, func(context.Context) (bool, error) { return false, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("release returned %v, want the guard's error", err)
	}
	if !rowExists(t, db, "alloc-a") {
		t.Error("a release whose guard failed freed the row")
	}
}

// A bound row is refused as bound, whatever its deadline: the guard is for
// retained rows and is not asked.
func TestABoundRowIsNotPutToTheUnleasedGuard(t *testing.T) {
	db := testdb.Pool(t)
	poolKey := seedPool(t, db, "unleased-bound", "10.67.0.0/16")
	allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)

	asked := false
	err := releaseWith(t, db, func(context.Context) (bool, error) { asked = true; return true, nil })
	var bound *AllocationBoundError
	if !errors.As(err, &bound) {
		t.Fatalf("release of a bound row returned %v, want *AllocationBoundError", err)
	}
	if asked {
		t.Error("the guard was asked about a bound row")
	}
}

// The unlocked read the registry uses to ask before locking agrees with the
// guard on which rows have no deadline.
func TestRetainedWithoutDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, expires string
		want          bool
	}{
		{"not evaluated", "NULL", true},
		{"no lease", "'infinity'", true},
		{"running lease", "now() + interval '1 hour'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Pool(t)
			retainWithDeadline(t, db, "unleased-peek", "10.68.0.0/16", tc.expires)
			got, err := RetainedWithoutDeadline(context.Background(), db, "alloc-a")
			if err != nil {
				t.Fatalf("RetainedWithoutDeadline: %v", err)
			}
			if got != tc.want {
				t.Errorf("RetainedWithoutDeadline = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("bound or missing", func(t *testing.T) {
		db := testdb.Pool(t)
		poolKey := seedPool(t, db, "unleased-peek-bound", "10.71.0.0/16")
		allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)
		for _, key := range []string{"alloc-a", "alloc-missing"} {
			if got, err := RetainedWithoutDeadline(context.Background(), db, key); err != nil || got {
				t.Errorf("RetainedWithoutDeadline(%s) = %v, %v; want false: it is not a retained row", key, got, err)
			}
		}
	})
}
