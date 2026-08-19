// Package scope canonicalizes the scope maps that claims, pools, and
// allocations carry, and reduces one to a fixed-width digest.
//
// A scope is a set of opaque references keyed by role — the network a claim is
// made for, the location it lives in, or anything else a class names. The
// allocator never interprets them; it compares them. That comparison happens in
// the database, over a digest, because a map cannot be a unique index key.
//
// There are two digests. They answer different questions, the tenant enters
// them differently, and using the wrong one fails on the success path rather
// than returning an error:
//
//	PoolDigest         identity of a POOL, keyed on the project that OWNS the
//	                   class and, when the class asked for it, the project
//	                   CONSUMING it
//	AddressSpaceDigest the uniqueness domain an ALLOCATION lives in
//
// Each documents where its tenant goes and what the other choice would break.
//
// Tenants are plain strings — a pair, PoolTenancy, for pools — and this package
// imports nothing but the API types. It is imported by the allocator and by three registries, none of which
// should acquire a transitive k8s.io/apiserver dependency through it.
package scope

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// The version tags. Each is the first field of its canonical form, so a digest
// produced by one encoding can never be read as a digest of another.
//
// Note what a tag does and does not buy: new digests cannot collide with old
// ones, but any two digests are 64 hex characters and are not distinguishable
// by looking at them. A stored digest therefore cannot be recognised, and
// cannot be recomputed in SQL: it is a SHA-256 over a string the schema does not
// store. A change to either encoding therefore has no backfill, only a reset of
// the rows that encoding wrote.
const (
	// canonicalPoolVersion must not change without a reset. A cascade pool's
	// NAME embeds its digest, so re-tagging renames every provisioned pool: the
	// identity lookup misses, a new pool is provisioned, and the scope is
	// renumbered — against a model that promises subnets appear on first use and
	// are never renumbered.
	//
	// v4 replaced v2's single tenant field with the owner/consumer pair
	// PoolTenancy carries, so that a class can declare whether the consuming
	// project is part of a pool's identity. Every v2 digest is therefore stale,
	// and migration 003 resets the rows that held them rather than backfilling
	// values no schema stores the inputs for.
	canonicalPoolVersion = "ipam.scope.v4"

	// canonicalAddressSpaceVersion tags the form in which the tenant qualifies
	// each ref rather than standing as its own field.
	canonicalAddressSpaceVersion = "ipam.scope.v3"
)

// The two reserved PoolPer roles. They are the answer to one question — does a
// class's pool belong to the consumer or to everyone — and a class that
// provisions pools must give one of them.
//
// Reserved rather than ordinary because neither is a reference: no claim
// supplies them, and the value behind ReservedRoleProject is read off the
// request tenant. An IPClass may not use either in UniqueWithin and an IPClaim
// may not supply either in spec.scope, both refused at write time — otherwise a
// claimant could name another project's pool by writing a scope ref.
//
// Requiring exactly one is what makes undeclared sharing unrepresentable. There
// is no shape of PoolPer that provisions pools without saying who they are for,
// so the two projects of milo-os/ipam#114 cannot end up in one range because
// nobody thought about it.
const (
	// ReservedRoleProject folds the consuming project into pool identity: one
	// pool per consumer, per whatever else PoolPer names.
	ReservedRoleProject = "project"

	// ReservedRoleAllProjects declares the opposite, and is a declaration
	// rather than an axis: one pool that every consuming project draws from.
	//
	// It contributes nothing to the digest, and that is the point — it exists
	// so that sharing has to be written down. Announceable public space needs
	// it: per-consumer /24s exhaust an aggregate after 256 projects rather than
	// 256 locations.
	ReservedRoleAllProjects = "allProjects"
)

// IsReservedRole reports whether a role name is one a claim may never supply
// and UniqueWithin may never name.
func IsReservedRole(role string) bool {
	return role == ReservedRoleProject || role == ReservedRoleAllProjects
}

