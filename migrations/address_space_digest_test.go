package migrations_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib"

	"go.miloapis.com/ipam/internal/testdb"
)

// The two digests 006 moves between. Spelled out rather than imported from
// internal/scope on purpose: this test is about what is IN the schema, and
// computing the expected value with the same function the migration's author
// used would make both sides derive from one source and prove nothing. The
// code-to-schema agreement is asserted separately, by
// TestEmptyDigestMatchesMigrationDefault in internal/scope.
const (
	v2EmptyPoolDigest         = "6139457f3fc41de42d41d373bf75cc032c63fbedb7def334f08f8b40803793d9"
	v3EmptyAddressSpaceDigest = "c86bbfc3761caa942844f05f5a8379f15cdd300f512a9d5b5baaa787c4695c42"
)

// migrationSchema gives one test its own schema, and is now a thin alias for
// testdb.RawDB — the repo's single database fixture. It used to hand-roll the
// schema and then `SET search_path` on a database/sql handle, which applies to
// one pooled connection rather than to the handle; the isolation now lives in
// the DSN, so every connection is covered.
func migrationSchema(t *testing.T, name string) *sql.DB {
	t.Helper()
	return testdb.RawDB(t, name)
}

// seedAllocation writes one row of the given purpose, plus the pool object its
// foreign key requires.
func seedAllocation(t *testing.T, db *sql.DB, name, purpose, digest, cidr string) {
	t.Helper()
	ctx := context.Background()
	poolKey := "/ipam.miloapis.com/ippools/" + name
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ipam_objects (key, kind, namespace, name, data)
		 VALUES ($1, 'IPPool', '', $2, '{}'::bytea) ON CONFLICT DO NOTHING`,
		poolKey, name,
	); err != nil {
		t.Fatalf("seed pool object %s: %v", name, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ipam_cidr_allocations
		     (allocation_key, pool_key, allocated_cidr, claim_key, ip_family, purpose, scope_digest)
		 VALUES ($1, $2, $3, $4, 'IPv4', $5, $6)`,
		"/alloc/"+name, poolKey, cidr, nil, purpose, digest,
	); err != nil {
		t.Fatalf("seed %s allocation: %v", purpose, err)
	}
}

// TestMigration006RefusesV2ClaimDigests exercises 006's guard against the state
// it exists for, and — the half that is easy to leave untested — against the
// states it must NOT refuse.
//
// 006 changes the encoding of the digest on Claim rows only. Pool identities,
// pool carves and reservations keep theirs, deliberately: a provisioned pool's
// name embeds its digest, so re-tagging that form would rename every cascade
// pool and renumber every scope. The guard therefore has to be narrower than
// 005's, and a guard that is too wide fails in a way nobody reports as a bug —
// it just refuses to deploy against a database that was fine.
func TestMigration006RefusesV2ClaimDigests(t *testing.T) {
	ctx := context.Background()
	db := migrationSchema(t, "migration_006_guard")

	// Up to 005 — the last version that wrote v2 digests onto Claim rows.
	if err := goose.UpTo(db, ".", 5); err != nil {
		t.Fatalf("goose up to 005: %v", err)
	}

	// The rows 006 must tolerate, seeded first so the refusal below cannot be
	// mistaken for a reaction to any row at all.
	seedAllocation(t, db, "carve-pool", "PoolCarve", v2EmptyPoolDigest, "10.0.0.0/24")
	seedAllocation(t, db, "res-pool", "Reservation", v2EmptyPoolDigest, "10.1.0.0/32")

	if err := goose.UpTo(db, ".", 6); err != nil {
		t.Fatalf("006 must apply over PoolCarve and Reservation rows — their encoding did not "+
			"change and refusing them would block a deploy against a healthy database: %v", err)
	}

	// The reservation backfill: constant to constant, the one rewrite in this
	// migration that is safe to perform.
	var resDigest string
	if err := db.QueryRowContext(ctx,
		`SELECT scope_digest FROM ipam_cidr_allocations WHERE purpose = 'Reservation'`,
	).Scan(&resDigest); err != nil {
		t.Fatalf("read reservation digest: %v", err)
	}
	if resDigest != v3EmptyAddressSpaceDigest {
		t.Errorf("reservation digest = %s, want the empty address-space digest %s: a reservation "+
			"belongs to no tenant's space and must participate in the exclusion constraint for "+
			"every one of them", resDigest, v3EmptyAddressSpaceDigest)
	}

	// The carve is left exactly as it was. This is the assertion that would
	// catch a migration that "helpfully" rewrote every row.
	var carveDigest string
	if err := db.QueryRowContext(ctx,
		`SELECT scope_digest FROM ipam_cidr_allocations WHERE purpose = 'PoolCarve'`,
	).Scan(&carveDigest); err != nil {
		t.Fatalf("read carve digest: %v", err)
	}
	if carveDigest != v2EmptyPoolDigest {
		t.Errorf("PoolCarve digest = %s, want it untouched at %s: a carve carries the child "+
			"pool's identity, whose encoding did not change", carveDigest, v2EmptyPoolDigest)
	}

	var def string
	if err := db.QueryRowContext(ctx,
		`SELECT column_default FROM information_schema.columns
		  WHERE table_schema = 'migration_006_guard' AND table_name = 'ipam_cidr_allocations'
		    AND column_name = 'scope_digest'`,
	).Scan(&def); err != nil {
		t.Fatalf("read column default: %v", err)
	}
	if !strings.Contains(def, v3EmptyAddressSpaceDigest) {
		t.Errorf("scope_digest default = %s, want the v3 empty address-space digest", def)
	}
}

