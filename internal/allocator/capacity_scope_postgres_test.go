package allocator

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// poolStatus reads the pool object as the API serves it.
func poolStatus(t *testing.T, db *pgxpool.Pool, poolKey string) ipamv1alpha1.IPPoolStatus {
	t.Helper()
	var data []byte
	var consumed *string
	if err := db.QueryRow(context.Background(),
		`SELECT o.data, c.consumed::text
		   FROM ipam_objects o
		   LEFT JOIN ipam_pool_consumption c ON c.pool_key = o.key
		  WHERE o.key = $1`, poolKey).Scan(&data, &consumed); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	// The stored status does not track consumption; derive it as a read does.
	if consumed != nil {
		n, ok := new(big.Int).SetString(*consumed, 10)
		if !ok {
			t.Fatalf("consumption %q is not an integer", *consumed)
		}
		applyConsumed(&pool, n)
	}
	return pool.Status
}

// Pool capacity counts addresses of pool space, not allocations. Two claims in
// different address spaces holding one address consume that address once, so
// status.capacity.allocated stays at the address count while both are bound,
// and drops only when the last holder releases it.
func TestCapacityCountsAnAddressSharedAcrossSpacesOnce(t *testing.T) {
	type op struct {
		release bool
		claim   string
		network string // address space; ignored for release
		address string // ignored for release
	}
	type want struct {
		allocated string
		percent   float64
	}

	cases := []struct {
		name  string
		pool  string
		cidr  string
		steps []op
		after []want // one per step
	}{
		{
			name: "same IPv6 address in two networks, then released one at a time",
			pool: "cap-v6-shared", cidr: "fd18:184::/120",
			steps: []op{
				{claim: "a", network: "net-a", address: "fd18:184::53"},
				{claim: "b", network: "net-b", address: "fd18:184::53"},
				{release: true, claim: "a"},
				{release: true, claim: "b"},
			},
			after: []want{{"1", 0.3906}, {"1", 0.3906}, {"1", 0.3906}, {"0", 0}},
		},
		{
			name: "releasing the other holder first leaves the address counted",
			pool: "cap-v4-shared", cidr: "10.184.0.0/24",
			steps: []op{
				{claim: "a", network: "net-a", address: "10.184.0.10"},
				{claim: "b", network: "net-b", address: "10.184.0.10"},
				{release: true, claim: "b"},
				{release: true, claim: "a"},
			},
			after: []want{{"1", 0.3906}, {"1", 0.3906}, {"1", 0.3906}, {"0", 0}},
		},
		{
			name: "shared and distinct addresses mixed",
			pool: "cap-v4-mixed", cidr: "10.185.0.0/24",
			steps: []op{
				{claim: "a", network: "net-a", address: "10.185.0.10"},
				{claim: "b", network: "net-b", address: "10.185.0.10"},
				{claim: "c", network: "net-a", address: "10.185.0.11"},
				{claim: "d", network: "net-b", address: "10.185.0.12"},
				{release: true, claim: "a"},
				{release: true, claim: "c"},
				{release: true, claim: "b"},
				{release: true, claim: "d"},
			},
			after: []want{
				{"1", 0.3906}, {"1", 0.3906}, {"2", 0.7813}, {"3", 1.1719},
				{"3", 1.1719}, {"2", 0.7813}, {"1", 0.3906}, {"0", 0},
			},
		},
		{
			name: "every address held twice reads full, not over full",
			pool: "cap-v4-full", cidr: "10.186.0.0/31",
			steps: []op{
				{claim: "a0", network: "net-a", address: "10.186.0.0"},
				{claim: "a1", network: "net-a", address: "10.186.0.1"},
				{claim: "b0", network: "net-b", address: "10.186.0.0"},
				{claim: "b1", network: "net-b", address: "10.186.0.1"},
				{release: true, claim: "a0"},
				{release: true, claim: "b0"},
			},
			after: []want{{"1", 50}, {"2", 100}, {"2", 100}, {"2", 100}, {"2", 100}, {"1", 50}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Pool(t)
			poolKey := seedPool(t, db, tc.pool, tc.cidr)
			family := "IPv4"
			prefixLen := 32
			if tc.cidr[0] == 'f' {
				family, prefixLen = "IPv6", 128
			}

			for i, s := range tc.steps {
				if s.release {
					release(t, db, s.claim)
				} else if _, err := claimAddress(t, db, PrefixRequest{
					PoolKey: poolKey, PrefixLen: prefixLen, IPFamily: family,
					ClaimKey: s.claim, ScopeDigest: spaceOf(s.network), Address: s.address,
				}); err != nil {
					t.Fatalf("step %d: claim %s: %v", i, s.claim, err)
				}

				st := poolStatus(t, db, poolKey)
				w := tc.after[i]
				if st.Capacity.Allocated != w.allocated {
					t.Errorf("step %d: capacity.allocated = %s, want %s", i, st.Capacity.Allocated, w.allocated)
				}
				if st.UtilizationPercent != w.percent {
					t.Errorf("step %d: utilizationPercent = %v, want %v", i, st.UtilizationPercent, w.percent)
				}
				if st.UtilizationPercent < 0 || st.UtilizationPercent > 100 {
					t.Errorf("step %d: utilizationPercent %v outside [0, 100]", i, st.UtilizationPercent)
				}
				total, ok1 := new(big.Int).SetString(st.Capacity.Total, 10)
				alloc, ok2 := new(big.Int).SetString(st.Capacity.Allocated, 10)
				avail, ok3 := new(big.Int).SetString(st.Capacity.Available, 10)
				if !ok1 || !ok2 || !ok3 {
					t.Fatalf("step %d: capacity not decimal: %+v", i, st.Capacity)
				}
				if avail.Sign() < 0 || alloc.Cmp(total) > 0 {
					t.Errorf("step %d: capacity out of bounds: %+v", i, st.Capacity)
				}
				if new(big.Int).Add(alloc, avail).Cmp(total) != 0 {
					t.Errorf("step %d: allocated + available != total: %+v", i, st.Capacity)
				}
				if fresh := measureNow(t, db, poolKey, tc.cidr); fresh != st.Capacity.Allocated {
					t.Errorf("step %d: capacity.allocated = %s, fresh measurement = %s", i, st.Capacity.Allocated, fresh)
				}
			}
		})
	}
}
