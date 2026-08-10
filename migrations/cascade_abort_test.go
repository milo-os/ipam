package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/testdb"
)

// TestCascadeLoserWhenWinnerAborts answers a question the recipe in
// 002_class_model.sql quietly assumes away.
//
// The documented losing path is:
//
//	INSERT ... ON CONFLICT (class_name, scope_digest) DO NOTHING RETURNING pool_key
//	  zero rows -> somebody else won; SELECT reads their pool_key
//
// "Zero rows" is read there as "somebody else committed". There is a third
// possibility: the conflicting inserter *aborted*. If a loser can be released
// from its wait and still find nothing — neither its own INSERT succeeding nor
// a committed row to SELECT — then a naive implementation returns an empty pool
// key, which resolves to a pool that does not exist several transactions later
// with nothing pointing back at the cause.
//
// This test forces exactly that: one caller inserts the identity row and then
// deliberately rolls back while a herd is blocked behind it.
//
// RESULT (PostgreSQL 17.10 and 13.23): the branch is NOT reachable. A waiter
// released by an abort re-runs its speculative insertion, finds the conflicting
// tuple dead, and inserts successfully — so it becomes the new winner and gets
// a row rather than zero rows. Exactly one caller wins, every other caller
// reads that winner's key, and no caller observes the empty state.
//
// The practical consequence for the allocator: a retry loop guarding this
// branch is dead code. Handling "zero rows and SELECT empty" as an error is
// still right — it costs nothing and something must be returned — but it should
// not be described as a hazard that occurs, and it does not need a retry.
func TestCascadeLoserWhenWinnerAborts(t *testing.T) {
	ctx := context.Background()
	// Sized above the herd on purpose — see testdb.MaxConns: an unset MaxConns
	// queues the herd at the client and lets each caller reach Postgres after
	// the previous one finished, so the test passes while never producing the
	// contention it exists to create.
	const losers = 32
	pool := testdb.Pool(t, "migration_cascade_abort", testdb.MaxConns(losers+8))

	for round := 0; round < 5; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			const class = "abort-test"
			digest := fmt.Sprintf("scope-abort-%d-%d", time.Now().UnixNano(), round)
			doomedKey := "/ippool/doomed-" + digest

			// The aborting caller takes the identity row and holds it.
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			if err := tx.QueryRow(ctx, `
				INSERT INTO ipam_pool_identity (class_name, scope_digest, pool_key)
				VALUES ($1,$2,$3)
				ON CONFLICT (class_name, scope_digest) DO NOTHING
				RETURNING pool_key`, class, digest, doomedKey).Scan(&got); err != nil {
				t.Fatalf("doomed insert: %v", err)
			}

			var (
				wg      sync.WaitGroup
				mu      sync.Mutex
				won     int
				lost    int
				empty   int
				errs    []error
				keys    = map[string]int{}
				blocked = make(chan struct{})
			)

			for i := 0; i < losers; i++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					<-blocked
					key, outcome, err := claimIdentity(ctx, pool, class, digest, fmt.Sprintf("/ippool/loser-%d-%s", id, digest))
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err != nil:
						errs = append(errs, fmt.Errorf("loser %d: %w", id, err))
					case outcome == "won":
						won++
						keys[key]++
					case outcome == "lost":
						lost++
						keys[key]++
					case outcome == "empty":
						empty++
					}
				}(i)
			}

			// Release the herd, give it time to pile up behind the held row,
			// then abort.
			close(blocked)
			time.Sleep(300 * time.Millisecond)
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			wg.Wait()

			for _, e := range errs {
				t.Errorf("unexpected error: %v", e)
			}
			t.Logf("winner-aborts: won=%d lost=%d empty=%d distinct-keys=%d", won, lost, empty, len(keys))

			// The finding: no caller observes the empty state.
			if empty > 0 {
				t.Errorf("%d callers saw zero rows AND an empty SELECT — the branch IS reachable, "+
					"and the allocator must handle it rather than assuming zero rows means someone committed", empty)
			}
			if won != 1 {
				t.Errorf("winners = %d, want exactly 1 (a waiter released by an abort should take over)", won)
			}
			if len(keys) != 1 {
				t.Errorf("distinct pool keys = %d, want 1 — the herd disagreed about which pool serves the scope", len(keys))
			}
			// The doomed key must not survive: its transaction aborted.
			if keys[doomedKey] != 0 {
				t.Errorf("%d callers were handed the aborted transaction's pool key %q", keys[doomedKey], doomedKey)
			}

			var surviving int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM ipam_pool_identity WHERE class_name=$1 AND scope_digest=$2`,
				class, digest).Scan(&surviving); err != nil {
				t.Fatal(err)
			}
			if surviving != 1 {
				t.Errorf("identity rows = %d, want 1", surviving)
			}
		})
	}
}

// claimIdentity is the documented recipe, with the third outcome made explicit
// rather than collapsed into "lost".
func claimIdentity(ctx context.Context, db *pgxpool.Pool, class, digest, myKey string) (string, string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var key string
	err = tx.QueryRow(ctx, `
		INSERT INTO ipam_pool_identity (class_name, scope_digest, pool_key)
		VALUES ($1,$2,$3)
		ON CONFLICT (class_name, scope_digest) DO NOTHING
		RETURNING pool_key`, class, digest, myKey).Scan(&key)

	switch {
	case err == nil:
		// Won. Create the pool object the deferred FK will check at COMMIT.
		if _, err := tx.Exec(ctx,
			`INSERT INTO ipam_objects (key, kind, name, data)
			 VALUES ($1,'IPPool',$1,convert_to('{"spec":{}}','UTF8'))
			 ON CONFLICT (key) DO NOTHING`, myKey); err != nil {
			return "", "", fmt.Errorf("create pool: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", fmt.Errorf("commit winner: %w", err)
		}
		return myKey, "won", nil

	case errors.Is(err, pgx.ErrNoRows):
		var winner string
		serr := tx.QueryRow(ctx,
			`SELECT pool_key FROM ipam_pool_identity WHERE class_name=$1 AND scope_digest=$2`,
			class, digest).Scan(&winner)
		if errors.Is(serr, pgx.ErrNoRows) {
			// The branch under investigation: nothing inserted, nothing found.
			_ = tx.Rollback(ctx)
			return "", "empty", nil
		}
		if serr != nil {
			return "", "", fmt.Errorf("read winner: %w", serr)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", fmt.Errorf("commit loser: %w", err)
		}
		return winner, "lost", nil

	default:
		return "", "", fmt.Errorf("claim identity: %w", err)
	}
}
