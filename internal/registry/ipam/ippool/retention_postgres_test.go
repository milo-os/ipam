package ippool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/component-base/metrics/testutil"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// seedRetained writes a retained claim row in poolKey, retained an hour ago,
// with the deadline expires (an SQL expression).
func seedRetained(t *testing.T, db *pgxpool.Pool, poolKey, key, cidr, expires string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, retained_at, expires_at)
		 VALUES ($1, $2, NULL, $3, 'IPv4', 'Claim', date_trunc('second', now()) - interval '1 hour', `+expires+`)`,
		poolKey, cidr, key); err != nil {
		t.Fatalf("seed retained row %s: %v", key, err)
	}
}

func leaseOfRow(t *testing.T, db *pgxpool.Pool, key string) string {
	t.Helper()
	var lease string
	if err := db.QueryRow(context.Background(),
		`SELECT CASE WHEN expires_at = 'infinity' THEN 'none' ELSE (expires_at - retained_at)::text END
		   FROM ipam_cidr_allocations WHERE allocation_key = $1`, key).Scan(&lease); err != nil {
		t.Fatalf("read lease of %s: %v", key, err)
	}
	return lease
}

// Lowering a pool's maxRetentionLease through the API brings forward what the
// pool already retains; an update that does not lower it moves nothing, and
// neither does one over a cap stored before validation refused it.
func TestUpdatingAPoolsCapTightensWhatItRetains(t *testing.T) {
	db := testdb.Pool(t)
	s := newPostgresPoolStorage(t, db)
	ctx := poolCtx("tenant-a")
	if err := createPool(t, s, ctx, rootPoolObj("capped", "10.64.0.0/16", ipam.IPv4)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	poolKey := poolStorageKey("tenant-a", "capped")
	seedRetained(t, db, poolKey, "long", "10.64.1.0/24", `date_trunc('second', now()) + interval '9 hours'`)
	seedRetained(t, db, poolKey, "none", "10.64.2.0/24", `'infinity'`)

	update := func(mutate func(*ipam.IPPool)) {
		t.Helper()
		obj, err := s.Get(ctx, "capped", &metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		p := obj.(*ipam.IPPool).DeepCopy()
		mutate(p)
		if _, _, err := s.Update(ctx, "capped", rest.DefaultUpdatedObjectInfo(p),
			nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	update(func(p *ipam.IPPool) { p.Spec.MaxRetentionLease = &metav1.Duration{Duration: 2 * time.Hour} })
	for _, key := range []string{"long", "none"} {
		if got := leaseOfRow(t, db, key); got != "02:00:00" {
			t.Errorf("%s: lease %s after a 2h cap, want 02:00:00", key, got)
		}
	}

	update(func(p *ipam.IPPool) { p.Spec.MaxRetentionLease = &metav1.Duration{Duration: 6 * time.Hour} })
	if got := leaseOfRow(t, db, "long"); got != "02:00:00" {
		t.Errorf("loosening the cap moved the lease to %s, want it kept at 02:00:00", got)
	}

	// A stored non-positive cap is not one: a label update over it must not
	// make the pool's retained rows due.
	seedRetained(t, db, poolKey, "later", "10.64.3.0/24", `date_trunc('second', now()) + interval '30 minutes'`)
	if _, err := db.Exec(context.Background(),
		`UPDATE ipam_objects
		    SET data = convert_to(jsonb_set(ipam_data_to_jsonb(data), '{spec,maxRetentionLease}', '"-1h0m0s"')::text, 'UTF8')
		  WHERE key = $1`, poolKey); err != nil {
		t.Fatalf("store a negative cap: %v", err)
	}
	update(func(p *ipam.IPPool) { p.Labels = map[string]string{"team": "edge"} })
	if got := leaseOfRow(t, db, "later"); got != "01:30:00" {
		t.Errorf("a stored negative cap moved the lease to %s, want 01:30:00", got)
	}
}

// A pool update whose cap could not be applied straight away still succeeds,
// because it has already committed, and the failure is counted; the next sweep
// pass applies the cap.
func TestACapTheUpdateCouldNotApplyIsAppliedByTheSweeper(t *testing.T) {
	db := testdb.Pool(t)
	s := newPostgresPoolStorage(t, db)
	ctx := poolCtx("tenant-a")
	if err := createPool(t, s, ctx, rootPoolObj("capped", "10.64.0.0/16", ipam.IPv4)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	poolKey := poolStorageKey("tenant-a", "capped")
	seedRetained(t, db, poolKey, "long", "10.64.1.0/24", `date_trunc('second', now()) + interval '9 hours'`)

	tightenRetention = func(context.Context, pgx.Tx, string) (int, int, error) { return 0, 0, errors.New("injected") }
	t.Cleanup(func() { tightenRetention = allocator.TightenRetention })
	before, _ := testutil.GetCounterMetricValue(metrics.RetentionCapApplyFailures)

	obj, err := s.Get(ctx, "capped", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p := obj.(*ipam.IPPool).DeepCopy()
	p.Spec.MaxRetentionLease = &metav1.Duration{Duration: 2 * time.Hour}
	if _, _, err := s.Update(ctx, "capped", rest.DefaultUpdatedObjectInfo(p),
		nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("an update whose cap could not be applied returned %v, want success", err)
	}
	if got, _ := testutil.GetCounterMetricValue(metrics.RetentionCapApplyFailures); got-before != 1 {
		t.Errorf("ipam_retention_cap_apply_failures_total moved by %v, want 1", got-before)
	}
	if got := leaseOfRow(t, db, "long"); got != "10:00:00" {
		t.Fatalf("setup: lease %s before the sweep, want the untouched 10:00:00", got)
	}

	if _, err := allocator.NewSweeper(db, allocator.SweepObserve).Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := leaseOfRow(t, db, "long"); got != "02:00:00" {
		t.Errorf("lease %s after a sweep pass, want the pool's 2h cap", got)
	}
}

// An IPAllocation that cannot be written keeps its row's deadline: the update
// still succeeds, the rest of the pool is tightened, and the failure is counted
// for the sweeper to retry.
func TestACapLeavesARowWhoseAllocationCannotBeRepublished(t *testing.T) {
	db := testdb.Pool(t)
	s := newPostgresPoolStorage(t, db)
	ctx := poolCtx("tenant-a")
	if err := createPool(t, s, ctx, rootPoolObj("capped", "10.64.0.0/16", ipam.IPv4)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	poolKey := poolStorageKey("tenant-a", "capped")
	seedRetained(t, db, poolKey, "bad", "10.64.1.0/24", `date_trunc('second', now()) + interval '9 hours'`)
	seedRetained(t, db, poolKey, "good", "10.64.2.0/24", `date_trunc('second', now()) + interval '9 hours'`)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ('bad', 'IPAllocation', 'bad', '{"status": "x"}')`); err != nil {
		t.Fatalf("seed an undecodable IPAllocation: %v", err)
	}
	before, _ := testutil.GetCounterMetricValue(metrics.RetentionCapApplyFailures)

	obj, err := s.Get(ctx, "capped", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p := obj.(*ipam.IPPool).DeepCopy()
	p.Spec.MaxRetentionLease = &metav1.Duration{Duration: 2 * time.Hour}
	if _, _, err := s.Update(ctx, "capped", rest.DefaultUpdatedObjectInfo(p),
		nil, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := leaseOfRow(t, db, "good"); got != "02:00:00" {
		t.Errorf("good: lease %s, want the 2h cap", got)
	}
	if got := leaseOfRow(t, db, "bad"); got != "10:00:00" {
		t.Errorf("bad: lease %s, want it kept at 10:00:00 while its status cannot be written", got)
	}
	if got, _ := testutil.GetCounterMetricValue(metrics.RetentionCapApplyFailures); got-before != 1 {
		t.Errorf("ipam_retention_cap_apply_failures_total moved by %v, want 1", got-before)
	}
}
