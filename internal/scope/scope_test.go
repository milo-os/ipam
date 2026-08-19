package scope

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func ref(name string) ipam.ScopeRef {
	return ipam.ScopeRef{APIGroup: "networking.datumapis.com", Kind: "Network", Name: name}
}

// own is the tenancy of a class whose poolPer does not name the reserved
// project role: one pool per owner, shared by every consumer. It is the shape
// most of these tests care about, so the consumer-bearing cases stand out.
func own(owner string) PoolTenancy { return PoolTenancy{Owner: owner} }

// by is the tenancy of a per-consumer class: the owner holds the definition,
// the consumer triggered the provisioning.
func by(owner, consumer string) PoolTenancy {
	return PoolTenancy{Owner: owner, Consumer: consumer}
}

func TestCanonicalGolden(t *testing.T) {
	// A golden form pins the encoding: changing it changes every digest in
	// every deployed database, which is a migration, not a refactor.
	full := map[string]ipam.ScopeRef{
		"network":  {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	}
	roles := "1:2" +
		"8:location" + "24:networking.datumapis.com" + "8:Location" + "12:us-central-1" +
		"7:network" + "24:networking.datumapis.com" + "7:Network" + "7:default"

	// A class that does not name the reserved project role: the consumer is a
	// zero-length field, not an absence, so it cannot be shifted into by a
	// neighbouring value.
	got := CanonicalPool(own("project-alpha"), full)
	want := "13:ipam.scope.v4" + "13:project-alpha" + "0:" + roles
	if got != want {
		t.Errorf("canonical form drifted\n got: %s\nwant: %s", got, want)
	}

	// A class that does. Owner and consumer are separate top-level fields
	// rather than one tenant, which is the whole of the fix: the owner keeps
	// two same-named classes apart on ipam_pool_identity's primary key, and the
	// consumer keeps two tenants' identically-scoped claims in two pools.
	got = CanonicalPool(by("project-alpha", "project-tenx"), full)
	want = "13:ipam.scope.v4" + "13:project-alpha" + "12:project-tenx" + roles
	if got != want {
		t.Errorf("per-consumer canonical form drifted\n got: %s\nwant: %s", got, want)
	}
}

// TestCanonicalAddressSpaceGolden pins the v3 encoding, for the same reason its
// v2 sibling above pins v2: changing it changes every Claim digest in every
// deployed database, which is a migration and not a refactor.
//
// Read the two goldens side by side — the difference IS the fix. v2 emits
// `13:project-alpha` once, before the role count. v3 emits it inside each role
// group, after the role name, and a scope with no roles therefore contains no
// tenant at all.
func TestCanonicalAddressSpaceGolden(t *testing.T) {
	got := CanonicalAddressSpace("project-alpha", map[string]ipam.ScopeRef{
		"network":  {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	})
	want := "13:ipam.scope.v3" + "1:2" +
		"8:location" + "13:project-alpha" + "24:networking.datumapis.com" + "8:Location" + "12:us-central-1" +
		"7:network" + "13:project-alpha" + "24:networking.datumapis.com" + "7:Network" + "7:default"
	if got != want {
		t.Errorf("canonical form drifted\n got: %s\nwant: %s", got, want)
	}

	// The empty scope, spelled out because it is the value the whole fix turns
	// on and the one the migration default is taken from. No tenant appears.
	if got, want := CanonicalAddressSpace("project-alpha", nil), "13:ipam.scope.v3"+"1:0"; got != want {
		t.Errorf("empty address-space form drifted\n got: %s\nwant: %s", got, want)
	}
}

func TestDigestIsStableAcrossMapOrder(t *testing.T) {
	// Go randomizes map iteration, so the same scope built repeatedly is the
	// real test: without the sort this fails within a handful of rounds.
	roles := []string{"network", "location", "site", "node", "link", "tenant", "zone"}
	want := ""
	for round := 0; round < 200; round++ {
		s := map[string]ipam.ScopeRef{}
		// Vary insertion order round to round as well.
		for i := range roles {
			r := roles[(i+round)%len(roles)]
			s[r] = ref(r + "-value")
		}
		got := PoolDigest(own(""), s)
		if round == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("digest varied with map order at round %d: %s != %s", round, got, want)
		}
	}
}

func TestEmptyScope(t *testing.T) {
	if PoolDigest(own(""), nil) != PoolDigest(own(""), map[string]ipam.ScopeRef{}) {
		t.Error("nil and empty scope must digest identically")
	}
	if EmptyPoolDigest(own("")) != PoolDigest(own(""), nil) {
		t.Error("EmptyDigest must equal the digest of the empty scope")
	}
	if len(EmptyPoolDigest(own(""))) != 64 {
		t.Errorf("digest width = %d, want 64", len(EmptyPoolDigest(own(""))))
	}
	// The empty scope is a real address space, not a missing value: it must
	// have a digest that a unique index can constrain.
	if strings.TrimLeft(EmptyPoolDigest(own("")), "0") == "" {
		t.Error("EmptyDigest must not be a zero/sentinel value")
	}
}

// Identity is name-based. A scope reference names an object; two references to
// the same name in the same tenant are the same address space, whether or not
// the object behind the name has been deleted and recreated in between.
//
// The consequence a caller must know: a recreated network inherits its
// predecessor's allocations rather than starting empty. Suppliers wanting fresh
// space give the replacement a different name.
func TestIdentityIsNameBased(t *testing.T) {
	first := map[string]ipam.ScopeRef{"network": ref("default")}
	recreated := map[string]ipam.ScopeRef{"network": ref("default")}

	if PoolDigest(own("p"), first) != PoolDigest(own("p"), recreated) {
		t.Error("the same name in the same tenant must be the same pool identity")
	}
	if AddressSpaceDigest("p", first) != AddressSpaceDigest("p", recreated) {
		t.Error("the same name in the same tenant must be the same address space")
	}

	renamed := map[string]ipam.ScopeRef{"network": ref("default-2")}
	if AddressSpaceDigest("p", first) == AddressSpaceDigest("p", renamed) {
		t.Error("a different name must be a different address space")
	}
}

// TestDigestCollisionAttempts is the adversarial case: every pair below is a
// deliberate attempt to make one scope encode as another by placing separator
// characters, digits, and colons inside role names and reference fields. Under
// a delimited encoding several of these pairs collide.
func TestDigestCollisionAttempts(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]ipam.ScopeRef
	}{
		{
			name: "delimiter in role name vs in ref name",
			a:    map[string]ipam.ScopeRef{"a": {Kind: "K", Name: "b|c"}},
			b:    map[string]ipam.ScopeRef{"a|b": {Kind: "K", Name: "c"}},
		},
		{
			name: "colon in role name shifts field boundary",
			a:    map[string]ipam.ScopeRef{"net": {Kind: "K", Name: "x"}},
			b:    map[string]ipam.ScopeRef{"net:1": {Kind: "K", Name: "x"}},
		},
		{
			name: "length prefix forged inside a value",
			a:    map[string]ipam.ScopeRef{"r": {APIGroup: "g", Kind: "K", Name: "1:x"}},
			b:    map[string]ipam.ScopeRef{"r": {APIGroup: "g", Kind: "K1", Name: ":x"}},
		},
		{
			name: "field shifted between apiGroup and kind",
			a:    map[string]ipam.ScopeRef{"r": {APIGroup: "ab", Kind: "c", Name: "n"}},
			b:    map[string]ipam.ScopeRef{"r": {APIGroup: "a", Kind: "bc", Name: "n"}},
		},
		{
			name: "role folded into the previous ref",
			a:    map[string]ipam.ScopeRef{"a": {Kind: "K", Name: "n"}, "b": {Kind: "K", Name: "n"}},
			b:    map[string]ipam.ScopeRef{"a": {Kind: "K", Name: "n1:b1:K1:n"}},
		},
		{
			name: "empty values against absent role",
			a:    map[string]ipam.ScopeRef{"a": {}},
			b:    map[string]ipam.ScopeRef{},
		},
		{
			name: "swapped values across roles",
			a:    map[string]ipam.ScopeRef{"x": ref("1"), "y": ref("2")},
			b:    map[string]ipam.ScopeRef{"x": ref("2"), "y": ref("1")},
		},
		{
			name: "role count differs, shared prefix",
			a:    map[string]ipam.ScopeRef{"a": ref("1")},
			b:    map[string]ipam.ScopeRef{"a": ref("1"), "b": ref("2")},
		},
		{
			name: "multibyte value with same rune count",
			a:    map[string]ipam.ScopeRef{"a": {Kind: "K", Name: "né"}},
			b:    map[string]ipam.ScopeRef{"a": {Kind: "K", Name: "ne"}},
		},
	}

	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if CanonicalPool(own(""), tc.a) == CanonicalPool(own(""), tc.b) {
				t.Fatalf("canonical forms collide: %s", CanonicalPool(own(""), tc.a))
			}
			if PoolDigest(own(""), tc.a) == PoolDigest(own(""), tc.b) {
				t.Fatalf("digests collide")
			}
			if SameRefs(tc.a, tc.b) {
				t.Fatalf("SameRefs reported two distinct scopes equal")
			}
		})
		// Also check across cases: no two distinct scopes anywhere in the
		// table share a digest.
		for _, s := range []map[string]ipam.ScopeRef{tc.a, tc.b} {
			c, d := CanonicalPool(own(""), s), PoolDigest(own(""), s)
			if prev, ok := seen[d]; ok && prev != c {
				t.Errorf("cross-case digest collision:\n  %s\n  %s", prev, c)
			}
			seen[d] = c
		}
	}
}

