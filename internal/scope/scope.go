// Package scope canonicalizes the scope maps that claims, pools, and
// allocations carry, and reduces one to a fixed-width digest.
//
// A scope is a set of opaque references keyed by role — the network a claim is
// made for, the location it lives in, or anything else a class names. The
// allocator never interprets them; it compares them. That comparison happens in
// the database, over a digest, because a map cannot be a unique index key.
//
// Three properties make a digest safe to build an index on:
//
//   - Deterministic. Roles are sorted, so Go's randomized map iteration order
//     cannot produce two digests for one scope.
//   - Unforgeable. Every field is length-prefixed, so no delimiter appears in
//     the encoding at all and no value — however chosen — can be made to parse
//     as a different scope. See CanonicalPool.
//   - Total. Every scope has a digest, including the empty one. An empty scope
//     is a real space, not a missing value, and a unique index over NULL would
//     not constrain it.
//
// # There are two digests here, and using the wrong one is silent
//
// They answer different questions and the tenant enters them differently.
// Getting it backwards produces two holders of one address in one direction and
// a renumbered subnet in the other, both on the success path.
//
//	PoolDigest         identity of a POOL a tenant owns
//	AddressSpaceDigest the uniqueness domain an ALLOCATION lives in
//
// PoolDigest folds the tenant in unconditionally, as its own field. It
// names an object, and that object's storage key is tenant-prefixed: if two
// tenants derived one pool identity, the second would be handed the first's
// pool_key — a key in another project's key space — and would allocate through
// it, bypassing the tenant prefixing every other path applies. Nothing about a
// class's fields can be allowed to make that collision possible, so the tenant
// is not negotiable here. Note in particular that a provisioning class may
// legitimately declare no poolPer at all, which projects to the empty scope:
// the tenant field is then the ONLY thing keeping two tenants' pools apart.
// Nothing in the IPClass registry requires a parent class to declare poolPer,
// so this is a live shape and not a hypothetical one — see
// TestPoolDigestSeparatesTenantsWithNoScope.
//
// AddressSpaceDigest qualifies each REF with the tenant instead, and emits
// no tenant field of its own. An address space is defined by what the class
// says separates two allocations, and the class is the authority:
//
//   - `uniqueWithin: []` says nothing separates them. Every tenant then derives
//     one digest, the pool is one address space, and no two claims may hold the
//     same block — which is what the strictest setting has to mean. A
//     public-unicast class is spelled this way.
//   - `uniqueWithin: [network]` says networks separate them. A network named
//     `default` in project A is a different NETWORK from `default` in project
//     B — that is a property of the ref, not of the space — so the qualified
//     refs differ and the two projects are two spaces that may each hold the
//     same address. This is what shared tenant IPv4 requires.
//
// Both properties hold at once because the tenant qualifies the thing it
// actually belongs to. An unconditional tenant field cannot express the first
// case: it makes `uniqueWithin: []` mean "one space per tenant", which is not
// the strictest setting but very nearly the loosest.
//
// Both simpler designs are wrong, in opposite directions, and both fail on the
// success path:
//
//   - No tenant anywhere: two projects claiming one class for their own
//     `default` network derive one digest, collide on ipam_pool_identity's
//     primary key, and are refused the addresses uniqueWithin exists to permit.
//   - An unconditional tenant field: a class with `uniqueWithin: []` over one
//     platform pool hands the same address to two projects. Both Bound, one
//     pool, nothing logged.
//
// # Why the ref qualifier is the claiming tenant
//
// A ScopeRef carries no project of its own, so the tenant qualifying it is
// the tenant of the caller whose scope it is. That is correct for what a claim
// can currently express: a claim's refs name objects in the claimant's own
// project. If ScopeRef ever gains a project field — which cross-project
// consumption would want — the qualifier becomes that field, and this comment
// is where to start.
//
// # Two encodings, two version tags, on purpose
//
// The two forms carry different version tags, v2 and v3, so a digest of one can
// never be read as a digest of the other.
//
// PoolDigest's tag must not change. A cascade pool's name embeds its digest, so
// re-tagging renames every provisioned pool: the identity lookup misses, a new
// pool is provisioned, and the scope is renumbered — against a model that
// promises subnets appear on first use and are never renumbered.
//
// The two values meet in exactly one column, ipam_cidr_allocations.scope_digest,
// which holds an address-space digest on Claim rows and a pool digest on
// PoolCarve rows. They are never compared: the search asks
// `purpose <> 'Claim' OR scope_digest = $1`, so a non-Claim row is matched by
// purpose and its digest is never read.
//
// # Why the tenant is a string
//
// So this package keeps depending on nothing but the API types — it is imported
// by the allocator and by three registries, and none of them should acquire a
// transitive k8s.io/apiserver dependency through it. The value must be the same
// discriminator that prefixes object keys (tenant.Identity.Name, empty for
// platform callers); if key prefixes ever start distinguishing more than the
// name, this must too, or two spaces the storage layer keeps apart will share a
// digest again.
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
	// canonicalPoolVersion must not change: a cascade pool's NAME embeds its
	// digest, so re-tagging renames every provisioned pool and renumbers every
	// scope. See the package comment.
	canonicalPoolVersion = "ipam.scope.v2"

	// canonicalAddressSpaceVersion tags the form in which the tenant qualifies
	// each ref rather than standing as its own field.
	canonicalAddressSpaceVersion = "ipam.scope.v3"
)

