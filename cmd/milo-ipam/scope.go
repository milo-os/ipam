package main

import (
	"fmt"
	"sort"
	"strings"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// Scope is the one genuinely new thing a user has to type, so its surface gets
// more care than the rest of the flag set.
//
// A scope entry is a role name mapped to an opaque {apiGroup, kind, name}
// reference. Making the user type the apiGroup and kind for every entry would
// be intolerable for the two roles that account for nearly every claim, so the
// short form resolves them from the table below:
//
//	--scope network=default
//	--scope location=us-central-1
//
// Roles outside the table — a class may name any role at all, since the
// allocator never interprets them — take the qualified form, which reads like
// kubectl's `resource.group` plus the familiar `kind/name`:
//
//	--scope site=Site.infra.example.com/dc-1
//
// The qualified form is also accepted for known roles, and overrides the table.
//
// A `#<uid>` suffix pins the reference to one instance of the named object.
// See scopeSetsUID for why the CLI leaves it empty unless asked.
var wellKnownScopeRoles = map[string]struct{ apiGroup, kind string }{
	"network":  {"networking.datumapis.com", "Network"},
	"location": {"networking.datumapis.com", "Location"},
	"project":  {"resourcemanager.miloapis.com", "Project"},
}

// wellKnownRoleNames lists the roles the short form resolves, for help text and
// error messages.
func wellKnownRoleNames() []string {
	names := make([]string, 0, len(wellKnownScopeRoles))
	for r := range wellKnownScopeRoles {
		names = append(names, r)
	}
	sort.Strings(names)
	return names
}

// scopeFlagUsage is shared by every command that takes --scope. It stays to one
// line — cobra does not wrap flag usage, and a multi-line string here shreds the
// alignment of the whole flag block. The grammar itself is spelled out in
// scopeGrammarHelp, which commands fold into their Long text.
func scopeFlagUsage(what string) string {
	return fmt.Sprintf("%s, as role=name (repeatable). Known roles: %s. See the description above",
		what, strings.Join(wellKnownRoleNames(), ", "))
}

// scopeGrammarHelp is the paragraph every --scope-taking command appends to its
// Long text, so the full grammar is one `--help` away from where it is needed.
func scopeGrammarHelp() string {
	return `Scope entries are role=value pairs, repeatable. For a role the CLI knows
(` + strings.Join(wellKnownRoleNames(), ", ") + `) a bare name is enough:

  --scope network=default --scope location=us-central-1

A class may name any role at all, since the allocator never interprets them.
Roles outside that list take a qualified reference, kind first:

  --scope site=Site.infra.example.com/dc-1

Append #<uid> to pin a reference to one instance of the named object, so an
object deleted and recreated under the same name is a different address space.
The CLI leaves it unset by default: it resolves no consumer objects, and a
stale UID silently splits a space in two.`
}

// parseScopeEntry parses one `role=value` argument into its role and reference.
func parseScopeEntry(arg string) (role string, ref ipamv1alpha1.ScopeRef, err error) {
	role, value, found := strings.Cut(arg, "=")
	role = strings.TrimSpace(role)
	value = strings.TrimSpace(value)
	if !found || role == "" || value == "" {
		return "", ipamv1alpha1.ScopeRef{}, usageErrorf(
			"invalid --scope %q: expected role=value, e.g. --scope network=default", arg)
	}
	ref, err = parseScopeValue(role, value)
	if err != nil {
		return "", ipamv1alpha1.ScopeRef{}, err
	}
	return role, ref, nil
}

// parseScopeValue resolves the value half of a scope entry. A bare name uses the
// well-known table for its role; the qualified form carries its own kind and
// group. Both accept a trailing #<uid>.
func parseScopeValue(role, value string) (ipamv1alpha1.ScopeRef, error) {
	value, uid := splitUID(value)
	if value == "" {
		return ipamv1alpha1.ScopeRef{}, usageErrorf("invalid --scope %s: a name is required before #<uid>", role)
	}

	qualifier, name, qualified := strings.Cut(value, "/")
	if !qualified {
		known, ok := wellKnownScopeRoles[role]
		if !ok {
			return ipamv1alpha1.ScopeRef{}, usageErrorf(
				"role %q is not one of the known roles (%s), so it needs a qualified reference:\n"+
					"       --scope %s=Kind.apiGroup/%s",
				role, strings.Join(wellKnownRoleNames(), ", "), role, value)
		}
		return ipamv1alpha1.ScopeRef{APIGroup: known.apiGroup, Kind: known.kind, Name: value, UID: uid}, nil
	}

	kind, apiGroup, hasGroup := strings.Cut(qualifier, ".")
	if kind == "" || !hasGroup || apiGroup == "" || name == "" {
		return ipamv1alpha1.ScopeRef{}, usageErrorf(
			"invalid --scope %s=%s: the qualified form is Kind.apiGroup/name, e.g. Site.infra.example.com/dc-1",
			role, value)
	}
	return ipamv1alpha1.ScopeRef{APIGroup: apiGroup, Kind: kind, Name: name, UID: uid}, nil
}

// splitUID peels a trailing #<uid> off a reference value.
func splitUID(value string) (rest, uid string) {
	rest, uid, found := strings.Cut(value, "#")
	if !found {
		return value, ""
	}
	return strings.TrimSpace(rest), strings.TrimSpace(uid)
}

// buildScope turns the repeated --scope arguments into the API's role→ref map.
// A repeated role is a usage error rather than a silent last-wins overwrite: a
// claim's scope is immutable and decides which address space it lands in, so
// quietly discarding half of a duplicated role is not a recoverable mistake.
func buildScope(args []string) (map[string]ipamv1alpha1.ScopeRef, error) {
	if len(args) == 0 {
		return nil, nil
	}
	scope := make(map[string]ipamv1alpha1.ScopeRef, len(args))
	for _, arg := range args {
		role, ref, err := parseScopeEntry(arg)
		if err != nil {
			return nil, err
		}
		if prev, dup := scope[role]; dup {
			return nil, usageErrorf("--scope %s given twice (%s and %s); a role holds one reference",
				role, prev.Name, ref.Name)
		}
		scope[role] = ref
	}
	return scope, nil
}

// parseObjectRef parses the --owner form, which is the scope grammar without a
// role: a qualified Kind.apiGroup/name, optionally namespaced as
// Kind.apiGroup/namespace/name, and optionally pinned with #<uid>.
//
// Unlike a scope UID, an owner UID takes no part in allocation identity — it
// only keeps "who holds this address" answerable after the holder has been
// deleted and recreated under the same name. It is therefore safe to record
// whenever the caller has it, and carries none of the risk that made the CLI
// leave ScopeRef.UID alone.
func parseObjectRef(value string) (*ipamv1alpha1.ObjectRef, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	value, uid := splitUID(value)
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, usageErrorf(
			"invalid --owner %q: expected Kind.apiGroup/name (or Kind.apiGroup/namespace/name)", value)
	}
	kind, apiGroup, hasGroup := strings.Cut(parts[0], ".")
	if kind == "" || !hasGroup || apiGroup == "" {
		return nil, usageErrorf(
			"invalid --owner %q: the reference must be qualified, e.g. Instance.compute.datumapis.com/hello-0", value)
	}
	ref := &ipamv1alpha1.ObjectRef{APIGroup: apiGroup, Kind: kind, UID: uid}
	if len(parts) == 3 {
		ref.Namespace, ref.Name = parts[1], parts[2]
	} else {
		ref.Name = parts[1]
	}
	if ref.Name == "" {
		return nil, usageErrorf("invalid --owner %q: a name is required", value)
	}
	return ref, nil
}