func TestSameRefs(t *testing.T) {
	a := map[string]ipam.ScopeRef{"network": ref("default"), "location": ref("us-central-1")}
	b := map[string]ipam.ScopeRef{"location": ref("us-central-1"), "network": ref("default")}
	if !SameRefs(a, b) {
		t.Error("scopes with the same contents must compare equal")
	}
	if !SameRefs(nil, map[string]ipam.ScopeRef{}) {
		t.Error("nil and empty must compare equal")
	}
}

func TestRoles(t *testing.T) {
	got := Roles(map[string]ipam.ScopeRef{"zone": {}, "network": {}, "location": {}})
	want := []string{"location", "network", "zone"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Roles = %v, want %v", got, want)
	}
	if got := Roles(nil); len(got) != 0 {
		t.Errorf("Roles(nil) = %v, want empty", got)
	}
}

func TestProject(t *testing.T) {
	full := map[string]ipam.ScopeRef{
		"network":  ref("default"),
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
		"tenant":   {APIGroup: "iam.miloapis.com", Kind: "Project", Name: "acme"},
	}

	tests := []struct {
		name         string
		scope        map[string]ipam.ScopeRef
		roles        []string
		want         map[string]ipam.ScopeRef
		missingRoles []string
	}{
		{
			name:  "poolPer projection",
			scope: full,
			roles: []string{"network", "location"},
			want: map[string]ipam.ScopeRef{
				"network":  full["network"],
				"location": full["location"],
			},
		},
		{
			name:  "uniqueWithin projection of one role",
			scope: full,
			roles: []string{"network"},
			want:  map[string]ipam.ScopeRef{"network": full["network"]},
		},
		{
			name:  "empty roles is the platform-wide space",
			scope: full,
			roles: nil,
			want:  map[string]ipam.ScopeRef{},
		},
		{
			name:  "duplicate roles collapse",
			scope: full,
			roles: []string{"network", "network"},
			want:  map[string]ipam.ScopeRef{"network": full["network"]},
		},
		{
			name:         "missing role is named, not widened",
			scope:        map[string]ipam.ScopeRef{"network": ref("default")},
			roles:        []string{"network", "location"},
			missingRoles: []string{"location"},
		},
		{
			name:         "missing role from an empty scope",
			scope:        nil,
			roles:        []string{"network"},
			missingRoles: []string{"network"},
		},
		{
			// One bad request naming both, not two round trips.
			name:         "every missing role is reported at once",
			scope:        map[string]ipam.ScopeRef{"tenant": full["tenant"]},
			roles:        []string{"network", "location"},
			missingRoles: []string{"network", "location"},
		},
		{
			// `network: {}` is a missing field wearing the shape of a present
			// one. Accepting it would digest to a nameless address space that
			// nothing else could ever land in.
			name:         "a ref with an empty name is missing, not a nameless space",
			scope:        map[string]ipam.ScopeRef{"network": {APIGroup: "g", Kind: "Network"}},
			roles:        []string{"network"},
			missingRoles: []string{"network"},
		},
		{
			name: "an entirely empty ref is missing",
			scope: map[string]ipam.ScopeRef{
				"network":  {},
				"location": full["location"],
			},
			roles:        []string{"network", "location"},
			missingRoles: []string{"network"},
		},
		{
			name:         "duplicate missing roles are reported once",
			scope:        nil,
			roles:        []string{"network", "network"},
			missingRoles: []string{"network"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Project(tc.scope, tc.roles)
			if tc.missingRoles != nil {
				var missing *MissingRoleError
				if !errors.As(err, &missing) {
					t.Fatalf("err = %v, want *MissingRoleError", err)
				}
				if !reflect.DeepEqual(missing.Roles, tc.missingRoles) {
					t.Errorf("missing roles = %q, want %q", missing.Roles, tc.missingRoles)
				}
				for _, role := range tc.missingRoles {
					if !strings.Contains(missing.Error(), role) {
						t.Errorf("message %q does not name role %q", missing.Error(), role)
					}
				}
				if got != nil {
					t.Errorf("got %v on error, want nil — a partial scope must never be used", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Project = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProjectReturnsAFreshMap(t *testing.T) {
	full := map[string]ipam.ScopeRef{"network": ref("default")}
	got, err := Project(full, []string{"network"})
	if err != nil {
		t.Fatal(err)
	}
	got["network"] = ref("mutated")
	if full["network"].Name != "default" {
		t.Error("Project must not alias the caller's map")
	}
}

func TestProjectForNamesTheRequiringField(t *testing.T) {
	_, err := ProjectFor(map[string]ipam.ScopeRef{}, []string{"location"}, "poolPer")
	var missing *MissingRoleError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want *MissingRoleError", err)
	}
	if missing.Required != "poolPer" {
		t.Errorf("Required = %q, want %q", missing.Required, "poolPer")
	}
	if !reflect.DeepEqual(missing.Roles, []string{"location"}) {
		t.Errorf("Roles = %q, want [location]", missing.Roles)
	}
	for _, want := range []string{"location", "poolPer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err.Error(), want)
		}
	}
}

func TestProjectDigest(t *testing.T) {
	full := map[string]ipam.ScopeRef{
		"network":  ref("default"),
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	}

	// The same claim projected onto two different classes' uniqueWithin lands
	// in two different spaces.
	perNetwork, err := ProjectPoolDigest(own(""), full, []string{"network"}, "uniqueWithin")
	if err != nil {
		t.Fatal(err)
	}
	perNetworkLocation, err := ProjectPoolDigest(own(""), full, []string{"network", "location"}, "poolPer")
	if err != nil {
		t.Fatal(err)
	}
	platformWide, err := ProjectPoolDigest(own(""), full, nil, "uniqueWithin")
	if err != nil {
		t.Fatal(err)
	}

	if perNetwork == perNetworkLocation || perNetwork == platformWide || perNetworkLocation == platformWide {
		t.Error("projections onto different role sets must produce different digests")
	}
	if platformWide != EmptyPoolDigest(own("")) {
		t.Error("an empty projection must be the platform-wide digest")
	}

	// Projecting is what makes two claims on the same network in different
	// locations share a pool under poolPer:[network] and not under
	// poolPer:[network,location].
	other := map[string]ipam.ScopeRef{
		"network":  ref("default"),
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "eu-west-1"},
	}
	otherPerNetwork, _ := ProjectPoolDigest(own(""), other, []string{"network"}, "poolPer")
	otherPerBoth, _ := ProjectPoolDigest(own(""), other, []string{"network", "location"}, "poolPer")
	if otherPerNetwork != perNetwork {
		t.Error("two locations on one network must share the per-network pool")
	}
	if otherPerBoth == perNetworkLocation {
		t.Error("two locations must not share a per-network-per-location pool")
	}

	if _, err := ProjectPoolDigest(own(""), full, []string{"site"}, "poolPer"); err == nil {
		t.Error("ProjectDigest must propagate a missing role")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		scope   map[string]ipam.ScopeRef
		wantErr string
	}{
		{name: "nil", scope: nil},
		{name: "valid", scope: map[string]ipam.ScopeRef{"network": ref("default")}},
		{
			name:  "empty apiGroup is the core group, not an error",
			scope: map[string]ipam.ScopeRef{"network": {Kind: "Network", Name: "default"}},
		},
		{
			name:    "empty role name",
			scope:   map[string]ipam.ScopeRef{"": ref("default")},
			wantErr: "empty role name",
		},
		{
			name:    "no kind",
			scope:   map[string]ipam.ScopeRef{"network": {APIGroup: "g", Name: "default"}},
			wantErr: `role "network" has no kind`,
		},
		{
			name:    "no name",
			scope:   map[string]ipam.ScopeRef{"network": {APIGroup: "g", Kind: "Network"}},
			wantErr: `role "network" has no name`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.scope)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestDigestSpread is a cheap sanity check that the digest is not degenerate:
// a thousand distinct scopes must produce a thousand distinct digests.
func TestDigestSpread(t *testing.T) {
	seen := make(map[string]string, 1000)
	for i := 0; i < 1000; i++ {
		s := map[string]ipam.ScopeRef{
			"network":  ref(fmt.Sprintf("net-%d", i%50)),
			"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: fmt.Sprintf("loc-%d", i/50)},
		}
		d := PoolDigest(own(""), s)
		if prev, ok := seen[d]; ok {
			t.Fatalf("collision between %s and %s", prev, CanonicalPool(own(""), s))
		}
		seen[d] = CanonicalPool(own(""), s)
	}
}

func BenchmarkDigest(b *testing.B) {
	s := map[string]ipam.ScopeRef{
		"network":  ref("default"),
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = PoolDigest(own(""), s)
	}
}

// TestTenantSeparatesAddressSpaces is the regression guard for the defect that
// introduced the tenant: two projects that each have a network named `default`
// are two address spaces, not one.
//
// `default` is not an adversarial choice. It is the design document's own
// example name and the name a platform that creates one network per project
// produces for every project it has, so a collision on it is the first case
// rather than an unlikely one.
// TestPoolDigestSeparatesTenants covers the half of the tenancy rule that
// belongs to pools: a pool is an object in one tenant's key space, so two
// tenants must never derive one pool identity, whatever their scopes hold.
func TestPoolDigestSeparatesTenants(t *testing.T) {
	s := map[string]ipam.ScopeRef{"network": ref("default")}

	alpha := PoolDigest(own("project-alpha"), s)
	beta := PoolDigest(own("project-beta"), s)
	platform := PoolDigest(own(""), s)

	if alpha == beta {
		t.Error("two projects' networks named `default` must get different pools")
	}
	if alpha == platform || beta == platform {
		t.Error("a project's pool must differ from the platform's for the same scope")
	}

	// The other half, and the one a fix could easily break: platform callers
	// carry no tenant, and two of them with the same scope must still land on
	// ONE pool. Without this, every platform claim would provision its own.
	if PoolDigest(own(""), s) != PoolDigest(own(""), map[string]ipam.ScopeRef{"network": ref("default")}) {
		t.Error("two platform callers with the same scope must share a digest")
	}
}

// TestPoolDigestSeparatesTenantsWithNoScope is the case that decides why
// PoolDigest keeps an unconditional tenant field while AddressSpaceDigest does
// not, and it is not hypothetical: nothing in the IPClass registry requires a
// class named as a parent to declare poolPer, so a provisioning class can
// legitimately project onto the empty scope.
//
// ipam_pool_identity's primary key is (class_name, scope_digest). If two
// tenants derived one digest here, the second would lose the ON CONFLICT and be
// handed the first's pool_key — a key in another project's key space — and
// would allocate through it, bypassing the tenant prefixing every other path
// applies. That is the defect #55 fixed, and applying the address-space rule to
// pools would reintroduce it.
func TestPoolDigestSeparatesTenantsWithNoScope(t *testing.T) {
	if EmptyPoolDigest(own("project-alpha")) == EmptyPoolDigest(own("project-beta")) {
		t.Error("a class with no poolPer must still provision one pool per tenant")
	}
	if EmptyPoolDigest(own("")) == EmptyPoolDigest(own("project-alpha")) {
		t.Error("the platform's pool must differ from a project's")
	}
}

// TestAddressSpaceDigestIgnoresTenantWithoutRefs is the regression guard for
// #64, at the unit level.
//
// `uniqueWithin: []` is the strictest setting the class model has: the class
// says nothing separates two allocations, so the pool is ONE address space and
// no two claims may hold the same block, whoever made them. A public-unicast
// IPv4 class is spelled exactly this way, and the failure this guards is one
// public address handed to two tenants.
//
// Measured before the fix: project-alpha and project-beta were both Bound to
// 10.202.0.0/32 out of one 10.202.0.0/24.
func TestAddressSpaceDigestIgnoresTenantWithoutRefs(t *testing.T) {
	if AddressSpaceDigest("project-alpha", nil) != AddressSpaceDigest("project-beta", nil) {
		t.Error("uniqueWithin: [] must be one address space across tenants, not one per tenant")
	}
	if AddressSpaceDigest("project-alpha", nil) != AddressSpaceDigest("", nil) {
		t.Error("a platform caller and a project caller with no refs share the one empty space")
	}
	if EmptyAddressSpaceDigest() != AddressSpaceDigest("anything", map[string]ipam.ScopeRef{}) {
		t.Error("EmptyAddressSpaceDigest must equal the digest of the empty scope, for any tenant")
	}
	// The tenant must be absent from the encoding entirely, not
	// cancelled out. A tenant that reached the string could be made to matter
	// again by a scope that happened to contain the right bytes.
	if strings.Contains(CanonicalAddressSpace("project-alpha", nil), "project-alpha") {
		t.Errorf("tenant leaked into the empty address-space form: %q",
			CanonicalAddressSpace("project-alpha", nil))
	}
}

// TestAddressSpaceDigestSeparatesTenantsWithRefs is the property #55
// established, which the fix for #64 must not undo.
//
// A network named `default` in project A is a different NETWORK from `default`
// in project B, so the two are two address spaces and may each hold the same
// address out of one shared pool. That is what shared tenant IPv4 requires.
func TestAddressSpaceDigestSeparatesTenantsWithRefs(t *testing.T) {
	s := map[string]ipam.ScopeRef{"network": ref("default")}

	alpha := AddressSpaceDigest("project-alpha", s)
	beta := AddressSpaceDigest("project-beta", s)

	if alpha == beta {
		t.Error("two projects' networks named `default` are two networks and must be two address spaces")
	}
	if alpha == AddressSpaceDigest("", s) {
		t.Error("a project's space must differ from the platform's for the same refs")
	}
	// Same tenant, same refs: one space. Otherwise every claim would be its own
	// space and nothing would ever conflict — which passes the check above
	// while making uniqueness meaningless.
	if alpha != AddressSpaceDigest("project-alpha", map[string]ipam.ScopeRef{"network": ref("default")}) {
		t.Error("one tenant's claims for one network must share a digest")
	}
	// Different networks in one tenant are different spaces, which is the
	// original point of uniqueWithin and is independent of tenancy.
	if alpha == AddressSpaceDigest("project-alpha", map[string]ipam.ScopeRef{"network": ref("other")}) {
		t.Error("two networks in one project must be two address spaces")
	}
}

// The two digests must never coincide. They are stored in the same column and
// distinguished only by the row's purpose, so a value that could be read as
// either would be undetectable.
func TestPoolAndAddressSpaceDigestsNeverCollide(t *testing.T) {
	scopes := []map[string]ipam.ScopeRef{
		nil,
		{"network": ref("default")},
		{"network": ref("default"), "location": ref("us-central-1")},
	}
	tenants := []string{"", "project-alpha", "project-beta"}
	// Both pool shapes: shared, and per-consumer. A v4 digest with a consumer
	// must be no more reachable from a v3 address-space form than one without,
	// and the consumer is where the two encodings come closest — v3 also emits
	// a project name after a role.
	tenancies := []PoolTenancy{}
	for _, owner := range tenants {
		tenancies = append(tenancies, own(owner))
		for _, consumer := range tenants {
			tenancies = append(tenancies, by(owner, consumer))
		}
	}

	// Only ACROSS the two kinds. Within the address-space kind, different
	// tenants sharing a digest is the fix working, not a collision.
	pools := map[string]string{}
	for _, t := range tenancies {
		for i, s := range scopes {
			pools[PoolDigest(t, s)] = fmt.Sprintf("pool(%q/%q,%d)", t.Owner, t.Consumer, i)
		}
	}
	for _, tenant := range tenants {
		for i, s := range scopes {
			d := AddressSpaceDigest(tenant, s)
			if prev, ok := pools[d]; ok {
				t.Errorf("space(%q,%d) collides with %s", tenant, i, prev)
			}
		}
	}
}

// TestAddressSpaceTenantCannotBeForged is the v3 form's version of the
// forging check, and it needs its own cases: the v2 attacks below include
// pairs that differ only in the tenant of an EMPTY scope, which v3 is
// deliberately blind to. Every case here therefore carries refs, which is
// exactly where v3 emits the tenant.
//
// What must hold: a tenant qualifier cannot be made to parse as the role it
// follows, nor as the APIGroup it precedes.
func TestAddressSpaceTenantCannotBeForged(t *testing.T) {
	pairs := []struct {
		name             string
		tenantA, tenantB string
		a, b             map[string]ipam.ScopeRef
	}{
		{
			// The role and the tenant are adjacent fields, so a split moved
			// between them must not produce one encoding.
			name:    "tenant absorbs the tail of the role name",
			tenantA: "work", tenantB: "",
			a: map[string]ipam.ScopeRef{"net": {Kind: "K", Name: "n"}},
			b: map[string]ipam.ScopeRef{"network": {Kind: "K", Name: "n"}},
		},
		{
			name:    "tenant absorbs the head of the APIGroup",
			tenantA: "", tenantB: "networking",
			a: map[string]ipam.ScopeRef{"r": {APIGroup: "networking.example", Kind: "K", Name: "n"}},
			b: map[string]ipam.ScopeRef{"r": {APIGroup: ".example", Kind: "K", Name: "n"}},
		},
		{
			name:    "colon in a tenant name shifts no boundary",
			tenantA: "p:1", tenantB: "p",
			a: map[string]ipam.ScopeRef{"1": {Kind: "K", Name: "x"}},
			b: map[string]ipam.ScopeRef{"1": {Kind: "K", Name: "x"}},
		},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			if CanonicalAddressSpace(tc.tenantA, tc.a) == CanonicalAddressSpace(tc.tenantB, tc.b) {
				t.Fatalf("canonical forms collide: %s", CanonicalAddressSpace(tc.tenantA, tc.a))
			}
			if AddressSpaceDigest(tc.tenantA, tc.a) == AddressSpaceDigest(tc.tenantB, tc.b) {
				t.Fatal("digests collide")
			}
		})
	}
}

// TestTenantCannotBeForged checks the tenant field against the same class of
// attack the rest of the encoding is length-prefixed to defeat: a tenant name
// must not be able to impersonate a role, and a role value must not be able to
// impersonate a tenant.
func TestTenantCannotBeForged(t *testing.T) {
	pairs := []struct {
		name             string
		tenantA, tenantB string
		a, b             map[string]ipam.ScopeRef
	}{
		{
			name:    "tenant absorbs the role count",
			tenantA: "acme", tenantB: "acme1:0",
			a: map[string]ipam.ScopeRef{}, b: map[string]ipam.ScopeRef{},
		},
		{
			name:    "tenant name vs role value",
			tenantA: "a", tenantB: "",
			a: map[string]ipam.ScopeRef{"r": {Kind: "K", Name: "n"}},
			b: map[string]ipam.ScopeRef{"r": {Kind: "K", Name: "n"}, "a": {Kind: "K", Name: "n"}},
		},
		{
			name:    "colon in a tenant name shifts no boundary",
			tenantA: "p:1", tenantB: "p",
			a: map[string]ipam.ScopeRef{"1": {Kind: "K", Name: "x"}},
			b: map[string]ipam.ScopeRef{"1": {Kind: "K", Name: "x"}},
		},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			if CanonicalPool(own(tc.tenantA), tc.a) == CanonicalPool(own(tc.tenantB), tc.b) {
				t.Fatalf("canonical forms collide: %s", CanonicalPool(own(tc.tenantA), tc.a))
			}
			if PoolDigest(own(tc.tenantA), tc.a) == PoolDigest(own(tc.tenantB), tc.b) {
				t.Fatal("digests collide")
			}
		})
	}
}

// TestEmptyDigestMatchesMigrationDefault pins the value literal in
// migrations/006_address_space_digest.sql.
//
// The column defaults to the empty scope's digest so that a writer which forgets
// to supply one lands in the strictest space, where the failure is a spurious
// conflict someone notices. A default that matched no digest any live code
// produces would do the opposite: those rows would sit in a space nothing can
// name, blocking nothing and blocked by nothing. That is why the constant is
// asserted here rather than trusted to stay in step.
//
// It is the ADDRESS-SPACE empty digest, not the pool one. The default exists
// for a row whose digest a writer failed to set, and the rows written without a
// digest are allocations; the strictest space for an allocation is the one
// every `uniqueWithin: []` claim shares, which is now tenant-independent. 005's
// default was the pool value because before the v3 split there was only one
// encoding.
func TestEmptyDigestMatchesMigrationDefault(t *testing.T) {
	const migrationDefault = "c86bbfc3761caa942844f05f5a8379f15cdd300f512a9d5b5baaa787c4695c42"
	if got := EmptyAddressSpaceDigest(); got != migrationDefault {
		t.Errorf("scope_digest default drifted from the schema\n  code:      %s\n  migration: %s\n"+
			"Update the DEFAULT in migrations/006_address_space_digest.sql, and read its header "+
			"first: changing the encoding invalidates every stored digest of that kind and there "+
			"is no backfill.",
			got, migrationDefault)
	}
}

// TestPoolDigestEncodingIsPinned pins the v4 canonical form.
//
// A cascade pool's NAME embeds the first eight characters of its digest, and
// its identity row is keyed on the whole thing — so a change here renames every
// provisioned pool, misses every identity lookup, and renumbers every scope,
// against a model that promises subnets appear on first use and are never
// renumbered. That is why the value is asserted rather than trusted.
//
// It has moved exactly once, and deliberately: v2 emitted one tenant field and
// could not express whether the CONSUMING project was part of a pool's
// identity, so two projects that each named a network `default` reached one
// prefix. v4 emits owner and consumer separately. The v2 value is kept below so
// the two are visibly different rather than differing somewhere nobody looks —
// changing this constant again means writing another reset migration, not
// editing a test.
func TestPoolDigestEncodingIsPinned(t *testing.T) {
	const (
		v4EmptyPlatform = "3a50033f384ac3b2790e85913e17cf59feb841c01a18375ab97106b94a0a6910"
		// What 002-era pools were named and keyed by. migrations/003 deletes
		// every row that carries a value from this encoding, because a digest
		// is a SHA-256 over a string no schema stores and there is nothing to
		// backfill from.
		v2EmptyPlatform = "6139457f3fc41de42d41d373bf75cc032c63fbedb7def334f08f8b40803793d9"
	)
	got := EmptyPoolDigest(own(""))
	if got != v4EmptyPlatform {
		t.Errorf("the pool canonical form changed: %s != %s\n"+
			"Every cascade-provisioned pool would be renamed and every scope renumbered. "+
			"If this is deliberate it is a data migration, not a refactor.", got, v4EmptyPlatform)
	}
	if got == v2EmptyPlatform {
		t.Error("v4 reproduced a v2 digest; the version tag is not doing its job")
	}
}

// TestRoleSetKey covers the comparability key uniqueWithin agreement is decided
// on. It is a set, so order and duplicates must not matter — and it must not
// collide across different sets, which is what the length prefixes are for.
func TestRoleSetKey(t *testing.T) {
	if RoleSetKey([]string{"network", "location"}) != RoleSetKey([]string{"location", "network"}) {
		t.Error("role order must not change the key")
	}
	if RoleSetKey([]string{"network", "network"}) != RoleSetKey([]string{"network"}) {
		t.Error("duplicate roles must collapse")
	}
	if RoleSetKey(nil) != RoleSetKey([]string{}) {
		t.Error("nil and empty must agree")
	}
	distinct := [][]string{
		nil,
		{"network"},
		{"location"},
		{"network", "location"},
		// The pair a delimited encoding would collide.
		{"a", "b"},
		{"ab"},
	}
	seen := map[string]int{}
	for i, roles := range distinct {
		k := RoleSetKey(roles)
		if prev, ok := seen[k]; ok {
			t.Errorf("role sets %v and %v share a key", distinct[prev], roles)
		}
		seen[k] = i
	}
}

// TestPoolDigestSeparatesConsumers is #114 at the unit level: the mechanism
// half of the fix.
//
// Two projects each reference one platform class and each name a network
// `default`. Their scopes are ref-identical and the class's owner is the same
// platform project for both, so under v2 the two derived one digest, lost the
// ipam_pool_identity race in turn, and allocated out of one prefix — no error,
// nothing to see, and one /64 backing two tenants' networks.
func TestPoolDigestSeparatesConsumers(t *testing.T) {
	s := map[string]ipam.ScopeRef{"network": ref("default")}

	x := PoolDigest(by("platform", "project-x"), s)
	y := PoolDigest(by("platform", "project-y"), s)
	if x == y {
		t.Error("two consumers of one class, each with a network named `default`, must reach two pools")
	}
	// The consumer must matter with no refs at all, which is the shape a
	// provisioning class with poolPer: [project] takes.
	if EmptyPoolDigest(by("platform", "project-x")) == EmptyPoolDigest(by("platform", "project-y")) {
		t.Error("poolPer: [project] must provision one pool per consumer")
	}
	// One consumer's claims into one scope must still share a pool. Otherwise
	// every claim would provision its own, which passes the check above while
	// making the class provision nothing shareable at all.
	if x != PoolDigest(by("platform", "project-x"), map[string]ipam.ScopeRef{"network": ref("default")}) {
		t.Error("one consumer's claims into one scope must share a pool")
	}
	// A per-consumer pool is not the shared pool. The two are different
	// identities for the same (class, scope), so a class that changed its mind
	// would provision beside the old pool rather than adopt it — which is why
	// poolPer is immutable.
	if x == PoolDigest(own("platform"), s) {
		t.Error("a per-consumer pool must not be the shared pool")
	}
}

// TestPoolDigestSharesAcrossConsumersWhenTheClassDidNotAsk is the guard against
// over-correcting, and it is the case the fix must not break.
//
// A location's announceable public IPv4 block is one /24 that every project
// with an instance there draws from. uniqueWithin: [] makes the pool one
// address space, so the exclusion constraint keeps every consumer apart and no
// two instances anywhere hold one address. Per-consumer /24s would exhaust the
// aggregate after 256 projects rather than 256 locations, and a project with
// one instance would burn 256 announceable addresses.
//
// So a class that does not name the reserved role must reach one pool for every
// consumer — deliberately, not because there was no way to ask for anything
// else.
func TestPoolDigestSharesAcrossConsumersWhenTheClassDidNotAsk(t *testing.T) {
	iad := map[string]ipam.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "iad"},
	}

	// The consumer is projected out entirely for a class whose poolPer is
	// [location], so two callers building the same scope independently — which
	// is what two claims in one location are — reach one digest.
	shared := PoolDigest(own("platform"), iad)
	if shared != PoolDigest(own("platform"), map[string]ipam.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "iad"},
	}) {
		t.Error("two projects claiming in one location must reach one announceable block")
	}
	// Two locations are still two pools. The shared case is shared across
	// CONSUMERS, not across the axis the class actually named.
	if shared == PoolDigest(own("platform"), map[string]ipam.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "fra"},
	}) {
		t.Error("two locations must not share one announceable block")
	}
}

