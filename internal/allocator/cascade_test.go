package allocator

import (
	"context"
	"encoding/json"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// --- the platform project ---------------------------------------------------

// testPlatformProject is what --platform-project is set to for these tests.
//
// It matters that this is a real project name and not "". The catalog and the
// operator-authored pools live in the platform's own project, under the same
// "project/<name>/" prefix every tenant's objects use, so a fixture seeded at an
// unprefixed key is a fixture in a keyspace no deployment populates — and a test
// over it would pass while proving nothing about what ships.
const testPlatformProject = "milo-platform"

// platformCtx is the context the apiserver's platformProjectFilter produces:
// one carrying the configured platform project. Every allocator entry point
// needs it, because a class lookup with no configured platform fails closed
// rather than falling back to a root that no longer exists.
func platformCtx() context.Context {
	return tenant.WithPlatformProject(context.Background(), testPlatformProject)
}

// platformKey is the ipam_objects key of a platform-owned object, so fixtures
// are seeded where the service would actually write them.
func platformKey(resource, name string) string {
	return tenant.Identity{Name: testPlatformProject}.ResourceKey(resource, name)
}

// --- a transaction that serves classes -------------------------------------

// classTx answers the one query the planning path makes — reading an IPClass by
// key — from an in-memory catalog. Everything else panics, so a test that
// accidentally exercises a write path fails loudly rather than silently passing.
type classTx struct {
	pgx.Tx
	classes map[string]*ipamv1alpha1.IPClass
}

func (t *classTx) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	key, _ := args[0].(string)
	name := key[strings.LastIndex(key, "/")+1:]
	class, ok := t.classes[name]
	if !ok {
		return errRow{pgx.ErrNoRows}
	}
	data, err := json.Marshal(class)
	if err != nil {
		return errRow{err}
	}
	return dataRow{data}
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type dataRow struct{ data []byte }

func (r dataRow) Scan(dest ...any) error {
	*(dest[0].(*[]byte)) = r.data
	return nil
}

// --- the doc's worked example ----------------------------------------------

func class(name string, mutate func(*ipamv1alpha1.IPClassSpec)) *ipamv1alpha1.IPClass {
	spec := ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv6}
	if mutate != nil {
		mutate(&spec)
	}
	return &ipamv1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

// tenantChain is the three-level IPv6 chain from the design doc:
// endpoint → subnet → network, with the network class at the root.
func tenantChain() *classTx {
	return &classTx{classes: map[string]*ipamv1alpha1.IPClass{
		"tenant-network-ipv6": class("tenant-network-ipv6", func(s *ipamv1alpha1.IPClassSpec) {
			s.PoolPer = []string{"network"}
			s.AllowedPrefixLengths = &ipamv1alpha1.PrefixLengthRange{Min: 48, Max: 48}
		}),
		"tenant-subnet-ipv6": class("tenant-subnet-ipv6", func(s *ipamv1alpha1.IPClassSpec) {
			s.ParentClassName = "tenant-network-ipv6"
			s.PoolPer = []string{"network", "location"}
			s.AllowedPrefixLengths = &ipamv1alpha1.PrefixLengthRange{Min: 64, Max: 64}
		}),
		"tenant-endpoint-ipv6": class("tenant-endpoint-ipv6", func(s *ipamv1alpha1.IPClassSpec) {
			s.ParentClassName = "tenant-subnet-ipv6"
			s.UniqueWithin = []string{"network"}
			s.AllowedPrefixLengths = &ipamv1alpha1.PrefixLengthRange{Min: 96, Max: 96}
		}),
	}}
}

func claimScope() map[string]ipam.ScopeRef {
	return map[string]ipam.ScopeRef{
		"network":  {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	}
}

// --- ancestry ---------------------------------------------------------------

// The leaf class provisions nothing — it binds an allocation directly — so it
// contributes no level. Its ancestors do, and they must come back nearest-first.
func TestLoadAncestryExcludesTheLeaf(t *testing.T) {
	tx := tenantChain()
	ancestry, err := LoadAncestry(platformCtx(), tx, tx.classes["tenant-endpoint-ipv6"])
	if err != nil {
		t.Fatalf("LoadAncestry: %v", err)
	}
	got := make([]string, len(ancestry))
	for i, c := range ancestry {
		got[i] = c.Name
	}
	want := []string{"tenant-subnet-ipv6", "tenant-network-ipv6"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ancestry = %v, want %v (nearest first, leaf excluded)", got, want)
	}
}

// The flat IPv4 endpoint class has no parent: its claims draw straight from
// whichever operator-authored pool offers it, and there is nothing to cascade.
func TestLoadAncestryOfARootClassIsEmpty(t *testing.T) {
	tx := tenantChain()
	ancestry, err := LoadAncestry(platformCtx(), tx, tx.classes["tenant-network-ipv6"])
	if err != nil {
		t.Fatalf("LoadAncestry: %v", err)
	}
	if len(ancestry) != 0 {
		t.Errorf("ancestry = %v, want empty", ancestry)
	}
}

// A cycle must terminate the walk rather than hang the request thread. The class
// registry rejects one at write time; this is the allocator's backstop against a
// catalog that became cyclic by some route the registry did not see.
func TestLoadAncestryRejectsACycle(t *testing.T) {
	tx := &classTx{classes: map[string]*ipamv1alpha1.IPClass{
		"a": class("a", func(s *ipamv1alpha1.IPClassSpec) { s.ParentClassName = "b" }),
		"b": class("b", func(s *ipamv1alpha1.IPClassSpec) { s.ParentClassName = "a" }),
	}}
	if _, err := LoadAncestry(platformCtx(), tx, tx.classes["a"]); err == nil {
		t.Fatal("expected a cycle to be rejected")
	}
}

// A chain that never closes must still terminate, at the cap.
func TestLoadAncestryCapsDepth(t *testing.T) {
	classes := map[string]*ipamv1alpha1.IPClass{}
	for i := range MaxClassChainDepth + 3 {
		name := string(rune('a' + i))
		parent := ""
		if i < MaxClassChainDepth+2 {
			parent = string(rune('a' + i + 1))
		}
		classes[name] = class(name, func(s *ipamv1alpha1.IPClassSpec) { s.ParentClassName = parent })
	}
	tx := &classTx{classes: classes}
	if _, err := LoadAncestry(platformCtx(), tx, classes["a"]); err == nil {
		t.Fatal("expected the depth cap to be enforced")
	}
}

// An ancestor of a different family would have the allocator carve an address of
// one family out of a pool of another.
func TestLoadAncestryRejectsFamilyDisagreement(t *testing.T) {
	tx := tenantChain()
	tx.classes["tenant-network-ipv6"].Spec.IPFamily = ipamv1alpha1.IPv4
	_, err := LoadAncestry(platformCtx(), tx, tx.classes["tenant-endpoint-ipv6"])
	if err == nil || !strings.Contains(err.Error(), "IPv4") {
		t.Fatalf("expected a family disagreement error, got %v", err)
	}
}

// --- planning ---------------------------------------------------------------

// Levels must come back root-first, because that is the order they have to be
// committed in: a level cannot be carved before the level it carves from exists.
func TestPlanCascadeOrdersLevelsRootFirst(t *testing.T) {
	tx := tenantChain()
	levels, err := PlanCascade(platformCtx(), tx, tx.classes["tenant-endpoint-ipv6"], claimScope(), "acme")
	if err != nil {
		t.Fatalf("PlanCascade: %v", err)
	}
	if len(levels) != 2 {
		t.Fatalf("levels = %d, want 2", len(levels))
	}
	if levels[0].Class.Name != "tenant-network-ipv6" {
		t.Errorf("first level = %q, want the root of the chain", levels[0].Class.Name)
	}
	if levels[1].Class.Name != "tenant-subnet-ipv6" {
		t.Errorf("second level = %q", levels[1].Class.Name)
	}
}

// Each level's scope is the claim's, projected onto that level's own poolPer —
// so the network pool is one per network and the subnet pool one per network and
// location, from a single claim carrying both.
func TestPlanCascadeProjectsPerLevel(t *testing.T) {
	tx := tenantChain()
	levels, err := PlanCascade(platformCtx(), tx, tx.classes["tenant-endpoint-ipv6"], claimScope(), "acme")
	if err != nil {
		t.Fatalf("PlanCascade: %v", err)
	}

	network := levels[0]
	if len(network.Scope) != 1 {
		t.Errorf("network level scope = %v, want only the network role", network.Scope)
	}
	if network.ScopeDigest != scope.PoolDigest("acme", map[string]ipam.ScopeRef{"network": claimScope()["network"]}) {
		t.Error("network level digest does not match its projected scope")
	}

	subnet := levels[1]
	if len(subnet.Scope) != 2 {
		t.Errorf("subnet level scope = %v, want network and location", subnet.Scope)
	}
	if subnet.ScopeDigest == network.ScopeDigest {
		t.Error("two levels with different poolPer must not share a digest")
	}
}

// A claim short a role some level of the chain needs is rejected during
// planning, before anything is built — so a claim that cannot be satisfied does
// not leave a half-built chain behind.
func TestPlanCascadeRejectsMissingRoleBeforeBuilding(t *testing.T) {
	tx := tenantChain()
	partial := map[string]ipam.ScopeRef{"network": claimScope()["network"]}

	_, err := PlanCascade(platformCtx(), tx, tx.classes["tenant-endpoint-ipv6"], partial, "acme")
	if err == nil {
		t.Fatal("expected a missing-role error")
	}
	var missing *scope.MissingRoleError
	if !asMissingRole(err, &missing) {
		t.Fatalf("expected a *scope.MissingRoleError, got %T: %v", err, err)
	}
	if strings.Join(missing.Roles, ",") != "location" {
		t.Errorf("missing roles = %v, want [location]", missing.Roles)
	}
}

// Pool keys carry the claiming project's prefix: the network's own space is
// project-scoped so the consumer can see what their network holds.
func TestPlanCascadeKeysAreProjectScoped(t *testing.T) {
	tx := tenantChain()
	levels, err := PlanCascade(platformCtx(), tx, tx.classes["tenant-endpoint-ipv6"], claimScope(), "acme")
	if err != nil {
		t.Fatalf("PlanCascade: %v", err)
	}
	for _, level := range levels {
		if !strings.HasPrefix(level.PoolKey, "project/acme/") {
			t.Errorf("pool key %q is not scoped to the claiming project", level.PoolKey)
		}
	}
}

// --- the race's central invariant -------------------------------------------

// Two racing requests must derive the *same* pool key from the same class and
// scope, or the identity row would not identify anything and the loser could not
// find the winner's pool.
//
// This is the property that makes the upsert's *row count* the answer rather
// than a key comparison: the keys are equal either way, so only whether a row
// came back can say which request inserted.
//
// It genuinely runs concurrently — Go maps randomise iteration order per range,
// so a naive implementation that ranged the scope map would produce different
// names across goroutines and this would fail. It does not test the database
// serialisation; see TestConcurrentFirstClaims in cascade_postgres_test.go for
// that, which needs a real Postgres and skips without one.
func TestPoolNameIsIdenticalAcrossConcurrentDerivations(t *testing.T) {
	const goroutines = 64
	names := make([]string, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine builds its own map, so no two share iteration order.
			s := claimScope()
			<-start
			names[i] = poolNameFor("", "tenant-subnet-ipv6", s)
		}()
	}
	close(start)
	wg.Wait()

	for i, name := range names {
		if name != names[0] {
			t.Fatalf("goroutine %d derived %q, goroutine 0 derived %q", i, name, names[0])
		}
	}
	if names[0] == "" {
		t.Fatal("derived an empty pool name")
	}
}

// The readable part of a name is a convenience; the digest suffix is what keeps
// distinct scopes distinct when sanitisation flattens them to the same text.
func TestPoolNameDistinguishesScopesThatSanitiseAlike(t *testing.T) {
	a := poolNameFor("", "c", map[string]ipam.ScopeRef{"network": {Kind: "Network", Name: "a.b"}})
	b := poolNameFor("", "c", map[string]ipam.ScopeRef{"network": {Kind: "Network", Name: "a_b"}})
	if a == b {
		t.Fatalf("two distinct scopes produced the same pool name: %q", a)
	}
}

// A reference pinned to a UID is a different address space from the same
// reference by name alone — that is what lets a network deleted and recreated
// under one name get fresh allocations rather than inheriting its predecessor's.
func TestPoolNameDistinguishesPinnedReferences(t *testing.T) {
	byName := poolNameFor("", "c", map[string]ipam.ScopeRef{"network": {Kind: "Network", Name: "default"}})
	byUID := poolNameFor("", "c", map[string]ipam.ScopeRef{"network": {Kind: "Network", Name: "default", UID: "4f2a"}})
	if byName == byUID {
		t.Fatal("a UID-pinned reference must not share a pool with the same name unpinned")
	}
}

func TestPoolNameFitsKubernetesLimit(t *testing.T) {
	long := strings.Repeat("verylongnetworkname", 40)
	name := poolNameFor("", "tenant-subnet-ipv6", map[string]ipam.ScopeRef{
		"network": {Kind: "Network", Name: long},
	})
	if len(name) > 253 {
		t.Fatalf("pool name is %d characters, over the 253 limit", len(name))
	}
	// Truncation must fall on the readable half, never the digest.
	if !strings.Contains(name, "-") || len(name) < 9 {
		t.Fatalf("name %q lost its digest suffix", name)
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"tenant-subnet-ipv6-default-us-central-1", "tenant-subnet-ipv6-default-us-central-1"},
		{"Network/Default", "network-default"},
		{"--leading-and-trailing--", "leading-and-trailing"},
		{"a...b", "a-b"},
		{"!!!", ""},
	}
	for _, tt := range tests {
		if got := sanitizeName(tt.in); got != tt.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- pool eligibility -------------------------------------------------------

// A pool declaring a location is not eligible for a claim from a different one,
// and an unlocated ancestor is never eligible in its located child's place.
func TestPoolServesScope(t *testing.T) {
	located := func(location string) *ipamv1alpha1.IPPool {
		return &ipamv1alpha1.IPPool{Spec: ipamv1alpha1.IPPoolSpec{
			Scope: map[string]ipamv1alpha1.ScopeRef{
				"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: location},
			},
		}}
	}

	tests := []struct {
		name string
		pool *ipamv1alpha1.IPPool
		in   map[string]ipam.ScopeRef
		want bool
	}{
		{
			name: "a pool declaring nothing serves everywhere",
			pool: &ipamv1alpha1.IPPool{},
			in:   claimScope(),
			want: true,
		},
		{
			name: "a pool serves a claim from its own location",
			pool: located("us-central-1"),
			in:   claimScope(),
			want: true,
		},
		{
			name: "a pool does not serve a claim from another location",
			pool: located("eu-west-1"),
			in:   claimScope(),
			want: false,
		},
		{
			name: "a located pool does not serve a claim that names no location",
			pool: located("us-central-1"),
			in:   map[string]ipam.ScopeRef{"network": claimScope()["network"]},
			want: false,
		},
		{
			name: "a claim carrying roles the pool does not declare is still served",
			pool: &ipamv1alpha1.IPPool{Spec: ipamv1alpha1.IPPoolSpec{
				Scope: map[string]ipamv1alpha1.ScopeRef{
					"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
				},
			}},
			in:   claimScope(),
			want: true,
		},
		{
			name: "a kind mismatch is not a match, whatever the name says",
			pool: &ipamv1alpha1.IPPool{Spec: ipamv1alpha1.IPPoolSpec{
				Scope: map[string]ipamv1alpha1.ScopeRef{
					"location": {APIGroup: "networking.datumapis.com", Kind: "Site", Name: "us-central-1"},
				},
			}},
			in:   claimScope(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := poolServesScope(tt.pool, tt.in); got != tt.want {
				t.Errorf("poolServesScope = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPreferPool(t *testing.T) {
	pool := func(util float64) *ipamv1alpha1.IPPool {
		return &ipamv1alpha1.IPPool{Status: ipamv1alpha1.IPPoolStatus{
			UtilizationPercent: util,
		}}
	}

	// FirstFit takes the first by storage key, which lets an operator steer
	// allocation by naming pools in the order they want them filled.
	if preferPool(ipamv1alpha1.PoolFirstFit, pool(1), pool(99)) {
		t.Error("FirstFit must never displace the first candidate")
	}
	if !preferPool(ipamv1alpha1.PoolLeastUtilized, pool(10), pool(50)) {
		t.Error("LeastUtilized must prefer the emptier pool")
	}
	if preferPool(ipamv1alpha1.PoolLeastUtilized, pool(50), pool(10)) {
		t.Error("LeastUtilized must not prefer the fuller pool")
	}

	// BestFit was removed with status.largestFreePrefix — it selected on
	// contiguous headroom, which is the one question the remaining status does
	// not answer. An unrecognised strategy must fall back to FirstFit's
	// behaviour rather than to LeastUtilized's, because silently substituting a
	// different selection rule is how a removed strategy keeps appearing to
	// work while doing something else.
	if preferPool(ipamv1alpha1.PoolSelectionStrategy("BestFit"), pool(1), pool(99)) {
		t.Error("a removed or unknown strategy must not displace the first candidate")
	}
}

// --- class-derived values ---------------------------------------------------

func TestEffectivePrefixLength(t *testing.T) {
	fixed := class("fixed", func(s *ipamv1alpha1.IPClassSpec) {
		s.AllowedPrefixLengths = &ipamv1alpha1.PrefixLengthRange{Min: 96, Max: 96}
	})
	ranged := class("ranged", func(s *ipamv1alpha1.IPClassSpec) {
		s.AllowedPrefixLengths = &ipamv1alpha1.PrefixLengthRange{Min: 64, Max: 96}
		s.DefaultPrefixLength = 96
	})

	// A fixed-size class states its size once, as min == max; requiring
	// defaultPrefixLength as well would be the same fact written twice.
	if got, err := EffectivePrefixLength(fixed, nil); err != nil || got != 96 {
		t.Errorf("fixed class default = %d, %v; want 96", got, err)
	}
	if got, err := EffectivePrefixLength(ranged, nil); err != nil || got != 96 {
		t.Errorf("ranged class default = %d, %v; want 96", got, err)
	}
	requested := int32(80)
	if got, err := EffectivePrefixLength(ranged, &requested); err != nil || got != 80 {
		t.Errorf("requested size = %d, %v; want 80", got, err)
	}
	tooWide := int32(48)
	if _, err := EffectivePrefixLength(ranged, &tooWide); err == nil {
		t.Error("a size outside the allowed range must be rejected")
	}
	silent := class("silent", nil)
	if _, err := EffectivePrefixLength(silent, nil); err == nil {
		t.Error("a class that states no size and a claim that requests none must be an error, not a guess")
	}
}

func TestEffectiveReclaimPolicy(t *testing.T) {
	retain := class("retain", func(s *ipamv1alpha1.IPClassSpec) { s.ReclaimPolicy = ipamv1alpha1.ReclaimRetain })
	silent := class("silent", nil)

	if got := EffectiveReclaimPolicy(retain, ""); got != ipamv1alpha1.ReclaimRetain {
		t.Errorf("class default = %q, want Retain", got)
	}
	if got := EffectiveReclaimPolicy(retain, ipamv1alpha1.ReclaimDelete); got != ipamv1alpha1.ReclaimDelete {
		t.Errorf("claim override = %q, want Delete", got)
	}
	if got := EffectiveReclaimPolicy(silent, ""); got != ipamv1alpha1.ReclaimDelete {
		t.Errorf("unstated policy = %q, want Delete", got)
	}
}

func TestScopeConversionRoundTrips(t *testing.T) {
	in := claimScope()
	out := scopeToInternal(scopeToVersioned(in))
	if !scope.SameRefs(in, out) {
		t.Fatalf("scope did not survive the round trip: %v -> %v", scope.CanonicalPool("", in), scope.CanonicalPool("", out))
	}
	if scopeToVersioned(nil) != nil || scopeToInternal(nil) != nil {
		t.Error("nil must convert to nil, not to an empty map")
	}
}

func asMissingRole(err error, target **scope.MissingRoleError) bool {
	for err != nil {
		if m, ok := err.(*scope.MissingRoleError); ok {
			*target = m
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- capacity ----------------------------------------------------------------

// A wide IPv6 pool used to report itself completely full while empty. Both sums
// saturated independently at MaxInt64 — total legitimately, allocated because
// 2^80 does not fit — so `available` came out 0. Anything reading capacity, such
// as an operator inventory or a "worst location" alert, would page someone about
// a pool at 0% utilization.
func TestCapacityForDoesNotRenderAnEmptyPoolAsFull(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "fd50::/20")}
	allocations := []net.IPNet{mustParseCIDR(t, "fd50::/48")}

	total, allocated, available := CapacityFor(parents, allocations)

	// Exact now, not saturated at MaxInt64: a /20 of IPv6 is 2^108 addresses
	// and the /48 carved from it is 2^80. This used to assert the ceiling.
	const (
		wantTotal     = "324518553658426726783156020576256"
		wantAllocated = "1208925819614629174706176"
		wantAvailable = "324518552449500907168526845870080"
	)
	if total != wantTotal {
		t.Errorf("total = %s, want %s (2^108, exact)", total, wantTotal)
	}
	if allocated != wantAllocated {
		t.Errorf("allocated = %s, want %s (2^80)", allocated, wantAllocated)
	}
	if available != wantAvailable {
		t.Fatalf("available = %s, want %s; a /20 with one /48 carved is very nearly empty", available, wantAvailable)
	}
	// The invariant every reader assumes.
	tot, _ := new(big.Int).SetString(total, 10)
	alloc, _ := new(big.Int).SetString(allocated, 10)
	avail, _ := new(big.Int).SetString(available, 10)
	if sum := new(big.Int).Add(alloc, avail); sum.Cmp(tot) != 0 {
		t.Errorf("allocated (%s) + available (%s) != total (%s)", allocated, available, total)
	}
}

// IPv4 was never affected and must stay exact — no scaling, no approximation.
func TestCapacityForIsExactWhenItFits(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "10.210.0.0/20")}

	total, allocated, available := CapacityFor(parents, nil)
	if total != "4096" || allocated != "0" || available != "4096" {
		t.Fatalf("empty /20 = {total %s, allocated %s, available %s}, want {4096, 0, 4096}", total, allocated, available)
	}

	// Four reserved host addresses — the leading/trailing pair from the
	// addressing plan — must show up as allocated, not as free space.
	reserved := []net.IPNet{
		mustParseCIDR(t, "10.210.0.0/32"),
		mustParseCIDR(t, "10.210.0.1/32"),
		mustParseCIDR(t, "10.210.15.254/32"),
		mustParseCIDR(t, "10.210.15.255/32"),
	}
	total, allocated, available = CapacityFor(parents, reserved)
	if total != "4096" || allocated != "4" || available != "4092" {
		t.Errorf("/20 with 4 reserved = {total %s, allocated %s, available %s}, want {4096, 4, 4092}", total, allocated, available)
	}
}

// A genuinely full pool must still read as full — the fix must not make
// exhaustion invisible in the other direction.
func TestCapacityForStillReportsAFullPool(t *testing.T) {
	parents := []net.IPNet{mustParseCIDR(t, "10.0.0.0/24")}
	_, allocated, available := CapacityFor(parents, []net.IPNet{mustParseCIDR(t, "10.0.0.0/24")})
	if available != "0" || allocated != "256" {
		t.Errorf("a fully-allocated /24 = {allocated %s, available %s}, want {256, 0}", allocated, available)
	}
}

func mustParseCIDR(t *testing.T, s string) net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return *n
}

// TestPlanCascadeSeparatesProjects is the planning half of the cross-tenant
// regression. Two projects that each have a network named `default` must plan
// distinct pools.
//
// Both halves matter and they fail differently. The DIGEST is what
// ipam_pool_identity's primary key is (class_name, scope_digest) — two projects
// agreeing on it means the second one loses the ON CONFLICT and is handed the
// first one's pool key, in the first one's key space. The KEY is what the
// storage layer prefixes; it was already per-project, which is precisely why
// the defect was invisible from the key alone.
func TestPlanCascadeSeparatesProjects(t *testing.T) {
	plan := func(project string) []CascadeLevel {
		tx := tenantChain()
		levels, err := PlanCascade(platformCtx(), tx, tx.classes["tenant-endpoint-ipv6"], claimScope(), project)
		if err != nil {
			t.Fatalf("PlanCascade for %q: %v", project, err)
		}
		return levels
	}

	acme, globex := plan("acme"), plan("globex")
	if len(acme) != len(globex) {
		t.Fatalf("plans differ in shape: %d vs %d levels", len(acme), len(globex))
	}
	for i := range acme {
		if acme[i].ScopeDigest == globex[i].ScopeDigest {
			t.Errorf("level %d (%s): two projects share address-space identity %q",
				i, acme[i].Class.Name, acme[i].ScopeDigest)
		}
		if acme[i].PoolKey == globex[i].PoolKey {
			t.Errorf("level %d (%s): two projects share pool key %q", i, acme[i].Class.Name, acme[i].PoolKey)
		}
	}

	// And the platform is its own tenant, not a synonym for "any of them".
	platform := plan("")
	for i := range acme {
		if platform[i].ScopeDigest == acme[i].ScopeDigest {
			t.Errorf("level %d (%s): a project shares the platform's address-space identity",
				i, acme[i].Class.Name)
		}
	}
}
