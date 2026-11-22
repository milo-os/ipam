package access

import (
	"context"

	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/request"
)

// VerbReleaseUnleased is the permission, on ipallocations, to release a
// retained allocation that has no lease deadline. It is granted to the
// ipam-admin role only (config/components/iam/roles/ipam-admin.yaml).
const VerbReleaseUnleased = "release-unleased"

// UnleasedReleaseChecker checks whether the caller may release a retained
// IPAllocation that has no lease deadline, either by deleting it or by
// reclaiming it with an IPClaim under reclaimPolicy Delete, which frees it when
// that claim is deleted.
//
// Deleting an IPAllocation is how a retained address is handed back, and a
// consumer may do that for one whose lease runs out anyway: the release only
// brings the deadline forward. An address held with no deadline is held on
// purpose, typically because something outside IPAM (a VPC, a subnet) still
// uses it, and freeing it lets the next claim take an address in use. That
// release needs this second permission, which the server checks itself
// because the apiserver's own authorization of the DELETE cannot see the
// deadline.
type UnleasedReleaseChecker interface {
	CanReleaseUnleased(ctx context.Context, namespace, name string) (bool, error)
}

// NewUnleasedReleaseChecker wraps an authorizer.Authorizer so the IPAllocation
// registry can ask "may the caller release this unleased allocation?".
func NewUnleasedReleaseChecker(authz authorizer.Authorizer) UnleasedReleaseChecker {
	return &sarChecker{authz: authz}
}

// CanReleaseUnleased runs a "release-unleased" authorization check against
// ipallocations/<name> in namespace, in the caller's own project: the same
// resource the DELETE itself was authorized against. Returns false without an
// error when the context carries no user; callers treat that as a denial.
func (c *sarChecker) CanReleaseUnleased(ctx context.Context, namespace, name string) (bool, error) {
	u, ok := request.UserFrom(ctx)
	if !ok {
		return false, nil
	}

	attrs := authorizer.AttributesRecord{
		User:            u,
		Verb:            VerbReleaseUnleased,
		APIGroup:        "ipam.miloapis.com",
		Resource:        "ipallocations",
		Namespace:       namespace,
		Name:            name,
		ResourceRequest: true,
	}
	decision, _, err := c.authz.Authorize(ctx, attrs)
	return decision == authorizer.DecisionAllow, err
}
