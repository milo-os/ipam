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

func TestCanonicalGolden(t *testing.T) {
	// A golden form pins the encoding: changing it changes every digest in
	// every deployed database, which is a migration, not a refactor.
	got := CanonicalPool("project-alpha", map[string]ipam.ScopeRef{
		"network":  {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1", UID: "4f2a"},
	})
	want := "13:ipam.scope.v2" + "13:project-alpha" + "1:2" +
		"8:location" + "24:networking.datumapis.com" + "8:Location" + "12:us-central-1" + "4:4f2a" +
		"7:network" + "24:networking.datumapis.com" + "7:Network" + "7:default" + "0:"
	if got != want {
		t.Errorf("canonical form drifted\n got: %s\nwant: %s", got, want)
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
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1", UID: "4f2a"},
	})
	want := "13:ipam.scope.v3" + "1:2" +
		"8:location" + "13:project-alpha" + "24:networking.datumapis.com" + "8:Location" + "12:us-central-1" + "4:4f2a" +
		"7:network" + "13:project-alpha" + "24:networking.datumapis.com" + "7:Network" + "7:default" + "0:"
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
		got := PoolDigest("", s)
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
	if PoolDigest("", nil) != PoolDigest("", map[string]ipam.ScopeRef{}) {
		t.Error("nil and empty scope must digest identically")
	}
	if EmptyPoolDigest("") != PoolDigest("", nil) {
		t.Error("EmptyDigest must equal the digest of the empty scope")
	}
	if len(EmptyPoolDigest("")) != 64 {
		t.Errorf("digest width = %d, want 64", len(EmptyPoolDigest("")))
	}
	// The empty scope is a real address space, not a missing value: it must
	// have a digest that a unique index can constrain.
	if strings.TrimLeft(EmptyPoolDigest(""), "0") == "" {
		t.Error("EmptyDigest must not be a zero/sentinel value")
	}
}

func TestUIDParticipatesInIdentity(t *testing.T) {
	byName := map[string]ipam.ScopeRef{"network": ref("default")}
	pinned := map[string]ipam.ScopeRef{"network": {
		APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default", UID: "4f2a",
	}}
	repinned := map[string]ipam.ScopeRef{"network": {
		APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default", UID: "9c81",
	}}

	if PoolDigest("", byName) == PoolDigest("", pinned) {
		t.Error("a UID-pinned ref must not share a digest with the same ref by name")
	}
	if PoolDigest("", pinned) == PoolDigest("", repinned) {
		t.Error("a network deleted and recreated (new UID) must be a different address space")
	}
	// An empty UID is exactly the name-based case, not a third thing.
	explicitEmpty := map[string]ipam.ScopeRef{"network": {
		APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default", UID: "",
	}}
	if PoolDigest("", byName) != PoolDigest("", explicitEmpty) {
		t.Error("an explicitly-empty UID must digest as an unset one")
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
			if CanonicalPool("", tc.a) == CanonicalPool("", tc.b) {
				t.Fatalf("canonical forms collide: %s", CanonicalPool("", tc.a))
			}
			if PoolDigest("", tc.a) == PoolDigest("", tc.b) {
				t.Fatalf("digests collide")
			}
			if SameRefs(tc.a, tc.b) {
				t.Fatalf("SameRefs reported two distinct scopes equal")
			}
		})
		// Also check across cases: no two distinct scopes anywhere in the
		// table share a digest.
		for _, s := range []map[string]ipam.ScopeRef{tc.a, tc.b} {
			c, d := CanonicalPool("", s), PoolDigest("", s)
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
	perNetwork, err := ProjectPoolDigest("", full, []string{"network"}, "uniqueWithin")
	if err != nil {
		t.Fatal(err)
	}
	perNetworkLocation, err := ProjectPoolDigest("", full, []string{"network", "location"}, "poolPer")
	if err != nil {
		t.Fatal(err)
	}
	platformWide, err := ProjectPoolDigest("", full, nil, "uniqueWithin")
	if err != nil {
		t.Fatal(err)
	}

	if perNetwork == perNetworkLocation || perNetwork == platformWide || perNetworkLocation == platformWide {
		t.Error("projections onto different role sets must produce different digests")
	}
	if platformWide != EmptyPoolDigest("") {
		t.Error("an empty projection must be the platform-wide digest")
	}

	// Projecting is what makes two claims on the same network in different
	// locations share a pool under poolPer:[network] and not under
	// poolPer:[network,location].
	other := map[string]ipam.ScopeRef{
		"network":  ref("default"),
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "eu-west-1"},
	}
	otherPerNetwork, _ := ProjectPoolDigest("", other, []string{"network"}, "poolPer")
	otherPerBoth, _ := ProjectPoolDigest("", other, []string{"network", "location"}, "poolPer")
	if otherPerNetwork != perNetwork {
		t.Error("two locations on one network must share the per-network pool")
	}
	if otherPerBoth == perNetworkLocation {
		t.Error("two locations must not share a per-network-per-location pool")
	}

	if _, err := ProjectPoolDigest("", full, []string{"site"}, "poolPer"); err == nil {
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
		d := PoolDigest("", s)
		if prev, ok := seen[d]; ok {
			t.Fatalf("collision between %s and %s", prev, CanonicalPool("", s))
		}
		seen[d] = CanonicalPool("", s)
	}
}

