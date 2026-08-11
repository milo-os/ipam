package allocator

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// AllocatePrefix writes a row to ipam_cidr_allocations, and every NOT NULL
// column on that table has to be one the insert supplies. Nothing else in this
// package exercises the write, so a column added to the schema without a
// matching change here fails only in a running cluster.
func TestAllocatePrefixWritesEveryRequiredColumn(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	const cidr = "10.60.0.0/16"
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "alloc-required-columns"},
		Spec:       ipamv1alpha1.IPPoolSpec{CIDR: cidr, IPFamily: ipamv1alpha1.IPv4},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: ipamv1alpha1.IPv4,
		},
	}
	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	poolKey := tenant.Identity{Name: "platform"}.ResourceKey("ippools", pool.Name)
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		poolKey, pool.Name, data); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	claimKey := tenant.Identity{Name: "platform"}.ResourceKey("ipclaims", "c1")

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	got, err := NewPostgresPrefixAllocator().AllocatePrefix(ctx, tx, PrefixRequest{
		PoolKey: poolKey, PrefixLen: 24, IPFamily: "IPv4",
		ClaimKey: claimKey, AllocationKey: claimKey + "/alloc",
		OwnerProject: "platform", ClassName: "standard",
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("AllocatePrefix: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got == "" {
		t.Fatal("AllocatePrefix returned no CIDR")
	}

	// The row must be reachable by the identity the release path uses.
	var allocationKey string
	if err := db.QueryRow(ctx,
		`SELECT allocation_key FROM ipam_cidr_allocations WHERE claim_key = $1`,
		claimKey).Scan(&allocationKey); err != nil {
		t.Fatalf("read back allocation: %v", err)
	}
	if allocationKey == "" {
		t.Error("allocation_key is empty; the release path finds rows by it")
	}
}
