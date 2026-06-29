package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"

	"go.miloapis.com/ipam/pkg/apis/ipam/install"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// postgresImage is the image used for the ephemeral test database. Pinned to a
// minor version so the test exercises the same Postgres family as production.
const postgresImage = "postgres:16-alpine"

// schemaDDL mirrors the relevant parts of migrations/001_initial_schema.sql:
// the RV sequence, the durable object table, and the prunable changelog with
// its commit_xid horizon column. Kept inline so the test exercises the real
// SQL without dragging in the goose runner.
const schemaDDL = `
CREATE SEQUENCE IF NOT EXISTS ipam_resource_version_seq;

CREATE TABLE IF NOT EXISTS ipam_objects (
    key              TEXT PRIMARY KEY,
    resource_version BIGINT NOT NULL DEFAULT nextval('ipam_resource_version_seq'),
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL DEFAULT '',
    name             TEXT NOT NULL,
    data             BYTEA NOT NULL,
    labels           jsonb NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ipam_changelog (
    id               BIGSERIAL PRIMARY KEY,
    key              TEXT NOT NULL,
    resource_version BIGINT NOT NULL,
    event_type       TEXT NOT NULL CHECK (event_type IN ('ADDED', 'MODIFIED', 'DELETED')),
    data             BYTEA,
    commit_xid       BIGINT NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

// newTestCodec builds a JSON codec for the IPPool v1alpha1 type so the Store
// can encode/decode real API objects in the integration test.
func newTestCodec(t *testing.T) runtime.Codec {
	t.Helper()
	scheme := runtime.NewScheme()
	install.Install(scheme)
	cf := serializer.NewCodecFactory(scheme)
	ser := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{})
	return cf.CodecForVersions(ser, ser, v1alpha1.SchemeGroupVersion, v1alpha1.SchemeGroupVersion)
}

func newIPPool(name string) runtime.Object {
	p := &v1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	p.SetGroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind("IPPool"))
	return p
}

// startEphemeralPostgres boots a throwaway PostgreSQL container on a free
// localhost port, applies schemaDDL, and returns an open *sql.DB. The
// container (and connection) are torn down via t.Cleanup. The test is skipped
// — never failed — when Docker is unavailable so the suite stays green on
// machines and CI lanes without a Docker daemon.
func startEphemeralPostgres(t *testing.T) *sql.DB {
	t.Helper()

	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not on PATH; skipping Postgres integration test")
	}
	if out, err := exec.Command(docker, "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not available; skipping Postgres integration test: %v\n%s", err, out)
	}

	port := freePort(t)
	args := []string{
		"run", "-d", "--rm",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"-e", "POSTGRES_PASSWORD=postgres",
		"-e", "POSTGRES_USER=postgres",
		"-e", "POSTGRES_DB=postgres",
		// data on tmpfs + fsync off keeps the throwaway DB fast.
		"--tmpfs", "/var/lib/postgresql/data",
		postgresImage,
		"-c", "fsync=off", "-c", "full_page_writes=off",
	}
	out, err := exec.Command(docker, args...).CombinedOutput()
	if err != nil {
		t.Skipf("failed to start postgres container: %v\n%s", err, out)
	}
	containerID := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_ = exec.Command(docker, "rm", "-f", containerID).Run()
	})

	dsn := fmt.Sprintf("host=127.0.0.1 port=%d user=postgres password=postgres dbname=postgres sslmode=disable", port)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := db.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("postgres container did not become ready within 30s")
		}
		time.Sleep(200 * time.Millisecond)
	}

	if _, err := db.ExecContext(context.Background(), schemaDDL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}
