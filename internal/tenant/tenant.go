// Package tenant extracts the calling tenant identity from a request context.
//
// Milo's IAM front gate forwards the parent organization or project identity
// as authentication extras (UserInfo.Extra). This package centralises the
// extra-key constants and provides a small Identity struct used across the
// storage and allocator layers to scope reads, writes, and capacity tracking
// to a single project.
//
// # There is no unprefixed keyspace, and no global view
//
// Every object this service stores belongs to a project. The platform's own
// address space is not an exception carved out of that rule — it is a project
// like any other, named by --platform-project, and its objects live under the
// same "project/<name>/" prefix as a tenant's. Platform pools and the class
// catalog live there; tenants consume out of it the same way they would consume
// out of each other's projects.
//
// That is a change from what this package used to say. An empty Name used to
// mean "platform scope", keys were left unprefixed, and IsPlatform() meant "no
// tenant extras". Two consequences made that untenable:
//
//   - The platform's own tooling authenticates as a project, like everything
//     else. Under the old rule it was not "the platform", so it lost the
//     bypasses in access.AuthorizeClassConsumption — while an unscoped caller,
//     which cmd/ipam/admission.go already refuses to serve for claim CREATE,
//     kept them. The boundary was inverted.
//   - Test populations differed from production. Every Chainsaw suite authored
//     its catalog through a kubeconfig with no tenant extras, so dev exercised a
//     keyspace production would never have.
//
// An empty Name now means only what it says: the request carries no tenant. It
// is not the platform, it is not privileged, and its keys are unprefixed only in
// the sense that there is nothing there to read.
package tenant

import (
	"context"
	"errors"
	"strings"

	"k8s.io/apiserver/pkg/endpoints/request"
)

// ErrNoPlatformProject reports that a platform-owned lookup was attempted on a
// server with no --platform-project configured.
//
// It is an error rather than a fallback to the old unprefixed keyspace, and
// that is the whole point of it existing: a silent fallback is how this change
// ships looking correct and breaks the first time it meets a real deployment.
// The class catalog would be read from a keyspace nothing writes to, every
// claim would fail with "class not found", and nothing would say why.
var ErrNoPlatformProject = errors.New("ipam: no platform project configured (--platform-project)")

// ErrNoTenant reports that a write was attempted by a caller carrying no
// project.
//
// Every object this service stores belongs to a project, including the
// platform's own — see the package comment. A caller with no project therefore
// has nowhere legitimate to write. Before this error existed such a write
// succeeded: it landed at an unprefixed key, in a keyspace no read path
// consults, and reported 201 Created for an object nothing would ever find.
//
// Rejecting is the fail-loud direction. The alternative is not "harmless" —
// it is a caller that believes it created something, an operator who cannot
// find it, and a table that accumulates rows migration 007 will later refuse
// to migrate. That residue is also indistinguishable at a glance from a broken
// tenancy cutover, which is a diagnosis this repo has already paid for once.
var ErrNoTenant = errors.New("ipam: request carries no project scope")

// platformProjectKey is the context key the configured platform project is
// carried under. It is a private zero-width type so nothing outside this
// package can write the value under a colliding key.
type platformProjectKey struct{}

// WithPlatformProject returns a context carrying the configured platform
// project.
//
// The value is server configuration, not request data. It reaches the request
// context through a handler-chain filter installed at startup (see
// cmd/ipam/serve.go), for the same reason the quota consumer context does: the
// functions that need it — tenant.FromContext, allocator.LoadClass,
// allocator.DiscoverPool — already take a ctx and are called from three
// packages and a background sweeper, so threading it through every signature
// would mean teaching every construction site of a value type about a flag.
//
// Nothing a caller sends can set it. The only client-controlled input in this
// package is UserInfo.Extra, and a caller naming the platform project in its
// extras is still only the platform if the server was configured to agree —
// see TestPlatformProjectIsNotClientSupplied.
func WithPlatformProject(ctx context.Context, project string) context.Context {
	if project == "" {
		return ctx
	}
	return context.WithValue(ctx, platformProjectKey{}, project)
}