// DO NOT MERGE THESE INTO ONE DIGEST FUNCTION. The two jobs need the tenant in
// different places: a pool identity must be tenant-distinct even when the scope
// is empty, and an address space must be tenant-INdistinct exactly when the
// scope is empty. A single function has to pick one, and whichever it picks is
// wrong for the other caller.

// EmptyPoolDigest is the digest of a pool scope with no roles, for a given
// tenant. Pools and the rows they hold carry this value rather than NULL so the
// uniqueness indexes constrain them like any other.
//
// It takes a tenant for the same reason PoolDigest does, and this is the case
// where it matters most: "no roles" is not one pool, it is one pool per tenant.
// A platform caller passes "".
func EmptyPoolDigest(tenant string) string { return PoolDigest(tenant, nil) }

// EmptyAddressSpaceDigest is the digest of the address space no refs separate:
// the one space every `uniqueWithin: []` claim in a pool shares, whatever
// tenant made it.
//
// It takes no tenant. That absence is deliberate: see the package comment.
func EmptyAddressSpaceDigest() string { return AddressSpaceDigest("", nil) }

// CanonicalPool returns the byte-exact serialization PoolDigest is taken over.
//
// tenant is the project or organization the pool belongs to — the same
// discriminator that prefixes storage keys — and is empty for platform callers,
// so every platform caller with the same scope still shares one digest.
//
// The encoding is a flat sequence of length-prefixed fields, `<len>:<bytes>`,
// where len is the byte length in decimal:
//
//	13:ipam.scope.v2 13:project-alpha 1:2 7:network 24:networking.datumapis.com 7:Network 7:default …
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
// Exported because a digest is opaque: when two claims land in different pools
// and someone needs to know why, the canonical forms are the answer and the
// digests are not.
func CanonicalPool(tenant string, s map[string]ipam.ScopeRef) string {
	var b strings.Builder
	writeField(&b, canonicalPoolVersion)
	// The tenant precedes the role count and is length-prefixed like every
	// other field, so a tenant name cannot be made to parse as a role and the
	// empty (platform) tenant is a zero-length field rather than an absence.
	writeField(&b, tenant)
	writeField(&b, strconv.Itoa(len(s)))
	for _, role := range Roles(s) {
		ref := s[role]
		// Five fields, fixed arity. The v3 form's group is six; neither can be
		// parsed as the other even before the version tag is read.
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

// PoolDigest reduces a tenant's scope to the value a POOL's identity is keyed
// on: the lowercase hex SHA-256 of its canonical form, 64 characters wide
// whatever the scope holds.
//
// The tenant is folded in unconditionally. This is the digest for
// ipam_pool_identity, for the digest suffix in a provisioned pool's name, for
// IPPool.status.scopeDigest, and for the PoolCarve row a child pool leaves
// against its parent — everything whose subject is a pool object living in one
// tenant's key space.
//
// It is NOT the digest an allocation's uniqueness is enforced on. See
// AddressSpaceDigest, and the package comment for what goes wrong if these are
// swapped.
func PoolDigest(tenant string, s map[string]ipam.ScopeRef) string {
	sum := sha256.Sum256([]byte(CanonicalPool(tenant, s)))
	return hex.EncodeToString(sum[:])
}

// AddressSpaceDigest reduces a claim's projected scope to the value uniqueness
// among ALLOCATIONS is enforced on — the space within which no two allocations
// may hold the same block.
//
// The tenant qualifies each ref rather than the scope as a whole, so an empty
// scope is one space across all tenants and a scope with refs is one space per
// tenant. That is what makes `uniqueWithin: []` the strictest setting and
// `uniqueWithin: [network]` the shared-IPv4 one, at the same time.
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
	return CanonicalPool("", a) == CanonicalPool("", b)
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
func ProjectFor(s map[string]ipam.ScopeRef, roles []string, required string) (map[string]ipam.ScopeRef, error) {
	out, err := Project(s, roles)
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
func ProjectPoolDigest(tenant string, s map[string]ipam.ScopeRef, roles []string, required string) (string, error) {
	sub, err := ProjectFor(s, roles, required)
	if err != nil {
		return "", err
	}
	return PoolDigest(tenant, sub), nil
}

// ProjectAddressSpaceDigest projects a claim's scope onto a class's
// uniqueWithin and digests the result: the address space the resulting
// allocation must be unique in. Use "uniqueWithin" as required.
//
// This is the claim path's entry point, and the one whose result is written to
// ipam_cidr_allocations.scope_digest on a Claim row.
func ProjectAddressSpaceDigest(tenant string, s map[string]ipam.ScopeRef, roles []string, required string) (string, error) {
	sub, err := ProjectFor(s, roles, required)
	if err != nil {
		return "", err
	}
	return AddressSpaceDigest(tenant, sub), nil
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