// TestPoolDigestSeparatesOwnersWithTheSameConsumer is the primary-key case the
// Owner field exists for, and the reason the fix ADDS a field rather than
// swapping one.
//
// ipam_pool_identity is keyed on (class_name, scope_digest), and class names
// are project-scoped: two projects may each define a class called
// `tenant-ipv6`. Replacing the owner with the consumer — the issue's literal
// suggestion — would make one consumer's claims against both classes collide on
// that primary key and merge into one pool: a strictly worse version of the bug
// being fixed.
func TestPoolDigestSeparatesOwnersWithTheSameConsumer(t *testing.T) {
	s := map[string]ipam.ScopeRef{"network": ref("default")}

	if PoolDigest(by("platform-a", "project-x"), s) == PoolDigest(by("platform-b", "project-x"), s) {
		t.Error("one consumer's claims against two same-named classes must not merge into one pool")
	}
	if EmptyPoolDigest(by("platform-a", "project-x")) == EmptyPoolDigest(by("platform-b", "project-x")) {
		t.Error("the owner must separate two same-named classes even with no scope at all")
	}
	// Owner and consumer must not be interchangeable: a digest that treated the
	// pair as a set would let (a, b) and (b, a) collide.
	if PoolDigest(by("alpha", "beta"), s) == PoolDigest(by("beta", "alpha"), s) {
		t.Error("owner and consumer are not interchangeable")
	}
}

