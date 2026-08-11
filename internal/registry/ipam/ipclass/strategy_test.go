package ipclass

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func definition(name string, mutate ...func(*ipam.IPClass)) *ipam.IPClass {
	c := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ipam.IPClassSpec{IPFamily: ipam.IPv6},
	}
	for _, m := range mutate {
		m(c)
	}
	return c
}

func reference(name, srcProject, srcName string, mutate ...func(*ipam.IPClass)) *ipam.IPClass {
	c := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipam.IPClassSpec{
			Source: &ipam.ClassSourceRef{Project: srcProject, Name: srcName},
		},
	}
	for _, m := range mutate {
		m(c)
	}
	return c
}

// Every field a definition may set is refused on a reference. A reference that
// could restate policy would be a copy, and two copies drift.
func TestAReferenceStatesNoPolicyOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		field  string
		mutate func(*ipam.IPClass)
	}{
		{"ipFamily", func(c *ipam.IPClass) { c.Spec.IPFamily = ipam.IPv6 }},
		{"parentClassName", func(c *ipam.IPClass) { c.Spec.ParentClassName = "backbone" }},
		{"poolPer", func(c *ipam.IPClass) { c.Spec.PoolPer = []string{"location"} }},
		{"uniqueWithin", func(c *ipam.IPClass) { c.Spec.UniqueWithin = []string{"network"} }},
		{"allowedPrefixLengths", func(c *ipam.IPClass) {
			c.Spec.AllowedPrefixLengths = &ipam.PrefixLengthRange{Min: 64, Max: 64}
		}},
		{"defaultPrefixLength", func(c *ipam.IPClass) { c.Spec.DefaultPrefixLength = 64 }},
		{"reservations", func(c *ipam.IPClass) { c.Spec.Reservations = &ipam.ReservationSpec{Leading: 1} }},
		{"routing", func(c *ipam.IPClass) { c.Spec.Routing = ipam.RoutingSpec{Internal: ipam.InternalRoutingHost} }},
		{"strategy", func(c *ipam.IPClass) { c.Spec.Strategy = ipam.PoolSelectionStrategy("FirstFit") }},
		{"reclaimPolicy", func(c *ipam.IPClass) { c.Spec.ReclaimPolicy = ipam.ReclaimRetain }},
		{"retentionLease", func(c *ipam.IPClass) {
			c.Spec.RetentionLease = &metav1.Duration{Duration: 1}
		}},
		{"provisioner", func(c *ipam.IPClass) { c.Spec.Provisioner = "someone" }},
		{"parameters", func(c *ipam.IPClass) { c.Spec.Parameters = map[string]string{"k": "v"} }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			errs := validateIPClass(reference("ours", "platform", "public-unicast", tc.mutate))
			if len(errs) == 0 {
				t.Fatalf("spec.%s accepted on a reference", tc.field)
			}
			if got := errs[0].Field; got != "spec."+tc.field {
				t.Errorf("error names %q, want %q — the message must say which field to remove", got, "spec."+tc.field)
			}
		})
	}
}

func TestAReferenceNamingOnlyItsSourceIsValid(t *testing.T) {
	if errs := validateIPClass(reference("ours", "platform", "public-unicast")); len(errs) != 0 {
		t.Fatalf("valid reference rejected: %v", errs)
	}
}

func TestAReferenceNeedsBothHalvesOfItsSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project string
		srcName string
		want    string
	}{
		{"no project", "", "public-unicast", "spec.source.project"},
		{"no name", "platform", "", "spec.source.name"},
		{"project is not a DNS name", "Platform Project", "public-unicast", "spec.source.project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateIPClass(reference("ours", tc.project, tc.srcName))
			if len(errs) == 0 {
				t.Fatalf("accepted source {project: %q, name: %q}", tc.project, tc.srcName)
			}
			if errs[0].Field != tc.want {
				t.Errorf("error names %q, want %q", errs[0].Field, tc.want)
			}
		})
	}
}

// A definition must say which family it hands out; the allocator has no other
// source for it and dual-stack is two classes.
func TestADefinitionRequiresAFamily(t *testing.T) {
	errs := validateIPClass(definition("standard", func(c *ipam.IPClass) { c.Spec.IPFamily = "" }))
	if len(errs) == 0 || errs[0].Field != "spec.ipFamily" {
		t.Fatalf("errors = %v, want a required error on spec.ipFamily", errs)
	}
}

func TestAClassCannotBeItsOwnParent(t *testing.T) {
	errs := validateIPClass(definition("backbone", func(c *ipam.IPClass) { c.Spec.ParentClassName = "backbone" }))
	if len(errs) == 0 || errs[0].Field != "spec.parentClassName" {
		t.Fatalf("errors = %v, want an error on spec.parentClassName", errs)
	}
}

