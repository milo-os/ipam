package ipamvalidation

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func TestReservations(t *testing.T) {
	path := field.NewPath("spec", "reservations")

	tests := []struct {
		name      string
		spec      *ipam.ReservationSpec
		family    ipam.IPFamily
		wantField string
		wantText  string
	}{
		{
			name:   "nil reserves nothing",
			spec:   nil,
			family: ipam.IPv6,
		},
		{
			name:   "a gateway reservation",
			spec:   &ipam.ReservationSpec{Leading: 1, UnitPrefixLength: 96},
			family: ipam.IPv6,
		},
		{
			// The capacity check cannot catch this: a /48 of /96 units holds 2^48
			// positions, so the arithmetic is happy and the transaction is not.
			name:      "an absurd count is rejected before it writes a billion rows",
			spec:      &ipam.ReservationSpec{Leading: 1000000000, UnitPrefixLength: 96},
			family:    ipam.IPv6,
			wantField: "spec.reservations",
			wantText:  "real allocation",
		},
		{
			name:      "leading + trailing are capped together",
			spec:      &ipam.ReservationSpec{Leading: 600, Trailing: 600, UnitPrefixLength: 96},
			family:    ipam.IPv6,
			wantField: "spec.reservations",
			wantText:  "must not exceed",
		},
		{
			name:      "a non-zero reservation must state its unit size",
			spec:      &ipam.ReservationSpec{Leading: 1},
			family:    ipam.IPv6,
			wantField: "spec.reservations.unitPrefixLength",
			wantText:  "required",
		},
		{
			// Zero reservations reserve nothing, so requiring a unit size for
			// positions that do not exist would be noise.
			name:   "an empty reservation needs no unit size",
			spec:   &ipam.ReservationSpec{},
			family: ipam.IPv6,
		},
		{
			name:      "a unit wider than the family is rejected",
			spec:      &ipam.ReservationSpec{Leading: 1, UnitPrefixLength: 64},
			family:    ipam.IPv4,
			wantField: "spec.reservations.unitPrefixLength",
			wantText:  "IPv4",
		},
		{
			name:      "a negative count is rejected",
			spec:      &ipam.ReservationSpec{Leading: -1, UnitPrefixLength: 96},
			family:    ipam.IPv6,
			wantField: "spec.reservations.leading",
			wantText:  "negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := Reservations(tt.spec, tt.family, path)
			if tt.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got %v", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Field == tt.wantField && strings.Contains(e.Error(), tt.wantText) {
					return
				}
			}
			t.Fatalf("expected an error on %q containing %q, got %v", tt.wantField, tt.wantText, errs)
		})
	}
}

// The cap is a policy number, and the boundary is where a policy number is most
// likely to be wrong by one.
func TestReservationCapBoundary(t *testing.T) {
	path := field.NewPath("spec", "reservations")
	at := &ipam.ReservationSpec{Leading: MaxReservedPositions, UnitPrefixLength: 96}
	if errs := Reservations(at, ipam.IPv6, path); len(errs) != 0 {
		t.Errorf("exactly the cap must be accepted, got %v", errs)
	}
	over := &ipam.ReservationSpec{Leading: MaxReservedPositions + 1, UnitPrefixLength: 96}
	if errs := Reservations(over, ipam.IPv6, path); len(errs) == 0 {
		t.Error("one over the cap must be rejected")
	}
}

func TestLease(t *testing.T) {
	path := field.NewPath("spec", "retentionLease")
	hour := metav1.Duration{Duration: time.Hour}
	zero := metav1.Duration{}
	negative := metav1.Duration{Duration: -time.Hour}

	// Unset is how "no expiry" is expressed, and it is the default — a lease
	// that defaulted to on would reclaim addresses nobody asked to reclaim.
	if errs := Lease(nil, path); len(errs) != 0 {
		t.Errorf("unset must be valid, got %v", errs)
	}
	if errs := Lease(&hour, path); len(errs) != 0 {
		t.Errorf("a positive lease must be valid, got %v", errs)
	}

	// Zero is rejected rather than read as "expire immediately", which would
	// make Retain behave as Delete while the API still said Retain.
	errs := Lease(&zero, path)
	if len(errs) == 0 {
		t.Fatal("a zero lease must be rejected")
	}
	if !strings.Contains(errs[0].Error(), "reclaimPolicy Delete") {
		t.Errorf("the error should point at the policy that does mean immediate release, got %v", errs[0])
	}
	if errs := Lease(&negative, path); len(errs) == 0 {
		t.Error("a negative lease must be rejected")
	}
}
