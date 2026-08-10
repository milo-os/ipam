package ipclass

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// tenantChain is the worked example from the design doc: a three-level IPv6
// chain plus the flat IPv4 endpoint class that deliberately has no parent.
func tenantChain() []ipam.IPClass {
	return []ipam.IPClass{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-network-ipv6"},
			Spec: ipam.IPClassSpec{
				IPFamily:             ipam.IPv6,
				PoolPer:              []string{"network"},
				AllowedPrefixLengths: &ipam.PrefixLengthRange{Min: 48, Max: 48},
				ReclaimPolicy:        ipam.ReclaimRetain,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-subnet-ipv6"},
			Spec: ipam.IPClassSpec{
				IPFamily:             ipam.IPv6,
				ParentClassName:      "tenant-network-ipv6",
				PoolPer:              []string{"network", "location"},
				AllowedPrefixLengths: &ipam.PrefixLengthRange{Min: 64, Max: 64},
				ReclaimPolicy:        ipam.ReclaimRetain,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "tenant-endpoint-ipv6",
				Annotations: map[string]string{ipam.IsDefaultClassAnnotation: "true"},
			},
			Spec: ipam.IPClassSpec{
				IPFamily:             ipam.IPv6,
				ParentClassName:      "tenant-subnet-ipv6",
				UniqueWithin:         []string{"network"},
				AllowedPrefixLengths: &ipam.PrefixLengthRange{Min: 96, Max: 96},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "tenant-endpoint-ipv4",
				Annotations: map[string]string{ipam.IsDefaultClassAnnotation: "true"},
			},
			Spec: ipam.IPClassSpec{
				IPFamily:             ipam.IPv4,
				UniqueWithin:         []string{"network"},
				AllowedPrefixLengths: &ipam.PrefixLengthRange{Min: 32, Max: 32},
			},
		},
	}
}

func findClass(catalog []ipam.IPClass, name string) *ipam.IPClass {
	for i := range catalog {
		if catalog[i].Name == name {
			return &catalog[i]
		}
	}
	return nil
}

// The doc's own example must validate clean, or the rules are wrong rather than
// the catalog.
func TestWorkedExampleValidates(t *testing.T) {
	catalog := tenantChain()
	for i := range catalog {
		class := catalog[i]
		t.Run(class.Name, func(t *testing.T) {
			if errs := validateIPClass(&class); len(errs) != 0 {
				t.Fatalf("intrinsic validation failed: %v", errs)
			}
			errs, _ := validateClassCatalog(&class, catalog)
			if len(errs) != 0 {
				t.Fatalf("catalog validation failed: %v", errs)
			}
		})
	}
}

func TestParentMustExist(t *testing.T) {
	orphan := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan"},
		Spec:       ipam.IPClassSpec{IPFamily: ipam.IPv6, ParentClassName: "nonexistent"},
	}
	errs, _ := validateClassCatalog(orphan, tenantChain())
	if !hasFieldError(errs, "spec.parentClassName", "does not exist") {
		t.Fatalf("expected a missing-parent error, got %v", errs)
	}
}

func TestParentMustShareAddressFamily(t *testing.T) {
	// An IPv4 class carving from the IPv6 network chain: there is no address in
	// the parent this class could ever hand out.
	mixed := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "mixed"},
		Spec: ipam.IPClassSpec{
			IPFamily:        ipam.IPv4,
			ParentClassName: "tenant-subnet-ipv6",
		},
	}
	errs, _ := validateClassCatalog(mixed, tenantChain())
	if !hasFieldError(errs, "spec.parentClassName", "share an address family") {
		t.Fatalf("expected a family-disagreement error, got %v", errs)
	}
}