// TestMigration006RefusesWhenClaimRowsExist is the other direction, in its own
// schema so it starts from a clean 005.
//
// Producing the state rather than reading the SQL: a guard nobody has watched
// fire is a comment.
func TestMigration006RefusesWhenClaimRowsExist(t *testing.T) {
	db := migrationSchema(t, "migration_006_claims")

	if err := goose.UpTo(db, ".", 5); err != nil {
		t.Fatalf("goose up to 005: %v", err)
	}

	// A RETAINED claim row — claim_key IS NULL — because that is the case with
	// no possible backfill even in Go: the IPClaim that carried the scope is
	// gone, so the digest cannot be recomputed from anything that still exists.
	seedAllocation(t, db, "claim-pool", "Claim", v2EmptyPoolDigest, "10.2.0.0/32")

	// UpTo 6 rather than Up, here and throughout. This test is about what 006
	// does, and running to head has it assert against whatever the newest
	// migration left behind — the staleness 005's test names in its own
	// comment. It stopped being hypothetical when 007 landed a guard that also
	// refuses unprefixed pool objects, which this test seeds to satisfy the
	// allocation foreign key: two of the three unbounded `Up` calls then failed
	// on 007 rather than on the thing they were asserting.
	//
	// This one did not, and the difference is worth recording because the
	// obvious reading is wrong. It is NOT the case that an unbounded Up here
	// could be satisfied by 007 firing in 006's place. Both halves were
	// measured:
	//
	//   * With 006's guard intact, goose runs 006 first and it refuses, so 007
	//     is never reached. Ordering alone decides it.
	//   * With 006's guard deliberately disabled, 007 does fire — and the
	//     message check below catches it, failing on both strings, because
	//     007's refusal says "unprefixed key space" and nothing about claims.
	//
	// So the bound is the right convention and the reason to keep it is
	// decoupling from future migrations, NOT that it closed a hole. The thing
	// that actually made this assertion safe is the message check: asserting
	// only `err != nil` would accept any migration failing for any reason.
	// Do not simplify that loop away.
	err := goose.UpTo(db, ".", 6)
	if err == nil {
		t.Fatal("006 applied over v2 Claim digests; it must refuse. Left in place, those rows " +
			"stop matching the address space they belong to and no longer block it — two " +
			"holders of one address, silently")
	}
	for _, want := range []string{"claim allocation row", "address-space digest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message does not mention %q:\n%v", want, err)
		}
	}

	// The positive control. Without it, a migration failing for any other
	// reason — a syntax error, a missing column — reads as the guard working.
	if _, err := db.Exec(`DELETE FROM ipam_cidr_allocations WHERE purpose = 'Claim'`); err != nil {
		t.Fatalf("clear claim rows: %v", err)
	}
	if err := goose.UpTo(db, ".", 6); err != nil {
		t.Fatalf("006 must apply once the Claim rows are gone: %v", err)
	}
}
