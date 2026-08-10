package ippool

// The structural sweep behind #47, #48, #49 and #79.
//
// Those four are one bug wearing four faces: **Create derived something from a
// spec field and Update did not.** Capacity (#47), the reservation rows (#48,
// #49) and status.scopeDigest (#79) were each found separately, each fixed
// separately, and each fix was correct and local. Nothing in the codebase said
// how many more there were.
//
// This file answers that, and answers it in a form that keeps answering. The
// property is:
//
//	every field of IPPoolSpec is either immutable, or its Create-time
//	derivation is reconciled on Update, or it has no Create-time derivation
//	at all and is read live from the stored object.
//
// The classification below is a hand-maintained judgement — no test can decide
// on its own whether a field leaves persistent state behind. What the test
// mechanises is the two halves that CAN be checked, and they are the two halves
// that go wrong:
//
//  1. **Completeness.** The set of fields is read by reflection, so adding a
//     field to IPPoolSpec fails this test until somebody classifies it. That is
//     the miss this family is made of: nobody asked the question for the new
//     field, because nothing asked it for them.
//
//  2. **The immutable claim is verified, not asserted.** Each field classified
//     immutable is actually mutated and pushed through ValidateUpdate, which
//     must refuse it by name. So relaxing an immutability rule — the exact
//     change #49 contemplates for spec.reservations — breaks this test until
//     the field is moved to `reconciledOnUpdate` and something reconciles it.
//     ValidateUpdate's own doc comment states that obligation; this is what
//     enforces it.
//
// The third bucket is the honest weak point: `readLive` rests on the claim that
// nothing persistent is derived, which only a reader can establish. Each entry
// therefore names where the field is read, so the claim is checkable in one
// jump rather than being taken on trust.

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// immutableSpecFields maps a Go field of IPPoolSpec to the spec path
// ValidateUpdate must refuse it under, and to a mutation that changes it.
//
// Membership here is a claim about ValidateUpdate and is tested against it.
var immutableSpecFields = map[string]struct {
	path   string
	mutate func(*ipam.IPPoolSpec)
}{
	// Every capacity figure and every reserved block is computed from the
	// pool's ranges. Frozen, so the Create-time computation stays true.
	"CIDR": {"spec.cidr", func(s *ipam.IPPoolSpec) { s.CIDR = "10.9.0.0/20" }},
	// Written onto every allocation row the pool hands out, and onto
	// status.ipFamily. A pool cannot change family under rows that record it.
	"IPFamily": {"spec.ipFamily", func(s *ipam.IPPoolSpec) { s.IPFamily = ipam.IPv6 }},
	// Names the pool this one's block was carved out of. The carve is a row
	// against that parent; re-pointing the reference would not move it.
	"ParentPoolRef": {"spec.parentPoolRef", func(s *ipam.IPPoolSpec) {
		s.ParentPoolRef = &ipam.LocalRef{Name: "somewhere-else"}
	}},
	// The size of that carve, already taken.
	"PrefixLength": {"spec.prefixLength", func(s *ipam.IPPoolSpec) { s.PrefixLength = 24 }},
	// #48. Materialised as real allocation rows at create and never
	// re-derived, so an edit was accepted and did nothing — while
	// status.capacity, correctly derived from those rows, agreed with the rows
	// and made the dropped edit look applied. #49 is the proposal to reconcile
	// it; until something does, immutability is what keeps the spec and the
	// rows from disagreeing.
	"Reservations": {"spec.reservations", func(s *ipam.IPPoolSpec) {
		s.Reservations = &ipam.ReservationSpec{Leading: 3, Trailing: 1, UnitPrefixLength: 32}
	}},
}

// reconciledOnUpdate names the mutable fields that DO leave persistent state
// behind, and where the reconciliation lives. These are the two that were
// found the hard way.
var reconciledOnUpdate = map[string]string{
	// #24. Create writes ipam_pool_class_offer rows; AllocatingIPPoolREST.Update
	// re-runs SyncClassOffers so the projection follows the spec.
	"ClassNames": "AllocatingIPPoolREST.Update -> allocator.SyncClassOffers",
	// #79. Create computes status.scopeDigest; ipPoolStrategy.PrepareForUpdate
	// recomputes it from the incoming spec, so a re-homed pool does not keep a
	// stale digest forever.
	"Scope": "ipPoolStrategy.PrepareForUpdate -> scope.PoolDigest",
}