func TestChildBlocksMustBeSmallerThanParents(t *testing.T) {
	// A /48 carved from a /64 parent: the child is wider than the block it
	// comes from.
	tooWide := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "too-wide"},
		Spec: ipam.IPClassSpec{
			IPFamily:             ipam.IPv6,
			ParentClassName:      "tenant-subnet-ipv6",
			AllowedPrefixLengths: &ipam.PrefixLengthRange{Min: 48, Max: 48},
		},
	}
	errs, _ := validateClassCatalog(tooWide, tenantChain())
	if !hasFieldError(errs, "spec.allowedPrefixLengths.min", "strictly smaller") {
		t.Fatalf("expected a prefix-length error, got %v", errs)
	}

	// Equal sizes are also wrong: a /64 carved from a /64 consumes the parent
	// whole and leaves nothing for a second claim.
	equal := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "equal"},
		Spec: ipam.IPClassSpec{
			IPFamily:             ipam.IPv6,
			ParentClassName:      "tenant-subnet-ipv6",
			AllowedPrefixLengths: &ipam.PrefixLengthRange{Min: 64, Max: 64},
		},
	}
	errs, _ = validateClassCatalog(equal, tenantChain())
	if !hasFieldError(errs, "spec.allowedPrefixLengths.min", "strictly smaller") {
		t.Fatalf("expected equal prefix lengths to be rejected, got %v", errs)
	}
}

// A cycle cannot be created one class at a time when parents must pre-exist,
// but the walk must terminate on one regardless — a corrupted or hand-edited
// store must not hang the request thread.
func TestCycleIsRejectedAndTerminates(t *testing.T) {
	catalog := []ipam.IPClass{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a"},
			Spec:       ipam.IPClassSpec{IPFamily: ipam.IPv6, ParentClassName: "b"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "b"},
			Spec:       ipam.IPClassSpec{IPFamily: ipam.IPv6, ParentClassName: "a"},
		},
	}
	errs, _ := validateClassCatalog(&catalog[0], catalog)
	if !hasFieldError(errs, "spec.parentClassName", "cycle") {
		t.Fatalf("expected a cycle error, got %v", errs)
	}
}

func TestChainDepthIsCapped(t *testing.T) {
	// A chain one longer than the cap, each level a valid parent of the next.
	depth := maxClassChainDepth + 2
	var catalog []ipam.IPClass
	for i := range depth {
		spec := ipam.IPClassSpec{IPFamily: ipam.IPv6}
		if i > 0 {
			spec.ParentClassName = fmt.Sprintf("level-%d", i-1)
		}
		catalog = append(catalog, ipam.IPClass{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("level-%d", i)},
			Spec:       spec,
		})
	}

	deepest := &catalog[depth-1]
	errs, _ := validateClassCatalog(deepest, catalog)
	if !hasFieldError(errs, "spec.parentClassName", "maximum depth") {
		t.Fatalf("expected a depth error at level %d, got %v", depth-1, errs)
	}

	// A chain right at the cap is still fine.
	atCap := &catalog[maxClassChainDepth]
	errs, _ = validateClassCatalog(atCap, catalog[:maxClassChainDepth+1])
	if hasFieldError(errs, "spec.parentClassName", "maximum depth") {
		t.Fatalf("a chain at the cap should be accepted, got %v", errs)
	}
}

func TestAtMostOneDefaultClassPerFamily(t *testing.T) {
	catalog := tenantChain()

	// A second IPv6 default collides with tenant-endpoint-ipv6.
	rival := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "rival-default-ipv6",
			Annotations: map[string]string{ipam.IsDefaultClassAnnotation: "true"},
		},
		Spec: ipam.IPClassSpec{IPFamily: ipam.IPv6},
	}
	errs, _ := validateClassCatalog(rival, catalog)
	if !hasFieldError(errs, "metadata.annotations[ipam.miloapis.com/is-default-class]", "already the default") {
		t.Fatalf("expected a duplicate-default error, got %v", errs)
	}

	// The marker is per family, so an IPv4 default alongside an IPv6 one is
	// exactly what the worked example has.
	catalogNoV4 := catalog[:3]
	v4 := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "public-default-ipv4",
			Annotations: map[string]string{ipam.IsDefaultClassAnnotation: "true"},
		},
		Spec: ipam.IPClassSpec{IPFamily: ipam.IPv4},
	}
	if errs, _ := validateClassCatalog(v4, catalogNoV4); len(errs) != 0 {
		t.Fatalf("an IPv4 default alongside an IPv6 one must be allowed, got %v", errs)
	}

	// Re-writing the class that already holds the marker must not collide with
	// itself.
	existing := findClass(catalog, "tenant-endpoint-ipv6")
	if errs, _ := validateClassCatalog(existing, catalog); len(errs) != 0 {
		t.Fatalf("a class must not collide with its own stored copy, got %v", errs)
	}
}

