package allocator

import (
	"net"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// The label keys as the outside world spells them. Written out as literals
// rather than referencing the constants on purpose: a test that compares a
// constant to itself passes after a rename, and the rename is exactly the
// change that breaks every caller outside this package (verification-conventions
// rule: the two sides of a check must not be derived from each other).
//
// Known consumers, which must be changed in the same commit as either key:
//   - test/load/Taskfile.yaml, cascade-cleanup — deletes by
//     provisioned-by-class selector, one request per level instead of one per
//     pool. A selector matching nothing deletes nothing and reports success.
const (
	wireLabelProvisionedBy = "ipam.miloapis.com/provisioned-by-class"
	wireLabelScopeDigest   = "ipam.miloapis.com/scope-digest"
)

// TestProvisionedPoolCarriesTeardownLabels pins the labels cascade teardown
// selects on.
//
// Without this, the labels are a convention: nothing in the schema or the API
// requires them, and a pool written without them is a perfectly valid pool that
// no cleanup will ever find. The failure is silent in the direction that hurts
// — `kubectl delete ippool -l <key>=<class>` against pools carrying no such
// label exits 0 having deleted nothing.
func TestProvisionedPoolCarriesTeardownLabels(t *testing.T) {
	_, cidr, err := net.ParseCIDR("2001:db8:beef::/48")
	if err != nil {
		t.Fatalf("parse test cidr: %v", err)
	}

	level := CascadeLevel{
		Class: &ipamv1alpha1.IPClass{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-subnet-ipv6"},
			Spec: ipamv1alpha1.IPClassSpec{
				IPFamily: ipamv1alpha1.IPv6,
			},
		},
		ScopeDigest: "774e2a2600000000000000000000000000000000000000000000000000000000",
		PoolName:    "tenant-subnet-ipv6-scope-774e2a26",
		PoolKey:     "project/p/ipam.miloapis.com/ippools/tenant-subnet-ipv6-scope-774e2a26",
	}

	pool := newProvisionedPool(level, "tenant-net-ipv6-root", 48, cidr)

	for _, tc := range []struct {
		key  string
		want string
	}{
		{wireLabelProvisionedBy, "tenant-subnet-ipv6"},
		{wireLabelScopeDigest, level.ScopeDigest},
	} {
		got, ok := pool.Labels[tc.key]
		if !ok {
			t.Errorf("provisioned pool is missing label %q; cascade teardown selects on it and will silently match nothing", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("label %q = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestOperatorAuthoredPoolIsNotSelectedByTeardown is the other half: teardown
// must delete what the cascade made and nothing else.
//
// It asserts the discriminator rather than the label — an operator's pool has
// no path through newProvisionedPool at all, so the guard that matters is that
// the label means "the allocator created this" and not "this pool draws from a
// class". spec.classRef is the field that fails that test, which is why
// teardown stopped using it.
func TestOperatorAuthoredPoolIsNotSelectedByTeardown(t *testing.T) {
	operatorPool := &ipamv1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "hand-written"},
		Spec: ipamv1alpha1.IPPoolSpec{
			// An operator may legitimately point a pool they authored at a class.
			ClassRef: &ipamv1alpha1.LocalRef{Name: "tenant-subnet-ipv6"},
		},
	}
	if _, ok := operatorPool.Labels[wireLabelProvisionedBy]; ok {
		t.Fatalf("an operator-authored pool must not carry %q", wireLabelProvisionedBy)
	}
	if operatorPool.Spec.ClassRef.Name != "tenant-subnet-ipv6" {
		t.Fatalf("fixture is wrong: classRef should name the class a teardown by classRef would have matched")
	}
}
