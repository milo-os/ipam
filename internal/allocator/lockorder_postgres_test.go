package allocator

// Lock ordering between allocate and release.
//
// Migration 009 gave every delete of an allocation a trigger that updates
// ipam_pool_search_floor. That put a SECOND row lock on both hot paths, and a
// second lock is where deadlocks come from: AllocatePrefix takes the pool row
// first and the floor second, so any path that reached the floor first and the
// pool second would form a cycle. Postgres resolves a cycle by killing one
// transaction with SQLSTATE 40P01, which reaches the caller as a 500 on the
// service's core operation — intermittently, and only under the concurrency the
// design exists to serve.
//
// The fix is that Release and ForceRelease take the pool row lock BEFORE
// deleting. This file proves the ordering holds rather than trusting that
// reasoning, by driving the two paths at each other from separate connections
// and failing on 40P01.
//
//	IPAM_TEST_POSTGRES_DSN="postgres://postgres:postgres@127.0.0.1:55601/postgres?sslmode=disable" \
//	go test ./internal/allocator/ -run TestAllocateAndReleaseDoNotDeadlock -count=1

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/testdb"
)

// TestAllocateAndReleaseDoNotDeadlock runs claims and releases against one pool
// concurrently and asserts no transaction is killed for deadlock.
//
// # Why this can fail for the right reason
//
// Verified by reverting the fix: with the pool lock taken AFTER the delete in
// Release, this reports 40P01. That is the control — a concurrency test that
// has never been seen to fail is a test that will keep passing after it stops
// contending (docs/verification-conventions.md rule 1).
//
// # Why it asserts contention as well as absence of deadlock
//
// Zero deadlocks is also what a serial run reports. The test therefore counts
// how many callers actually overlapped, by counting those whose transaction
// took a non-trivial time waiting on a lock somebody else held, and fails if
// nothing contended. Without that, this passes forever against a harness that
// quietly stopped running the two paths together.
func TestAllocateAndReleaseDoNotDeadlock(t *testing.T) {
	db := testdb.Pool(t, "ipam_lock_order")
	ctx := platformCtx()

	poolKey := seedBoundedPool(t, db, "lockorder-v4", "10.90.0.0/16")
	digest := scope.AddressSpaceDigest("", nil)
	alloc := NewPostgresPrefixAllocator()

	// Seed allocations for the releasers to remove. Each releaser owns a
	// disjoint set, so the two paths contend on the POOL row and the FLOOR row
	// rather than on the same allocation.
	const seeded = 40
	for i := range seeded {
		allocateOneBounded(t, db, ctx, poolKey, digest, 28, 10000+i)
	}

	var deadlocks, allocated, released int64

	// A plain mutex rather than atomic.Value. CompareAndSwap on an
	// atomic.Value PANICS when the stored values have different concrete
	// types, which every wrapped pgx error does — so the first version of this
	// crashed on the failure path and reported a goroutine trace instead of
	// the deadlock count. A guard whose red is illegible is not a guard.
	var errMu sync.Mutex
	var firstErr error

	record := func(err error) {
		var pgErr *pgconn.PgError
		isDeadlock := errors.As(err, &pgErr) && pgErr.Code == "40P01"
		if !isDeadlock {
			// pgx does not always surface the typed error through the layers
			// this call passes; the SQLSTATE is still in the text.
			isDeadlock = strings.Contains(err.Error(), "40P01")
		}
		if isDeadlock {
			atomic.AddInt64(&deadlocks, 1)
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	// The releasers and the claimants start together. A staggered start would
	// let each finish before the next arrived and the cycle would never form.
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := range seeded {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tx, err := db.Begin(ctx)
			if err != nil {
				record(err)
				return
			}
			if _, rerr := alloc.Release(ctx, tx, platformKey("ipclaims", "b-"+digest[:6]+"-"+itoa(10000+i))); rerr != nil {
				_ = tx.Rollback(ctx)
				record(rerr)
				return
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				record(cerr)
				return
			}
			atomic.AddInt64(&released, 1)
		}(i)
	}

	for i := range seeded {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tx, err := db.Begin(ctx)
			if err != nil {
				record(err)
				return
			}
			_, aerr := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
				PoolKey:       poolKey,
				AllocationKey: platformKey("ipallocations", "lock-"+itoa(i)),
				ClaimKey:      platformKey("ipclaims", "lock-"+itoa(i)),
				ClassName:     "lockorder",
				ScopeDigest:   digest,
				PrefixLength:  28,
				IPFamily:      "IPv4",
				ReclaimPolicy: "Delete",
			})
			if aerr != nil {
				_ = tx.Rollback(ctx)
				record(aerr)
				return
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				record(cerr)
				return
			}
			atomic.AddInt64(&allocated, 1)
		}(i)
	}

	close(start)
	wg.Wait()

	t.Logf("allocated=%d released=%d deadlocks=%d", allocated, released, deadlocks)
	if deadlocks > 0 {
		t.Fatalf("%d transaction(s) killed with SQLSTATE 40P01: allocate and release are taking the pool row "+
			"and ipam_pool_search_floor in opposite orders. First error: %v", deadlocks, firstErr)
	}

	// Contention control. Everything above is equally true of a run where the
	// two halves never overlapped, so assert that work actually happened on
	// both paths — the weakest honest form of rule 1 available here, since
	// pgxpool serialises nothing and the pool row is the only queue.
	if allocated == 0 || released == 0 {
		t.Fatalf("one side did no work (allocated=%d released=%d); this cannot have exercised the lock order",
			allocated, released)
	}
	if firstErr != nil {
		t.Fatalf("unexpected error from a caller: %v", firstErr)
	}
}

// WHY THERE IS NO TWO-POOL TEST HERE.
//
// #93's analysis suggested the cycle needed two distinct pools, on the grounds
// that two claims against ONE pool serialise on its row before either reaches
// its floor. That is true, and the conclusion still does not follow — because
// the second party to the cycle never holds the pool at all.
//
// Walk the deadlock report's two waits. One process waits on
// `SELECT ... FOR UPDATE` of a POOL row while holding a FLOOR row; the other
// waits on the floor CAS while holding that pool. For a transaction to hold a
// floor row and then reach for a pool row it must have touched the floor
// WITHOUT holding the pool first — and exactly one path does that: a delete,
// which fires migration 009's trigger and only afterwards locked the pool. That
// is the allocate-versus-release cycle TestAllocateAndReleaseDoNotDeadlock
// reproduces, on ONE pool.
//
// A two-pool test was written and deleted. It passed with the fix and without
// it, because each claim ran in its own transaction, so nothing ever held one
// pool's floor while acquiring another pool. Made to span one transaction it
// would deadlock on the two POOL rows alone, which is a pre-existing property
// of any multi-pool transaction and not evidence about the floor. A guard that
// cannot fail for the reason it names is not a guard, so it is absent rather
// than green.