// poolPer on a class nothing carves from is inert, but rejecting it would make
// the catalog impossible to author: a parent is necessarily written before its
// children.
func TestPoolPerWithoutChildrenWarnsRatherThanFails(t *testing.T) {
	lone := &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-network-ipv6"},
		Spec: ipam.IPClassSpec{
			IPFamily: ipam.IPv6,
			PoolPer:  []string{"network"},
		},
	}
	errs, warnings := validateClassCatalog(lone, nil)
	if len(errs) != 0 {
		t.Fatalf("poolPer without children must not be an error, got %v", errs)
	}
	if !hasWarning(warnings, "poolPer has no effect") {
		t.Fatalf("expected a poolPer warning, got %v", warnings)
	}

	// Once a child names it, the warning goes away.
	_, warnings = validateClassCatalog(findClass(tenantChain(), "tenant-network-ipv6"), tenantChain())
	if hasWarning(warnings, "poolPer has no effect") {
		t.Fatalf("a class with children must not warn, got %v", warnings)
	}
}

func hasFieldError(errs field.ErrorList, path, substr string) bool {
	for _, e := range errs {
		if e.Field == path && strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// requiredScopeRoles must be the same set the claim path rejects against, or a
// client that satisfies it still gets a missing-role 400 — which is worse than
// not publishing the field at all, because it looks authoritative.
func TestRequiredScopeRoles(t *testing.T) {
	catalog := tenantChain()

	tests := []struct {
		class string
		want  []string
	}{
		{
			// Three levels down: its own uniqueWithin, plus the poolPer of both
			// ancestors. The location comes only from the subnet class.
			class: "tenant-endpoint-ipv6",
			want:  []string{"location", "network"},
		},
		{
			// A container class named as a parent still has a chain above it.
			class: "tenant-subnet-ipv6",
			want:  []string{"network"},
		},
		{
			// The root of a chain needs nothing beyond its own uniqueWithin,
			// which here is empty.
			class: "tenant-network-ipv6",
			want:  nil,
		},
		{
			// A chain of one: no parent, so uniqueWithin is the whole answer.
			class: "tenant-endpoint-ipv4",
			want:  []string{"network"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.class, func(t *testing.T) {
			got := requiredScopeRoles(findClass(catalog, tt.class), catalog)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("requiredScopeRoles = %v, want %v", got, tt.want)
			}
		})
	}
}

// Sorted output means a rewrite that changes nothing produces no diff, so a
// change to the field is a real change to the class.
func TestRequiredScopeRolesIsStable(t *testing.T) {
	catalog := tenantChain()
	class := findClass(catalog, "tenant-endpoint-ipv6")
	first := requiredScopeRoles(class, catalog)
	for range 8 {
		if got := requiredScopeRoles(class, catalog); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("requiredScopeRoles is not stable: %v then %v", first, got)
		}
	}
}

// A class stating reservations while provisioning nothing means the author
// expected a reservation somewhere and would get none.
func TestReservationsRequirePoolPer(t *testing.T) {
	class := classFor("leaf", ipam.IPv6, func(c *ipam.IPClass) {
		c.Spec.Reservations = &ipam.ReservationSpec{Leading: 1, UnitPrefixLength: 96}
	})
	errs := validateIPClass(class)
	if !hasFieldError(errs, "spec.reservations", "provisions none") {
		t.Fatalf("expected reservations without poolPer to be rejected, got %v", errs)
	}

	provisioner := classFor("subnet", ipam.IPv6, func(c *ipam.IPClass) {
		c.Spec.PoolPer = []string{"network", "location"}
		c.Spec.Reservations = &ipam.ReservationSpec{Leading: 1, UnitPrefixLength: 96}
	})
	if errs := validateIPClass(provisioner); len(errs) != 0 {
		t.Fatalf("a provisioning class may state reservations, got %v", errs)
	}
}
