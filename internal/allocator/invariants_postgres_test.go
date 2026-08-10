package allocator

// Which allocation invariants does the DATABASE enforce, and which are held up
// by application code alone? (#91)
//
// The axis was found twice in one afternoon from opposite directions:
//
//   - From the test side: within one address-space digest the exclusion
//     constraint enforces non-overlap whatever the search does, so a test
//     asserting non-overlap inside one digest proves nothing about our code.
//     Across digests nothing compares the rows, and only the search stands
//     between correct and wrong. #66's second defect lived exactly there.
//   - From the implementation side: PostgreSQL orders inet by network address
//     then mask length, so a block sorts below the bare address it starts at
//     and an ordered scan skipped blocks beginning at the cursor. Within one
//     address space the constraint caught it as a 23P01. Across address spaces
//     it would have been SILENT.
//
// The general form, and the reason this file exists:
//
//	TIER 1  enforced by a database constraint. Cannot fail quietly. A test
//	        asserting it is testing PostgreSQL, not us.
//	TIER 2  enforced only by application code. Fails silently. This is where
//	        the defects are.
//	TIER 3  enforced by nothing. Believed, asserted, and true only by accident
//	        of how the fixtures happen to be arranged.
//
// Nobody could previously say which was which. The point of the file is that
// after it, everybody can — and that moving between tiers becomes loud.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// TIER 1 — enforced by the database
// ---------------------------------------------------------------------------

// dbEnforced records a constraint, the exact definition it must still have, and
// the invariant it is claimed to back.
//
// `def` is compared verbatim against pg_get_constraintdef. That is deliberate
// and is the whole mechanism: a constraint that is DROPPED fails the existence
// check, and one that is NARROWED — a column removed from an exclusion, a CHECK
// weakened, ON DELETE RESTRICT relaxed to CASCADE — fails the definition check.
// Either way the invariant has silently moved from tier 1 to tier 2, and the
// test says so at the moment the migration lands rather than when something
// downstream breaks.
type dbEnforced struct {
	table string
	def   string
	rule  string
}

