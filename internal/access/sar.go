// Package access provides authorization helpers used by the allocation
// registries. The PoolAccessChecker translates an internal pool key into a
// SubjectAccessReview against the ipam.miloapis.com API group, letting Milo's
// IAM enforce who can claim from which pool without leaking implementation
// details into the registry layer.
package access

import (
	"context"
	"strings"

	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/request"
)

// PoolAccessChecker checks whether the caller can use a specific pool.
//
// poolKey is the storage key of the pool (e.g. "project/foo/ipprefix/bar" or
// "project/foo/asnpool/bar"). The checker derives the resource type and name
// from the key and runs an authorization decision against the configured
// authorizer.
type PoolAccessChecker interface {
	CanUsePool(ctx context.Context, poolKey string) (bool, error)
}

type sarChecker struct {
	authz authorizer.Authorizer
}

// NewPoolAccessChecker wraps an authorizer.Authorizer so the registry layer
// can ask "can the caller use this pool?" without reaching into UserInfo
// extraction itself.
func NewPoolAccessChecker(authz authorizer.Authorizer) PoolAccessChecker {
	return &sarChecker{authz: authz}
}

// CanUsePool runs a "use" verb authorization check against the pool resource
// implied by poolKey. Returns false (no error) if no user info is on the
// context — callers should treat that as a 401/403 boundary, not as a system
// error.
func (c *sarChecker) CanUsePool(ctx context.Context, poolKey string) (bool, error) {
	user, ok := request.UserFrom(ctx)
	if !ok {
		return false, nil
	}

	resource, name := resourceAndNameFromPoolKey(poolKey)

	attrs := authorizer.AttributesRecord{
		User:            user,
		Verb:            "use",
		APIGroup:        "ipam.miloapis.com",
		Resource:        resource,
		Name:            name,
		ResourceRequest: true,
	}
	decision, _, err := c.authz.Authorize(ctx, attrs)
	return decision == authorizer.DecisionAllow, err
}

// resourceAndNameFromPoolKey extracts the resource plural and the pool name
// from a storage key. The storage layer produces keys in two shapes:
//
//	"/ipam.miloapis.com/<plural>/<name>"                        (platform-scoped)
//	"project/<id>/ipam.miloapis.com/<plural>/<name>"            (tenant-scoped)
//
// The plural ("ippools") sits two segments before the end in both shapes;
// the name is always the last segment. The IPAM apiserver serves pools as the
// "ippools" resource (see internal/apiserver/apiserver.go and the IAM
// ProtectedResource registration), and the storage layer encodes pool keys as
// ".../ippools/<name>" (see ippool/storage.go poolStorageKey). The "use" SAR
// must therefore target "ippools" — the resource that actually exists and that
// IAM grants are written against. Unknown plurals fall back to "ippools" so the
// SAR fails closed at the apiserver's RBAC layer rather than here. A bare
// "<name>" (no slashes) is treated as an "ippools/<name>" reference for
// defensive symmetry with older callers that may pass an unqualified name.
func resourceAndNameFromPoolKey(poolKey string) (resource, name string) {
	parts := strings.Split(poolKey, "/")
	// Drop empty leading segment from "/ipam.miloapis.com/..." so indexing
	// is uniform across the platform / tenant / bare cases.
	if len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return "ippools", poolKey
	}
	plural := parts[len(parts)-2]
	name = parts[len(parts)-1]
	switch plural {
	case "ippools":
		return "ippools", name
	default:
		// Unknown plural — fail closed at the SAR layer by sending it to a
		// resource the caller almost certainly does not have "use" on.
		return "ippools", name
	}
}
