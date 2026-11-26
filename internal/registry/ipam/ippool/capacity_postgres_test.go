package ippool

import (
	"context"
	"testing"

	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// A pool's capacity is derived when it is read, not stored: carving a child
// out of a parent leaves the parent's row alone, and a Get or a List of the
// parent still reports the carve.
func TestPoolCapacityIsDerivedOnRead(t *testing.T) {
	db := testdb.Pool(t)
	s := newPostgresPoolStorage(t, db)
	ctx := poolCtx("tenant-a")

	if err := createPool(t, s, ctx, rootPoolObj("parent", "10.172.0.0/24", ipam.IPv4)); err != nil {
		t.Fatalf("create root pool: %v", err)
	}
	var rvBefore int64
	parentKey := poolStorageKey("tenant-a", "parent")
	if err := db.QueryRow(context.Background(),
		`SELECT resource_version FROM ipam_objects WHERE key = $1`, parentKey).Scan(&rvBefore); err != nil {
		t.Fatalf("read parent row: %v", err)
	}

	child := &ipam.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "child"},
		Spec:       ipam.IPPoolSpec{ParentPoolRef: &ipam.LocalRef{Name: "parent"}, PrefixLength: 26},
	}
	if err := createPool(t, s, ctx, child); err != nil {
		t.Fatalf("carve child pool: %v", err)
	}

	var rvAfter int64
	if err := db.QueryRow(context.Background(),
		`SELECT resource_version FROM ipam_objects WHERE key = $1`, parentKey).Scan(&rvAfter); err != nil {
		t.Fatalf("read parent row: %v", err)
	}
	if rvAfter != rvBefore {
		t.Errorf("the carve rewrote the parent pool (resource version %d -> %d)", rvBefore, rvAfter)
	}

	want := ipam.PoolCapacity{Total: "256", Allocated: "64", Available: "192"}
	obj, err := s.Get(ctx, "parent", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if got := obj.(*ipam.IPPool).Status; got.Capacity != want || got.UtilizationPercent != 25 {
		t.Errorf("Get: capacity = %+v at %v%%, want %+v at 25%%", got.Capacity, got.UtilizationPercent, want)
	}

	list, err := s.List(ctx, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, p := range list.(*ipam.IPPoolList).Items {
		if p.Name != "parent" {
			continue
		}
		found = true
		if p.Status.Capacity != want || p.Status.UtilizationPercent != 25 {
			t.Errorf("List: capacity = %+v at %v%%, want %+v at 25%%", p.Status.Capacity, p.Status.UtilizationPercent, want)
		}
	}
	if !found {
		t.Fatal("List did not return the parent pool")
	}
}
