package ippool

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func poolWithReservations(res *ipam.ReservationSpec) *ipam.IPPool {
	return &ipam.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-v4"},
		Spec: ipam.IPPoolSpec{
			CIDR:         "10.0.0.0/20",
			IPFamily:     ipam.IPv4,
			ClassNames:   []string{"tenant-endpoint-ipv4"},
			Reservations: res,
		},
	}
}

// #48: editing spec.reservations was accepted and did nothing. The reserved
// blocks are materialised as allocation rows at create and never re-derived, so
// the pool kept exactly what it started with while the spec claimed otherwise —
// and status.capacity, correctly derived from those rows, agreed with the rows
// and made the dropped edit look applied.
func TestReservationsAreImmutable(t *testing.T) {
	old := poolWithReservations(&ipam.ReservationSpec{Leading: 2, Trailing: 2, UnitPrefixLength: 32})

	for _, tt := range []struct {
		name string
		next *ipam.ReservationSpec
	}{
		{"widened", &ipam.ReservationSpec{Leading: 8, Trailing: 2, UnitPrefixLength: 32}},
		{"narrowed", &ipam.ReservationSpec{Leading: 1, Trailing: 1, UnitPrefixLength: 32}},
		{"unit size changed", &ipam.ReservationSpec{Leading: 2, Trailing: 2, UnitPrefixLength: 30}},
		{"removed", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			errs := ipPoolStrategy{}.ValidateUpdate(context.Background(), poolWithReservations(tt.next), old)
			if len(errs) == 0 {
				t.Fatalf("%s reservations accepted; the edit would have been silently dropped", tt.name)
			}
			// The message has to say what to do instead. An immutability error
			// that only says "no" is the thing operators most dislike about
			// immutable fields, and here the alternative is not obvious.
			msg := errs[0].Error()
			if !strings.Contains(msg, "reservations") {
				t.Errorf("error does not name the field: %s", msg)
			}
			if !strings.Contains(msg, "Create a new pool") {
				t.Errorf("error does not say what to do instead: %s", msg)
			}
		})
	}
}

// Adding reservations to a pool that had none is the same edit in the other
// direction and equally silent, so it must be refused too.
func TestAddingReservationsToAPoolWithNoneIsRefused(t *testing.T) {
	errs := ipPoolStrategy{}.ValidateUpdate(context.Background(),
		poolWithReservations(&ipam.ReservationSpec{Leading: 1, Trailing: 0, UnitPrefixLength: 32}),
		poolWithReservations(nil))
	if len(errs) == 0 {
		t.Fatal("adding reservations to a pool with none was accepted")
	}
}

// An update that leaves reservations alone must still pass — the rule must not
// block the edits an operator legitimately makes, which is every other field.
func TestUnchangedReservationsDoNotBlockAnUpdate(t *testing.T) {
	res := &ipam.ReservationSpec{Leading: 2, Trailing: 2, UnitPrefixLength: 32}
	old := poolWithReservations(res)
	next := poolWithReservations(&ipam.ReservationSpec{Leading: 2, Trailing: 2, UnitPrefixLength: 32})
	next.Spec.ClassNames = []string{"tenant-endpoint-ipv4", "another-class"}

	if errs := (ipPoolStrategy{}).ValidateUpdate(context.Background(), next, old); len(errs) != 0 {
		t.Fatalf("an unrelated edit was rejected: %v", errs)
	}
	// Distinct pointers with equal values must compare equal: comparing by
	// pointer would reject every update, since the decoded object is a fresh
	// allocation every time.
	if !reservationSpecEqual(res, next.Spec.Reservations) {
		t.Error("equal reservation specs compared unequal")
	}
}
