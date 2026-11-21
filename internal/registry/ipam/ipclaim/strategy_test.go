package ipclaim

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

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

// The reserved role names the CONSUMING project, and the server reads it off
// the request rather than the body. Accepting it here is the whole attack: a
// claimant could otherwise name another project's pool by writing a scope
// reference.
//
// It is rejected rather than ignored. A claim that supplies it has
// misunderstood which pool it will reach, and dropping the field silently would
// confirm the misunderstanding — the claim would succeed, against a pool the
// author did not mean.
func TestTheReservedScopeRoleIsRefused(t *testing.T) {
	errs := validateIPClaim(claim(func(c *ipam.IPClaim) {
		c.Spec.Scope = map[string]ipam.ScopeRef{
			"project": {APIGroup: "resourcemanager.miloapis.com", Kind: "Project", Name: "somebody-else"},
		}
	}))
	if len(errs) == 0 {
		t.Fatal("spec.scope accepted the reserved role")
	}
	if got := errs[0].Field; got != "spec.scope.project" {
		t.Errorf("error names %q, want spec.scope.project", got)
	}
	if !strings.Contains(errs[0].Detail, "reserved") {
		t.Errorf("message %q does not say the name is reserved", errs[0].Detail)
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

// Validation runs before the class is resolved, so it cannot know the family.
// Assuming IPv4 rejected a /64 from an IPv6 class whenever the claim named the
// class rather than the family — which is the form the class model encourages.
// Only the bound that holds for every family belongs here; the family-specific
// one is applied against the resolved class in EffectivePrefixLength.
func TestValidatePrefixLengthDoesNotAssumeIPv4(t *testing.T) {
	prefix := func(n int32) *int32 { return &n }

	for _, tc := range []struct {
		name    string
		claim   *ipam.IPClaim
		wantErr bool
	}{
		{
			name: "IPv6-sized prefix on a claim that names only its class",
			claim: &ipam.IPClaim{Spec: ipam.IPClaimSpec{
				ClassName: "datum-subnet-ipv6", PrefixLength: prefix(64),
			}},
		},
		{
			name: "a /96 endpoint prefix, likewise",
			claim: &ipam.IPClaim{Spec: ipam.IPClaimSpec{
				ClassName: "datum-endpoint-ipv6", PrefixLength: prefix(96),
			}},
		},
		{
			name: "beyond any address family",
			claim: &ipam.IPClaim{Spec: ipam.IPClaimSpec{
				ClassName: "c", PrefixLength: prefix(129),
			}},
			wantErr: true,
		},
		{
			name: "zero is not a prefix length",
			claim: &ipam.IPClaim{Spec: ipam.IPClaimSpec{
				ClassName: "c", PrefixLength: prefix(0),
			}},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateIPClaim(tc.claim)
			var got *field.Error
			for _, e := range errs {
				if e.Field == "spec.prefixLength" {
					got = e
				}
			}
			if tc.wantErr && got == nil {
				t.Errorf("no prefixLength error, want one")
			}
			if !tc.wantErr && got != nil {
				t.Errorf("prefixLength rejected: %v", got)
			}
		})
	}
}

func TestAClaimAddressScopeMustBeKnown(t *testing.T) {
	for _, tc := range []struct {
		scope ipam.AddressScope
		valid bool
	}{
		{"", true},
		{ipam.AddressScopePublic, true},
		{ipam.AddressScopePrivate, true},
		{"Internet", false},
	} {
		t.Run(string(tc.scope), func(t *testing.T) {
			errs := validateIPClaim(claim(func(c *ipam.IPClaim) { c.Spec.AddressScope = tc.scope }))
			if tc.valid && len(errs) != 0 {
				t.Fatalf("addressScope %q rejected: %v", tc.scope, errs)
			}
			if !tc.valid && (len(errs) == 0 || errs[0].Field != "spec.addressScope") {
				t.Fatalf("errors = %v, want one on spec.addressScope", errs)
			}
		})
	}
}

func TestAClaimAddressScopeIsImmutable(t *testing.T) {
	old := claim(func(c *ipam.IPClaim) { c.Spec.AddressScope = ipam.AddressScopePublic })
	updated := old.DeepCopy()
	updated.Spec.AddressScope = ipam.AddressScopePrivate

	errs := NewStrategy(nil).ValidateUpdate(context.Background(), updated, old)
	for _, e := range errs {
		if e.Field == "spec.addressScope" {
			return
		}
	}
	t.Fatalf("errors = %v, want one forbidding spec.addressScope", errs)
}

func int32p(v int32) *int32 { return &v }

// spec.address is checked for form before anything else runs. What fits the
// class is decided on create; what cannot be an address at all is refused here.
func TestAddressForm(t *testing.T) {
	tests := []struct {
		name         string
		address      string
		prefixLength *int32
		wantErr      string
	}{
		{name: "bare IPv4", address: "10.0.0.10"},
		{name: "bare IPv6", address: "fd00::53"},
		{name: "IPv4 CIDR", address: "10.0.0.16/28"},
		{name: "IPv6 CIDR", address: "fd00:0:0:1::/64"},
		{name: "CIDR agreeing with prefixLength", address: "10.0.0.16/28", prefixLength: int32p(28)},
		{name: "bare address with prefixLength", address: "10.0.0.16", prefixLength: int32p(28)},
		// Alignment is the allocator's to judge once the size is known.
		{name: "unaligned CIDR is a form the server judges later", address: "10.0.0.5/28"},

		{name: "garbage", address: "not-an-ip", wantErr: "must be an IP address or a CIDR"},
		{name: "octet out of range", address: "10.0.0.999", wantErr: "must be an IP address or a CIDR"},
		{name: "malformed CIDR", address: "10.0.0.0/33", wantErr: "must be an IP address or a CIDR"},
		{name: "zoned", address: "fe80::1%eth0", wantErr: "zone"},
		{name: "IPv4-mapped IPv6", address: "::ffff:10.0.0.1", wantErr: "IPv4-mapped"},
		{name: "IPv4-mapped IPv6 CIDR", address: "::ffff:10.0.0.0/120", wantErr: "IPv4-mapped"},
		{name: "CIDR disagreeing with prefixLength", address: "10.0.0.32/28", prefixLength: int32p(29), wantErr: "spec.prefixLength is 29"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateIPClaim(claim(func(c *ipam.IPClaim) {
				c.Spec.Address = tc.address
				c.Spec.PrefixLength = tc.prefixLength
			}))
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("validateIPClaim() = %v, want no errors", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("validateIPClaim() accepted %q", tc.address)
			}
			if errs[0].Field != "spec.address" {
				t.Errorf("error names %q, want spec.address", errs[0].Field)
			}
			if !strings.Contains(errs[0].Detail, tc.wantErr) {
				t.Errorf("error %q does not mention %q", errs[0].Detail, tc.wantErr)
			}
		})
	}
}

