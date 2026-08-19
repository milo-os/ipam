package ipclaim

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func claim(mutate ...func(*ipam.IPClaim)) *ipam.IPClaim {
	c := &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       ipam.IPClaimSpec{ClassName: "tenant-subnet"},
	}
	for _, m := range mutate {
		m(c)
	}
	return c
}

// The reserved roles declare who a class's pools belong to, and the consuming
// project is read off the request rather than the body. Accepting them here is
// the whole attack: a claimant could otherwise name another project's pool by
// writing a scope reference.
//
// It is rejected rather than ignored. A claim that supplies it has
// misunderstood which pool it will reach, and dropping the field silently would
// confirm the misunderstanding — the claim would succeed, against a pool the
// author did not mean.
func TestTheReservedScopeRolesAreRefused(t *testing.T) {
	for _, role := range []string{"project", "allProjects"} {
		t.Run(role, func(t *testing.T) {
			errs := validateIPClaim(claim(func(c *ipam.IPClaim) {
				c.Spec.Scope = map[string]ipam.ScopeRef{
					role: {APIGroup: "resourcemanager.miloapis.com", Kind: "Project", Name: "somebody-else"},
				}
			}))
			if len(errs) == 0 {
				t.Fatal("spec.scope accepted a reserved role")
			}
			if got, want := errs[0].Field, "spec.scope."+role; got != want {
				t.Errorf("error names %q, want %q", got, want)
			}
			if !strings.Contains(errs[0].Detail, "reserved") {
				t.Errorf("message %q does not say the name is reserved", errs[0].Detail)
			}
		})
	}
}

// And a well-formed scope that does not name it is still accepted, so the check
// above is a check of one name rather than of scope in general.
func TestAnOrdinaryScopeIsStillAccepted(t *testing.T) {
	errs := validateIPClaim(claim(func(c *ipam.IPClaim) {
		c.Spec.Scope = map[string]ipam.ScopeRef{
			"network":  {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
			"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "iad"},
		}
	}))
	if len(errs) != 0 {
		t.Fatalf("valid scope rejected: %v", errs)
	}
}

// The structural check on every other role is unchanged: a ref with no kind or
// no name is a missing field wearing the shape of a present one.
func TestAScopeRefNeedsAKindAndAName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  ipam.ScopeRef
	}{
		{"no kind", ipam.ScopeRef{APIGroup: "networking.datumapis.com", Name: "default"}},
		{"no name", ipam.ScopeRef{APIGroup: "networking.datumapis.com", Kind: "Network"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateIPClaim(claim(func(c *ipam.IPClaim) {
				c.Spec.Scope = map[string]ipam.ScopeRef{"network": tc.ref}
			}))
			if len(errs) == 0 || errs[0].Field != "spec.scope.network" {
				t.Fatalf("errors = %v, want an error on spec.scope.network", errs)
			}
		})
	}
}
