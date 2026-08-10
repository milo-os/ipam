package ipallocation

import (
	"context"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// An IPAllocation is a record of an allocation the server already made. Its
// spec is entirely server-derived, so no client edit of it is legitimate — and
// spec.scope in particular being editable is what let status.scopeDigest go
// stale (#82).
//
// These tests assert the PROPERTY — no spec change survives — rather than
// enumerating fields, for the same reason the check itself is structural: a
// field-by-field test is a list someone has to remember to extend, and a spec
// field added without a matching case would be silently mutable.

func newAllocation() *ipam.IPAllocation {
	return &ipam.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{Name: "alloc-1", Namespace: "default"},
		Spec: ipam.IPAllocationSpec{
			IPFamily:      ipam.IPv4,
			PoolRef:       ipam.LocalRef{Name: "pool-a"},
			ClassName:     "class-a",
			Purpose:       ipam.PurposeClaim,
			ClaimRef:      &ipam.LocalRef{Name: "claim-a"},
			Scope:         map[string]ipam.ScopeRef{"network": {Name: "net-a"}},
			ReclaimPolicy: ipam.ReclaimDelete,
		},
		Status: ipam.IPAllocationStatus{
			Phase:         ipam.AllocationReady,
			AllocatedCIDR: "10.0.0.5/32",
			ScopeDigest:   "digest-as-bound",
		},
	}
}

// mutations covers every spec field. If a new one is added without a case here,
// TestEverySpecFieldIsCovered fails and names it.
var mutations = map[string]func(*ipam.IPAllocation){
	"IPFamily":      func(a *ipam.IPAllocation) { a.Spec.IPFamily = ipam.IPv6 },
	"PoolRef":       func(a *ipam.IPAllocation) { a.Spec.PoolRef = ipam.LocalRef{Name: "pool-b"} },
	"ClassName":     func(a *ipam.IPAllocation) { a.Spec.ClassName = "class-b" },
	"Purpose":       func(a *ipam.IPAllocation) { a.Spec.Purpose = ipam.PurposePoolCarve },
	"ClaimRef":      func(a *ipam.IPAllocation) { a.Spec.ClaimRef = &ipam.LocalRef{Name: "claim-b"} },
	"Scope":         func(a *ipam.IPAllocation) { a.Spec.Scope = map[string]ipam.ScopeRef{"network": {Name: "net-b"}} },
	"ReclaimPolicy": func(a *ipam.IPAllocation) { a.Spec.ReclaimPolicy = ipam.ReclaimRetain },
	"OwnerRef":      func(a *ipam.IPAllocation) { a.Spec.OwnerRef = &ipam.ObjectRef{Name: "owner-b"} },
}

func TestSpecIsImmutable(t *testing.T) {
	s := ipAllocationStrategy{}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			old := newAllocation()
			updated := old.DeepCopy()
			mutate(updated)

			errs := s.ValidateUpdate(context.Background(), updated, old)
			if len(errs) == 0 {
				t.Fatalf("spec.%s was accepted as mutable", name)
			}
			if !strings.Contains(errs.ToAggregate().Error(), "spec is immutable") {
				t.Errorf("error does not explain the rule: %v", errs.ToAggregate())
			}
		})
	}
}

// Guards the map above against drift. A spec field with no mutation case would
// otherwise be untested while looking covered.
func TestEverySpecFieldIsCovered(t *testing.T) {
	specType := reflect.TypeOf(ipam.IPAllocationSpec{})
	for i := 0; i < specType.NumField(); i++ {
		name := specType.Field(i).Name
		if _, ok := mutations[name]; !ok {
			t.Errorf("spec field %q has no mutation case in this file; add one so "+
				"TestSpecIsImmutable actually covers it", name)
		}
	}
}

// An update that changes nothing must pass. Without this the freeze could
// "work" by rejecting every update, which would break the metadata edits that
// are still legitimate.
func TestNoOpUpdateIsAllowed(t *testing.T) {
	s := ipAllocationStrategy{}
	old := newAllocation()
	updated := old.DeepCopy()
	updated.Labels = map[string]string{"team": "net"}
	updated.Annotations = map[string]string{"note": "audited"}

	if errs := s.ValidateUpdate(context.Background(), updated, old); len(errs) > 0 {
		t.Fatalf("metadata-only update was rejected: %v", errs.ToAggregate())
	}
}

// The digest must not be recomputed from spec.Scope. Nothing reads that field;
// the value uniqueness is actually enforced on lives in
// ipam_cidr_allocations.scope_digest, and a recomputed status would disagree
// with it — a confident wrong answer in place of a stale one. Freezing the spec
// is what makes staleness unreachable, so the digest should simply be carried
// through untouched.
func TestUpdateDoesNotRecomputeScopeDigest(t *testing.T) {
	s := ipAllocationStrategy{}
	old := newAllocation()
	updated := old.DeepCopy()
	updated.Status.ScopeDigest = "client-supplied-nonsense"

	s.PrepareForUpdate(context.Background(), updated, old)

	if got := updated.Status.ScopeDigest; got != "digest-as-bound" {
		t.Errorf("scopeDigest = %q, want the bound value %q carried through unchanged",
			got, "digest-as-bound")
	}
}

// Direct creation must be refused. The object and the ipam_cidr_allocations row
// that holds the address are written together in the claim transaction; an
// object created alone names an address it does not hold.
func TestDirectCreateIsRefused(t *testing.T) {
	r := &IPAllocationStorage{}

	obj, err := r.Create(context.Background(), newAllocation(), nil, &metav1.CreateOptions{})
	if err == nil {
		t.Fatal("direct create was accepted; a phantom allocation is reachable")
	}
	if obj != nil {
		t.Errorf("refused create returned a non-nil object: %#v", obj)
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("error is not Forbidden: %v", err)
	}
	// The message must point at the supported gesture, or the caller has a
	// refusal with no route forward.
	if !strings.Contains(err.Error(), "IPClaim") {
		t.Errorf("refusal does not name the supported alternative: %v", err)
	}
}

// Create must refuse regardless of what it is handed, including an object of
// the wrong type — the guard must not depend on a successful type assertion.
func TestCreateRefusesUnknownObject(t *testing.T) {
	r := &IPAllocationStorage{}

	if _, err := r.Create(context.Background(), runtime.Object(&ipam.IPPool{}), nil, &metav1.CreateOptions{}); err == nil {
		t.Fatal("create accepted an object of the wrong type")
	}
}
