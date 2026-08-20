package migrations_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/migrations"
)

func TestMain(m *testing.M) { testdb.TestMain(m) }

// A database written by the binary that took the kind from TypeMeta holds pools
// with an empty kind and an empty offer table. 004 has to repair both, and the
// offers are the part a kind-only UPDATE would not fix: the trigger is scoped to
// changes in the document, so nothing republishes them.
func TestBackfillRepairsPoolsWrittenWithNoKind(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()

	db, err := sql.Open("pgx", pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.DownTo(db, ".", 3); err != nil {
		t.Fatalf("roll back 004: %v", err)
	}

	doc, err := json.Marshal(map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPPool",
		"metadata":   map[string]any{"name": "root"},
		"spec": map[string]any{
			"cidr":       "10.0.0.0/16",
			"ipFamily":   "IPv4",
			"classNames": []string{"standard"},
		},
	})
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	const key = "project/datum-cloud/ipam.miloapis.com/ippools/root"
	if _, err := pool.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1, '', 'root', $2)`, key, doc); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	var offers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ipam_pool_class_offer`).Scan(&offers); err != nil {
		t.Fatalf("count offers: %v", err)
	}
	if offers != 0 {
		t.Fatalf("pre-migration offers = %d, want 0: the fixture no longer reproduces the broken state", offers)
	}

	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("apply 004: %v", err)
	}

	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM ipam_objects WHERE key = $1`, key).Scan(&kind); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kind != "IPPool" {
		t.Errorf("kind after migration = %q, want IPPool", kind)
	}

	var className string
	if err := pool.QueryRow(ctx,
		`SELECT class_name FROM ipam_pool_class_offer WHERE pool_key = $1`, key).Scan(&className); err != nil {
		t.Fatalf("read offer: %v", err)
	}
	if className != "standard" {
		t.Errorf("offer class = %q, want standard", className)
	}
}