// TestPoolTenancyCannotBeForged extends the forging check to the second tenancy
// field. Owner and consumer are adjacent, so a boundary moved between them must
// not produce one encoding — otherwise a project could be named such that its
// pool identity reproduced another (owner, consumer) pair's.
func TestPoolTenancyCannotBeForged(t *testing.T) {
	pairs := []struct {
		name string
		a, b PoolTenancy
	}{
		{
			name: "consumer absorbs the tail of the owner",
			a:    by("platform", "x"), b: by("platfor", "mx"),
		},
		{
			name: "consumer absorbs the whole owner",
			a:    by("platform", ""), b: by("", "platform"),
		},
		{
			name: "consumer absorbs the role count",
			a:    by("acme", "x"), b: by("acme", "x1:0"),
		},
		{
			name: "a colon in either name shifts no boundary",
			a:    by("p:1", "q"), b: by("p", "1:q"),
		},
	}
	scopes := []map[string]ipam.ScopeRef{
		nil,
		{"network": ref("default")},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			for i, s := range scopes {
				if CanonicalPool(tc.a, s) == CanonicalPool(tc.b, s) {
					t.Fatalf("canonical forms collide at scope %d: %s", i, CanonicalPool(tc.a, s))
				}
				if PoolDigest(tc.a, s) == PoolDigest(tc.b, s) {
					t.Fatalf("digests collide at scope %d", i)
				}
			}
		})
	}
}

