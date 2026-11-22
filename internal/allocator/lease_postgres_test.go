package allocator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

// setPoolCap rewrites a seeded pool's maxRetentionLease.
func setPoolCap(t *testing.T, db *pgxpool.Pool, poolKey string, poolCap *metav1.Duration) {
	t.Helper()
	ctx := context.Background()
	var data []byte
	if err := db.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, poolKey).Scan(&data); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	pool.Spec.MaxRetentionLease = poolCap
	if data, err := json.Marshal(&pool); err != nil {
		t.Fatalf("encode pool: %v", err)
	} else if _, err := db.Exec(ctx, `UPDATE ipam_objects SET data = $2 WHERE key = $1`, poolKey, data); err != nil {
		t.Fatalf("write pool: %v", err)
	}
}

// seedLeaseClass writes the "standard" class allocateWithPolicy allocates
// under, with the given lease, into the fixtures' project.
func seedLeaseClass(t *testing.T, db *pgxpool.Pool, lease *metav1.Duration) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	definition(t, tx, testProject, "standard", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4, RetentionLease: lease})
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit class: %v", err)
	}
}

// retainedRetention reads a row's stored retention back.
func retainedRetention(t *testing.T, db *pgxpool.Pool, allocationKey string) RetainedAllocation {
	t.Helper()
	var (
		retainedAt *time.Time
		expiresAt  pgtype.Timestamptz
	)
	if err := db.QueryRow(context.Background(),
		`SELECT retained_at, expires_at FROM ipam_cidr_allocations WHERE allocation_key = $1`, allocationKey,
	).Scan(&retainedAt, &expiresAt); err != nil {
		t.Fatalf("read retention of %s: %v", allocationKey, err)
	}
	if retainedAt == nil {
		t.Fatalf("%s has no retained_at", allocationKey)
	}
	return retainedFrom(allocationKey, *retainedAt, expiresAt)
}