// ErrPoolPerUndeclared reports a class that provisions pools without saying
// whether its consumers share them.
//
// It is returned when planning a cascade rather than only when writing a class,
// because validation runs on write and a class stored before the rule existed
// was never subject to it. PoolPer is immutable, so such a class is replaced
// rather than corrected; refusing its claims is what keeps the guarantee true
// of the pools that exist rather than only of the ones written from here on.
var ErrPoolPerUndeclared = errors.New("class does not declare whether its pools are per-consumer or shared")

// RequirePoolPerDeclaration reports whether a class's PoolPer names exactly one
// of the reserved roles, and returns the reason it does not.
//
// Empty PoolPer is not a declaration and not an exemption: it means the class
// provisions no pools at all. Callers that provision decide what to do with
// that case; validation of a stored class rejects it, since a class named as a
// parent does provision.
func RequirePoolPerDeclaration(poolPer []string) error {
	var perConsumer, shared bool
	for _, r := range poolPer {
		switch r {
		case ReservedRoleProject:
			perConsumer = true
		case ReservedRoleAllProjects:
			shared = true
		}
	}
	switch {
	case perConsumer && shared:
		return fmt.Errorf("names both %q and %q, which contradict each other",
			ReservedRoleProject, ReservedRoleAllProjects)
	case !perConsumer && !shared:
		return fmt.Errorf("names neither %q (each consuming project gets its own pool) nor %q (one pool every consuming project draws from)",
			ReservedRoleProject, ReservedRoleAllProjects)
	}
	return nil
}

// PoolTenancy is the pair of projects a pool's identity depends on.
//
// A struct rather than two strings, deliberately: see RoleSetKey for why an
// empty tenant string is a hazard on its own, and note that two adjacent
// project-name arguments add a silent swap to it.
type PoolTenancy struct {
	// Owner holds the class DEFINITION. It keeps two classes that share a name
	// in different projects from colliding on
	// ipam_pool_identity(class_name, scope_digest): the primary key is the
	// class NAME, and class names are project-scoped objects.
	//
	// It is also the key prefix of the pool object itself, so the digest is
	// never coarser than the key space the storage layer keeps apart.
	Owner string

	// Consumer is the project whose claim triggered provisioning. It is set
	// exactly when the class names ReservedRoleProject in PoolPer, and empty
	// otherwise.
	//
	// Empty is not "unknown" and not "platform": it is a class that said
	// ReservedRoleAllProjects, which is the correct and safest shape for public
	// unicast space — one pool per location shared by every project, one
	// address space, and an exclusion constraint that keeps every consumer
	// apart.
	Consumer string
}

// PoolPerRoles splits a class's PoolPer into the scope roles projected from the
// claim body and whether the reserved project role was named.
//
// Neither reserved role reaches Project: they are not ScopeRefs and never
// arrive on a request, so looking for them in a claim's scope would fail every
// claim of the class with a missing-role error it cannot satisfy.
func PoolPerRoles(poolPer []string) (roles []string, perConsumer bool) {
	roles = make([]string, 0, len(poolPer))
	for _, r := range poolPer {
		if r == ReservedRoleProject {
			perConsumer = true
		}
		if IsReservedRole(r) {
			continue
		}
		roles = append(roles, r)
	}
	return roles, perConsumer
}

// WithoutReservedRoles drops the reserved roles from a list of role names.
//
// It is what IPClass.status.requiredScopeRoles is filtered through: that field
// tells a client what to put in spec.scope, and the reserved roles are the
// names a client must never put there.
func WithoutReservedRoles(roles []string) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if !IsReservedRole(r) {
			out = append(out, r)
		}
	}
	return out
}

// DO NOT MERGE THESE INTO ONE DIGEST FUNCTION. The two jobs need the tenant in
// different places: a pool identity must be tenant-distinct even when the scope
// is empty, and an address space must be tenant-INdistinct exactly when the
// scope is empty. A single function has to pick one, and whichever it picks is
// wrong for the other caller.

// EmptyPoolDigest is the digest of a pool scope with no roles, for a given
// tenancy. Pools and the rows they hold carry this value rather than NULL so
// the uniqueness indexes constrain them like any other.
//
// It takes a tenancy for the same reason PoolDigest does, and this is the case
// where it matters most: "no roles" is not one pool, it is one pool per owner —
// and, for a class naming the reserved project role, one per consumer as well.
func EmptyPoolDigest(t PoolTenancy) string { return PoolDigest(t, nil) }

