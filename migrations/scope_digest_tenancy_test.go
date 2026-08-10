package migrations_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/migrations"
)

// TestMigration005RefusesV1Digests exercises the guard in
// 005_scope_digest_tenancy.sql against the state it exists for.
//
// 005 changes the scope-digest encoding, and a v1 digest is indistinguishable
// from a v2 one — both are 64 hex characters, and nothing records which
// encoding wrote them. So there is no backfill, and a v1 row left in place is
// not inert: an identity row renumbers its scope, and an allocation row stops
// blocking the space it belongs to, which is two holders of one address.
//
// The guard is the only thing standing between a populated database and that
// outcome, so it is tested by producing the state rather than by reading the
// SQL. The database is migrated to 004, given one row, and then asked to go the
// rest of the way.
func TestMigration005RefusesV1Digests(t *testing.T) {
	ctx := context.Background()
	// A private schema, so this does not fight the other migration tests and so
	// the truncation below cannot reach real data.
	const schema = "migration_005_guard"
	db := testdb.RawDB(t, schema)

	// Up to 004 — the last version that wrote v1 digests.
	if err := goose.UpTo(db, ".", 4); err != nil {
		t.Fatalf("goose up to 004: %v", err)
	}

	// One pool object, one identity row against it. The object is required by
	// the identity table's foreign key, so this is the smallest population that
	// is actually reachable rather than the smallest that inserts.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ipam_objects (key, kind, namespace, name, data)
		 VALUES ('/ipam.miloapis.com/ippools/p', 'IPPool', '', 'p', '{}'::bytea)`,
	); err != nil {
		t.Fatalf("seed pool object: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ipam_pool_identity (class_name, scope_digest, pool_key)
		 VALUES ('c', 'e3c2bb77ee53dba0fd2bfae23530b5e487f017115ec74806bb60cc3f09daf3fa',
		         '/ipam.miloapis.com/ippools/p')`,
	); err != nil {
		t.Fatalf("seed identity row: %v", err)
	}

	// UpTo 5 rather than Up, throughout: this test is about what 005 does, and
	// running to head would have it assert against whatever the newest
	// migration left behind. 006 moves the same column default again, so an
	// unbounded Up made the check below read 006's value and fail — a stale
	// test reporting a healthy change as a defect.
	err := goose.UpTo(db, ".", 5)
	if err == nil {
		t.Fatal("005 applied over v1 digest rows; it must refuse, because leaving them " +
			"renumbers their scope and stops their allocations blocking the space they hold")
	}
	// The message has to name the counts, or an operator hitting this at deploy
	// time cannot tell a stray row from a populated production database.
	for _, want := range []string{"v1 scope digests", "pool identity row"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message does not mention %q:\n%v", want, err)
		}
	}

	// The positive control. Without it, a migration that failed for any other
	// reason — a syntax error, a missing table — reads as the guard working.
	if _, err := db.ExecContext(ctx, "DELETE FROM ipam_pool_identity"); err != nil {
		t.Fatalf("clear identity rows: %v", err)
	}
	if err := goose.UpTo(db, ".", 5); err != nil {
		t.Fatalf("005 must apply once the v1 rows are gone: %v", err)
	}

	// And the default it sets must be the digest the code produces for the
	// empty scope, which is asserted against internal/scope by
	// TestEmptyDigestMatchesMigrationDefault. Here it is only checked to have
	// actually moved off the v1 value.
	var def string
	if err := db.QueryRowContext(ctx,
		`SELECT column_default FROM information_schema.columns
		  WHERE table_schema = $1 AND table_name = 'ipam_cidr_allocations'
		    AND column_name = 'scope_digest'`, schema,
	).Scan(&def); err != nil {
		t.Fatalf("read column default: %v", err)
	}
	if strings.Contains(def, "e3c2bb77ee53dba0fd2bfae23530b5e487f017115ec74806bb60cc3f09daf3fa") {
		t.Errorf("scope_digest still defaults to the v1 empty digest: %s", def)
	}
	if !strings.Contains(def, "6139457f3fc41de42d41d373bf75cc032c63fbedb7def334f08f8b40803793d9") {
		t.Errorf("scope_digest default = %s, want the v2 empty digest", def)
	}
}

// TestMigration005NamesRealCleanupTasks keeps the task names in 005's hint
// honest.
//
// The hint tells an operator to run `task load:cleanup` — a concrete
// instruction, which is the point, because "delete the corresponding objects
// through the API" is the step someone improvises badly while a deployment is
// wedged. A concrete instruction that no longer resolves is worse than the
// vague one: it reads as authoritative and fails at the moment it is relied on,
// which is the same failure shape as a runbook step that can never match.
//
// It EXTRACTS the cited names rather than checking for a known list, and that
// is not a stylistic choice. The first version of this test asked whether the
// SQL contained "task load:cleanup" and whether the Taskfile defined `cleanup`.
// Renaming the citation to `task load:cleanupX` left it green, because the
// typo still contains the string it searched for — the check could only ever
// fail in one of the two directions it claimed to cover. Extracting the names
// makes a mistyped or invented citation fail as loudly as a renamed task.
//
// Nothing else couples the migrations to the Taskfile, so this is the only
// thing that would notice either. It needs no database — the point is the text.
//
// It walks EVERY migration rather than the one that first needed it. 005 was
// not the last migration to tell an operator to run a task: 006 does too, and
// naming a single file meant its citations were unchecked from the moment it
// landed. A check scoped to one instance of a recurring pattern is the "one
// entry point of several" failure in miniature.
func TestMigrationsNameRealCleanupTasks(t *testing.T) {
	taskfile, err := os.ReadFile(filepath.Join("..", "test", "load", "Taskfile.yaml"))
	if err != nil {
		t.Fatalf("read load Taskfile: %v", err)
	}
	// A task is defined at two-space indentation under the top-level `tasks:`
	// map, which is how every task in that file is written.
	defined := regexp.MustCompile(`(?m)^  ([a-zA-Z0-9-]+):`).FindAllStringSubmatch(string(taskfile), -1)
	have := make(map[string]bool, len(defined))
	for _, m := range defined {
		have[m[1]] = true
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		sql, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		cited := regexp.MustCompile(`task load:([a-zA-Z0-9-]+)`).FindAllStringSubmatch(string(sql), -1)
		for _, m := range cited {
			checked++
			if !have[m[1]] {
				t.Errorf("%s tells operators to run `task load:%s`, but "+
					"test/load/Taskfile.yaml defines no such task", e.Name(), m[1])
			}
		}
	}

	// A positive control: this test passes trivially if the citation pattern
	// stops matching — a reworded hint, a renamed prefix — and a silent zero is
	// indistinguishable from a clean run.
	if checked == 0 {
		t.Error("no `task load:<name>` citations found in any migration; either the hints were " +
			"reworded or this test's pattern has stopped matching, and it can no longer fail")
	}
}
