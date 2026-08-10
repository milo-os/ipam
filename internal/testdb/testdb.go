// Package testdb is the single PostgreSQL fixture for this repo's tests — the
// ones that need the real database rather than a fake: constraint behaviour,
// SQLSTATE translation, concurrency.
//
// It is a normal (non-_test) package so any test package can import it,
// including the external test package for `migrations` (testdb imports
// migrations to run goose; `package migrations_test` importing testdb is not a
// cycle).
//
// # One backend, because five harnesses was the bug
//
// Five fixtures used to coexist: this one, a container-per-test constructor in
// internal/allocator, another in internal/watch, another in
// internal/storage/postgres, and a copy of the schema logic in migrations
// (itself with two ad-hoc variants). #97 was filed against three of them; the
// other two turned up while fixing it, which is the argument in miniature.
//
// Five harnesses meant five skip conditions, five sets of parallelism
// properties, two disagreeing Postgres image pins, and no way to tell from a
// red suite which one bit you. It also meant a fix could reach one and miss the
// rest — exactly what happened to the btree_gist teardown fix (#97), and to the
// `SET search_path` isolation that two migration files never got.
//
// So the BACKEND is resolved once per test binary, here, and nowhere else:
//
//	IPAM_TEST_POSTGRES_DSN set   -> that database
//	otherwise, docker available  -> one throwaway container for the whole package
//	neither                      -> t.Skip, with one message
//
// That matters in both directions:
//
//   - Guards that must never silently skip (the #91 completeness fixtures) keep
//     running in a default `go test ./...` with no DSN set, because Docker is
//     the fallback rather than a separate opt-in harness.
//   - Container-backed tests no longer take a container each. Eighteen
//     containers on eighteen freshly-probed ports was the real cause of
//     TestTwoTenantsWithOverlappingRootsAreServedFromTheirOwnPool failing under
//     `go test ./...` while passing alone — port and daemon contention, not
//     anything about the schema.
//
// # Which one to reach for
//
// The thing #97 said was missing. In order of preference:
//
//	Pool             the default. Migrated schema, pgxpool, isolated.
//	RawDB            you drive goose yourself. Migration tests only.
//	PrivateDatabase  schema isolation is not enough. Say why at the call site;
//	                 today the only reason is LISTEN/NOTIFY, which is scoped to
//	                 the database rather than the schema.
//
// # Isolation is the point, not a nicety
//
// Every fixture lands in a private schema that is dropped when the test ends.
// IPAM_TEST_POSTGRES_DSN is routinely pointed at a dev cluster's database, and
// residue left in a shared one is indistinguishable at a glance from the defect
// somebody else is mid-investigation on — that reading cost a round trip during
// the platform-as-a-project cutover (verification-conventions rule 11a). The
// schema mechanics live in internal/testschema, which exists as its own package
// only because migrations cannot import this one.
//
// # Packages that may reach the container backend must call TestMain
//
// The container outlives any single test, so no test can own its teardown. Add
// to the package:
//
//	func TestMain(m *testing.M) { testdb.TestMain(m) }
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

	"go.miloapis.com/ipam/internal/testschema"
	"go.miloapis.com/ipam/migrations"
)

// Option adjusts the pool configuration before it is opened.
type Option func(*pgxpool.Config)

// MaxConns sizes the connection pool.
//
// Concurrency tests must set this above their herd size. pgxpool defaults
// MaxConns to max(4, NumCPU), which queues a herd at the CLIENT and lets each
// caller reach Postgres after the previous one finished — the test then passes
// while never producing the contention it exists to create.
func MaxConns(n int32) Option {
	return func(c *pgxpool.Config) { c.MaxConns = n }
}

// Pool returns a pgxpool connected to a freshly migrated private schema, or
// skips the test when no PostgreSQL is reachable by either route.
//
// schema must be unique per test. It is dropped and recreated on entry as well
// as on exit, so a previous run killed mid-test cannot leave a half-built
// schema that makes this one fail for a reason unrelated to what it tests.
//
// Migrations are applied through goose rather than as inlined DDL, so tests
// exercise the schema the service actually deploys — including the deferred
// foreign key the cascade's write ordering depends on.
func Pool(t *testing.T, schema string, opts ...Option) *pgxpool.Pool {
	t.Helper()

	scoped := testschema.Prepare(t, baseDSN(t), schema)

	migrateDB, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatalf("open migration connection: %v", err)
	}
	defer func() { _ = migrateDB.Close() }()
	prepareGoose(t)
	goose.SetLogger(goose.NopLogger())
	if err := goose.Up(migrateDB, "."); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(scoped)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	for _, opt := range opts {
		opt(cfg)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// RawDB returns a database/sql handle onto an isolated, EMPTY private schema,
// with goose configured against this repo's migrations but nothing applied.
//
// It exists for the migration tests, which drive goose themselves — up to a
// specific version, then asserting that the next one refuses. Use Pool for
// everything else; a test that just wants the schema should not be choosing
// migration versions.
//
// The isolation is in the DSN, not a `SET search_path` statement. database/sql
// pools connections, so a SET applies to whichever connection happened to run
// it and every other query in the test can land in `public` — which is how
// these tests leaked schemas and, in one file, wrote to the shared schema
// outright.
func RawDB(t *testing.T, schema string) *sql.DB {
	t.Helper()

	scoped := testschema.Prepare(t, baseDSN(t), schema)
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatalf("open %s: %v", schema, err)
	}
	// Registered before testschema's DROP SCHEMA cleanup runs? No — cleanups
	// are LIFO and testschema registered its drop first, so this Close runs
	// BEFORE the drop, on a handle the drop does not use (it reopens). That
	// ordering is deliberate: an earlier version closed the handle the drop
	// needed, so every run leaked its schema.
	t.Cleanup(func() { _ = db.Close() })

	prepareGoose(t)
	return db
}