// EmptyAddressSpaceDigest is the digest of the address space no refs separate:
// the one space every `uniqueWithin: []` claim in a pool shares, whatever
// tenant made it.
//
// It takes no tenant, so every `uniqueWithin: []` claim in a pool shares this
// space whoever made it. See AddressSpaceDigest.
func EmptyAddressSpaceDigest() string { return AddressSpaceDigest("", nil) }

// CanonicalPool returns the byte-exact serialization PoolDigest is taken over.
//
// The tenancy is the pair of projects a pool's identity depends on: the OWNER
// holding the class definition, and the CONSUMER whose claim triggered
// provisioning. The consumer is set exactly when the class names
// ReservedRoleProject in PoolPer; a class that does not name it derives one
// pool for every consumer, which is what public unicast space requires.
//
// The owner must be the same discriminator that prefixes object keys
// (tenant.Identity.Name). If key prefixes ever distinguish more than the name,
// this must too, or two spaces the storage layer keeps apart will share a
// digest.
//
// The encoding is a flat sequence of length-prefixed fields, `<len>:<bytes>`,
// where len is the byte length in decimal:
//
//	13:ipam.scope.v4 13:project-alpha 12:project-tenx 1:2 7:network 24:networking.datumapis.com 7:Network 7:default …
//
// (spaces added for readability; there are none in the real encoding).
//
// Length prefixing is what makes the digest unforgeable. A delimited encoding
// lets a role or a name containing the delimiter impersonate another field —
// role "a" with name "b|c" and role "a|b" with name "c" serialize identically.
// Here every field is preceded by its own length, so the parse is unambiguous
// before any byte of content is read, and there is no character an attacker can
// place in a name to shift a field boundary. The digits of the prefix are not
// themselves ambiguous because ':' terminates them and cannot occur inside a
// decimal length.
//
// The role count is emitted before the roles so that a scope is not a prefix of
// a longer one, and each role's four fields — role, apiGroup, kind, name — are
// emitted as a fixed-arity group so a role name cannot be mistaken for an
// APIGroup.
//
// THE CONSUMER IS A TOP-LEVEL FIELD, NOT A ROLE GROUP, and that is not a
// layout preference. A role group carries a client-supplied apiGroup and kind;
// the consumer is a server-supplied fact read off the request tenant, and it
// must not be encoded in a shape that has fields a client could vary. The
// reserved role in PoolPer is the DECLARATION; this field is the mechanism.
//
// Exported because a digest is opaque: when two claims land in different pools
// and someone needs to know why, the canonical forms are the answer and the
// digests are not.
func CanonicalPool(t PoolTenancy, s map[string]ipam.ScopeRef) string {
	var b strings.Builder
	writeField(&b, canonicalPoolVersion)
	// Owner and consumer precede the role count and are length-prefixed like
	// every other field, so neither can be made to parse as a role and the
	// empty consumer (a class that does not carve per consumer) is a
	// zero-length field rather than an absence.
	writeField(&b, t.Owner)
	writeField(&b, t.Consumer)
	writeField(&b, strconv.Itoa(len(s)))
	for _, role := range Roles(s) {
		ref := s[role]
		// Four fields, fixed arity. The v3 address-space group is five; neither
		// can be parsed as the other even before the version tag is read.
		writeField(&b, role)
		writeField(&b, ref.APIGroup)
		writeField(&b, ref.Kind)
		writeField(&b, ref.Name)
	}
	return b.String()
}