// The address is what the claim is bound to; editing it would leave the claim
// naming one address while holding another.
func TestAddressIsImmutable(t *testing.T) {
	s := NewStrategy(nil)
	old := claim(func(c *ipam.IPClaim) { c.Spec.Address = "10.0.0.10" })
	for _, next := range []string{"10.0.0.11", ""} {
		updated := old.DeepCopy()
		updated.Spec.Address = next
		errs := s.ValidateUpdate(context.Background(), updated, old)
		found := false
		for _, e := range errs {
			if e.Field == "spec.address" && e.Type == field.ErrorTypeForbidden && strings.Contains(e.Detail, "immutable") {
				found = true
			}
		}
		if !found {
			t.Errorf("changing spec.address %q -> %q: errors %v, want spec.address immutable", old.Spec.Address, next, errs)
		}
	}
	added := claim()
	withAddr := added.DeepCopy()
	withAddr.Spec.Address = "10.0.0.10"
	if errs := s.ValidateUpdate(context.Background(), withAddr, added); len(errs) == 0 {
		t.Error("adding spec.address to a bound claim was accepted")
	}
	if errs := s.ValidateUpdate(context.Background(), old.DeepCopy(), old); len(errs) != 0 {
		t.Errorf("an unchanged address was refused: %v", errs)
	}
}

// A ScopeRange claim may name its range, as the CIDR of the whole range: the
// class decides the length, so a bare address names nothing yet. Its length and
// alignment against the class are judged on create.
func TestAScopeRangeClaimNamesItsRangeAsACIDR(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{name: "IPv6 range", address: "fd00:0:a::/48"},
		{name: "IPv4 range", address: "10.20.0.0/16"},
		{name: "unaligned is judged on create", address: "fd00:0:a:1::/48"},
		{name: "bare address", address: "fd00:0:a::", wantErr: true},
		{name: "garbage", address: "not-a-range/48", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := claim(func(c *ipam.IPClaim) {
				c.Spec.Target = ipam.TargetScopeRange
				c.Spec.Address = tc.address
			})
			errs := validateIPClaim(c)
			if !tc.wantErr {
				if len(errs) != 0 {
					t.Fatalf("validateIPClaim() refused %q: %v", tc.address, errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("validateIPClaim() accepted %q", tc.address)
			}
			if errs[0].Field != "spec.address" {
				t.Errorf("error names %q, want spec.address", errs[0].Field)
			}
		})
	}
}
