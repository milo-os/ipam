package allocator

import (
	"net"
	"testing"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics/legacyregistry"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// proving found that ipam_pool_utilization_ratio publishes no series on a live
// cluster, and that ipam_provisioned_pool_capacity_skipped_total is absent too.
// The second half is the discriminator: a registered counter emits at zero
// whether or not anything ever increments it, so its absence points below this
// function — at the registry, the endpoint, or the process being scraped.
//
// These tests fix which half is which. They gather from
// legacyregistry.DefaultGatherer, the same registry the apiserver serves, so a
// pass here means the allocator publishes correctly and the fault is in the
// plumbing under it.
func TestPublishPrefixUtilizationEmitsSeriesForAnOperatorPool(t *testing.T) {
	resetPoolGauges(t)

	pool := &ipamv1alpha1.IPPool{}
	pool.Spec.IPFamily = ipamv1alpha1.IPv4
	parents := []net.IPNet{mustParseCIDR(t, "10.0.0.0/24")}
	allocated := []net.IPNet{mustParseCIDR(t, "10.0.0.0/26")}

	publishPrefixUtilization(pool, "/registry/ipam.miloapis.com/ippools/c41-pool", "IPv4", parents, allocated)

	for _, name := range []string{
		"ipam_pool_utilization_ratio",
		"ipam_pool_capacity_total",
		"ipam_pool_allocated_total",
	} {
		if n := countSeries(t, name); n == 0 {
			t.Errorf("%s: no series after publishing for an operator-authored pool", name)
		}
	}

	// 64 of 256 addresses.
	if got := firstGaugeValue(t, "ipam_pool_utilization_ratio"); got != 0.25 {
		t.Errorf("ipam_pool_utilization_ratio = %v, want 0.25", got)
	}
}

// The #37 exclusion has to be visible rather than silent: a cascade-provisioned
// pool publishes no per-pool series, but the skip counter advances so an
// operator can tell "excluded by design" from "the metric is broken".
func TestPublishPrefixUtilizationSkipsCascadePoolsVisibly(t *testing.T) {
	resetPoolGauges(t)
	before := firstCounterValue(t, "ipam_provisioned_pool_capacity_skipped_total")

	pool := &ipamv1alpha1.IPPool{}
	pool.Spec.ClassRef = &ipamv1alpha1.LocalRef{Name: "tenant-ipv4"}
	parents := []net.IPNet{mustParseCIDR(t, "10.1.0.0/24")}

	publishPrefixUtilization(pool, "/registry/ipam.miloapis.com/ippools/cascade-pool", "IPv4", parents, nil)

	if n := countSeries(t, "ipam_pool_utilization_ratio"); n != 0 {
		t.Errorf("ipam_pool_utilization_ratio: %d series for a cascade pool, want 0", n)
	}
	if got := firstCounterValue(t, "ipam_provisioned_pool_capacity_skipped_total"); got != before+1 {
		t.Errorf("skip counter = %v, want %v", got, before+1)
	}
}

// A registered counter emits at zero before anything increments it. That is the
// assumption proving's finding rests on — it is what makes the counter's
// absence from a live scrape evidence rather than ambiguity — so it is worth
// pinning rather than assuming.
func TestSkipCounterIsRegisteredEvenBeforeAnyIncrement(t *testing.T) {
	if n := countSeries(t, "ipam_provisioned_pool_capacity_skipped_total"); n != 1 {
		t.Fatalf("ipam_provisioned_pool_capacity_skipped_total: %d series, want 1 (registered, at zero)", n)
	}
}

func resetPoolGauges(t *testing.T) {
	t.Helper()
	reset := func() {
		metrics.PoolUtilization.Reset()
		metrics.PoolCapacity.Reset()
		metrics.PoolAllocated.Reset()
	}
	reset()
	t.Cleanup(reset)
}

func countSeries(t *testing.T, name string) int {
	t.Helper()
	for _, f := range gather(t) {
		if f.GetName() == name {
			return len(f.GetMetric())
		}
	}
	return 0
}

func firstGaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	for _, f := range gather(t) {
		if f.GetName() == name {
			for _, m := range f.GetMetric() {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("%s: no series", name)
	return 0
}

func firstCounterValue(t *testing.T, name string) float64 {
	t.Helper()
	for _, f := range gather(t) {
		if f.GetName() == name {
			for _, m := range f.GetMetric() {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("%s: no series", name)
	return 0
}

func gather(t *testing.T) []*dto.MetricFamily {
	t.Helper()
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return families
}

// The whole-path version of the question, against a real database.
//
// proving reported that the ordinary claim path runs IPPool's registry status
// writer, which never publishes, and that this is why no series appear. This
// test drives AllocatePrefix — the function the IPClaim registry actually calls
// — end to end and asks the registry the apiserver serves whether a series
// exists afterwards.
//
// It exists because a unit test calling publishPrefixUtilization directly
// cannot answer "is it reached", which is the only question in dispute.
func TestAllocatePrefixPublishesTheGaugeEndToEnd(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()
	alloc := NewPostgresPrefixAllocator()
	resetPoolGauges(t)

	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "operator-authored"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       "10.200.0.0/24",
			IPFamily:   ipamv1alpha1.IPv4,
			ClassNames: []string{"tenant-endpoint-ipv4"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "10.200.0.0/24", IPFamily: ipamv1alpha1.IPv4,
		},
	}
	poolKey := platformKey("ippools", "operator-authored")
	seedObject(t, db, poolKey, "IPPool", pool.Name, pool)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := alloc.AllocatePrefix(ctx, tx, AllocateRequest{
		PoolKey:       poolKey,
		AllocationKey: "/alloc/one",
		ClaimKey:      "/claim/one",
		ClassName:     "tenant-endpoint-ipv4",
		ScopeDigest:   scope.AddressSpaceDigest("", map[string]ipam.ScopeRef{}),
		PrefixLength:  26,
		IPFamily:      "IPv4",
		ReclaimPolicy: string(ipamv1alpha1.ReclaimDelete),
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("allocate: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n := countSeries(t, "ipam_pool_utilization_ratio"); n == 0 {
		t.Fatal("no ipam_pool_utilization_ratio series after a real allocation: the claim path does not publish")
	}
	// 64 of 256 addresses.
	if got := firstGaugeValue(t, "ipam_pool_utilization_ratio"); got != 0.25 {
		t.Errorf("ipam_pool_utilization_ratio = %v, want 0.25", got)
	}
}

// #44: a pool created and never claimed must be visible as empty, not absent.
//
// The capacity arithmetic was centralised into PoolCapacityFor so the two
// writers could not disagree; the publication was left duplicated in the same
// shape, so the IPPool registry wrote a correct status and no series. A pool
// sitting at 0% and a pool that had never been created looked identical.
//
// PublishPoolCapacity is the shared entry point both writers now use. These
// pin its two branches directly, since the registry's own path needs a
// database and this is the behaviour that path depends on.
func TestPublishPoolCapacityMakesAnEmptyPoolVisible(t *testing.T) {
	resetPoolGauges(t)
	parents := []net.IPNet{mustParseCIDR(t, "10.30.0.0/24")}

	PublishPoolCapacity("/registry/ipam.miloapis.com/ippools/fresh", "IPv4", false, parents, nil)

	if n := countSeries(t, "ipam_pool_utilization_ratio"); n == 0 {
		t.Fatal("a pool with no allocations publishes no series: empty is indistinguishable from absent")
	}
	if got := firstGaugeValue(t, "ipam_pool_utilization_ratio"); got != 0 {
		t.Errorf("ipam_pool_utilization_ratio = %v, want 0", got)
	}
	if got := firstGaugeValue(t, "ipam_pool_capacity_total"); got != 256 {
		t.Errorf("ipam_pool_capacity_total = %v, want 256", got)
	}
}

// The #37 exclusion has to survive being routed through the shared entry point
// — a cascade-provisioned pool created by the registry must skip just as one
// created by the allocator does, or the bounded-cardinality policy holds on one
// path and not the other.
func TestPublishPoolCapacityStillExcludesCascadePools(t *testing.T) {
	resetPoolGauges(t)
	before := firstCounterValue(t, "ipam_provisioned_pool_capacity_skipped_total")
	parents := []net.IPNet{mustParseCIDR(t, "10.31.0.0/24")}

	PublishPoolCapacity("/registry/ipam.miloapis.com/ippools/cascade", "IPv4", true, parents, nil)

	if n := countSeries(t, "ipam_pool_utilization_ratio"); n != 0 {
		t.Errorf("%d series for a cascade pool, want 0", n)
	}
	if got := firstCounterValue(t, "ipam_provisioned_pool_capacity_skipped_total"); got != before+1 {
		t.Errorf("skip counter = %v, want %v", got, before+1)
	}
}