var dbEnforcedInvariants = map[string]dbEnforced{
	// --- the one this whole file is about -----------------------------------
	"ipam_cidr_alloc_no_overlap": {
		table: "ipam_cidr_allocations",
		def:   "EXCLUDE USING gist (pool_key WITH =, scope_digest WITH =, allocated_cidr inet_ops WITH &&)",
		rule: "Two allocations in the SAME pool and the SAME address space never overlap. " +
			"Note both equality columns: this says nothing about two rows in one pool whose " +
			"scope_digests differ, which is tier 2 below and is where #66 and the inet-ordering " +
			"bug both lived. pool_key leading is also the subject of #87.",
	},

	// --- identity and binding -----------------------------------------------
	"ipam_cidr_alloc_allocation_key_key": {
		table: "ipam_cidr_allocations",
		def:   "UNIQUE (allocation_key)",
		rule: "An allocation key names at most one row. This is what makes ErrAllocationExists " +
			"a server-side bookkeeping fault rather than a caller error — a caller never " +
			"chooses an allocation key.",
	},
	"ipam_cidr_allocations_claim_key_key": {
		table: "ipam_cidr_allocations",
		def:   "UNIQUE (claim_key)",
		rule: "A claim holds at most one CIDR allocation. NULL is exempt (Postgres UNIQUE " +
			"permits many NULLs), which is what lets retained and reserved rows exist " +
			"unbound — deliberate, and the reason retention is implemented as not-unbinding.",
	},
	"ipam_asn_allocations_claim_key_key": {
		table: "ipam_asn_allocations",
		def:   "UNIQUE (claim_key)",
		rule:  "A claim holds at most one ASN, with the same NULL exemption.",
	},
	"ipam_asn_allocations_pool_key_asn_key": {
		table: "ipam_asn_allocations",
		def:   "UNIQUE (pool_key, asn)",
		rule: "One ASN is handed out once per pool. Note this has NO scope_digest column, " +
			"unlike the CIDR case — ASNs are not carved into address spaces, so the ASN path " +
			"has no tier-2 cross-digest gap at all.",
	},

	// --- domains ------------------------------------------------------------
	"ipam_cidr_alloc_purpose_check": {
		table: "ipam_cidr_allocations",
		def:   "CHECK ((purpose = ANY (ARRAY['Claim'::text, 'Reservation'::text, 'PoolCarve'::text])))",
		rule: "purpose is one of three values. The search reads this column to decide whether a " +
			"row belongs to an address space at all, so a fourth value would be silently " +
			"treated as non-Claim by `purpose <> 'Claim'`.",
	},
	"ipam_cidr_allocations_ip_family_check": {
		table: "ipam_cidr_allocations",
		def:   "CHECK ((ip_family = ANY (ARRAY['IPv4'::text, 'IPv6'::text])))",
		rule:  "One address family per allocation row; dual-stack is two resources.",
	},
	"ipam_changelog_event_type_check": {
		table: "ipam_changelog",
		def:   "CHECK ((event_type = ANY (ARRAY['ADDED'::text, 'MODIFIED'::text, 'DELETED'::text])))",
		rule:  "Watch events are one of the three kinds a client can decode.",
	},

	// --- referential ---------------------------------------------------------
	"ipam_cidr_allocations_pool_key_fkey": {
		table: "ipam_cidr_allocations",
		def:   "FOREIGN KEY (pool_key) REFERENCES ipam_objects(key) ON DELETE RESTRICT",
		rule: "A pool cannot be deleted while it still holds allocations. RESTRICT rather than " +
			"CASCADE on purpose: deleting a pool must not silently destroy the record of " +
			"addresses somebody is using. This is also what forces the reclaim's delete order " +
			"(#88) — allocations first, objects second.",
	},
	"ipam_asn_allocations_pool_key_fkey": {
		table: "ipam_asn_allocations",
		def:   "FOREIGN KEY (pool_key) REFERENCES ipam_objects(key) ON DELETE RESTRICT",
		rule:  "Same, for ASN pools.",
	},
	"ipam_pool_identity_pool_fk": {
		table: "ipam_pool_identity",
		def:   "FOREIGN KEY (pool_key) REFERENCES ipam_objects(key) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED",
		rule: "An identity row never outlives the pool it names. DEFERRABLE because the cascade " +
			"writes the identity row before the object in one transaction; CASCADE because an " +
			"identity pointing at a deleted pool would wedge the scope forever.",
	},
	"ipam_pool_class_offer_pool_key_fkey": {
		table: "ipam_pool_class_offer",
		def:   "FOREIGN KEY (pool_key) REFERENCES ipam_objects(key) ON DELETE CASCADE",
		rule:  "Offer rows are a projection of a pool's spec and go when the pool does.",
	},
	"ipam_pool_search_floor_pool_key_fkey": {
		table: "ipam_pool_search_floor",
		def:   "FOREIGN KEY (pool_key) REFERENCES ipam_objects(key) ON DELETE CASCADE",
		rule:  "Search-floor rows are a cache keyed by pool and go when the pool does.",
	},

	// --- primary keys --------------------------------------------------------
	"ipam_objects_pkey":           {table: "ipam_objects", def: "PRIMARY KEY (key)", rule: "One object per storage key."},
	"ipam_cidr_allocations_pkey":  {table: "ipam_cidr_allocations", def: "PRIMARY KEY (id)", rule: "Surrogate key."},
	"ipam_asn_allocations_pkey":   {table: "ipam_asn_allocations", def: "PRIMARY KEY (id)", rule: "Surrogate key."},
	"ipam_changelog_pkey":         {table: "ipam_changelog", def: "PRIMARY KEY (id)", rule: "Surrogate key; also the watch cursor."},
	"ipam_pool_class_offer_pkey":  {table: "ipam_pool_class_offer", def: "PRIMARY KEY (pool_key, class_name)", rule: "A pool offers a class at most once."},
	"ipam_pool_search_floor_pkey": {table: "ipam_pool_search_floor", def: "PRIMARY KEY (pool_key, scope_digest)", rule: "One search floor per pool per address space."},
	"ipam_pool_identity_pkey": {
		table: "ipam_pool_identity",
		def:   "PRIMARY KEY (class_name, scope_digest)",
		rule: "EXACTLY ONE POOL PER (class, scope). This is the contention point the whole " +
			"cascade is built around: concurrent first-claims into one new scope all conflict " +
			"here, one wins, and the losers read the winner's pool.",
	},
	"ipam_pool_identity_pool_key_key": {
		table: "ipam_pool_identity",
		def:   "UNIQUE (pool_key) DEFERRABLE INITIALLY DEFERRED",
		rule: "One identity per pool. DEFERRABLE is load-bearing and looks removable: made " +
			"immediate it raises 23505 on roughly one concurrent claim in twenty, because " +
			"ON CONFLICT arbitrates only the index it names (migrations/002 header).",
	},
}