// readLive names the mutable fields that derive nothing persistent. Each entry
// is the read site, so the claim can be checked rather than believed.
var readLive = map[string]string{
	// Decoded from the stored pool on every allocation
	// (internal/allocator/prefix.go, selectBlock). Changing it changes the next
	// search and nothing already written.
	"Allocation": "internal/allocator/prefix.go selectBlock",
	// Read when a lease is resolved (internal/allocator/lease.go), not captured
	// at allocation time, so lowering the cap applies to allocations already
	// out. That is the intent: the pool is the thing that runs out.
	"MaxRetentionLease": "internal/allocator/lease.go",
	// A label input on the utilization gauge and nothing else
	// (internal/allocator/prefix.go publishPrefixUtilization). The allocator
	// sets it on cascade-provisioned pools; it records lineage rather than
	// driving behaviour.
	"ClassRef": "internal/allocator/prefix.go publishPrefixUtilization",
	// Validated on write and read nowhere in this service. If that changes,
	// this entry is what has to be revisited.
	"Visibility": "validated in ipPoolStrategy; no read site in this service",
}

func baseSpecForDerivationTest() ipam.IPPoolSpec {
	return ipam.IPPoolSpec{
		CIDR:       "10.0.0.0/20",
		IPFamily:   ipam.IPv4,
		ClassNames: []string{"tenant-endpoint-ipv4"},
		Allocation: ipam.AllocationSpec{Strategy: ipam.FirstFit},
	}
}

func poolWithSpec(s ipam.IPPoolSpec) *ipam.IPPool {
	return &ipam.IPPool{ObjectMeta: metav1.ObjectMeta{Name: "sweep-v4"}, Spec: s}
}

// TestEveryIPPoolSpecFieldIsClassified is the completeness half.
//
// A new spec field lands here before it lands in production. The failure
// message is the question the family is made of not being asked.
func TestEveryIPPoolSpecFieldIsClassified(t *testing.T) {
	specType := reflect.TypeOf(ipam.IPPoolSpec{})
	for i := 0; i < specType.NumField(); i++ {
		name := specType.Field(i).Name
		_, immutable := immutableSpecFields[name]
		_, reconciled := reconciledOnUpdate[name]
		_, live := readLive[name]

		n := 0
		for _, in := range []bool{immutable, reconciled, live} {
			if in {
				n++
			}
		}
		switch n {
		case 1:
			// Classified exactly once.
		case 0:
			t.Errorf("IPPoolSpec.%s is not classified in internal/registry/ipam/ippool/spec_derivation_test.go.\n"+
				"Answer one question and add it to one of the three maps: does Create derive persistent "+
				"state from this field?\n"+
				"  no             -> readLive, naming where it is read\n"+
				"  yes, frozen    -> immutableSpecFields, and refuse it in ValidateUpdate\n"+
				"  yes, editable  -> reconciledOnUpdate, and reconcile it on the update path\n"+
				"This is the miss behind #47, #48, #49 and #79: each was a field Create derived from "+
				"and Update left alone, and each was found in production rather than here.", name)
		default:
			t.Errorf("IPPoolSpec.%s is classified %d times; the three buckets are exclusive", name, n)
		}
	}
}

// TestImmutableSpecFieldsAreActuallyRefused is the verification half.
//
// Without it the classification is a comment, and a comment does not fail when
// somebody relaxes the rule it describes. With it, relaxing an immutability
// rule breaks this test until the field moves to reconciledOnUpdate — which is
// the point at which somebody has to write the reconciliation.
func TestImmutableSpecFieldsAreActuallyRefused(t *testing.T) {
	for name, tc := range immutableSpecFields {
		t.Run(name, func(t *testing.T) {
			old := poolWithSpec(baseSpecForDerivationTest())
			next := poolWithSpec(baseSpecForDerivationTest())
			tc.mutate(&next.Spec)

			// The mutation must actually change something, or the test proves
			// nothing about the rule and passes for free.
			if reflect.DeepEqual(old.Spec, next.Spec) {
				t.Fatalf("the mutation for %s changed nothing; the case would pass against no rule at all", name)
			}

			errs := ipPoolStrategy{}.ValidateUpdate(context.Background(), next, old)
			for _, e := range errs {
				if e.Field == tc.path {
					return
				}
			}
			t.Errorf("editing spec.%s was not refused under %q (errors: %v).\n"+
				"If this rule was relaxed deliberately, move %s to reconciledOnUpdate and reconcile "+
				"its Create-time derivation on the update path. ValidateUpdate's doc comment states "+
				"that obligation; this test is what enforces it.", name, tc.path, errs, name)
		})
	}
}

// TestUnchangedSpecPassesValidateUpdate is the control the two tests above
// need. Both are about a rule firing; this one is about it not firing when it
// should not, so neither can pass against a ValidateUpdate that refuses
// everything.
func TestUnchangedSpecPassesValidateUpdate(t *testing.T) {
	old := poolWithSpec(baseSpecForDerivationTest())
	next := poolWithSpec(baseSpecForDerivationTest())
	if errs := (ipPoolStrategy{}).ValidateUpdate(context.Background(), next, old); len(errs) != 0 {
		t.Fatalf("an update that changes nothing was refused: %v", errs)
	}
}
