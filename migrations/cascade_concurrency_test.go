package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/testdb"
)

// This file proves the one property of migration 002 that cannot be checked by
// reading the DDL: that a herd of simultaneous first claims into one brand-new
// scope produces exactly one pool, with every loser reading the winner's rather
// than erroring.
//
// It found a real defect. An immediate UNIQUE (pool_key) on ipam_pool_identity
// raises 23505 on roughly one concurrent claim in twenty, because ON CONFLICT
// suppresses conflicts only on the arbiter index it names. Nothing about the
// DDL looks wrong; only the herd shows it. Keep this test.
//
// It needs a live PostgreSQL. internal/testdb finds one: it uses
// IPAM_TEST_POSTGRES_DSN when set, and otherwise starts a throwaway container
// for this package, so the default `go test ./...` runs these rather than
// skipping them.

const herdSize = 20

// Schema isolation comes from internal/testschema, which is the single
// implementation of it in this repo. It cannot live in internal/testdb — that
// package imports this one to run goose — and the duplicate that used to sit
// here is how the btree_gist teardown fix reached only one of the two harnesses.
//
// What it buys these tests specifically: they write objects named
// `loser-N-scope-abort-…` and `tenant-subnet-ipv6-scope-…` at keys of the form
// `/ippool/<name>`. Left in a shared database that residue is indistinguishable
// at a glance from cascade-provisioned pools in an unprefixed keyspace — which
// is exactly how it was read during the platform-as-a-project cutover, and it
// cost a round trip to disprove.

// TestCascadeHerd releases herdSize goroutines at one instant against a scope
// no pool exists for, and asserts the outcome the cascade depends on.
func TestCascadeHerd(t *testing.T) {
	ctx := context.Background()
	pool := connect(ctx, t, "migration_cascade_herd")

	// Repeat: the failure this guards against is a race, and one round passes
	// by luck better than half the time.
	for round := 0; round < 10; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			const class = "tenant-subnet-ipv6"
			digest := fmt.Sprintf("scope-%d-%d", time.Now().UnixNano(), round)
			// Deterministic, as the allocator derives it from class and scope.
			poolKey := "/ippool/" + class + "-" + digest

			var (
				wg        sync.WaitGroup
				mu        sync.Mutex
				results   = map[string]int{}
				wins      int
				contended int
				late      int
				errs      []error
			)

			start := make(chan struct{})
			for i := 0; i < herdSize; i++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					<-start // release the herd at one instant
					got, out, err := cascade(ctx, pool, class, digest, poolKey)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err != nil:
						errs = append(errs, fmt.Errorf("worker %d: %w", id, err))
					default:
						results[got]++
						switch out {
						case won:
							wins++
						case lostRace:
							contended++
						case arrivedLate:
							late++
						}
					}
				}(i)
			}
			close(start)
			wg.Wait()

			for _, err := range errs {
				t.Errorf("cascade failed: %v", err)
			}
			if wins != 1 {
				t.Errorf("winners = %d, want exactly 1", wins)
			}

			// THE ASSERTION THAT MAKES THIS A CONCURRENCY TEST.
			//
			// Everything else here — one winner, one pool, one identity row,
			// every caller agreeing — is equally true of a herd that arrived
			// one at a time. Throttle the client (an unset pgxpool MaxConns
			// defaults to max(4, NumCPU)) and each caller reaches Postgres
			// after the previous one committed: every assertion above still
			// passes while nothing contends. This test would have been green
			// and worthless.
			//
			// A caller that genuinely raced found no pool at step 1 and then
			// lost the INSERT, so it is counted as contended. A caller that
			// arrived after the winner committed short-circuits at step 1 and
			// is counted as late. The split is the evidence.
			//
			// The floor is deliberately loose: some late arrivals are normal
			// once the winner commits mid-herd. What must not happen is a herd
			// that barely contended at all.
			minContended := herdSize / 2
			if contended < minContended {
				t.Errorf(
					"only %d/%d callers contended (%d arrived after the winner committed, %d won); "+
						"want at least %d. The herd did not actually race, so this round proves "+
						"nothing about concurrency — check that the client is not throttling "+
						"(pgxpool MaxConns must exceed the herd)",
					contended, herdSize, late, wins, minContended)
			}
			t.Logf("herd of %d: %d won, %d contended, %d arrived late", herdSize, wins, contended, late)
			if len(results) != 1 || results[poolKey] != herdSize {
				t.Errorf("pool keys seen = %v, want all %d workers on %s", results, herdSize, poolKey)
			}

			var identities, objects int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM ipam_pool_identity WHERE class_name=$1 AND scope_digest=$2`,
				class, digest).Scan(&identities); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM ipam_objects WHERE key=$1`, poolKey).Scan(&objects); err != nil {
				t.Fatal(err)
			}
			if identities != 1 {
				t.Errorf("identity rows = %d, want 1", identities)
			}
			if objects != 1 {
				t.Errorf("pool objects = %d, want 1 — the cascade provisioned more than one pool", objects)
			}
		})
	}
}