// CanonicalAddressSpace returns the byte-exact serialization
// AddressSpaceDigest is taken over.
//
// It differs from CanonicalPool in exactly one way, and the difference is the
// point: there is no tenant field, and the tenant is instead emitted inside
// every ref's group, immediately after the role. A scope with no refs therefore
// mentions no tenant at all and every tenant derives one digest; a scope with
// refs derives one digest per tenant, because each ref names an object that
// belongs to one.
//
// The group is a fixed arity of five — role, tenant, apiGroup, kind, name —
// against v2's four, so nothing in the v2 layout can be reproduced by a v3
// encoding even before the version tag is considered. Every unforgeability
// property of CanonicalPool carries over: the tenant is one more
// length-prefixed field in a fixed-arity group, so it can no more impersonate a
// role than an APIGroup can.
//
// Exported for the same reason as CanonicalPool: when two claims land in
// different address spaces, this is the explanation and the digest is not.
func CanonicalAddressSpace(tenant string, s map[string]ipam.ScopeRef) string {
	var b strings.Builder
	writeField(&b, canonicalAddressSpaceVersion)
	writeField(&b, strconv.Itoa(len(s)))
	for _, role := range Roles(s) {
		ref := s[role]
		// Six fields, fixed arity. The tenant sits inside the group because it
		// qualifies THIS ref; a scope with no refs therefore mentions no tenant
		// anywhere, which is what makes the empty space shared.
		writeField(&b, role)
		writeField(&b, tenant)
		writeField(&b, ref.APIGroup)
		writeField(&b, ref.Kind)
		writeField(&b, ref.Name)
	}
	return b.String()
}

func writeField(b *strings.Builder, v string) {
	b.WriteString(strconv.Itoa(len(v)))
	b.WriteByte(':')
	b.WriteString(v)
}

// PoolDigest reduces a tenancy and a scope to the value a POOL's identity is
// keyed on: the lowercase hex SHA-256 of its canonical form, 64 characters wide
// whatever the scope holds.
//
// THE OWNER IS FOLDED IN UNCONDITIONALLY, as its own field, and that is not
// negotiable. A pool is an object in a tenant-prefixed key space: if two owners
// derived one pool identity, the second would be handed the first's pool_key —
// a key in another project's space — and would allocate through it, bypassing
// the prefixing every other path applies. It is also what keeps two classes
// that share a NAME in different projects off one another's identity row, since
// ipam_pool_identity's primary key is (class_name, scope_digest).
//
// A provisioning class may legitimately declare no poolPer, which projects to
// the empty scope. The owner field is then the only thing keeping two owners'
// pools apart. Nothing in the IPClass registry requires a parent class to
// declare poolPer, so this shape is live rather than hypothetical — see
// TestPoolDigestSeparatesTenantsWithNoScope.
//
// THE CONSUMER IS FOLDED IN ONLY WHEN THE CLASS ASKED FOR IT, by naming
// ReservedRoleProject in PoolPer. Both directions are load-bearing:
//
//   - Set, two consumers claiming into identically-named scopes — a network
//     each project calls `default` — reach two pools rather than one, which is
//     what per-tenant address space means.
//   - Empty, every consumer in a location reaches one pool, which is what an
//     announceable public IPv4 block requires: per-consumer /24s would exhaust
//     the aggregate after 256 projects rather than 256 locations.
//
// This is the digest for ipam_pool_identity, for the digest suffix in a
// provisioned pool's name, for IPPool.status.scopeDigest, and for the PoolCarve
// row a child pool leaves against its parent.
//
// It is NOT the digest an allocation's uniqueness is enforced on. See
// AddressSpaceDigest.
func PoolDigest(t PoolTenancy, s map[string]ipam.ScopeRef) string {
	sum := sha256.Sum256([]byte(CanonicalPool(t, s)))
	return hex.EncodeToString(sum[:])
}