// TestPoolPerRolesSplitsOutTheReservedRole covers the projection half of the
// fix: the reserved role is a DECLARATION on the class, not a reference a claim
// supplies, so it must never reach Project.
func TestPoolPerRolesSplitsOutTheReservedRole(t *testing.T) {
	for _, tc := range []struct {
		name        string
		poolPer     []string
		wantRoles   []string
		wantPerCons bool
	}{
		{"nothing declared", nil, []string{}, false},
		{"one pool per location, shared", []string{"location"}, []string{"location"}, false},
		{"one pool per location per consumer", []string{"location", "project"}, []string{"location"}, true},
		{"one pool per consumer and nothing else", []string{"project"}, []string{}, true},
		{"order does not matter", []string{"project", "network", "location"}, []string{"network", "location"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roles, perConsumer := PoolPerRoles(tc.poolPer)
			if !reflect.DeepEqual(roles, tc.wantRoles) {
				t.Errorf("roles = %v, want %v", roles, tc.wantRoles)
			}
			if perConsumer != tc.wantPerCons {
				t.Errorf("perConsumer = %v, want %v", perConsumer, tc.wantPerCons)
			}
		})
	}
}

// TestProjectForIgnoresTheReservedRole is the failure the special case exists to
// prevent: without it, every claim of a per-consumer class is refused for a role
// no client can ever supply, because the value is read off the request rather
// than the body.
func TestProjectForIgnoresTheReservedRole(t *testing.T) {
	claimScope := map[string]ipam.ScopeRef{"network": ref("default")}

	got, err := ProjectFor(claimScope, []string{"network", "project"}, "poolPer")
	if err != nil {
		t.Fatalf("ProjectFor() error = %v, want nil — the reserved role must not be reported missing", err)
	}
	if _, ok := got["project"]; ok {
		t.Error("the reserved role must not appear in the projection")
	}
	if !SameRefs(got, map[string]ipam.ScopeRef{"network": ref("default")}) {
		t.Errorf("projection = %v, want the network alone", got)
	}
	// Every other missing role is still reported, and the reserved one is not
	// added to the list.
	_, err = ProjectFor(claimScope, []string{"location", "project"}, "poolPer")
	var missing *MissingRoleError
	if !errors.As(err, &missing) {
		t.Fatalf("ProjectFor() error = %v, want a MissingRoleError", err)
	}
	if !reflect.DeepEqual(missing.Roles, []string{"location"}) {
		t.Errorf("missing roles = %v, want [location] alone", missing.Roles)
	}
}

// TestWithoutReservedRoles covers what status.requiredScopeRoles is filtered
// through. Listing the reserved role there would tell a client to set a field
// the claim registry rejects.
func TestWithoutReservedRoles(t *testing.T) {
	got := WithoutReservedRoles([]string{"network", "project", "location"})
	if !reflect.DeepEqual(got, []string{"network", "location"}) {
		t.Errorf("WithoutReservedRoles() = %v, want [network location]", got)
	}
	if got := WithoutReservedRoles(nil); len(got) != 0 {
		t.Errorf("WithoutReservedRoles(nil) = %v, want empty", got)
	}
}
