// Package testschema gives a test its own PostgreSQL schema in a shared
// database, and is the single implementation of that for the whole repo.
//
// # Why this is its own package
//
// internal/testdb needs it, and so do the migration tests — but testdb imports
// migrations to run goose, so migrations cannot import testdb. That cycle is
// why a second, older copy of this logic lived in
// migrations/cascade_concurrency_test.go rather than through carelessness, and
// it is why the primitive lives here, in a package that imports neither.
//
// The duplication was not free. The btree_gist fix below reached only the
// testdb copy, so a migration test running alone still created the extension
// inside its own private schema and destroyed it on teardown — the exact defect
// the fix removed, surviving in the copy nobody edited. One implementation is
// the point.
//
// # Isolation is the point, not a nicety
//
// IPAM_TEST_POSTGRES_DSN is routinely pointed at a dev cluster's database, and
// residue left in a shared one is indistinguishable at a glance from the defect
// somebody else is mid-investigation on — that reading cost a round trip during
// the platform-as-a-project cutover (verification-conventions rule 11a).
package testschema

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver
)

// Prepare drops and recreates a private schema in the database named by base,
// registers its teardown, and returns a DSN scoped to it.
//
// schema must be unique per test. It is dropped on entry as well as on exit, so
// a previous run killed mid-test cannot leave a half-built schema that makes
// this one fail for a reason unrelated to what it tests.
func Prepare(t *testing.T, base, schema string) string {
	t.Helper()
	ctx := context.Background()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	// Announce the target before writing to it. "Which database did that just
	// write to" should not require reading the test.
	t.Logf("testschema: schema %q in the database named by IPAM_TEST_POSTGRES_DSN", schema)

	ensureSharedExtensions(t, admin)

	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Fatalf("drop schema %s: %v", schema, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", base)
		if err != nil {
			t.Logf("cleanup: reopen for DROP SCHEMA %s: %v", schema, err)
			return
		}
		defer func() { _ = db.Close() }()
		if _, err := db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("cleanup: DROP SCHEMA %s: %v", schema, err)
		}
	})

	return withSearchPath(t, base, schema)
}

// ensureSharedExtensions creates btree_gist in `public`, once, before any
// private schema is built.
//
// # Why this cannot be left to the migration
//
// migrations/002 says `CREATE EXTENSION IF NOT EXISTS btree_gist` with no
// SCHEMA clause, so the extension lands in the first schema on the search_path.
// Under this harness that is the caller's PRIVATE schema, and two facts follow
// that together break `go test ./...` against one database. Both were measured
// rather than reasoned about:
//
//  1. **pg_extension is global and unique on extname.** Two schemas cannot each
//     hold btree_gist. `CREATE EXTENSION IF NOT EXISTS` in schema A then schema
//     B reports success twice and leaves ONE row, in A.
//
//     Opclass resolution is NOT the problem — measured: a constraint in B builds
//     fine with the extension sitting in A, because Postgres finds the opclass by
//     type and access method rather than by search_path. An earlier version of
//     this comment claimed otherwise and was wrong.
//
//  2. **The real breakage is the teardown, and it is why an advisory lock around
//     creation is not enough.** `DROP SCHEMA s_a CASCADE` answers
//     `NOTICE: drop cascades to extension btree_gist` and pg_extension goes to
//     zero — so package A finishing destroys the extension package B is still
//     using, and CASCADE takes B's EXCLUDE constraint with it. That is a
//     destruction happening at teardown, outside any window a setup lock covers,
//     and it changes B's schema silently rather than erroring.
//
//     Without btree_gist the constraint cannot be built at all: migration 002
//     fails with "must specify an operator class", because the `text` column in
//     the EXCLUDE needs the gist opclass the extension supplies.
//
// Creating it once in `public`, which no test drops, removes the teardown
// hazard. The advisory lock covers the narrower create-vs-create race between
// packages starting together, where `IF NOT EXISTS` is not atomic and the loser
// gets a unique violation on pg_extension_name_index.
//
// search_path keeps `public` trailing the private schema. That is belt-and-braces
// rather than load-bearing, given opclass resolution does not need it, and it
// costs a fallback: a table missing from the private schema could in principle
// resolve to one in public. Nothing puts IPAM tables in public any more (#77).
func ensureSharedExtensions(t *testing.T, admin *sql.DB) {
	t.Helper()
	ctx := context.Background()

	// One writer at a time. `IF NOT EXISTS` is not atomic against a concurrent
	// create: both sessions see it absent and the loser gets a unique violation
	// on pg_extension_name_index. The key is arbitrary but must be shared by
	// every caller, so it is a fixed constant rather than derived per package.
	const extensionLockKey = 7318402931 // arbitrary, shared by every caller
	if _, err := admin.ExecContext(ctx, "SELECT pg_advisory_lock($1)", extensionLockKey); err != nil {
		t.Fatalf("acquire extension advisory lock: %v", err)
	}
	defer func() {
		if _, err := admin.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", extensionLockKey); err != nil {
			t.Logf("release extension advisory lock: %v", err)
		}
	}()

	if _, err := admin.ExecContext(ctx,
		"CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public"); err != nil {
		t.Fatalf("create btree_gist in public: %v\n\n"+
			"  The test database needs btree_gist, which migration 002 requires for the\n"+
			"  allocation EXCLUDE constraint. If this database's role cannot create\n"+
			"  extensions, create it once by hand:\n"+
			"      CREATE EXTENSION btree_gist WITH SCHEMA public;",
			err)
	}
}

// withSearchPath returns base with a search_path connection option, so EVERY
// connection opened from it starts in the named schema.
//
// It must be in the DSN string rather than a pgx ConnConfig because both
// consumers have to honour it: goose goes through sql.Open("pgx", …) and the
// pool through pgxpool. A per-connection `SET search_path` would cover only the
// connection that ran it, leaving the rest of a pool writing to public — the
// split-brain version of this bug rather than a fix for it.
func withSearchPath(t *testing.T, base, schema string) string {
	t.Helper()
	// public trails the private schema so btree_gist's operator class resolves
	// (see ensureSharedExtensions). CREATE lands in the first entry, so tables
	// still go to the private schema.
	opt := "options=-csearch_path%3D" + schema + ",public"

	u, err := url.Parse(base)
	if err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		if u.RawQuery == "" {
			u.RawQuery = opt
		} else {
			u.RawQuery += "&" + opt
		}
		return u.String()
	}
	// Keyword/value DSN form.
	if strings.Contains(base, "options=") {
		t.Fatalf("DSN already sets options=; cannot add search_path: %s", base)
	}
	return base + " options=-csearch_path=" + schema
}