// AddressSpaceDigest reduces a claim's projected scope to the value uniqueness
// among ALLOCATIONS is enforced on — the space within which no two allocations
// may hold the same block.
//
// THE TENANT QUALIFIES EACH REF rather than the scope as a whole, and no tenant
// field of its own is emitted. The class is the authority on what separates two
// allocations:
//
//   - `uniqueWithin: []` says nothing separates them. Every tenant derives one
//     digest, the pool is one address space, and no two claims may hold the
//     same block. A public-unicast class is spelled this way.
//   - `uniqueWithin: [network]` says networks separate them. A network named
//     `default` in project A is a different NETWORK from `default` in project
//     B, so the qualified refs differ and the two projects are two spaces that
//     may each hold the same address. This is what shared tenant IPv4 requires.
//
// An unconditional tenant field cannot express the first case: it would make
// `uniqueWithin: []` mean "one space per tenant", which is not the strictest
// setting but very nearly the loosest.
//
// The qualifier is the CLAIMING tenant, because a ScopeRef carries no project
// of its own and a claim's refs name objects in the claimant's own project. If
// ScopeRef ever gains a project field, the qualifier becomes that field.
//
// It is NOT the digest a pool's identity is keyed on. See PoolDigest.
func AddressSpaceDigest(tenant string, s map[string]ipam.ScopeRef) string {
	sum := sha256.Sum256([]byte(CanonicalAddressSpace(tenant, s)))
	return hex.EncodeToString(sum[:])
}

// SameRefs reports whether two scopes carry the same references.
//
// It is NOT a space comparison, and the name says so because the name it used
// to have — Equal — did not. Two tenants' scopes can be ref-identical and be
// different pools, and (with refs) different address spaces. Use this only
// where the question really is "are these the same references", such as
// checking a conversion round trip.
//
// It goes through the pool form with an empty tenant because that form emits
// the tenant exactly once, so passing "" leaves the refs and nothing else. The choice of form is not observable — the result
// is never stored, only compared against another call of this same function.
func SameRefs(a, b map[string]ipam.ScopeRef) bool {
	return CanonicalPool(PoolTenancy{}, a) == CanonicalPool(PoolTenancy{}, b)
}

// RoleSetKey reduces a list of role names to a comparable value, ignoring order
// and duplicates.
//
// It exists so that "do these two classes declare the same uniqueWithin roles"
// can be asked without going through Canonical. That question is about a set of
// role *names* and has no tenant and no references, so answering it with a
// scope digest meant passing placeholder values for both — and a call that
// passes an empty tenant to mean "not applicable" is one refactor away from a
// call that passes an empty tenant to mean "platform".
func RoleSetKey(roles []string) string {
	uniq := make([]string, 0, len(roles))
	for _, r := range roles {
		if !slices.Contains(uniq, r) {
			uniq = append(uniq, r)
		}
	}
	sort.Strings(uniq)
	var b strings.Builder
	writeField(&b, strconv.Itoa(len(uniq)))
	for _, r := range uniq {
		writeField(&b, r)
	}
	return b.String()
}