func releaseRetained(t *testing.T, db *pgxpool.Pool, claim string) []RetainedAllocation {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	retained, err := NewPostgresPrefixAllocator().Release(ctx, tx, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("release %s: %v", claim, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit release: %v", err)
	}
	return retained
}

// The lease is the shorter of the class's and the pool's, measured from
// retention, fixed when the claim goes, and returned to the caller as stored.
func TestRetentionFixesTheShorterLease(t *testing.T) {
	for _, tc := range []struct {
		name       string
		noClass    bool
		classLease *metav1.Duration
		poolCap    *metav1.Duration
		want       *time.Duration
	}{
		{name: "neither sets one", want: nil},
		{name: "class only", classLease: dur(2 * time.Hour), want: ptr(2 * time.Hour)},
		{name: "pool caps the class", classLease: dur(2 * time.Hour), poolCap: dur(time.Hour), want: ptr(time.Hour)},
		{name: "class under the cap", classLease: dur(30 * time.Minute), poolCap: dur(time.Hour), want: ptr(30 * time.Minute)},
		{name: "pool only", poolCap: dur(45 * time.Minute), want: ptr(45 * time.Minute)},
		{name: "class gone, pool caps", noClass: true, poolCap: dur(20 * time.Minute), want: ptr(20 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Pool(t)
			poolKey := seedPool(t, db, "lease", "10.60.0.0/16")
			setPoolCap(t, db, poolKey, tc.poolCap)
			if !tc.noClass {
				seedLeaseClass(t, db, tc.classLease)
			}
			allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)

			got := releaseRetained(t, db, "claim-a")
			if len(got) != 1 {
				t.Fatalf("Release returned %d retained rows, want 1", len(got))
			}
			stored := retainedRetention(t, db, "alloc-a")
			if got[0] != stored {
				t.Errorf("Release returned %+v, stored %+v", got[0], stored)
			}
			if tc.want == nil {
				if stored.Lease != LeaseNone {
					t.Errorf("lease = %s, want none ('infinity')", stored.Lease)
				}
				return
			}
			if stored.Lease != LeaseFinite {
				t.Fatalf("lease = %s, want finite", stored.Lease)
			}
			if d := leaseOf(stored); d != *tc.want {
				t.Errorf("expires_at - retained_at = %s, want %s", d, *tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// leaseOf is the whole-second lease a stored deadline gives. A deadline is
// retention plus the lease rounded up to the second, so it is the lease plus
// less than a second after retention, and a whole second itself.
func leaseOf(r RetainedAllocation) time.Duration {
	if !r.ExpiresAt.Equal(r.ExpiresAt.Truncate(time.Second)) {
		return -1
	}
	return r.ExpiresAt.Sub(r.RetainedAt).Truncate(time.Second)
}

// A class that cannot be read leaves the lease for the sweeper and does not
// fail the delete: the claim goes either way.
func TestAnUnreadableClassLeavesTheLeaseUnevaluated(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "lease-unreadable", "10.61.0.0/16")
	setPoolCap(t, db, poolKey, dur(time.Hour))
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1, 'IPClass', 'standard', '{"spec": "not a spec"}')`,
		classStorageKey(testProject, "standard")); err != nil {
		t.Fatalf("seed unreadable class: %v", err)
	}
	allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)

	got := releaseRetained(t, db, "claim-a")
	if len(got) != 1 || got[0].Lease != LeaseUnevaluated {
		t.Fatalf("Release returned %+v, want one unevaluated row", got)
	}
	if stored := retainedRetention(t, db, "alloc-a"); stored.Lease != LeaseUnevaluated {
		t.Errorf("stored lease = %s, want unevaluated (expires_at NULL)", stored.Lease)
	}
}

// Binding a retained row to a claim, by reclaim or any other path, ends its
// retention and clears its deadline, so a later retention starts a fresh one.
func TestRebindingClearsTheDeadline(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	poolKey := seedPool(t, db, "lease-rebind", "10.62.0.0/16")
	seedLeaseClass(t, db, dur(time.Hour))
	allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)
	releaseRetained(t, db, "claim-a")
	if retainedRetention(t, db, "alloc-a").Lease != LeaseFinite {
		t.Fatal("setup: the retained row has no finite deadline")
	}

	inTx(t, ctx, db.Begin, func(tx pgx.Tx) error {
		if _, err := NewPostgresPrefixAllocator().LockClaimAllocation(ctx, tx, "alloc-a"); err != nil {
			return err
		}
		return NewPostgresPrefixAllocator().ReclaimAllocation(ctx, tx, "alloc-a", "claim-a2", ipamv1alpha1.ReclaimRetain)
	})

	var retainedAt, expiresAt *time.Time
	if err := db.QueryRow(ctx,
		`SELECT retained_at, expires_at FROM ipam_cidr_allocations WHERE allocation_key = 'alloc-a'`,
	).Scan(&retainedAt, &expiresAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if retainedAt != nil || expiresAt != nil {
		t.Errorf("after reclaim retained_at = %v, expires_at = %v; want both NULL", retainedAt, expiresAt)
	}

	// And the next retention is measured afresh.
	second := releaseRetained(t, db, "claim-a2")
	if len(second) != 1 || second[0].Lease != LeaseFinite || leaseOf(second[0]) != time.Hour {
		t.Errorf("second retention = %+v, want a fresh one-hour lease", second)
	}
}

// The class is found from the row alone, without a tenant on the context: the
// recorded class_project when there is one. A row written before it was
// recorded uses its pool's project only where the pool offers the class by
// name, which puts the pool in the defining project; anywhere else the class
// is not known.
func TestRetentionClassProject(t *testing.T) {
	poolKey := tenant.Identity{Name: "consumer"}.ResourceKey("ippools", "p")
	offering := &ipamv1alpha1.IPPool{Spec: ipamv1alpha1.IPPoolSpec{ClassNames: []string{"standard"}}}
	provisioned := &ipamv1alpha1.IPPool{}
	for _, tc := range []struct {
		name         string
		pool         *ipamv1alpha1.IPPool
		classProject *string
		want         string
		known        bool
	}{
		{"recorded", provisioned, strp("platform"), "platform", true},
		{"not recorded, pool offers the class", offering, nil, "consumer", true},
		{"recorded empty, pool offers the class", offering, strp(""), "consumer", true},
		{"not recorded, pool does not offer it", provisioned, nil, "", false},
		{"not recorded, no pool", nil, nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := retentionClassProject(tc.pool, poolKey, "standard", tc.classProject)
			if got != tc.want || known != tc.known {
				t.Errorf("retentionClassProject = %q, %v; want %q, %v", got, known, tc.want, tc.known)
			}
		})
	}
}

// A row that does not record its class's project, in a pool that does not
// offer the class, could belong to another project's class of the same name.
// Its lease is then the pool's cap alone, which can only hold the address
// longer than the class meant, never shorter; a same-named class with a
// shorter lease in the pool's project is not applied to it.
func TestAnUnidentifiedClassGetsOnlyThePoolsCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offers bool
		want   time.Duration
	}{
		{"pool provisioned under another project's class", false, 2 * time.Hour},
		{"pool offers the class", true, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Pool(t)
			ctx := context.Background()
			poolKey := seedPool(t, db, "lease-legacy", "10.65.0.0/16")
			setPoolCap(t, db, poolKey, dur(2*time.Hour))
			if tc.offers {
				setPoolClasses(t, db, poolKey, "standard")
			}
			seedLeaseClass(t, db, dur(10*time.Minute))
			allocateWithPolicy(t, db, poolKey, "claim-a", "alloc-a", ipamv1alpha1.ReclaimRetain)
			if _, err := db.Exec(ctx,
				`UPDATE ipam_cidr_allocations SET class_project = NULL, address_scope = NULL WHERE allocation_key = 'alloc-a'`); err != nil {
				t.Fatalf("unrecord the class: %v", err)
			}
			got := releaseRetained(t, db, "claim-a")
			if len(got) != 1 || got[0].Lease != LeaseFinite || leaseOf(got[0]) != tc.want {
				t.Errorf("retained = %+v (lease %s), want a %s lease", got, leaseOf(got[0]), tc.want)
			}
		})
	}
}

// setPoolClasses rewrites a seeded pool's spec.classNames.
func setPoolClasses(t *testing.T, db *pgxpool.Pool, poolKey string, classNames ...string) {
	t.Helper()
	ctx := context.Background()
	var data []byte
	if err := db.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, poolKey).Scan(&data); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	pool.Spec.ClassNames = classNames
	if data, err := json.Marshal(&pool); err != nil {
		t.Fatalf("encode pool: %v", err)
	} else if _, err := db.Exec(ctx, `UPDATE ipam_objects SET data = $2 WHERE key = $1`, poolKey, data); err != nil {
		t.Fatalf("write pool: %v", err)
	}
}
