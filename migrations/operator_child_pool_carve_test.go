package migrations_test

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// 008 reclassifies the carves the IPPool registry wrote as 'Claim' (#66).
//
// The Go fix only changes what is written from now on. Every child pool an
// operator created before it still holds a row saying 'Claim', and the search
// reads that column rather than the code — so without the backfill the parent
// keeps handing out addresses inside those pools indefinitely, on a service
// whose binary contains the fix. That is the failure this test exists for, and
// it is invisible from the application side.
//
// Three populations, because the discriminator has to separate them and a test
// that only seeds the guilty row passes against `UPDATE ... SET purpose =
// 'PoolCarve'` with no WHERE clause at all.
func TestMigration008ReclassifiesOperatorChildPoolCarves(t *testing.T) {
	ctx := context.Background()
	db := openMigratedSchema(t, "migration_008_guard", 7)

	// A parent pool, a child pool, and an IPAllocation. The child pool and the
	// allocation are what the two allocation rows below name.
	for _, obj := range []struct{ key, kind, name string }{
		{"project/plat/ipam.miloapis.com/ippools/parent", "IPPool", "parent"},
		{"project/plat/ipam.miloapis.com/ippools/child", "IPPool", "child"},
		{"project/tenant-a/ipam.miloapis.com/ipallocations/a1", "IPAllocation", "a1"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO ipam_objects (key, kind, namespace, name, data)
			 VALUES ($1, $2, '', $3, '{}'::bytea)`, obj.key, obj.kind, obj.name,
		); err != nil {
			t.Fatalf("seed %s: %v", obj.key, err)
		}
	}

	// Three rows against the parent:
	//
	//   child   — the defect. An operator-created child pool's carve, written by
	//             AllocatePrefix and so labelled 'Claim'. Must become PoolCarve.
	//   a1      — a real claim, also labelled 'Claim'. Must stay 'Claim'; it is
	//             the row a WHERE-less UPDATE would destroy.
	//   #res/0  — an edge reservation. Names no object at all, which is what
	//             distinguishes it from a carve, and must stay 'Reservation'.
	//
	// Distinct CIDRs: the exclusion constraint compares within a
	// (pool_key, scope_digest) and these share one, so overlapping fixtures
	// would fail at INSERT for a reason that has nothing to do with 008.
	rows := []struct{ allocKey, cidr, purpose string }{
		{"project/plat/ipam.miloapis.com/ippools/child", "10.204.0.0/28", "Claim"},
		{"project/tenant-a/ipam.miloapis.com/ipallocations/a1", "10.204.0.16/32", "Claim"},
		{"project/plat/ipam.miloapis.com/ippools/parent#reservation/0", "10.204.0.255/32", "Reservation"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO ipam_cidr_allocations
			   (allocation_key, pool_key, allocated_cidr, ip_family, purpose, class_name, scope_digest, reclaim_policy)
			 VALUES ($1, 'project/plat/ipam.miloapis.com/ippools/parent', $2, 'IPv4', $3, '', 'digest-x', 'Retain')`,
			r.allocKey, r.cidr, r.purpose,
		); err != nil {
			t.Fatalf("seed allocation %s: %v", r.allocKey, err)
		}
	}

	if err := goose.UpTo(db, ".", 8); err != nil {
		t.Fatalf("goose up to 008: %v", err)
	}

	for _, want := range []struct{ allocKey, purpose, why string }{
		{
			"project/plat/ipam.miloapis.com/ippools/child", "PoolCarve",
			"a row whose allocation_key names an IPPool object is a child pool's carve, and " +
				"labelled 'Claim' it is withheld only from claims sharing its digest",
		},
		{
			"project/tenant-a/ipam.miloapis.com/ipallocations/a1", "Claim",
			"a real claim's allocation_key names an IPAllocation; reclassifying it would make " +
				"one tenant's address block every other tenant's",
		},
		{
			"project/plat/ipam.miloapis.com/ippools/parent#reservation/0", "Reservation",
			"an edge reservation names no object, and the pool delete guard is the one reader " +
				"that has to tell it apart from a carve",
		},
	} {
		var got string
		if err := db.QueryRowContext(ctx,
			`SELECT purpose FROM ipam_cidr_allocations WHERE allocation_key = $1`, want.allocKey,
		).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", want.allocKey, err)
		}
		if got != want.purpose {
			t.Errorf("%s: purpose = %q, want %q — %s", want.allocKey, got, want.purpose, want.why)
		}
	}
}
