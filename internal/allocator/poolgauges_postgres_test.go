package allocator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// The gauges come from what each pool row commits, and nothing else: its key,
// its family, and its capacity counts. A pool whose capacity was never computed
// is left out rather than exported as empty, and other kinds are ignored.
func TestLoadPoolGaugesReadsCommittedCapacity(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	insert := func(kind, name string, obj any) string {
		t.Helper()
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		key := tenant.Identity{Name: "p1"}.ResourceKey(strings.ToLower(kind)+"s", name)
		if _, err := db.Exec(ctx,
			`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,$2,$3,$4)`,
			key, kind, name, data); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return key
	}
	pool := func(name, family string, capacity ipamv1alpha1.PoolCapacity) *ipamv1alpha1.IPPool {
		return &ipamv1alpha1.IPPool{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       ipamv1alpha1.IPPoolSpec{IPFamily: ipamv1alpha1.IPFamily(family)},
			Status:     ipamv1alpha1.IPPoolStatus{Capacity: capacity},
		}
	}

	v4 := insert("IPPool", "v4", pool("v4", "IPv4", ipamv1alpha1.PoolCapacity{Total: "256", Allocated: "64", Available: "192"}))
	// 2^108: far past int64, which is why the counts are strings.
	v6 := insert("IPPool", "v6", pool("v6", "IPv6", ipamv1alpha1.PoolCapacity{Total: "324518553658426726783156020576256", Allocated: "0"}))
	insert("IPPool", "uncomputed", pool("uncomputed", "IPv4", ipamv1alpha1.PoolCapacity{}))
	insert("IPClass", "not-a-pool", map[string]any{"kind": "IPClass", "status": map[string]any{"capacity": map[string]string{"total": "1"}}})

	got, err := LoadPoolGauges(ctx, db)
	if err != nil {
		t.Fatalf("LoadPoolGauges: %v", err)
	}
	want := []metrics.PoolGauge{
		{PoolKey: v4, IPFamily: "IPv4", Resource: "ippools", Project: "p1", Total: 256, Allocated: 64},
		{PoolKey: v6, IPFamily: "IPv6", Resource: "ippools", Project: "p1", Total: 324518553658426726783156020576256, Allocated: 0},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("gauges =\n  %+v\nwant\n  %+v", got, want)
	}
}

// The read runs every 15s on every replica. Its cost at 10,000 pools of a
// realistic size, with managed fields, is measured here and logged; the
// assertion is only a ceiling that catches a read gone pathological.
func TestLoadPoolGaugesStaysCheapAtTenThousandPools(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 10,000 pools")
	}
	db := testdb.Pool(t)
	ctx := context.Background()

	const pools = 10000
	// About 2.5KB per row: what an applied pool carries once managedFields,
	// conditions and labels are on it.
	padding := strings.Repeat(`{"manager":"kubectl","operation":"Apply","fieldsV1":{"f:spec":{"f:cidr":{},"f:classNames":{}}}},`, 20)
	if _, err := db.Exec(ctx, `
		INSERT INTO ipam_objects (key, kind, name, data)
		SELECT 'projects/p'||i||'/ippools/pool-'||i, 'IPPool', 'pool-'||i,
		       convert_to(json_build_object(
		         'kind', 'IPPool',
		         'metadata', json_build_object('name', 'pool-'||i, 'managedFields', ('['||$1||'{}]')::json),
		         'spec', json_build_object('cidr', '10.0.0.0/16', 'ipFamily', 'IPv4'),
		         'status', json_build_object('ipFamily', 'IPv4',
		           'capacity', json_build_object('total', '65536', 'allocated', (i % 65536)::text, 'available', '1'))
		       )::text, 'UTF8')
		  FROM generate_series(1, $2) AS i`, padding, pools); err != nil {
		t.Fatalf("seed pools: %v", err)
	}
	if _, err := db.Exec(ctx, `ANALYZE ipam_objects`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var size int
	if err := db.QueryRow(ctx, `SELECT avg(length(data))::int FROM ipam_objects WHERE kind = 'IPPool'`).Scan(&size); err != nil {
		t.Fatalf("row size: %v", err)
	}

	const runs = 5
	var total time.Duration
	for range runs {
		start := time.Now()
		got, err := LoadPoolGauges(ctx, db)
		total += time.Since(start)
		if err != nil {
			t.Fatalf("LoadPoolGauges: %v", err)
		}
		if len(got) != pools {
			t.Fatalf("read %d pools, want %d", len(got), pools)
		}
	}
	avg := total / runs
	t.Logf("%d pools, %d bytes per row: %v per read, %.2f%% of a 15s interval", pools, size, avg, 100*avg.Seconds()/15)
	if avg > 5*time.Second {
		t.Errorf("one read of %d pools took %v", pools, avg)
	}
}