// liveConstraints reads every constraint on the ipam tables.
func liveConstraints(t *testing.T, db *pgxpool.Pool) map[string]dbEnforced {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT c.conname, t.relname, pg_get_constraintdef(c.oid)
		  FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		 WHERE t.relname LIKE 'ipam%' AND n.nspname = current_schema()`)
	if err != nil {
		t.Fatalf("read pg_constraint: %v", err)
	}
	defer rows.Close()

	out := map[string]dbEnforced{}
	for rows.Next() {
		var name, table, def string
		if err := rows.Scan(&name, &table, &def); err != nil {
			t.Fatalf("scan constraint: %v", err)
		}
		out[name] = dbEnforced{table: table, def: def}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate constraints: %v", err)
	}
	return out
}

// TestEveryDatabaseConstraintIsClassified is the completeness half: a migration
// that adds a constraint must say which invariant it backs.
//
// A new constraint is not a neutral event. It moves an invariant from tier 2 to
// tier 1 — which is good, and means the code-enforced list above it is now
// wrong, and means any test that was carefully exercising the code path is now
// also testing PostgreSQL.
func TestEveryDatabaseConstraintIsClassified(t *testing.T) {
	db := newMigratedPool(t)
	live := liveConstraints(t, db)

	if len(live) == 0 {
		t.Fatal("no constraints found on the ipam tables; the query is broken and this test " +
			"would pass against a schema with no constraints at all")
	}

	for name, got := range live {
		if _, ok := dbEnforcedInvariants[name]; !ok {
			t.Errorf("constraint %s on %s is not classified in invariants_postgres_test.go:\n"+
				"    %s\n"+
				"Say which invariant it backs. If it newly enforces something the code was "+
				"holding up alone, move that entry out of the code-enforced list too — a "+
				"rule in both places is a rule nobody knows the owner of.",
				name, got.table, got.def)
		}
	}
}

// TestClassifiedConstraintsStillBackTheirRule is the verification half.
//
// Existence is not enough. A constraint can be narrowed in place — a column
// dropped from an exclusion, a CHECK weakened, RESTRICT relaxed to CASCADE —
// and keep its name, so an existence check keeps passing while the invariant
// quietly moves to tier 2. Comparing the definition verbatim is what catches
// that, and it is the failure this whole file is about.
func TestClassifiedConstraintsStillBackTheirRule(t *testing.T) {
	db := newMigratedPool(t)
	live := liveConstraints(t, db)

	for name, want := range dbEnforcedInvariants {
		got, ok := live[name]
		if !ok {
			t.Errorf("constraint %s is gone. The invariant it backed is now enforced by "+
				"application code alone, if at all:\n    %s", name, want.rule)
			continue
		}
		if got.def != want.def {
			t.Errorf("constraint %s changed definition.\n  was:  %s\n  now:  %s\n"+
				"It backs: %s\nIf this is a deliberate widening, update the expectation. If it "+
				"is a narrowing, an invariant just moved from database-enforced to "+
				"code-enforced without anything else noticing.", name, want.def, got.def, want.rule)
		}
		if got.table != want.table {
			t.Errorf("constraint %s moved table: %s -> %s", name, want.table, got.table)
		}
	}
}

// ---------------------------------------------------------------------------
// TIER 2 — enforced only by application code
// ---------------------------------------------------------------------------

// TestCrossDigestOverlapIsNotEnforcedByTheDatabase makes tier-2 membership
// FALSIFIABLE rather than asserted.
//
// The claim "only the code enforces this" is usually a comment, and a comment
// cannot notice when it stops being true. This proves it by writing the
// violating rows and watching the database accept them — and asserts the
// same-digest case is rejected, so it cannot pass against a database with no
// constraints at all.
//
// The pair is the finding, stated as an executable difference:
//
//	same digest      -> 23P01, the constraint refuses it
//	different digest -> accepted, silently
//
// A PoolCarve overlapping a live Claim is the exact shape of #66's second
// defect and of the inet-ordering bug in the bounded search. Neither could have
// been caught by the database, and any test that "proved" non-overlap using one
// address space was proving something about PostgreSQL.
//
// If a future migration ever does enforce this, this test fails — and that
// failure is the signal to move the invariant to tier 1 and to stop relying on
// the search alone.
func TestCrossDigestOverlapIsNotEnforcedByTheDatabase(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()

	poolKey := platformKey("ippools", "tier2-pool-v4")
	seedObject(t, db, poolKey, "IPPool", "tier2-pool-v4", map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1", "kind": "IPPool",
		"metadata": map[string]any{"name": "tier2-pool-v4"},
		"spec":     map[string]any{"cidr": "10.240.0.0/20", "ipFamily": "IPv4"},
	})

	insert := func(allocKey, cidr, purpose, digest string) error {
		_, err := db.Exec(ctx,
			`INSERT INTO ipam_cidr_allocations
			   (allocation_key, pool_key, allocated_cidr, ip_family, purpose,
			    class_name, scope_digest, reclaim_policy)
			 VALUES ($1, $2, $3, 'IPv4', $4, '', $5, 'Delete')`,
			allocKey, poolKey, cidr, purpose, digest)
		return err
	}

	// A live claim in address space A.
	if err := insert("/alloc/tier2-claim", "10.240.0.0/32", "Claim", "digest-space-A"); err != nil {
		t.Fatalf("seed the claim: %v", err)
	}

	// Same address space, overlapping. The database must refuse this — the
	// positive control that proves the constraint is present and working, so
	// the acceptance below is a real gap rather than a schema that enforces
	// nothing.
	err := insert("/alloc/tier2-same-space", "10.240.0.0/28", "Claim", "digest-space-A")
	if err == nil {
		t.Fatal("an overlapping allocation in the SAME address space was accepted; " +
			"ipam_cidr_alloc_no_overlap is not doing its job, and the rest of this test " +
			"proves nothing")
	}
	if !strings.Contains(err.Error(), "23P01") && !strings.Contains(err.Error(), "exclusion") {
		t.Fatalf("same-space overlap failed for the wrong reason: %v", err)
	}

	// Different address space, overlapping the same live claim, recorded as a
	// PoolCarve — space that has really left the pool and must never overlap
	// ANY claim. The database has no opinion.
	if err := insert("/alloc/tier2-cross-space", "10.240.0.0/28", "PoolCarve", "digest-space-B"); err != nil {
		t.Fatalf("the cross-digest overlap was REFUSED: %v\n"+
			"That is good news and this test is now wrong: the invariant has moved from "+
			"code-enforced to database-enforced. Move it to dbEnforcedInvariants, name the "+
			"constraint, and delete this test.", err)
	}

	// State it plainly for whoever reads the output: the row is there, on top of
	// a live claim, and nothing complained.
	var overlapping int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND allocated_cidr >>= '10.240.0.0/32'::inet`, poolKey).Scan(&overlapping); err != nil {
		t.Fatalf("count overlapping rows: %v", err)
	}
	if overlapping != 2 {
		t.Errorf("rows covering 10.240.0.0 = %d, want 2 (the claim and the carve on top of it)",
			overlapping)
	}
}

