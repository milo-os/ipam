package ipclass

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// classFor builds a minimally-valid class the tests then perturb.
func classFor(name string, family ipam.IPFamily, mutate func(*ipam.IPClass)) *ipam.IPClass {
	c := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ipam.IPClassSpec{IPFamily: family},
	}
	if mutate != nil {
		mutate(c)
	}
	return c
}

func TestValidateIPClass(t *testing.T) {
	tests := []struct {
		name      string
		class     *ipam.IPClass
		wantField string // "" means expect no errors
	}{
		{
			name:  "minimal IPv6 class is valid",
			class: classFor("tenant-endpoint-ipv6", ipam.IPv6, nil),
		},
		{
			name:      "ipFamily is required",
			class:     classFor("no-family", "", nil),
			wantField: "spec.ipFamily",
		},
		{
			name:      "ipFamily must be a known family",
			class:     classFor("bad-family", ipam.IPFamily("IPv5"), nil),
			wantField: "spec.ipFamily",
		},
		{
			name: "a class cannot be its own parent",
			class: classFor("loop", ipam.IPv4, func(c *ipam.IPClass) {
				c.Spec.ParentClassName = "loop"
			}),
			wantField: "spec.parentClassName",
		},
		{
			name: "prefix length beyond the family width is rejected",
			class: classFor("too-long-v4", ipam.IPv4, func(c *ipam.IPClass) {
				c.Spec.AllowedPrefixLengths = &ipam.PrefixLengthRange{Min: 64, Max: 64}
			}),
			wantField: "spec.allowedPrefixLengths.min",
		},
		{
			name: "a /64 range is fine for IPv6",
			class: classFor("ok-v6", ipam.IPv6, func(c *ipam.IPClass) {
				c.Spec.AllowedPrefixLengths = &ipam.PrefixLengthRange{Min: 64, Max: 96}
			}),
		},
		{
			name: "min above max is rejected",
			class: classFor("inverted", ipam.IPv6, func(c *ipam.IPClass) {
				c.Spec.AllowedPrefixLengths = &ipam.PrefixLengthRange{Min: 96, Max: 64}
			}),
			wantField: "spec.allowedPrefixLengths",
		},
		{
			name: "defaultPrefixLength outside the allowed range is rejected",
			class: classFor("bad-default", ipam.IPv6, func(c *ipam.IPClass) {
				c.Spec.AllowedPrefixLengths = &ipam.PrefixLengthRange{Min: 96, Max: 96}
				c.Spec.DefaultPrefixLength = 64
			}),
			wantField: "spec.defaultPrefixLength",
		},
		{
			name: "duplicate roles in uniqueWithin are rejected",
			class: classFor("dupe-roles", ipam.IPv4, func(c *ipam.IPClass) {
				c.Spec.UniqueWithin = []string{"network", "network"}
			}),
			wantField: "spec.uniqueWithin[1]",
		},
		{
			name: "an empty role name is rejected",
			class: classFor("empty-role", ipam.IPv4, func(c *ipam.IPClass) {
				c.Spec.PoolPer = []string{"network", ""}
			}),
			wantField: "spec.poolPer[1]",
		},
		{
			name: "the default-class annotation must be a boolean string",
			class: classFor("bad-anno", ipam.IPv4, func(c *ipam.IPClass) {
				c.Annotations = map[string]string{ipam.IsDefaultClassAnnotation: "yes"}
			}),
			wantField: "metadata.annotations[ipam.miloapis.com/is-default-class]",
		},
		{
			name: "an unknown visibility is rejected",
			class: classFor("bad-vis", ipam.IPv4, func(c *ipam.IPClass) {
				c.Spec.Visibility = "secret"
			}),
			wantField: "spec.visibility",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateIPClass(tt.class)
			if tt.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got %v", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Field == tt.wantField {
					return
				}
			}
			t.Fatalf("expected an error on %q, got %v", tt.wantField, errs)
		})
	}
}