// outcome distinguishes the three paths a caller can take. Which one it took
// is the only evidence that the herd actually contended — see the assertion on
// contended in the test above.
type outcome int

const (
	// won: this caller inserted the identity row.
	won outcome = iota
	// lostRace: this caller found no pool, tried to insert, and was beaten —
	// which means it was in flight at the same time as the winner.
	lostRace
	// arrivedLate: the pool already existed at step 1. This caller never
	// contended for anything.
	arrivedLate
)

// cascade is the recipe documented in 002_class_model.sql, in the same shape as
// internal/allocator/cascade.go: a cheap lookup first, then the identity claim
// only if nothing was found. The step-1 lookup is what makes the outcome
// meaningful — without it, a caller arriving long after the winner committed is
// indistinguishable from one that genuinely raced, because both get zero rows
// from the INSERT.
func cascade(ctx context.Context, db *pgxpool.Pool, class, digest, poolKey string) (string, outcome, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", arrivedLate, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 1 — the common path, and the discriminator.
	var existing string
	err = tx.QueryRow(ctx,
		`SELECT pool_key FROM ipam_pool_identity WHERE class_name=$1 AND scope_digest=$2`,
		class, digest).Scan(&existing)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return "", arrivedLate, fmt.Errorf("commit lookup: %w", err)
		}
		return existing, arrivedLate, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", arrivedLate, fmt.Errorf("lookup identity: %w", err)
	}

	// Step 2 — claim the identity before the pool object exists. The FK is
	// deferred, so no pool lock is taken and ipam_objects is not read at all.
	var claimed string
	err = tx.QueryRow(ctx, `
		INSERT INTO ipam_pool_identity (class_name, scope_digest, pool_key)
		VALUES ($1, $2, $3)
		ON CONFLICT (class_name, scope_digest) DO NOTHING
		RETURNING pool_key`, class, digest, poolKey).Scan(&claimed)

	switch {
	case err == nil:
		// Won: create the pool object the identity row already names. The
		// deferred FK is checked at COMMIT, by which time it exists.
		if _, err := tx.Exec(ctx,
			`INSERT INTO ipam_objects (key, kind, name, data)
			 VALUES ($1, 'IPPool', $1, convert_to('{"spec":{}}', 'UTF8'))`, poolKey); err != nil {
			return "", won, fmt.Errorf("create pool: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", won, fmt.Errorf("commit winner: %w", err)
		}
		return poolKey, won, nil

	case errors.Is(err, pgx.ErrNoRows):
		// Lost. The INSERT blocked on the winner's speculative index entry
		// until they committed and raised no error, so this transaction is
		// still usable — no savepoint needed.
		var winner string
		if err := tx.QueryRow(ctx,
			`SELECT pool_key FROM ipam_pool_identity WHERE class_name=$1 AND scope_digest=$2`,
			class, digest).Scan(&winner); err != nil {
			return "", lostRace, fmt.Errorf("read winner: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", lostRace, fmt.Errorf("commit loser: %w", err)
		}
		return winner, lostRace, nil

	default:
		return "", lostRace, fmt.Errorf("claim identity: %w", err)
	}
}

func connect(ctx context.Context, t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	// Sized above the herd. An unset MaxConns defaults to max(4, NumCPU), which
	// queues callers at the client so they reach Postgres one wave at a time —
	// the contended/late split below is what detects that. Overridable so the
	// detection itself can be exercised: IPAM_TEST_MAXCONNS=2 should make the
	// contention assertion fail.
	maxConns := int32(herdSize + 4)
	if v := os.Getenv("IPAM_TEST_MAXCONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("IPAM_TEST_MAXCONNS=%q: %v", v, err)
		}
		maxConns = int32(n)
	}
	pool := testdb.Pool(t, schema, testdb.MaxConns(maxConns))
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Establish the connections before any herd runs.
	//
	// pgxpool opens connections lazily. On a cold pool the first herd spends
	// its time in TCP+auth rather than at the identity tuple, so callers
	// trickle in, the winner commits before most of them arrive, and they are
	// correctly counted as late — the contention assertion then fails for a
	// reason that has nothing to do with the code under test. Observed
	// directly: the first round of a fresh pool contended 12/20 while later
	// rounds contended 19/20.
	var warm sync.WaitGroup
	conns := make([]*pgxpool.Conn, 0, maxConns)
	var mu sync.Mutex
	for i := int32(0); i < maxConns; i++ {
		warm.Add(1)
		go func() {
			defer warm.Done()
			c, err := pool.Acquire(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}()
	}
	warm.Wait()
	for _, c := range conns {
		c.Release()
	}
	return pool
}

// migrateUp applies the migrations from the embedded FS — the same call
// cmd/ipam/migrate.go makes — so the test brings its own schema rather than
// assuming one.

// TestMain removes the throwaway Postgres container testdb starts when
// IPAM_TEST_POSTGRES_DSN is unset. It is shared by every test in the package,
// so no single test can own its teardown.
func TestMain(m *testing.M) { testdb.TestMain(m) }
