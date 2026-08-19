package allocator

import (
	"context"
	"sync"
	"testing"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// A claim of a chained class provisions the intermediate pool on first use and
// allocates out of it.
func TestFirstClaimProvisionsTheChain(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", "backbone", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 20, PoolPer: []string{"location"},
	})
	definition(t, tx, "platform", "subnets", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, ParentClassName: "backbone", DefaultPrefixLength: 24,
	})
	offerPool(t, tx, "platform", "root", "10.0.0.0/12", "backbone", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	claimScope := map[string]ipam.ScopeRef{"location": claimScopeRef("Location", "lon1")}

	read := begin(t, db)
	leaf, err := LoadClass(ctxIn("platform"), read, "subnets")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_ = read.Rollback(ctx)

	poolKey, err := ResolvePool(ctxIn("platform"), db, leaf, claimScope)
	if err != nil {
		t.Fatalf("ResolvePool: %v", err)
	}

	// The pool the claim draws from is the one backbone provisioned, carved out
	// of the operator's /12.
	var cidr, provisionedBy string
	if err := db.QueryRow(ctx,
		`SELECT ipam_data_to_jsonb(data) -> 'status' ->> 'allocatedCIDR',
		        ipam_data_to_jsonb(data) -> 'metadata' -> 'labels' ->> 'ipam.miloapis.com/provisioned-by'
		   FROM ipam_objects WHERE key = $1`, poolKey).Scan(&cidr, &provisionedBy); err != nil {
		t.Fatalf("read provisioned pool: %v", err)
	}
	if cidr != "10.0.0.0/20" {
		t.Errorf("provisioned pool holds %q, want the first /20 of the root", cidr)
	}
	if provisionedBy != "backbone" {
		t.Errorf("provisioned-by = %q, want backbone", provisionedBy)
	}

	// The carve against the root blocks EVERY address space, so it is recorded
	// as a PoolCarve rather than a Claim.
	var purpose string
	if err := db.QueryRow(ctx,
		`SELECT purpose FROM ipam_cidr_allocations WHERE allocation_key = $1`, poolKey).Scan(&purpose); err != nil {
		t.Fatalf("read carve: %v", err)
	}
	if purpose != "PoolCarve" {
		t.Errorf("carve recorded as %q, want PoolCarve; a Claim would block only its own space", purpose)
	}
}

// One pool per (class, scope), however many claims arrive at once. The identity
// row is the serialisation point; every loser reads the winner's key.
func TestAHerdOfFirstClaimsProducesOnePool(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(24))
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", "backbone", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 20, PoolPer: []string{"location"},
	})
	definition(t, tx, "platform", "subnets", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, ParentClassName: "backbone", DefaultPrefixLength: 24,
	})
	offerPool(t, tx, "platform", "root", "10.0.0.0/12", "backbone", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	read := begin(t, db)
	leaf, err := LoadClass(ctxIn("platform"), read, "subnets")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_ = read.Rollback(ctx)

	claimScope := map[string]ipam.ScopeRef{"location": claimScopeRef("Location", "lon1")}

	const herd = 16
	keys := make([]string, herd)
	errs := make([]error, herd)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range herd {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys[i], errs[i] = ResolvePool(ctxIn("platform"), db, leaf, claimScope)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	for i, k := range keys {
		if k != keys[0] {
			t.Errorf("caller %d resolved %q, caller 0 resolved %q", i, k, keys[0])
		}
	}

	var pools int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_objects
		  WHERE kind = 'IPPool'
		    AND ipam_data_to_jsonb(data) -> 'metadata' -> 'labels' ->> 'ipam.miloapis.com/provisioned-by' = 'backbone'`,
	).Scan(&pools); err != nil {
		t.Fatalf("count provisioned pools: %v", err)
	}
	if pools != 1 {
		t.Errorf("%d callers provisioned %d pools, want 1", herd, pools)
	}
}

// Each distinct scope gets its own pool, because that is what poolPer means.
func TestEachScopeGetsItsOwnPool(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", "backbone", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 20, PoolPer: []string{"location"},
	})
	definition(t, tx, "platform", "subnets", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, ParentClassName: "backbone", DefaultPrefixLength: 24,
	})
	offerPool(t, tx, "platform", "root", "10.0.0.0/12", "backbone", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	read := begin(t, db)
	leaf, err := LoadClass(ctxIn("platform"), read, "subnets")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_ = read.Rollback(ctx)

	lon, err := ResolvePool(ctxIn("platform"), db, leaf, map[string]ipam.ScopeRef{"location": claimScopeRef("Location", "lon1")})
	if err != nil {
		t.Fatalf("ResolvePool lon1: %v", err)
	}
	fra, err := ResolvePool(ctxIn("platform"), db, leaf, map[string]ipam.ScopeRef{"location": claimScopeRef("Location", "fra1")})
	if err != nil {
		t.Fatalf("ResolvePool fra1: %v", err)
	}
	if lon == fra {
		t.Errorf("both locations resolved to %q; poolPer must give each its own pool", lon)
	}
}

// A claim missing a role its chain needs is refused before anything is built,
// rather than leaving a half-provisioned chain behind.
func TestAClaimMissingAScopeRoleProvisionsNothing(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", "backbone", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 20, PoolPer: []string{"location"},
	})
	definition(t, tx, "platform", "subnets", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, ParentClassName: "backbone", DefaultPrefixLength: 24,
	})
	offerPool(t, tx, "platform", "root", "10.0.0.0/12", "backbone", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	read := begin(t, db)
	leaf, err := LoadClass(ctxIn("platform"), read, "subnets")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_ = read.Rollback(ctx)

	if _, err := ResolvePool(ctxIn("platform"), db, leaf, nil); err == nil {
		t.Fatal("a claim with no location resolved a pool")
	}

	var pools int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_pool_identity`).Scan(&pools); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if pools != 0 {
		t.Errorf("a rejected claim left %d pool identities behind", pools)
	}
}
