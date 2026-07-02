package allocator

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func classPool(name string, family ipamv1alpha1.IPFamily, util, largestFree int32, classes ...string) ipamv1alpha1.IPPool {
	return ipamv1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			IPFamily:   family,
			CIDR:       "10.0.0.0/16",
			ClassNames: classes,
		},
		Status: ipamv1alpha1.IPPoolStatus{
			IPFamily:           family,
			UtilizationPercent: util,
			LargestFreePrefix:  largestFree,
		},
	}
}

func TestPoolBacksClass(t *testing.T) {
	p := classPool("a", ipamv1alpha1.IPv4, 0, 0, "public-egress", "internal")
	if !poolBacksClass(&p, "internal") {
		t.Errorf("expected pool to back class internal")
	}
	if poolBacksClass(&p, "absent") {
		t.Errorf("did not expect pool to back class absent")
	}
	none := classPool("b", ipamv1alpha1.IPv4, 0, 0)
	if poolBacksClass(&none, "internal") {
		t.Errorf("pool with no classNames should back nothing")
	}
}

func TestPickPoolForClass(t *testing.T) {
	// keys must be sorted to mirror listPools ORDER BY key.
	keys := []string{"/pool-a", "/pool-b", "/pool-c"}
	pools := []ipamv1alpha1.IPPool{
		classPool("pool-a", ipamv1alpha1.IPv4, 80, 26, "egress"),
		classPool("pool-b", ipamv1alpha1.IPv4, 20, 25, "egress"),
		classPool("pool-c", ipamv1alpha1.IPv6, 10, 60, "egress"),
	}

	tests := []struct {
		name     string
		family   string
		strategy string
		wantKey  string
		wantOK   bool
	}{
		{
			name:     "FirstFit picks lowest key among family matches",
			family:   "IPv4",
			strategy: "FirstFit",
			wantKey:  "/pool-a",
			wantOK:   true,
		},
		{
			name:     "empty strategy behaves as first-fit",
			family:   "IPv4",
			strategy: "",
			wantKey:  "/pool-a",
			wantOK:   true,
		},
		{
			name:     "LeastUtilized picks lowest utilization",
			family:   "IPv4",
			strategy: "LeastUtilized",
			wantKey:  "/pool-b",
			wantOK:   true,
		},
		{
			name:     "family filter selects the IPv6 pool",
			family:   "IPv6",
			strategy: "FirstFit",
			wantKey:  "/pool-c",
			wantOK:   true,
		},
		{
			name:     "no family match returns not found",
			family:   "IPv6",
			strategy: "LeastUtilized",
			wantKey:  "/pool-c",
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ok := pickPoolForClass(keys, pools, "egress", tt.family, tt.strategy)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if key != tt.wantKey {
				t.Fatalf("key = %q, want %q", key, tt.wantKey)
			}
		})
	}
}

func TestPickPoolForClass_NoBackingPool(t *testing.T) {
	keys := []string{"/pool-a"}
	pools := []ipamv1alpha1.IPPool{classPool("pool-a", ipamv1alpha1.IPv4, 0, 24, "other")}
	if _, ok := pickPoolForClass(keys, pools, "egress", "IPv4", "FirstFit"); ok {
		t.Fatalf("expected no pool to back class egress")
	}
}

func TestPoolScopes(t *testing.T) {
	if got := poolScopes(""); len(got) != 1 || got[0] != "" {
		t.Errorf("platform caller scopes = %v, want [\"\"]", got)
	}
	got := poolScopes("acme")
	if len(got) != 2 || got[0] != "acme" || got[1] != "" {
		t.Errorf("project caller scopes = %v, want [acme \"\"]", got)
	}
}

func TestSortPoolsByKey(t *testing.T) {
	// Simulate the union of a project-scoped list and a platform-scoped list,
	// which arrive individually sorted but interleaved after concatenation.
	keys := []string{"project/acme/p2", "/platform-p1"}
	pools := []ipamv1alpha1.IPPool{
		classPool("p2", ipamv1alpha1.IPv4, 0, 0, "egress"),
		classPool("p1", ipamv1alpha1.IPv4, 0, 0, "egress"),
	}
	sortPoolsByKey(keys, pools)
	if keys[0] != "/platform-p1" || keys[1] != "project/acme/p2" {
		t.Fatalf("keys not sorted: %v", keys)
	}
	// The pool paired with each key must move with it.
	if pools[0].Name != "p1" || pools[1].Name != "p2" {
		t.Fatalf("pools not permuted with keys: %s, %s", pools[0].Name, pools[1].Name)
	}
}

func TestPoolScore(t *testing.T) {
	p := classPool("a", ipamv1alpha1.IPv4, 42, 26)
	if s := poolScore("LeastUtilized", &p); s != 42 {
		t.Errorf("LeastUtilized score = %d, want 42", s)
	}
	if s := poolScore("FirstFit", &p); s != 0 {
		t.Errorf("FirstFit score = %d, want 0", s)
	}
	// BestFit prefers a tighter pool (larger largestFreePrefix → smaller score).
	tight := classPool("t", ipamv1alpha1.IPv4, 0, 28)
	loose := classPool("l", ipamv1alpha1.IPv4, 0, 20)
	if poolScore("BestFit", &tight) >= poolScore("BestFit", &loose) {
		t.Errorf("BestFit should score the tighter pool lower")
	}
	// Exhausted/unknown (largestFreePrefix 0) ranks worst.
	exhausted := classPool("e", ipamv1alpha1.IPv4, 0, 0)
	if poolScore("BestFit", &exhausted) != 128 {
		t.Errorf("BestFit exhausted score = %d, want 128", poolScore("BestFit", &exhausted))
	}
}
