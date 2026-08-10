package allocator

// Reclaiming the unprefixed keyspace (#88).
//
// The fixture reproduces the shape measured on the dev cluster rather than a
// convenient one, because the awkward half is what the reclaim is for:
//
//   - residue pools carved out of OTHER residue pools (the cascade's own
//     levels), and
//   - residue pools carved out of a SURVIVING, operator-authored pool in the
//     platform keyspace.
//
// The second is the one that makes this more than tidying. Those carves hold
// real address space in a reachable pool, so deleting only the unreachable
// objects leaves rows behind that name nothing — the state
// TestNoCarveOutlivesTheChildPoolItNames forbids — and the surviving pool goes
// on reporting the space as allocated forever.
//
// Real Postgres because the delete ORDER is forced by a foreign key
// (ipam_cidr_allocations.pool_key is ON DELETE RESTRICT) and by an ON DELETE
// CASCADE on the identity table. Neither exists in a fake, so a fake would
// accept any order and prove nothing.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

const (
	survivingRootName = "reclaim-root-v6"
	survivingRootCIDR = "fd30::/20"
)

// seedUnprefixedResidue builds the measured shape: one surviving platform pool,
// two residue pools carved from it, and one residue pool carved from a residue
// pool.
func seedUnprefixedResidue(t *testing.T, db *pgxpool.Pool) (survivingKey string, residueKeys []string) {
	t.Helper()
	ctx := platformCtx()

	survivingKey = platformKey("ippools", survivingRootName)
	root := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: survivingRootName},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:     survivingRootCIDR,
			IPFamily: ipamv1alpha1.IPv6,
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: survivingRootCIDR,
			IPFamily:      ipamv1alpha1.IPv6,
		},
	}
	seedObject(t, db, survivingKey, "IPPool", survivingRootName, root)

	// Two levels of residue, exactly as the cascade writes them: level 1 carved
	// from the surviving root, level 2 carved from level 1.
	type residue struct {
		name, cidr, parentKey string
	}
	all := []residue{
		{"resid-net-a", "fd30::/48", survivingKey},
		{"resid-net-b", "fd30:0:1::/48", survivingKey},
	}

	for _, r := range all {
		key := "/ipam.miloapis.com/ippools/" + r.name
		obj := &ipamv1alpha1.IPPool{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
			ObjectMeta: metav1.ObjectMeta{Name: r.name},
			Spec: ipamv1alpha1.IPPoolSpec{
				IPFamily: ipamv1alpha1.IPv6,
				ClassRef: &ipamv1alpha1.LocalRef{Name: "resid-class"},
			},
			Status: ipamv1alpha1.IPPoolStatus{
				Phase:         ipamv1alpha1.PoolReady,
				AllocatedCIDR: r.cidr,
				IPFamily:      ipamv1alpha1.IPv6,
			},
		}
		seedObject(t, db, key, "IPPool", r.name, obj)
		residueKeys = append(residueKeys, key)

		// The carve against the parent, and the identity row the cascade claims.
		if _, err := db.Exec(ctx,
			`INSERT INTO ipam_cidr_allocations
			   (allocation_key, pool_key, allocated_cidr, ip_family, purpose, class_name, scope_digest, reclaim_policy)
			 VALUES ($1, $2, $3, 'IPv6', 'PoolCarve', 'resid-class', $4, 'Retain')`,
			key, r.parentKey, r.cidr, "digest-"+r.name); err != nil {
			t.Fatalf("seed carve for %s: %v", r.name, err)
		}
		if _, err := db.Exec(ctx,
			`INSERT INTO ipam_pool_identity (class_name, scope_digest, pool_key)
			 VALUES ('resid-class', $1, $2)`, "digest-"+r.name, key); err != nil {
			t.Fatalf("seed identity for %s: %v", r.name, err)
		}
	}

	// A level-2 residue pool carved out of a level-1 residue pool. This is the
	// row that makes the delete order matter: it references a residue pool in
	// pool_key, so the object delete is refused while it exists.
	child := "/ipam.miloapis.com/ippools/resid-subnet-a"
	childObj := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "resid-subnet-a"},
		Spec:       ipamv1alpha1.IPPoolSpec{IPFamily: ipamv1alpha1.IPv6},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: "fd30::/64",
			IPFamily:      ipamv1alpha1.IPv6,
		},
	}
	seedObject(t, db, child, "IPPool", "resid-subnet-a", childObj)
	residueKeys = append(residueKeys, child)
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		   (allocation_key, pool_key, allocated_cidr, ip_family, purpose, class_name, scope_digest, reclaim_policy)
		 VALUES ($1, $2, 'fd30::/64', 'IPv6', 'PoolCarve', 'resid-class', 'digest-subnet-a', 'Retain')`,
		child, residueKeys[0]); err != nil {
		t.Fatalf("seed level-2 carve: %v", err)
	}

	return survivingKey, residueKeys
}

func scanIn(t *testing.T, db *pgxpool.Pool) *UnprefixedResidue {
	t.Helper()
	ctx := platformCtx()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := ScanUnprefixed(ctx, tx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return res
}

// TestReclaimUnprefixedRemovesEverythingItNames is the whole-job assertion.
func TestReclaimUnprefixedRemovesEverythingItNames(t *testing.T) {
	db := newMigratedPool(t)
	survivingKey, residueKeys := seedUnprefixedResidue(t, db)
	ctx := platformCtx()

	// Positive control on the fixture. Without it, "everything is gone
	// afterwards" is equally true of a fixture that never wrote anything.
	before := scanIn(t, db)
	if before.Objects != len(residueKeys) {
		t.Fatalf("seeded objects = %d, want %d", before.Objects, len(residueKeys))
	}
	if len(before.SurvivingParents) != 1 || before.SurvivingParents[0] != survivingKey {
		t.Fatalf("surviving parents = %v, want [%s]", before.SurvivingParents, survivingKey)
	}
	if len(before.Sample) == 0 {
		t.Error("scan returned no sample keys; a count with no rows behind it is what started this task")
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := ReclaimUnprefixed(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("reclaim: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after := scanIn(t, db)
	if !after.Empty() {
		t.Errorf("residue remains after reclaim: %d objects, %d allocations",
			after.Objects, after.Allocations)
	}
	if after.IdentityRows != 0 {
		t.Errorf("identity rows = %d, want 0: they go by ON DELETE CASCADE with the objects",
			after.IdentityRows)
	}

	// The surviving pool must survive. A reclaim that took it too would pass
	// every emptiness check above.
	var survives int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_objects WHERE key = $1`, survivingKey).Scan(&survives); err != nil {
		t.Fatalf("check surviving pool: %v", err)
	}
	if survives != 1 {
		t.Fatalf("the surviving platform pool was deleted; the predicate must match only "+
			"the unprefixed keyspace (found %d)", survives)
	}

	// And no carve may outlive the pool it names — the invariant
	// TestNoCarveOutlivesTheChildPoolItNames asserts, which a reclaim that
	// deleted only objects would break wholesale.
	var orphans int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_cidr_allocations a
		  WHERE a.purpose = 'PoolCarve'
		    AND NOT EXISTS (SELECT 1 FROM ipam_objects o WHERE o.key = a.allocation_key)`,
	).Scan(&orphans); err != nil {
		t.Fatalf("check orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("orphaned carves = %d, want 0: a carve naming a deleted pool holds address "+
			"space with nothing left to release it", orphans)
	}
}

// TestReclaimRefreshesTheSurvivingParentsCapacity is the half that is easy to
// leave out, and it is what stops the cleanup tool reintroducing #47.
//
// The surviving pool's status.capacity counts the carves the reclaim releases.
// Nothing re-derives it until something else allocates from that pool, so
// without the refresh the pool reports space it no longer holds — indefinitely,
// and with no error anywhere.
func TestReclaimRefreshesTheSurvivingParentsCapacity(t *testing.T) {
	db := newMigratedPool(t)
	survivingKey, _ := seedUnprefixedResidue(t, db)
	ctx := platformCtx()

	// Put the surviving pool's status where a real one would be: capacity
	// computed while the residue carves were present.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := RefreshPoolCapacity(ctx, tx, survivingKey); err != nil {
		t.Fatalf("seed capacity: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	occupied := poolAllocatedFromStatus(t, db, survivingKey)
	if occupied == "0" {
		t.Fatal("the surviving pool reports nothing allocated before the reclaim; the fixture " +
			"must leave real carves in it or this test cannot see the refresh")
	}

	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := ReclaimUnprefixed(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("reclaim: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := poolAllocatedFromStatus(t, db, survivingKey); got != "0" {
		t.Errorf("surviving pool still reports %s addresses allocated after the reclaim "+
			"(was %s); the space was returned but status was not re-derived, so the pool "+
			"claims capacity it does not hold until something else happens to allocate from it",
			got, occupied)
	}
}

// poolAllocatedFromStatus reads status.capacity.allocated straight out of the
// stored object, which is what a client sees.
// poolAllocatedFromStatus reads status.capacity.allocated as a decimal string.
//
// NUMERIC rather than BIGINT, and a string rather than an int64, because the
// counts are exact now: a /20 of IPv6 holds 2^108 addresses and casting that to
// bigint overflows. They used to saturate at MaxInt64, which is what let this
// query assume 64 bits — the cast was correct only because the value had
// already been clamped to fit.
func poolAllocatedFromStatus(t *testing.T, db *pgxpool.Pool, poolKey string) string {
	t.Helper()
	var allocated string
	if err := db.QueryRow(platformCtx(),
		`SELECT COALESCE(NULLIF(ipam_data_to_jsonb(data)->'status'->'capacity'->>'allocated', ''), '0')
		   FROM ipam_objects WHERE key = $1`, poolKey).Scan(&allocated); err != nil {
		t.Fatalf("read capacity for %s: %v", poolKey, err)
	}
	return allocated
}

// TestReclaimLeavesTestResidueAlone.
//
// "/ippool/<name>" is written by this repository's own migration tests (#77)
// and no registry produces it. It looks close enough to the real unprefixed
// shape that confusing the two already cost an hour once, and a tool that
// deleted both would be a tool that removes real objects and a tool that tidies
// up after tests at the same time. Those want different levels of care.
func TestReclaimLeavesTestResidueAlone(t *testing.T) {
	db := newMigratedPool(t)
	seedUnprefixedResidue(t, db)
	ctx := platformCtx()

	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, namespace, name, data)
		 VALUES ('/ippool/leftover-1786236205011645000-0', 'IPPool', '', 'leftover', '{}'::bytea)`,
	); err != nil {
		t.Fatalf("seed test residue: %v", err)
	}

	if got := scanIn(t, db).Objects; got != 3 {
		t.Errorf("scan counted %d objects, want 3: the /ippool/ shape is not part of the "+
			"unprefixed keyspace and must not be swept in with it", got)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := ReclaimUnprefixed(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("reclaim: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var left int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_objects WHERE key LIKE '/ippool/%'`).Scan(&left); err != nil {
		t.Fatalf("count test residue: %v", err)
	}
	if left != 1 {
		t.Errorf("test-residue rows = %d, want 1 left untouched", left)
	}
}

// TestScanChangesNothing. The command runs the scan on both paths and only
// commits with --confirm, so the scan must be genuinely read-only.
func TestScanChangesNothing(t *testing.T) {
	db := newMigratedPool(t)
	seedUnprefixedResidue(t, db)

	before := scanIn(t, db)
	_ = scanIn(t, db)
	after := scanIn(t, db)

	if before.Objects != after.Objects || before.Allocations != after.Allocations {
		t.Errorf("scan mutated state: objects %d -> %d, allocations %d -> %d",
			before.Objects, after.Objects, before.Allocations, after.Allocations)
	}
	if after.Objects == 0 {
		t.Fatal("nothing to observe; the fixture failed")
	}
}