func TestPrepareForCreateDefaults(t *testing.T) {
	c := classFor("defaults", ipam.IPv6, nil)
	NewStrategy(runtime.NewScheme()).PrepareForCreate(context.Background(), c)

	if c.Spec.Strategy != ipam.PoolFirstFit {
		t.Errorf("strategy = %q, want FirstFit", c.Spec.Strategy)
	}
	if c.Spec.ReclaimPolicy != ipam.ReclaimDelete {
		t.Errorf("reclaimPolicy = %q, want Delete", c.Spec.ReclaimPolicy)
	}
	if c.Spec.Routing.Internal != ipam.InternalRoutingNone || c.Spec.Routing.External != ipam.ExternalRoutingNone {
		t.Errorf("routing = %+v, want None/None", c.Spec.Routing)
	}
	// A class provisions nothing of its own, so there is no state to wait for.
	if c.Status.Phase != ipam.ClassReady {
		t.Errorf("phase = %q, want Ready", c.Status.Phase)
	}
}

// The immutable set is the load-bearing half of class validation: every field
// in it, changed, strands allocations that were made under its old value.
func TestValidateUpdateImmutableFields(t *testing.T) {
	base := func() *ipam.IPClass {
		return classFor("tenant-endpoint-ipv6", ipam.IPv6, func(c *ipam.IPClass) {
			c.Spec.ParentClassName = "tenant-subnet-ipv6"
			c.Spec.UniqueWithin = []string{"network"}
			c.Spec.PoolPer = []string{"network", "location"}
			c.Spec.Provisioner = "ipam.miloapis.com/native"
		})
	}

	tests := []struct {
		name      string
		mutate    func(*ipam.IPClass)
		wantField string
	}{
		{"ipFamily", func(c *ipam.IPClass) { c.Spec.IPFamily = ipam.IPv4 }, "spec.ipFamily"},
		{"parentClassName", func(c *ipam.IPClass) { c.Spec.ParentClassName = "other" }, "spec.parentClassName"},
		{"uniqueWithin", func(c *ipam.IPClass) { c.Spec.UniqueWithin = []string{"network", "location"} }, "spec.uniqueWithin"},
		{"poolPer", func(c *ipam.IPClass) { c.Spec.PoolPer = []string{"network"} }, "spec.poolPer"},
		{"provisioner", func(c *ipam.IPClass) { c.Spec.Provisioner = "someone-else" }, "spec.provisioner"},
	}

	strategy := NewStrategy(runtime.NewScheme())
	for _, tt := range tests {
		t.Run(tt.name+" is immutable", func(t *testing.T) {
			old := base()
			updated := base()
			tt.mutate(updated)

			errs := strategy.ValidateUpdate(context.Background(), updated, old)
			for _, e := range errs {
				if e.Field == tt.wantField {
					return
				}
			}
			t.Fatalf("expected %q to be rejected, got %v", tt.wantField, errs)
		})
	}

	t.Run("an unchanged class updates cleanly", func(t *testing.T) {
		if errs := strategy.ValidateUpdate(context.Background(), base(), base()); len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	// The default marker is an annotation precisely so it can move between
	// classes without either class changing meaning.
	t.Run("the default marker is mutable", func(t *testing.T) {
		old := base()
		updated := base()
		updated.Annotations = map[string]string{ipam.IsDefaultClassAnnotation: "true"}
		if errs := strategy.ValidateUpdate(context.Background(), updated, old); len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})
}

func TestSelectableFields(t *testing.T) {
	c := classFor("tenant-endpoint-ipv6", ipam.IPv6, func(c *ipam.IPClass) {
		c.Spec.ParentClassName = "tenant-subnet-ipv6"
	})
	got := SelectableFields(c)
	if got["spec.ipFamily"] != "IPv6" {
		t.Errorf("spec.ipFamily = %q", got["spec.ipFamily"])
	}
	if got["spec.parentClassName"] != "tenant-subnet-ipv6" {
		t.Errorf("spec.parentClassName = %q", got["spec.parentClassName"])
	}
}

// The strategy must reject an IPClass carrying a namespace, since the resource
// is cluster-scoped and a namespaced key would not be found by the allocator.
func TestNamespaceScoped(t *testing.T) {
	if NewStrategy(runtime.NewScheme()).NamespaceScoped() {
		t.Error("IPClass must be cluster-scoped")
	}
	if NewStatusStrategy(runtime.NewScheme()).NamespaceScoped() {
		t.Error("IPClass status must be cluster-scoped")
	}
}

func TestGetAttrsRejectsWrongType(t *testing.T) {
	if _, _, err := GetAttrs(&ipam.IPPool{}); err == nil {
		t.Fatal("expected an error for a non-IPClass object")
	} else if !strings.Contains(err.Error(), "IPClass") {
		t.Errorf("error should name the expected kind, got %v", err)
	}
}