// PrivateDatabase returns a DSN for a brand-new, empty DATABASE on the resolved
// backend, dropped when the test ends. Nothing is applied to it — the caller
// owns its schema.
//
// Use this instead of Pool/RawDB only when schema isolation is not enough, and
// say why at the call site. The case it exists for is LISTEN/NOTIFY: channel
// names are scoped to the DATABASE, not the schema, so two tests sharing one
// database hear each other's notifications, and a DSN pointed at a dev cluster
// would deliver the live service's changelog into the test.
func PrivateDatabase(t *testing.T, name string) string {
	t.Helper()
	base := baseDSN(t)
	ctx := context.Background()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	t.Logf("testdb: database %q on the backend named by IPAM_TEST_POSTGRES_DSN", name)

	// Dropped on entry as well as exit, so a run killed mid-test cannot leave a
	// half-built database that fails this one for an unrelated reason.
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatalf("drop database %s: %v", name, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", base)
		if err != nil {
			t.Logf("cleanup: reopen for DROP DATABASE %s: %v", name, err)
			return
		}
		defer func() { _ = db.Close() }()
		// FORCE terminates stragglers. A watcher's LISTEN connection routinely
		// outlives the test that opened it by a few milliseconds, and without
		// FORCE the drop fails on "database is being accessed by other users"
		// — a leak that only shows up on the next run.
		if _, err := db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: DROP DATABASE %s: %v", name, err)
		}
	})

	return withDatabase(t, base, name)
}

// withDatabase returns base pointed at a different database name.
func withDatabase(t *testing.T, base, name string) string {
	t.Helper()
	if u, err := url.Parse(base); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return u.String()
	}
	// Keyword/value form. Replace an existing dbname rather than appending: a
	// duplicate key is not an error in libpq, the LAST one wins, and relying on
	// that is a trap for whoever reads the DSN in a log line.
	fields := strings.Fields(base)
	out := fields[:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, "dbname=") {
			out = append(out, f)
		}
	}
	return strings.Join(append(out, "dbname="+name), " ")
}

// prepareGoose points goose at this repo's migrations. The version table is
// left at its default name because search_path already puts it in the private
// schema — naming it explicitly is how a stale global leaked between tests.
func prepareGoose(t *testing.T) {
	t.Helper()
	goose.SetBaseFS(migrations.FS)
	goose.SetTableName("goose_db_version")
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
}

// TestMain removes the package's throwaway container, if one was started.
//
// Packages whose tests can reach the container backend must call this; without
// it a container survives the test binary. Calling it when no container is ever
// started costs nothing.
func TestMain(m *testing.M) {
	code := m.Run()
	stopContainer()
	os.Exit(code)
}

const postgresImage = "postgres:17-alpine"

var (
	backendOnce sync.Once
	backendDSN  string
	backendSkip string // non-empty means: no database available, skip with this

	containerMu sync.Mutex
	containerID string
)

// baseDSN resolves the backend once per test binary and skips if there is none.
func baseDSN(t *testing.T) string {
	t.Helper()
	backendOnce.Do(func() { backendDSN, backendSkip = resolveBackend() })
	if backendSkip != "" {
		t.Skip(backendSkip)
	}
	return backendDSN
}

func resolveBackend() (dsn, skip string) {
	if d := os.Getenv("IPAM_TEST_POSTGRES_DSN"); d != "" {
		return d, ""
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return "", "no PostgreSQL: IPAM_TEST_POSTGRES_DSN unset and docker not on PATH"
	}
	if out, err := exec.Command(docker, "info").CombinedOutput(); err != nil {
		return "", fmt.Sprintf("no PostgreSQL: IPAM_TEST_POSTGRES_DSN unset and docker daemon unavailable: %v\n%s", err, out)
	}
	return startContainer(docker)
}

func startContainer(docker string) (dsn, skip string) {
	port, err := freeTCPPort()
	if err != nil {
		return "", fmt.Sprintf("no PostgreSQL: cannot find a free port: %v", err)
	}

	out, err := exec.Command(docker, "run", "-d", "--rm",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"-e", "POSTGRES_PASSWORD=postgres",
		"-e", "POSTGRES_USER=postgres",
		"-e", "POSTGRES_DB=postgres",
		"--tmpfs", "/var/lib/postgresql/data",
		"--label", "go.miloapis.com/ipam-testdb=1",
		postgresImage,
		// max_connections is well above any single herd because one container
		// now serves every test in the package: the SERVER must not become the
		// thing that serialises a test's herd.
		"-c", "fsync=off", "-c", "full_page_writes=off", "-c", "max_connections=500",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Sprintf("no PostgreSQL: failed to start container: %v\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	containerMu.Lock()
	containerID = id
	containerMu.Unlock()

	dsn = fmt.Sprintf("host=127.0.0.1 port=%d user=postgres password=postgres dbname=postgres sslmode=disable", port)
	if err := waitReady(dsn); err != nil {
		stopContainer()
		return "", fmt.Sprintf("no PostgreSQL: container did not become ready: %v", err)
	}
	return dsn, ""
}

func waitReady(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if last = db.Ping(); last == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("not ready within 60s: %w", last)
}

func stopContainer() {
	containerMu.Lock()
	id := containerID
	containerID = ""
	containerMu.Unlock()
	if id == "" {
		return
	}
	if docker, err := exec.LookPath("docker"); err == nil {
		_ = exec.Command(docker, "rm", "-f", id).Run()
	}
}

func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}
