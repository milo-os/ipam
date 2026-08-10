// Package tenant extracts the calling tenant identity from a request context.
//
// Milo's IAM front gate forwards the parent organization or project identity
// as authentication extras (UserInfo.Extra). This package centralises the
// extra-key constants and provides a small Identity struct used across the
// storage and allocator layers to scope reads, writes, and capacity tracking
// to a single project.
package tenant

import (
	"context"
	"strings"

	"k8s.io/apiserver/pkg/endpoints/request"
)

// Extra keys forwarded by Milo's front gate. Values are emitted as
// UserInfo.Extra entries on the impersonated request that reaches this
// aggregated apiserver.
const (
	ExtraParentAPIGroup = "iam.miloapis.com/parent-api-group"
	ExtraParentType     = "iam.miloapis.com/parent-type"
	ExtraParentName     = "iam.miloapis.com/parent-name"
)

// Identity captures the tenant scope of an incoming request. An empty Name
// means the request is platform-scoped (no project), in which case storage
// keys are not prefixed and the caller sees the global view.
type Identity struct {
	APIGroup string
	Kind     string
	Name     string
}

// IsPlatform reports whether the request carries no tenant scope.
func (id Identity) IsPlatform() bool { return id.Name == "" }

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

// KeyPrefix returns "project/<name>/" for project-scoped requests and "" for
// platform requests. Storage layers prepend this to object keys so per-project
// reads and writes never overlap.
func (id Identity) KeyPrefix() string {
	if id.Name == "" {
		return ""
	}
	return "project/" + id.Name + "/"
}

// ApplyPrefix applies the tenant prefix to the given key, stripping the key's
// leading "/" when a non-empty prefix is present to avoid a double slash.
// Platform keys keep their leading slash; project keys are "project/<id>/ipam.miloapis.com/...".
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

// FromContext extracts the tenant identity from the user info attached to
// the context. Returns the zero-value Identity (platform scope) when no
// user info is present or no parent extras are set.
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
