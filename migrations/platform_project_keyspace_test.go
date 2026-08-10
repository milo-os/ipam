package migrations_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/migrations"
)

// openMigratedSchema gives this test its own schema and migrates it to `upTo`.
//
// It used to open on the raw DSN and migrate whatever schema that landed in —
// in practice the shared `public` one, because nothing here ever isolated it.
func openMigratedSchema(t *testing.T, name string, upTo int64) *sql.DB {
	t.Helper()
	db := testdb.RawDB(t, name)
	if err := goose.UpTo(db, ".", upTo); err != nil {
		t.Fatalf("goose up to %03d: %v", upTo, err)
	}
	return db
}

// 007 guards the cutover to the platform-as-a-project key space.
//
// Objects at "/ipam.miloapis.com/..." are not merely stale after the cutover —
// they are unreachable. No code path builds that key any more, so the catalog
// reads as empty and every claim fails with "class not found" while the objects
// sit there in `SELECT * FROM ipam_objects`. That is the quietest possible
// version of this change going wrong, which is why it is a migration guard and
// not a release note.
func TestMigration007RefusesUnprefixedObjects(t *testing.T) {
	ctx := context.Background()
	db := openMigratedSchema(t, "migration_007_guard", 6)

	// A platform-owned pool and class at the old unprefixed keys, plus an offer
	// row referencing the pool. The offer row matters: its foreign key is not
	// ON UPDATE CASCADE, so it is the thing that turns the documented one-line
	// rewrite into a four-line one, and the message has to count it.
	for _, obj := range []struct{ key, kind, name string }{
		{"/ipam.miloapis.com/ippools/p", "IPPool", "p"},
		{"/ipam.miloapis.com/ipclasses/c", "IPClass", "c"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO ipam_objects (key, kind, namespace, name, data)
			 VALUES ($1, $2, '', $3, '{}'::bytea)`, obj.key, obj.kind, obj.name,
		); err != nil {
			t.Fatalf("seed %s: %v", obj.key, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ipam_pool_class_offer (pool_key, class_name)
		 VALUES ('/ipam.miloapis.com/ippools/p', 'c')`,
	); err != nil {
		t.Fatalf("seed offer row: %v", err)
	}

	err := goose.UpTo(db, ".", 7)
	if err == nil {
		t.Fatal("007 applied over unprefixed objects; it must refuse, because the " +
			"running code cannot address them and reads the catalog as empty")
	}

	// The message has to carry the counts and the remedy. An operator hitting
	// this at deploy time needs to tell a stray row from a populated database,
	// and needs the rewrite — 005 and 006 could only say "delete everything"
	// because their backfill was impossible; this one's is a string operation,
	// so refusing without the command would be withholding the answer.
	for _, want := range []string{
		"unprefixed key space",
		"--platform-project",
		// The load-bearing instruction, and the one this migration got wrong on
		// the first attempt: the constraint has to come OFF. "Update the child
		// columns in the same transaction" reads plausible and fails on the
		// first statement, because the foreign key is neither ON UPDATE CASCADE
		// nor DEFERRABLE.
		"ipam_pool_class_offer_pool_key_fkey",
		"ON UPDATE CASCADE",
		"007_platform_project_keyspace.sql",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message does not mention %q:\n%v", want, err)
		}
	}

	// The hint points at the file header for the exact statements rather than
	// embedding them, because SQL nested inside a RAISE HINT string is three
	// levels of quoting and is how the first version of this got its own
	// instruction wrong. A pointer is only better than prose if it resolves:
	// 005's test makes the same check for the task names in its hint, for the
	// same reason. A concrete instruction that has drifted is worse than a
	// vague one — it reads as authoritative and fails when it is relied on.
	header, rerr := migrations.FS.ReadFile("007_platform_project_keyspace.sql")
	if rerr != nil {
		t.Fatalf("read 007: %v", rerr)
	}
	for _, want := range []string{
		"ALTER TABLE ipam_pool_class_offer DROP CONSTRAINT ipam_pool_class_offer_pool_key_fkey;",
		"UPDATE ipam_objects",
		"UPDATE ipam_pool_class_offer",
		"ADD CONSTRAINT ipam_pool_class_offer_pool_key_fkey",
	} {
		if !strings.Contains(string(header), want) {
			t.Errorf("the hint sends an operator to this file's header, but it does not contain %q", want)
		}
	}

	// The positive control. Without it a migration that failed for any other
	// reason — a syntax error, a missing table — reads as the guard working.
	// The rewrite below is the one the hint prescribes, run verbatim against
	// the referencing table first.
	const platform = "milo-platform"
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin rewrite: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE ipam_pool_class_offer
		   DROP CONSTRAINT ipam_pool_class_offer_pool_key_fkey`); err != nil {
		t.Fatalf("drop offer fk: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ipam_objects SET key = 'project/' || $1 || '/' || ltrim(key, '/')
		  WHERE key LIKE '/ipam.miloapis.com/%'`, platform); err != nil {
		t.Fatalf("rewrite object keys: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ipam_pool_class_offer SET pool_key = 'project/' || $1 || '/' || ltrim(pool_key, '/')
		  WHERE pool_key LIKE '/ipam.miloapis.com/%'`, platform); err != nil {
		t.Fatalf("rewrite offer keys: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE ipam_pool_class_offer
		   ADD CONSTRAINT ipam_pool_class_offer_pool_key_fkey
		   FOREIGN KEY (pool_key) REFERENCES ipam_objects (key) ON DELETE CASCADE`); err != nil {
		t.Fatalf("restore offer fk: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rewrite: %v", err)
	}

	if err := goose.UpTo(db, ".", 7); err != nil {
		t.Fatalf("007 must apply once the objects have been re-homed: %v", err)
	}

	// And the rewrite must have preserved every object rather than dropped any.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ipam_objects WHERE key LIKE 'project/' || $1 || '/%'`, platform,
	).Scan(&n); err != nil {
		t.Fatalf("count re-homed objects: %v", err)
	}
	if n != 2 {
		t.Errorf("re-homed objects = %d, want 2", n)
	}
}

// A database with nothing in the unprefixed key space must migrate cleanly.
// This is the case every fresh install and every already-migrated deployment
// hits, so a guard that refuses here would block the change outright.
func TestMigration007AppliesToACleanDatabase(t *testing.T) {
	ctx := context.Background()
	db := openMigratedSchema(t, "migration_007_clean", 6)

	// A project-scoped object, to prove the guard keys off the unprefixed shape
	// rather than off "any object at all".
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ipam_objects (key, kind, namespace, name, data)
		 VALUES ('project/acme/ipam.miloapis.com/ippools/p', 'IPPool', '', 'p', '{}'::bytea)`,
	); err != nil {
		t.Fatalf("seed project-scoped pool: %v", err)
	}

	if err := goose.UpTo(db, ".", 7); err != nil {
		t.Fatalf("007 must apply to a database with no unprefixed objects: %v", err)
	}
}