// Reservations are applied to the pools a class provisions. Without poolPer it
// provisions none, so accepting them would record a reservation that never
// reserves anything.
func TestReservationsNeedAClassThatProvisionsPools(t *testing.T) {
	withReservations := func(c *ipam.IPClass) {
		c.Spec.Reservations = &ipam.ReservationSpec{Leading: 1, UnitPrefixLength: 96}
	}

	errs := validateIPClass(definition("subnets", withReservations))
	if len(errs) == 0 || errs[0].Field != "spec.reservations" {
		t.Fatalf("errors = %v, want an error on spec.reservations", errs)
	}

	provisioning := definition("subnets", withReservations, func(c *ipam.IPClass) {
		c.Spec.PoolPer = []string{"location"}
	})
	if errs := validateIPClass(provisioning); len(errs) != 0 {
		t.Fatalf("reservations rejected on a class that provisions pools: %v", errs)
	}
}

func TestPrefixLengthBoundsMustBeCoherent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ipam.IPClass)
		want   string
	}{
		{
			name: "min above max",
			mutate: func(c *ipam.IPClass) {
				c.Spec.AllowedPrefixLengths = &ipam.PrefixLengthRange{Min: 96, Max: 64}
			},
			want: "spec.allowedPrefixLengths",
		},
		{
			name: "default outside the allowed range",
			mutate: func(c *ipam.IPClass) {
				c.Spec.AllowedPrefixLengths = &ipam.PrefixLengthRange{Min: 64, Max: 80}
				c.Spec.DefaultPrefixLength = 96
			},
			want: "spec.defaultPrefixLength",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateIPClass(definition("standard", tc.mutate))
			if len(errs) == 0 || errs[0].Field != tc.want {
				t.Fatalf("errors = %v, want an error on %s", errs, tc.want)
			}
		})
	}
}

// The fields an existing allocation's identity was derived from cannot move.
// An allocation records the address it holds, not the policy that produced it.
func TestIdentityFieldsAreImmutable(t *testing.T) {
	strategy := ipClassStrategy{}
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		old    *ipam.IPClass
		mutate func(*ipam.IPClass)
		want   string
	}{
		{
			name:   "family",
			old:    definition("standard"),
			mutate: func(c *ipam.IPClass) { c.Spec.IPFamily = ipam.IPv4 },
			want:   "spec.ipFamily",
		},
		{
			name:   "parent",
			old:    definition("standard"),
			mutate: func(c *ipam.IPClass) { c.Spec.ParentClassName = "backbone" },
			want:   "spec.parentClassName",
		},
		{
			name:   "uniqueWithin",
			old:    definition("standard"),
			mutate: func(c *ipam.IPClass) { c.Spec.UniqueWithin = []string{"network"} },
			want:   "spec.uniqueWithin",
		},
		{
			name:   "poolPer",
			old:    definition("standard"),
			mutate: func(c *ipam.IPClass) { c.Spec.PoolPer = []string{"location"} },
			want:   "spec.poolPer",
		},
		{
			name:   "a definition becoming a reference",
			old:    definition("standard"),
			mutate: func(c *ipam.IPClass) { c.Spec.Source = &ipam.ClassSourceRef{Project: "p", Name: "n"} },
			want:   "spec.source",
		},
		{
			name:   "a reference re-pointed at another project",
			old:    reference("ours", "platform", "public-unicast"),
			mutate: func(c *ipam.IPClass) { c.Spec.Source.Project = "elsewhere" },
			want:   "spec.source",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := tc.old.DeepCopy()
			tc.mutate(updated)

			errs := strategy.ValidateUpdate(ctx, updated, tc.old)
			var found bool
			for _, e := range errs {
				if e.Field == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("errors = %v, want one forbidding %s", errs, tc.want)
			}
		})
	}
}

// Editing a field that no allocation's identity depends on stays allowed.
func TestPolicyFieldsRemainEditable(t *testing.T) {
	old := definition("standard", func(c *ipam.IPClass) { c.Spec.DefaultPrefixLength = 64 })

	updated := old.DeepCopy()
	updated.Spec.DefaultPrefixLength = 80
	updated.Spec.ReclaimPolicy = ipam.ReclaimRetain

	strategy := ipClassStrategy{}
	if errs := strategy.ValidateUpdate(context.Background(), updated, old); len(errs) != 0 {
		t.Fatalf("editable fields rejected: %v", errs)
	}
}

// The error tells the author what to do, not just that they are wrong.
func TestTheReferenceErrorSaysWhereThePolicyBelongs(t *testing.T) {
	errs := validateIPClass(reference("ours", "platform", "public-unicast", func(c *ipam.IPClass) {
		c.Spec.UniqueWithin = []string{"network"}
	}))
	if len(errs) == 0 {
		t.Fatal("no error")
	}
	if !strings.Contains(errs[0].Detail, "the class it references") {
		t.Errorf("detail = %q, want it to name where the field belongs", errs[0].Detail)
	}
}
