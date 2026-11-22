package migrations_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/registry/ipam/ipclass"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/migrations"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

const classKeyPrefix = "project/datum-cloud/ipam.miloapis.com/ipclasses/"

// openAtVersion returns a database/sql handle on pool's database with the
// migrations rolled back to version.
func openAtVersion(t *testing.T, pool *pgxpool.Pool, version int64) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.DownTo(db, ".", version); err != nil {
		t.Fatalf("roll back to %d: %v", version, err)
	}
	return db
}

func seedObject(t *testing.T, pool *pgxpool.Pool, key, kind, name string, doc map[string]any) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1, $2, $3, $4)`,
		key, kind, name, data); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

func classDoc(name string, status any) map[string]any {
	doc := map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPClass",
		"metadata": map[string]any{
			"name":              name,
			"uid":               "uid-" + name,
			"creationTimestamp": "2026-08-01T10:00:00Z",
			"labels":            map[string]any{"team": "network"},
			"annotations":       map[string]any{"note": "keep me"},
		},
		"spec": map[string]any{"ipFamily": "IPv6", "prefixLength": 48},
	}
	if status != nil {
		doc["status"] = status
	}
	return doc
}

type objectRow struct {
	rv   int64
	data []byte
}

func readObject(t *testing.T, pool *pgxpool.Pool, key string) objectRow {
	t.Helper()
	var r objectRow
	if err := pool.QueryRow(context.Background(),
		`SELECT resource_version, data FROM ipam_objects WHERE key = $1`, key).Scan(&r.rv, &r.data); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return r
}

func changelogRows(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_changelog WHERE key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("count changelog for %s: %v", key, err)
	}
	return n
}

// withoutStatus is the document with status removed, for comparing everything
// the migration must not touch.
func withoutStatus(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	delete(m, "status")
	return m
}

// A class created before admission stamped its status is stored as
// {phase: Pending} with no conditions and is never advanced. 006 rewrites
// exactly that shape to what admission stamps now, and nothing else.
func TestBackfillMarksLegacyClassesAccepted(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	db := openAtVersion(t, pool, 5)

	acceptedBefore := map[string]any{
		"phase": "Ready",
		"conditions": []any{map[string]any{
			"type": "Accepted", "status": "True", "reason": "ClassAccepted",
			"message":            "IPClass passed validation and is available to claims",
			"lastTransitionTime": "2026-09-01T00:00:00Z",
		}},
	}
	otherCondition := []any{map[string]any{
		"type": "Accepted", "status": "False", "reason": "SomethingElse",
		"message": "set by another writer", "lastTransitionTime": "2026-09-01T00:00:00Z",
	}}

	legacy := []string{"legacy-pending", "legacy-no-status", "legacy-null-conditions"}
	untouched := []string{"ready", "error-with-conditions", "pending-with-conditions", "ready-no-conditions"}

	seedObject(t, pool, classKeyPrefix+"legacy-pending", "IPClass", "legacy-pending",
		classDoc("legacy-pending", map[string]any{"phase": "Pending"}))
	seedObject(t, pool, classKeyPrefix+"legacy-no-status", "IPClass", "legacy-no-status",
		classDoc("legacy-no-status", nil))
	seedObject(t, pool, classKeyPrefix+"legacy-null-conditions", "IPClass", "legacy-null-conditions",
		classDoc("legacy-null-conditions", map[string]any{"phase": "Pending", "conditions": nil}))
	seedObject(t, pool, classKeyPrefix+"ready", "IPClass", "ready",
		classDoc("ready", acceptedBefore))
	seedObject(t, pool, classKeyPrefix+"error-with-conditions", "IPClass", "error-with-conditions",
		classDoc("error-with-conditions", map[string]any{"phase": "Error", "conditions": otherCondition}))
	seedObject(t, pool, classKeyPrefix+"pending-with-conditions", "IPClass", "pending-with-conditions",
		classDoc("pending-with-conditions", map[string]any{"phase": "Pending", "conditions": otherCondition}))
	seedObject(t, pool, classKeyPrefix+"ready-no-conditions", "IPClass", "ready-no-conditions",
		classDoc("ready-no-conditions", map[string]any{"phase": "Ready"}))
	// A pool reports a phase too; the migration is scoped to classes.
	const poolKey = "project/datum-cloud/ipam.miloapis.com/ippools/pending-pool"
	seedObject(t, pool, poolKey, "IPPool", "pending-pool", map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind":       "IPPool",
		"metadata":   map[string]any{"name": "pending-pool"},
		"spec":       map[string]any{"cidr": "10.0.0.0/16", "ipFamily": "IPv4"},
		"status":     map[string]any{"phase": "Pending"},
	})

	before := map[string]objectRow{}
	for _, name := range append(append([]string{}, legacy...), untouched...) {
		before[name] = readObject(t, pool, classKeyPrefix+name)
	}
	poolBefore := readObject(t, pool, poolKey)

	startedAt := time.Now().Add(-time.Minute)
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("apply 006: %v", err)
	}

	want := ipclass.AcceptedStatus(metav1.Now())
	for _, name := range legacy {
		key := classKeyPrefix + name
		after := readObject(t, pool, key)

		var got v1alpha1.IPClass
		if err := json.Unmarshal(after.data, &got); err != nil {
			t.Fatalf("%s: decode repaired class: %v", name, err)
		}
		if string(got.Status.Phase) != string(want.Phase) {
			t.Errorf("%s: phase = %q, want %q", name, got.Status.Phase, want.Phase)
		}
		if len(got.Status.Conditions) != 1 {
			t.Fatalf("%s: conditions = %+v, want one", name, got.Status.Conditions)
		}
		gc, wc := got.Status.Conditions[0], want.Conditions[0]
		if gc.Type != wc.Type || gc.Status != wc.Status || gc.Reason != wc.Reason || gc.Message != wc.Message {
			t.Errorf("%s: condition = %+v, want what ipclass.AcceptedStatus builds: %+v", name, gc, wc)
		}
		if gc.LastTransitionTime.Time.Before(startedAt.Truncate(time.Second)) {
			t.Errorf("%s: lastTransitionTime = %v, want the time of the repair", name, gc.LastTransitionTime)
		}

		if !reflect.DeepEqual(withoutStatus(t, after.data), withoutStatus(t, before[name].data)) {
			t.Errorf("%s: spec or metadata changed:\nbefore %s\nafter  %s", name, before[name].data, after.data)
		}
		if after.rv <= before[name].rv {
			t.Errorf("%s: resource version = %d, want greater than %d so watchers see the repair", name, after.rv, before[name].rv)
		}

		var events int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM ipam_changelog WHERE key = $1 AND event_type = 'MODIFIED' AND resource_version = $2`,
			key, after.rv).Scan(&events); err != nil {
			t.Fatalf("%s: count changelog: %v", name, err)
		}
		if events != 1 {
			t.Errorf("%s: MODIFIED changelog rows at the new resource version = %d, want 1", name, events)
		}
	}

	for _, name := range untouched {
		after := readObject(t, pool, classKeyPrefix+name)
		if after.rv != before[name].rv || string(after.data) != string(before[name].data) {
			t.Errorf("%s: changed by the migration:\nbefore rv %d %s\nafter  rv %d %s",
				name, before[name].rv, before[name].data, after.rv, after.data)
		}
		if n := changelogRows(t, pool, classKeyPrefix+name); n != 0 {
			t.Errorf("%s: changelog rows = %d, want 0", name, n)
		}
	}
	if after := readObject(t, pool, poolKey); after.rv != poolBefore.rv || string(after.data) != string(poolBefore.data) {
		t.Errorf("pool with a Pending phase was changed: %s", after.data)
	}

	// Running it again changes nothing. Down is a no-op, so this re-applies
	// the same statement to the repaired rows.
	repaired := map[string]objectRow{}
	for _, name := range append(append([]string{}, legacy...), untouched...) {
		repaired[name] = readObject(t, pool, classKeyPrefix+name)
	}
	var changelogBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ipam_changelog`).Scan(&changelogBefore); err != nil {
		t.Fatalf("count changelog: %v", err)
	}
	if err := goose.DownTo(db, ".", 5); err != nil {
		t.Fatalf("roll back 006: %v", err)
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("re-apply 006: %v", err)
	}
	for name, r := range repaired {
		if after := readObject(t, pool, classKeyPrefix+name); after.rv != r.rv || string(after.data) != string(r.data) {
			t.Errorf("%s: changed by the second run: rv %d -> %d", name, r.rv, after.rv)
		}
	}
	var changelogAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ipam_changelog`).Scan(&changelogAfter); err != nil {
		t.Fatalf("count changelog: %v", err)
	}
	if changelogAfter != changelogBefore {
		t.Errorf("second run wrote %d changelog rows, want 0", changelogAfter-changelogBefore)
	}
}

