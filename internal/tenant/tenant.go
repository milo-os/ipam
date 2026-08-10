// Package tenant extracts the calling tenant identity from a request context.
//
// Milo's IAM front gate forwards the parent organization or project identity
// as authentication extras (UserInfo.Extra). This package centralises the
// extra-key constants and provides a small Identity struct used across the
// storage and allocator layers to scope reads, writes, and capacity tracking
// to a single project.
//
// Every object this service stores belongs to a project, and there is no
// unprefixed keyspace and no global view. An empty Name is a caller carrying no
// tenant: it is not privileged, and it has nothing to read and nowhere to
// write.
//
// There is deliberately no notion here of a project the service treats as
// special. A class one project consumes from another is named by the reference
// that points at it, so nothing needs a server-wide answer to "which project is
// the platform" — and a server-wide answer would be an authorization input that
// no request could see.
package tenant

import (
	"context"
	"errors"
	"strings"

	"k8s.io/apiserver/pkg/endpoints/request"
)

// ErrNoTenant reports that a write was attempted by a caller carrying no
// project.
//
// Every object this service stores belongs to a project — see the package
// comment. A caller with no project has nowhere legitimate to write.
//
// Accepting the write is the harmful direction, not the lenient one. It lands
// at an unprefixed key that no read path consults and returns 201 Created, so
// the caller believes it created something an operator can never find, and the
// rows it leaves are indistinguishable at a glance from a broken tenancy
// cutover.
var ErrNoTenant = errors.New("ipam: request carries no project scope")

// The parent pair Milo's resourcemanager uses for a Project. Named here so code
// synthesising an identity nothing forwarded produces the same pair a real
// project-scoped request arrives with.
const (
	ParentAPIGroupProject = "resourcemanager.miloapis.com"
	ParentTypeProject     = "Project"
)

// Extra keys forwarded by Milo's front gate. Values are emitted as
// UserInfo.Extra entries on the impersonated request that reaches this
// aggregated apiserver.
const (
	ExtraParentAPIGroup = "iam.miloapis.com/parent-api-group"
	ExtraParentType     = "iam.miloapis.com/parent-type"
	ExtraParentName     = "iam.miloapis.com/parent-name"
)

// Identity captures the tenant scope of an incoming request.
//
// An empty Name means the request carries no tenant at all. That is not the
// platform and not a global view — it is a caller with nothing to read. See the
// package comment.
type Identity struct {
	APIGroup string
	Kind     string
	Name     string
}

// Project returns the project ID when the request is project-scoped,
// otherwise the empty string. Used as a low-cardinality metric label;
// callers should pair it with Org() on the same Identity.
func (id Identity) Project() string {
	if id.Kind == "Project" {
		return id.Name
	}
	return ""
}

// Org returns the organization ID when the request is organization-scoped,
// otherwise the empty string. Used as a low-cardinality metric label.
//
// NOTE: Milo's front-door filter currently forwards exactly one parent
// (Project OR Organization) via the iam.miloapis.com/parent-* extras. For
// project-scoped requests the org is therefore not directly known to IPAM
// today — Org() returns "" in that case. When Milo begins forwarding the
// owning organization alongside the project (planned), update FromContext
// to populate this field from the new extra and the metric label will start
// resolving without further changes at call sites.
func (id Identity) Org() string {
	if id.Kind == "Organization" {
		return id.Name
	}
	return ""
}

// KeyPrefix returns "project/<name>/" for a request carrying a tenant, and ""
// for one carrying none. Storage layers prepend this to object keys so
// per-project reads and writes never overlap.
//
// The platform project is not a special case here and must not become one: its
// prefix is "project/<platform>/" like everyone else's. The empty prefix is
// what a caller with no tenant gets, and it names a keyspace nothing writes to.
func (id Identity) KeyPrefix() string {
	if id.Name == "" {
		return ""
	}
	return "project/" + id.Name + "/"
}

// ApplyPrefix applies the tenant prefix to the given key, stripping the key's
// leading "/" when a non-empty prefix is present to avoid a double slash.
// A tenant's keys are "project/<id>/ipam.miloapis.com/..."; an untenanted
// caller's keep their leading slash and reach the keyspace nothing populates.
func (id Identity) ApplyPrefix(key string) string {
	prefix := id.KeyPrefix()
	if prefix == "" {
		return key
	}
	return prefix + strings.TrimPrefix(key, "/")
}

// ResourceKey returns the full storage key for a resource.
// resource is the plural form (e.g. "ipprefixes", "asnpools").
// The result matches the key format used in ipam_objects.
func (id Identity) ResourceKey(resource, name string) string {
	return id.ApplyPrefix("/ipam.miloapis.com/" + resource + "/" + name)
}

// FromContext extracts the tenant identity from the user info attached to the
// context.
//
// Returns an Identity with an empty Name when no user info is present or no
// parent extras are set. That value is a caller with no tenant, and it is not
// privileged.
func FromContext(ctx context.Context) Identity {
	user, ok := request.UserFrom(ctx)
	if !ok {
		return Identity{}
	}
	extra := user.GetExtra()
	return Identity{
		APIGroup: first(extra[ExtraParentAPIGroup]),
		Kind:     first(extra[ExtraParentType]),
		Name:     first(extra[ExtraParentName]),
	}
}

// RequireTenant returns the calling identity, or ErrNoTenant when the request
// carries no project.
//
// Callers on a write path use this instead of FromContext. Reads deliberately
// do NOT: an untenanted read already returns nothing, which is both correct and
// the documented answer to "why does my kubectl show zero pools" — it is the
// tenancy model working, not a failure. Only writes need refusing, because only
// a write leaves something behind.
func RequireTenant(ctx context.Context) (Identity, error) {
	id := FromContext(ctx)
	if id.Name == "" {
		return id, ErrNoTenant
	}
	return id, nil
}

func first(vals []string) string {
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// ProjectFromKey extracts the project ID from a tenant-scoped storage key.
// Returns "" if the key is not project-scoped (platform key starting with
// "/ipam.miloapis.com/...").
//
// Format reminder: project keys look like
//
//	project/<projectID>/ipam.miloapis.com/<resource>/<name>
//
// platform keys look like
//
//	/ipam.miloapis.com/<resource>/<name>
//
// Used by metrics emission paths that only have a key in hand (e.g. the
// pool-utilization gauge published from the allocator), to derive the same
// `project` label value that the registry layer derives from request
// context. There is no analogous OrgFromKey helper because the storage key
// only carries the immediate parent — see tenant.Identity.Org for why.
func ProjectFromKey(key string) string {
	const prefix = "project/"
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	rest := key[len(prefix):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return ""
	}
	return rest[:slash]
}
