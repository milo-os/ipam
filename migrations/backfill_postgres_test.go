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

// A pool provisioned by a class, or an allocation materialised by a claim, was
// written with neither metadata.uid nor metadata.creationTimestamp: both are
// stamped by the apiserver's create path, and neither object goes through it.
// 005 repairs them, taking the timestamp from the row's own created_at.
func TestBackfillStampsObjectMetadataSystemFields(t *testing.T) {
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
	if err := goose.DownTo(db, ".", 4); err != nil {
		t.Fatalf("roll back 005: %v", err)
	}

	// creationTimestamp: null is what a zero metav1.Time marshals to, so it is
	// what the unrepaired documents carry — not an absent key.
	unstamped, err := json.Marshal(map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPPool",
		"metadata":   map[string]any{"name": "provisioned", "creationTimestamp": nil},
		"spec":       map[string]any{"ipFamily": "IPv6", "prefixLength": 48},
	})
	if err != nil {
		t.Fatalf("marshal provisioned pool: %v", err)
	}
	stamped, err := json.Marshal(map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPPool",
		"metadata": map[string]any{
			"name":              "root",
			"uid":               "11111111-2222-3333-4444-555555555555",
			"creationTimestamp": "2026-09-18T21:30:23Z",
		},
		"spec": map[string]any{"cidr": "2001:db8::/32", "ipFamily": "IPv6"},
	})
	if err != nil {
		t.Fatalf("marshal root pool: %v", err)
	}

	const (
		unstampedKey = "project/datum-cloud/ipam.miloapis.com/ippools/provisioned"
		stampedKey   = "project/datum-cloud/ipam.miloapis.com/ippools/root"
	)
	const createdAt = "2026-09-19T08:00:00Z"
	if _, err := pool.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data, created_at)
		 VALUES ($1, 'IPPool', 'provisioned', $2, $3), ($4, 'IPPool', 'root', $5, NOW())`,
		unstampedKey, unstamped, createdAt, stampedKey, stamped); err != nil {
		t.Fatalf("seed pools: %v", err)
	}

	var rvBefore int64
	if err := pool.QueryRow(ctx,
		`SELECT resource_version FROM ipam_objects WHERE key = $1`, unstampedKey).Scan(&rvBefore); err != nil {
		t.Fatalf("read resource version: %v", err)
	}

	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("apply 005: %v", err)
	}

	var gotTimestamp, gotUID string
	var rvAfter int64
	if err := pool.QueryRow(ctx,
		`SELECT ipam_data_to_jsonb(data) -> 'metadata' ->> 'creationTimestamp',
		        ipam_data_to_jsonb(data) -> 'metadata' ->> 'uid',
		        resource_version
		   FROM ipam_objects WHERE key = $1`, unstampedKey,
	).Scan(&gotTimestamp, &gotUID, &rvAfter); err != nil {
		t.Fatalf("read repaired pool: %v", err)
	}
	if gotTimestamp != "2026-09-19T08:00:00Z" {
		t.Errorf("creationTimestamp = %q, want the row's created_at", gotTimestamp)
	}
	if gotUID == "" {
		t.Error("uid is still empty after the migration")
	}
	if rvAfter <= rvBefore {
		t.Errorf("resource version = %d, want greater than %d so watchers see the repair", rvAfter, rvBefore)
	}

	var events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ipam_changelog WHERE key = $1 AND event_type = 'MODIFIED'`,
		unstampedKey).Scan(&events); err != nil {
		t.Fatalf("count changelog: %v", err)
	}
	if events != 1 {
		t.Errorf("changelog rows for the repair = %d, want 1", events)
	}

	// A pool that already carried both keeps exactly what it had, and is not
	// reversioned.
	if err := pool.QueryRow(ctx,
		`SELECT ipam_data_to_jsonb(data) -> 'metadata' ->> 'creationTimestamp',
		        ipam_data_to_jsonb(data) -> 'metadata' ->> 'uid'
		   FROM ipam_objects WHERE key = $1`, stampedKey,
	).Scan(&gotTimestamp, &gotUID); err != nil {
		t.Fatalf("read untouched pool: %v", err)
	}
	if gotTimestamp != "2026-09-18T21:30:23Z" || gotUID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("already-stamped pool changed: timestamp %q, uid %q", gotTimestamp, gotUID)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ipam_changelog WHERE key = $1`, stampedKey).Scan(&events); err != nil {
		t.Fatalf("count changelog: %v", err)
	}
	if events != 0 {
		t.Errorf("changelog rows for the untouched pool = %d, want 0", events)
	}
}