// Two replicas starting together each run the migration. The second must wait
// on the first's row lock, find the class already repaired, and write nothing.
func TestBackfillClassStatusConcurrentRunsWriteOnce(t *testing.T) {
	pool := testdb.Pool(t, testdb.MaxConns(4))
	ctx := context.Background()
	openAtVersion(t, pool, 5)

	const key = classKeyPrefix + "legacy"
	seedObject(t, pool, key, "IPClass", "legacy", classDoc("legacy", map[string]any{"phase": "Pending"}))

	stmt := upStatement(t)

	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	tag, err := first.Exec(ctx, stmt)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("first run wrote %d changelog rows, want 1", tag.RowsAffected())
	}

	type result struct {
		rows int64
		err  error
	}
	second := make(chan result, 1)
	go func() {
		tag, err := pool.Exec(ctx, stmt)
		second <- result{tag.RowsAffected(), err}
	}()

	// Commit only once the second run is blocked on the row, so the test
	// exercises the re-check rather than two runs that never overlapped.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("poll locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second run never blocked on the first one's row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit first run: %v", err)
	}

	r := <-second
	if r.err != nil {
		t.Fatalf("second run: %v", r.err)
	}
	if r.rows != 0 {
		t.Errorf("second run wrote %d changelog rows, want 0", r.rows)
	}
	if n := changelogRows(t, pool, key); n != 1 {
		t.Errorf("changelog rows = %d, want exactly one write", n)
	}
}

// upStatement is the single Up statement of 006, as goose would run it.
func upStatement(t *testing.T) string {
	t.Helper()
	src, err := migrations.FS.ReadFile("006_class_accepted_status.sql")
	if err != nil {
		t.Fatalf("read 006: %v", err)
	}
	s := string(src)
	_, after, ok := strings.Cut(s, "-- +goose StatementBegin\n")
	if !ok {
		t.Fatal("006 has no StatementBegin")
	}
	stmt, _, ok := strings.Cut(after, "-- +goose StatementEnd")
	if !ok {
		t.Fatal("006 has no StatementEnd")
	}
	return stmt
}
