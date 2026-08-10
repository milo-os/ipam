package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"

	"go.miloapis.com/ipam/pkg/apis/ipam/install"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"

	"go.miloapis.com/ipam/internal/testdb"
)

// The Postgres image is no longer pinned here. internal/testdb pins it once for
// the whole repo, which moves these tests from 16-alpine to 17-alpine — the
// family the service actually deploys, and the one the migration tests already
// used. Two pins that disagreed was the smaller half of the same problem.

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

// startEphemeralPostgres returns an open *sql.DB on a private database with
// schemaDDL applied.
//
// It used to start a Postgres container per test. internal/testdb now owns
// backend selection for the whole repo, so at most one container is started per
// package and the skip condition is stated in one place.
//
// A private DATABASE rather than a private schema, which is the exception
// testdb.PrivateDatabase exists for: the store opens a LISTEN connection, and
// notification channel names are scoped to the database rather than the schema.
func startEphemeralPostgres(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("pgx", testdb.PrivateDatabase(t, "ipam_store_"+testDBName(t)))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(context.Background(), schemaDDL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}

// testDBName derives a unique, legal identifier from the test name.
func testDBName(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, r := range strings.ToLower(t.Name()) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// TestMain removes the throwaway Postgres container testdb starts when
// IPAM_TEST_POSTGRES_DSN is unset. It is shared by every test in the package,
// so no single test can own its teardown.
func TestMain(m *testing.M) { testdb.TestMain(m) }
