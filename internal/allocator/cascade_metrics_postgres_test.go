package allocator

import (
	"context"
	"sync"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"k8s.io/component-base/metrics/legacyregistry"

	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// A herd of simultaneous first claims is the normal way a chain gets built, and
// all but one member of it loses the race. Losing must land on the same counter
// as winning, under its own outcome — if it were recorded as an error, routine
// first use of a class would page somebody.
//
// Class names are unique to this test so the counters can be read absolutely
// rather than as deltas against whatever else the package has run.
func TestALostRaceIsRecordedAsAnOutcomeNotAnError(t *testing.T) {
	const (
		parent = "metrics-backbone"
		leafCl = "metrics-subnets"
		herd   = 16
	)

	db := testdb.Pool(t, testdb.MaxConns(24))
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", parent, ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 20, PoolPer: []string{"location"},
	})
	definition(t, tx, "platform", leafCl, ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, ParentClassName: parent, DefaultPrefixLength: 24,
	})
	offerPool(t, tx, "platform", "root", "10.0.0.0/12", parent, nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	read := begin(t, db)
	leaf, err := LoadClass(ctxIn("platform"), read, leafCl)
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_ = read.Rollback(ctx)

	claimScope := map[string]ipam.ScopeRef{"location": claimScopeRef("Location", "lon1")}

	errs := make([]error, herd)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range herd {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = ResolvePool(ctx, db, leaf, claimScope)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}

	// The cascade provisions the leaf's ancestors; the leaf itself allocates out
	// of the pool they produced, so the parent class is where the levels land.
	got := cascadeOutcomes(t, parent)
	if got[levelError] != 0 {
		t.Errorf("%v levels recorded as errors, want 0", got[levelError])
	}
	if got[levelProvisioned] != 1 {
		t.Errorf("%v levels provisioned, want 1", got[levelProvisioned])
	}
	// Every caller that did not provision either lost the race or found the pool
	// already there. Both are counted, so the steady state stays visible and not
	// only the moment of creation.
	if other := got[levelLost] + got[levelReused]; other != herd-1 {
		t.Errorf("%v levels lost or reused, want %d", other, herd-1)
	}
	t.Logf("provisioned=%v lost=%v reused=%v",
		got[levelProvisioned], got[levelLost], got[levelReused])
}

// cascadeOutcomes reads the per-outcome level counts recorded for one class.
func cascadeOutcomes(t *testing.T, class string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, mf := range gatherFamilies(t) {
		if mf.GetName() != "ipam_cascade_levels_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["class"] == class {
				out[labels["outcome"]] = m.GetCounter().GetValue()
			}
		}
	}
	return out
}

func gatherFamilies(t *testing.T) []*dto.MetricFamily {
	t.Helper()
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	return families
}