// PlatformProjectFromContext returns the configured platform project, and
// whether one is configured at all. An unconfigured server reports false rather
// than an empty string that a caller might use as a key prefix.
func PlatformProjectFromContext(ctx context.Context) (string, bool) {
	project, ok := ctx.Value(platformProjectKey{}).(string)
	return project, ok && project != ""
}

// PlatformIdentity returns the Identity of the platform project itself, for
// code that must read or write platform-owned objects regardless of who is
// calling — the class catalog, above all.
//
// It fails rather than returning a zero Identity when nothing is configured,
// because a zero Identity produces an unprefixed key and that key names a
// keyspace this service no longer writes to.
func PlatformIdentity(ctx context.Context) (Identity, error) {
	project, ok := PlatformProjectFromContext(ctx)
	if !ok {
		return Identity{}, ErrNoPlatformProject
	}
	return Identity{
		APIGroup:        ParentAPIGroupProject,
		Kind:            ParentTypeProject,
		Name:            project,
		platformProject: project,
	}, nil
}

// PlatformKeyPrefix returns the storage key prefix platform-owned objects live
// under — "project/<platform>/" — or an error when nothing is configured.
//
// Callers building a LIKE predicate over ipam_objects want this rather than
// assembling it from PlatformProjectFromContext, so there is one definition of
// where platform objects live.
func PlatformKeyPrefix(ctx context.Context) (string, error) {
	id, err := PlatformIdentity(ctx)
	if err != nil {
		return "", err
	}
	return id.KeyPrefix(), nil
}

// The parent pair Milo's resourcemanager uses for a Project. Named here because
// PlatformIdentity has to synthesise an identity nothing forwarded, and it must
// synthesise the same pair a real project-scoped request arrives with.
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

	// platformProject is the server's configured platform project, copied from
	// the context by FromContext so IsPlatform has an answer without a package
	// global.
	//
	// Unexported deliberately. Identity is constructed as a literal in several
	// places purely for its key helpers (poolStorageKey, the cascade's pool
	// key), and none of those literals should be able to mint a value that
	// answers "yes, I am the platform". A literal therefore always fails
	// closed — see TestZeroIdentityIsNotPlatform.
	platformProject string
}

// IsPlatform reports whether the caller is the configured platform project.
//
// It is NOT "carries no tenant scope", which is what it used to mean. The
// change is security-relevant in both directions and the package comment says
// why; access.AuthorizeClassConsumption is the consumer that makes it so.
//
// An unconfigured server has no platform project, so nobody is the platform and
// every caller takes the tenant path. That is the fail-closed direction: the
// alternative — treating "unconfigured" as "everyone is trusted" — would hand
// the class-consumption bypass to every caller on a server whose operator
// simply had not set a flag yet.
func (id Identity) IsPlatform() bool {
	return id.platformProject != "" && id.Name == id.platformProject
}

// PlatformProject returns the server's configured platform project as seen by
// this Identity, and whether one is configured at all.
//
// It exists so an error message can name where platform-owned objects belong
// without re-reading the context the Identity already came from. Callers that
// only need a yes/no should use IsPlatform.
func (id Identity) PlatformProject() (string, bool) {
	return id.platformProject, id.platformProject != ""
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
// context, stamping on the server's configured platform project so the returned
// Identity can answer IsPlatform without consulting a global.
//
// Returns an Identity with an empty Name when no user info is present or no
// parent extras are set. That value is a caller with no tenant — not the
// platform, and not privileged.
func FromContext(ctx context.Context) Identity {
	platformProject, _ := PlatformProjectFromContext(ctx)
	user, ok := request.UserFrom(ctx)
	if !ok {
		return Identity{platformProject: platformProject}
	}
	extra := user.GetExtra()
	return Identity{
		APIGroup:        first(extra[ExtraParentAPIGroup]),
		Kind:            first(extra[ExtraParentType]),
		Name:            first(extra[ExtraParentName]),
		platformProject: platformProject,
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