// TestCarveMayOutliveItsPoolObjectAsFarAsTheSchemaCares is the second tier-2
// entry, and the one with a note already in migrations/003.
//
// ipam_cidr_allocations.pool_key has a foreign key; allocation_key has none. So
// nothing in the schema stops a carve from naming a child pool that no longer
// exists — the invariant lives entirely in the registry's delete paths, and one
// of them did not maintain it (DeleteCollection, rule 4). 003's Down section
// depends on the invariant for its backfill and says so.
//
// TestNoCarveOutlivesTheChildPoolItNames asserts the invariant HOLDS. This
// asserts that only our code makes it hold, which is the fact that decides how
// much a passing run of that test is worth.
func TestCarveMayOutliveItsPoolObjectAsFarAsTheSchemaCares(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()

	parent := platformKey("ippools", "tier2-parent-v4")
	seedObject(t, db, parent, "IPPool", "tier2-parent-v4", map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1", "kind": "IPPool",
		"metadata": map[string]any{"name": "tier2-parent-v4"},
		"spec":     map[string]any{"cidr": "10.241.0.0/20", "ipFamily": "IPv4"},
	})

	// A carve naming a child pool object that was never created at all.
	_, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		   (allocation_key, pool_key, allocated_cidr, ip_family, purpose,
		    class_name, scope_digest, reclaim_policy)
		 VALUES ($1, $2, '10.241.0.0/24', 'IPv4', 'PoolCarve', '', 'digest-x', 'Retain')`,
		platformKey("ippools", "tier2-child-that-does-not-exist"), parent)
	if err != nil {
		t.Fatalf("the schema refused a carve naming a nonexistent pool: %v\n"+
			"That means allocation_key gained a foreign key. Move this invariant to tier 1 "+
			"and delete this test — and check migrations/003's Down section, which reasons "+
			"about exactly this.", err)
	}

	// And the health query from verification-conventions 11a now finds it,
	// which is the observable cost of the gap.
	var orphans int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_cidr_allocations a
		  WHERE a.purpose = 'PoolCarve'
		    AND NOT EXISTS (SELECT 1 FROM ipam_objects o WHERE o.key = a.allocation_key)`,
	).Scan(&orphans); err != nil {
		t.Fatalf("orphan query: %v", err)
	}
	if orphans != 1 {
		t.Errorf("orphaned carves = %d, want 1: the row the schema just accepted", orphans)
	}
}