func BenchmarkDigest(b *testing.B) {
	s := map[string]ipam.ScopeRef{
		"network":  ref("default"),
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = PoolDigest("", s)
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

	alpha := PoolDigest("project-alpha", s)
	beta := PoolDigest("project-beta", s)
	platform := PoolDigest("", s)

	if alpha == beta {
		t.Error("two projects' networks named `default` must get different pools")
	}
	if alpha == platform || beta == platform {
		t.Error("a project's pool must differ from the platform's for the same scope")
	}

	// The other half, and the one a fix could easily break: platform callers
	// carry no tenant, and two of them with the same scope must still land on
	// ONE pool. Without this, every platform claim would provision its own.
	if PoolDigest("", s) != PoolDigest("", map[string]ipam.ScopeRef{"network": ref("default")}) {
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
	if EmptyPoolDigest("project-alpha") == EmptyPoolDigest("project-beta") {
		t.Error("a class with no poolPer must still provision one pool per tenant")
	}
	if EmptyPoolDigest("") == EmptyPoolDigest("project-alpha") {
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

	// Only ACROSS the two kinds. Within the address-space kind, different
	// tenants sharing a digest is the fix working, not a collision.
	pools := map[string]string{}
	for _, tenant := range tenants {
		for i, s := range scopes {
			pools[PoolDigest(tenant, s)] = fmt.Sprintf("pool(%q,%d)", tenant, i)
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
			if CanonicalPool(tc.tenantA, tc.a) == CanonicalPool(tc.tenantB, tc.b) {
				t.Fatalf("canonical forms collide: %s", CanonicalPool(tc.tenantA, tc.a))
			}
			if PoolDigest(tc.tenantA, tc.a) == PoolDigest(tc.tenantB, tc.b) {
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

// TestPoolDigestEncodingIsUnchanged pins the v2 canonical form against the
// value 005 shipped, from the other direction.
//
// The v3 split must not have perturbed pool digests at all. A cascade pool's
// NAME embeds the first eight characters of its digest, and its identity row is
// keyed on the whole thing — so a change here renames every provisioned pool,
// misses every identity lookup, and renumbers every scope, against a model that
// promises subnets appear on first use and are never renumbered. The constant
// is 005's default, which was this exact value.
func TestPoolDigestEncodingIsUnchanged(t *testing.T) {
	const v2EmptyPlatform = "6139457f3fc41de42d41d373bf75cc032c63fbedb7def334f08f8b40803793d9"
	if got := EmptyPoolDigest(""); got != v2EmptyPlatform {
		t.Errorf("the pool canonical form changed: %s != %s\n"+
			"Every cascade-provisioned pool would be renamed and every scope renumbered. "+
			"If this is deliberate it is a data migration, not a refactor.", got, v2EmptyPlatform)
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
