// Package testdb gives a test a migrated PostgreSQL database of its own. Keep
// it the only such fixture.
//
// A test calls Pool and must add:
//
//	func TestMain(m *testing.M) { testdb.TestMain(m) }
//
// The backend is resolved once per test binary:
//
//	IPAM_TEST_POSTGRES_DSN set   -> that server
//	otherwise, docker available  -> one testcontainers container for the package
//	neither                      -> t.Skip, or fail if IPAM_TEST_REQUIRE_POSTGRES=1
package testdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"go.miloapis.com/ipam/migrations"
)

// Option adjusts the pool configuration before it is opened.
type Option func(*pgxpool.Config)

// MaxConns sizes the connection pool. Concurrency tests must set it above their
// herd size: pgxpool defaults to max(4, NumCPU) and queues the rest at the
// client, so the test passes without producing contention.
func MaxConns(n int32) Option {
	return func(c *pgxpool.Config) { c.MaxConns = n }
}

// Pool returns a pgxpool connected to a migrated database named after the test
// and dropped when the test ends. It skips the test if no PostgreSQL is
// reachable.
func Pool(t *testing.T, opts ...Option) *pgxpool.Pool {
	t.Helper()

	base := baseDSN(t)
	name := databaseName(t.Name())
	ctx := context.Background()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	t.Logf("testdb: database %q on %s", name, describe(base))

	// Also dropped on entry: a run killed mid-test leaves one behind.
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatalf("drop database %s: %v", name, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	// Registered before the pool's Close so it runs after: cleanups are LIFO.
	t.Cleanup(func() {
		db, err := sql.Open("pgx", base)
		if err != nil {
			t.Logf("cleanup: reopen for DROP DATABASE %s: %v", name, err)
			return
		}
		defer func() { _ = db.Close() }()
		if _, err := db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: DROP DATABASE %s: %v", name, err)
		}
	})

	scoped := withDatabase(base, name)

	migrateDB, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatalf("open migration connection: %v", err)
	}
	defer func() { _ = migrateDB.Close() }()
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
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
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// databaseName renders a test name as a legal, unique database identifier. The
// hash keeps two long names sharing a prefix distinct past the 63-byte cap.
func databaseName(testName string) string {
	sum := sha256.Sum256([]byte(testName))
	suffix := "_" + hex.EncodeToString(sum[:4])

	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '_'
		}
	}, testName)

	const prefix = "ipam_test_"
	max := 63 - len(prefix) - len(suffix)
	if len(sanitized) > max {
		sanitized = sanitized[:max]
	}
	return prefix + sanitized + suffix
}

func withDatabase(base, name string) string {
	if u, err := url.Parse(base); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return u.String()
	}
	// Replace dbname rather than appending: libpq takes the last of a duplicate
	// key, but a DSN naming two databases misleads a reader.
	fields := strings.Fields(base)
	out := fields[:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, "dbname=") {
			out = append(out, f)
		}
	}
	return strings.Join(append(out, "dbname="+name), " ")
}

// describe renders a DSN as "host:port/database" for logging. It never returns
// the DSN itself: both formats carry a password, and the keyword/value form
// parses as a URL without error.
func describe(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "an unparseable DSN"
		}
		return u.Host + "/" + strings.TrimPrefix(u.Path, "/")
	}

	fields := map[string]string{}
	for _, kv := range strings.Fields(dsn) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			fields[k] = v
		}
	}
	host, db := fields["host"], fields["dbname"]
	if host == "" || db == "" {
		return "an unrecognised DSN"
	}
	if port := fields["port"]; port != "" {
		host += ":" + port
	}
	return host + "/" + db
}

// TestMain removes the package's container, if one was started.
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

	container testcontainers.Container
)

// baseDSN resolves the backend once per test binary.
// IPAM_TEST_REQUIRE_POSTGRES turns the no-backend skip into a failure.
func baseDSN(t *testing.T) string {
	t.Helper()
	backendOnce.Do(func() { backendDSN, backendSkip = resolveBackend() })
	if backendSkip != "" {
		if os.Getenv("IPAM_TEST_REQUIRE_POSTGRES") != "" {
			t.Fatalf("IPAM_TEST_REQUIRE_POSTGRES is set: %s", backendSkip)
		}
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
	if err := configureDockerEnv(docker); err != nil {
		return "", fmt.Sprintf("no PostgreSQL: %v", err)
	}
	return startContainer()
}

// configureDockerEnv points testcontainers at the daemon the docker CLI uses.
//
// testcontainers looks for DOCKER_HOST and then /var/run/docker.sock, neither
// of which need exist: a VM-backed daemon keeps its socket elsewhere, and
// /var/run/docker.sock may be a symlink left behind by a removed install.
func configureDockerEnv(docker string) error {
	if os.Getenv("DOCKER_HOST") == "" {
		out, err := exec.Command(docker, "context", "inspect",
			"--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			return fmt.Errorf("resolve docker endpoint: %w", err)
		}
		if err := os.Setenv("DOCKER_HOST", strings.TrimSpace(string(out))); err != nil {
			return fmt.Errorf("set DOCKER_HOST: %w", err)
		}
	}

	// Ryuk bind mounts the socket, and the mount source is a path inside the
	// daemon's filesystem, not the host's.
	const inDaemon = "/var/run/docker.sock"
	if os.Getenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE") == "" &&
		!strings.HasSuffix(os.Getenv("DOCKER_HOST"), inDaemon) {
		if err := os.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", inDaemon); err != nil {
			return fmt.Errorf("set TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE: %w", err)
		}
	}
	return nil
}

func startContainer() (dsn, skip string) {
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.WithDatabase("postgres"),
		// Waits for the readiness log twice: Postgres restarts after initdb, and
		// a connect-only check passes against the server it then discards.
		postgres.BasicWaitStrategies(),
		testcontainers.WithTmpfs(map[string]string{"/var/lib/postgresql/data": "rw"}),
		// One container serves the whole package, so the server must not be
		// what serialises a concurrency test.
		testcontainers.WithCmd("postgres",
			"-c", "fsync=off",
			"-c", "full_page_writes=off",
			"-c", "max_connections=500",
		),
	)
	if err != nil {
		return "", fmt.Sprintf("no PostgreSQL: %v", err)
	}
	container = ctr

	dsn, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", fmt.Sprintf("no PostgreSQL: connection string: %v", err)
	}
	return dsn, ""
}

func stopContainer() {
	if container != nil {
		_ = testcontainers.TerminateContainer(container)
		container = nil
	}
}