// ---------------------------------------------------------------------------
// TIER 3 — enforced by nothing
// ---------------------------------------------------------------------------
//
// Properties this service is believed to have and does not. Each was found by
// somebody assuming it, and each is true today only where the data happens to
// be arranged that way.
//
// The reason this is a separate tier rather than "more tier 2": a tier-2
// invariant has an owner you can go and read, so a defect in it is a bug in a
// named function. A tier-3 property has no owner at all, so there is nothing to
// review, nothing to break, and no test that could fail. It is discovered when
// somebody arranges the data the other way.
//
//  1. A ROOT POOL'S RANGE IS UNIQUE.
//     Refused for two roots of the SAME tenant on CREATE (rootoverlap.go, and
//     that fix is what #87 A/C shipped). Not refused across tenants — deliberate,
//     since private space is tenant-scoped and the digest carries the tenant.
//     Not refused for pools that ALREADY overlap: it is a create-time check that
//     touches no stored row, so a tenant carrying an overlapping pair keeps it.
//     loadtest's own fixtures had five overlapping roots and no assertion in the
//     suite could have falsified it, because every uniqueness check compares
//     within one pool. #87 B is the open decision.
//
//  2. AN ALLOCATION LIES INSIDE ITS POOL'S RANGE.
//     The search only ever chooses inside `parents`, and blockWithinAny rejects
//     an explicitly-named address outside them — so the write paths hold it. The
//     schema does not: allocated_cidr has no relationship to the pool object's
//     spec.cidr, which lives in JSON in another table. Anything writing this
//     table directly (a migration, a reclaim, a repro harness) can place an
//     allocation outside its pool and nothing will say so.
//
//  3. AN ALLOCATION'S claim_key NAMES A CLAIM THAT EXISTS.
//     pool_key has a foreign key; claim_key does not. Release deletes by
//     claim_key and the claim registry maintains the pairing, but nothing
//     prevents a row naming a claim object that was never created or has been
//     removed by another path. Same shape as the carve case above, which at
//     least has a test.
//
//  4. status.capacity AGREES WITH THE ALLOCATIONS IT SUMMARISES.
//     Recomputed by every allocator write path and by RefreshPoolCapacity, and
//     true immediately after each. Nothing re-derives it on a schedule or
//     asserts it continuously, so any path that changes allocations without
//     recomputing leaves the pool misreporting indefinitely — #47 was exactly
//     this, and #88's reclaim would have reintroduced it.
//
// Not listed, because they are tier 2 with a named owner rather than tier 3:
// cross-digest non-overlap (the search), carve/pool-object lifetime (the
// registry delete paths), reservation exclusion from every space (purpose).

// TestAnAllocationMayLieOutsideItsPoolAsFarAsTheSchemaCares makes tier-3 entry
// 2 executable, since it is the one most likely to be assumed by someone
// writing directly to this table — which is what a migration, the reclaim and
// the repro harness all do.
func TestAnAllocationMayLieOutsideItsPoolAsFarAsTheSchemaCares(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()

	poolKey := platformKey("ippools", "tier3-pool-v4")
	seedObject(t, db, poolKey, "IPPool", "tier3-pool-v4", map[string]any{
		"apiVersion": "ipam.miloapis.com/v1alpha1", "kind": "IPPool",
		"metadata": map[string]any{"name": "tier3-pool-v4"},
		"spec":     map[string]any{"cidr": "10.242.0.0/24", "ipFamily": "IPv4"},
	})

	// An address from a completely different range, recorded against this pool.
	_, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		   (allocation_key, pool_key, allocated_cidr, ip_family, purpose,
		    class_name, scope_digest, reclaim_policy)
		 VALUES ('/alloc/tier3-outside', $1, '192.0.2.7/32', 'IPv4', 'Claim', '', 'd', 'Delete')`,
		poolKey)
	if err != nil {
		t.Fatalf("the schema refused an allocation outside its pool's range: %v\n"+
			"Good news, and this test is now wrong: move the property to tier 1, name the "+
			"constraint, and delete this.", err)
	}

	// Stated as the operator would see it: the pool holds an address it does not
	// contain, and no error was raised at any layer.
	var outside int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND NOT (allocated_cidr <<= '10.242.0.0/24'::inet)`, poolKey).Scan(&outside); err != nil {
		t.Fatalf("count out-of-range rows: %v", err)
	}
	if outside != 1 {
		t.Errorf("out-of-range allocations = %d, want 1", outside)
	}
}