// Roles returns the role names of a scope in sorted order. Callers that need to
// iterate a scope deterministically — building an error message, writing an
// index row — should go through this rather than ranging the map.
//
// The canonical encoders go through it so that Go's randomized map iteration
// order cannot produce two digests for one scope.
func Roles(s map[string]ipam.ScopeRef) []string {
	roles := make([]string, 0, len(s))
	for role := range s {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// MissingRoleError reports that a scope did not carry roles something required
// of it — a class's PoolPer or UniqueWithin entries.
//
// It names them because the allocator rejects a claim missing a required role
// rather than falling back to a wider comparison, and a
// wider comparison would look correct while refusing addresses the narrow one
// was meant to allow. That failure surfaces as unexplained exhaustion. This one
// surfaces as "you forgot `location`".
//
// All missing roles are reported at once. A claim short two roles should be one
// bad request naming both, not two round trips.
type MissingRoleError struct {
	// Roles are the scope roles that were required and absent, in the order the
	// caller asked for them.
	Roles []string
	// Required, when set, names the field that required them — "poolPer" or
	// "uniqueWithin" — so the message points at the class field to look at.
	Required string
}

func (e *MissingRoleError) Error() string {
	quoted := make([]string, len(e.Roles))
	for i, r := range e.Roles {
		quoted[i] = strconv.Quote(r)
	}
	noun, list := "role", strings.Join(quoted, ", ")
	if len(e.Roles) != 1 {
		noun = "roles"
	}
	if e.Required != "" {
		return fmt.Sprintf("scope is missing %s %s required by %s", noun, list, e.Required)
	}
	return fmt.Sprintf("scope is missing required %s %s", noun, list)
}

// Project narrows a scope to the named roles — a claim's scope onto a class's
// PoolPer to find the pool it belongs in, or onto its UniqueWithin to find the
// space its allocation must be unique in.
//
// Every named role must be supplied. Any that are not produce a single
// *MissingRoleError naming all of them, never a silently widened scope.
//
// A role present but carrying a ref with an empty Name counts as *not
// supplied*. `network: {}` is a claim into a nameless space, which is not a
// narrower space than `network: {name: default}` — it is a missing field
// wearing the shape of a present one, and it would otherwise digest to a real
// address space that nothing else could ever land in. Empty Kind is not treated
// this way: that is a malformed ref rather than an absent one, and Validate
// reports it.
//
// Duplicate role names are tolerated and collapse; the result is a fresh map
// the caller owns.
func Project(s map[string]ipam.ScopeRef, roles []string) (map[string]ipam.ScopeRef, error) {
	out := make(map[string]ipam.ScopeRef, len(roles))
	var missing []string
	for _, role := range roles {
		ref, ok := s[role]
		if !ok || ref.Name == "" {
			if !slices.Contains(missing, role) {
				missing = append(missing, role)
			}
			continue
		}
		out[role] = ref
	}
	if len(missing) > 0 {
		return nil, &MissingRoleError{Roles: missing}
	}
	return out, nil
}

// ProjectFor is Project with the name of the class field that asked, so the
// error can say which one. Use "poolPer" or "uniqueWithin".
//
// The reserved project role is dropped before projecting. It names the
// consuming project, which arrives on the request rather than in the request
// body, so a claim can never supply it — and looking for it would fail every
// claim of a per-consumer class with a missing-role error nothing could fix.
// PoolPerRoles is the caller-side half of the same rule; this is the backstop,
// so no path can reintroduce the lookup by passing PoolPer through unfiltered.
func ProjectFor(s map[string]ipam.ScopeRef, roles []string, required string) (map[string]ipam.ScopeRef, error) {
	out, err := Project(s, WithoutReservedRoles(roles))
	if err != nil {
		var missing *MissingRoleError
		if errors.As(err, &missing) {
			missing.Required = required
		}
		return nil, err
	}
	return out, nil
}

// ProjectPoolDigest projects a scope onto the named roles and takes the pool
// digest of the result — the steps every caller of Project for a pool's
// identity takes together. Use "poolPer" as required.
func ProjectPoolDigest(t PoolTenancy, s map[string]ipam.ScopeRef, roles []string, required string) (string, error) {
	sub, err := ProjectFor(s, roles, required)
	if err != nil {
		return "", err
	}
	return PoolDigest(t, sub), nil
}

// ProjectAddressSpaceDigest projects a claim's scope onto a class's
// uniqueWithin and digests the result: the address space the resulting
// allocation must be unique in. Use "uniqueWithin" as required.
//
// This is the claim path's entry point, and the one whose result is written to
// ipam_cidr_allocations.scope_digest on a Claim row.
//
// It returns the projection as well as the digest because the allocation
// records both, and a caller that projected twice could record a scope its
// digest was not taken over.
func ProjectAddressSpaceDigest(tenant string, s map[string]ipam.ScopeRef, roles []string, required string) (map[string]ipam.ScopeRef, string, error) {
	sub, err := ProjectFor(s, roles, required)
	if err != nil {
		return nil, "", err
	}
	return sub, AddressSpaceDigest(tenant, sub), nil
}

// Validate reports the first structural problem with a scope. It is not called
// by Digest — a digest is defined for any map, including a malformed one — so
// registries should call it during validation, where the error reaches a user.
func Validate(s map[string]ipam.ScopeRef) error {
	for _, role := range Roles(s) {
		ref := s[role]
		switch {
		case role == "":
			return fmt.Errorf("scope has an entry with an empty role name")
		case ref.Kind == "":
			return fmt.Errorf("scope role %q has no kind", role)
		case ref.Name == "":
			return fmt.Errorf("scope role %q has no name", role)
		}
	}
	return nil
}