// formatScope renders a scope map as a stable, single-line `role=name` list for
// table cells. Roles are sorted so the same scope always reads the same way.
func formatScope(scope map[string]ipamv1alpha1.ScopeRef) string {
	if len(scope) == 0 {
		return "—"
	}
	roles := make([]string, 0, len(scope))
	for r := range scope {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	cells := make([]string, 0, len(roles))
	for _, r := range roles {
		cells = append(cells, r+"="+scope[r].Name)
	}
	return strings.Join(cells, " ")
}

// formatScopeRef renders one reference in the qualified form the CLI accepts, so
// a value read out of `show` can be pasted back into `--scope`.
func formatScopeRef(ref ipamv1alpha1.ScopeRef) string {
	s := fmt.Sprintf("%s.%s/%s", ref.Kind, ref.APIGroup, ref.Name)
	if ref.UID != "" {
		s += "#" + ref.UID
	}
	return s
}

// formatObjectRef renders an owner reference for a table cell — kind and name,
// no UID, because a UID does not fit a column and is not what a scanning reader
// is matching on.
func formatObjectRef(ref *ipamv1alpha1.ObjectRef) string {
	if ref == nil || ref.Name == "" {
		return "—"
	}
	name := ref.Name
	if ref.Namespace != "" {
		name = ref.Namespace + "/" + name
	}
	if ref.Kind == "" {
		return name
	}
	return ref.Kind + " " + name
}

// formatObjectRefWithUID renders an owner for the detail and reverse-lookup
// views, appending an abbreviated UID when one is recorded. "Instance hello-0"
// is ambiguous across a delete and recreate under the same name — which is
// exactly the case an operator is looking at when they ask who holds an
// address — so the identity gets said out loud where there is room for it.
func formatObjectRefWithUID(ref *ipamv1alpha1.ObjectRef) string {
	base := formatObjectRef(ref)
	if ref == nil || ref.UID == "" {
		return base
	}
	return base + " (uid " + abbreviateUID(ref.UID) + ")"
}

// abbreviateUID shortens a UID to its leading characters. It is enough to
// distinguish two instances at a glance, and the full value is in -o yaml for
// anyone who needs to match on it exactly.
func abbreviateUID(uid string) string {
	const keep = 8
	if len(uid) <= keep {
		return uid
	}
	return uid[:keep] + "…"
}

// sortedScopeRoles returns a scope map's roles in stable order, for detail views
// that print one row per role.
func sortedScopeRoles(scope map[string]ipamv1alpha1.ScopeRef) []string {
	roles := make([]string, 0, len(scope))
	for r := range scope {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	return roles
}
