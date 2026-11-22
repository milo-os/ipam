package access

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/request"
)

// VerbReleaseUnleased is the permission, on ipallocations, to release a
// retained allocation that has no lease deadline. It is granted to the
// ipam-admin role only (config/components/iam/roles/ipam-admin.yaml).
const VerbReleaseUnleased = "release-unleased"

// VerbDeleteCollection also admits the release of an unleased allocation. It
// is the verb the namespace controller holds, through Kubernetes RBAC on every
// resource, and deleting a namespace has always freed what its allocations
// hold. The ipallocations ProtectedResource does not declare it, so no Milo
// IAM role can grant it to a consumer.
const VerbDeleteCollection = "deletecollection"

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
// and IPClaim registries can ask "may the caller release this unleased
// allocation?".
func NewUnleasedReleaseChecker(authz authorizer.Authorizer) UnleasedReleaseChecker {
	return &sarChecker{authz: authz}
}

// CanReleaseUnleased asks two questions, in the caller's own project: may the
// caller release-unleased ipallocations/<name> in namespace, and failing that,
// may it deletecollection ipallocations in namespace, as the namespace
// controller may. Either admits the release. Returns false without an error
// when the context carries no user; callers treat that as a denial. An error
// is returned only if neither question was answered yes.
func (c *sarChecker) CanReleaseUnleased(ctx context.Context, namespace, name string) (bool, error) {
	u, ok := request.UserFrom(ctx)
	if !ok {
		return false, nil
	}

	var firstErr error
	for _, attrs := range []authorizer.AttributesRecord{
		{Verb: VerbReleaseUnleased, Name: name},
		{Verb: VerbDeleteCollection},
	} {
		attrs.User = u
		attrs.APIGroup = "ipam.miloapis.com"
		attrs.Resource = "ipallocations"
		attrs.Namespace = namespace
		attrs.ResourceRequest = true
		decision, _, err := c.authz.Authorize(ctx, attrs)
		if decision == authorizer.DecisionAllow {
			return true, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return false, firstErr
}

// UnleasedReleaseAsk is one request's question to an UnleasedReleaseChecker
// about one allocation. It is asked at most once and the answer remembered,
// so a registry can ask before it takes any lock and the allocator's guard,
// run under the locks, reuses the answer rather than calling the authorizer
// again.
type UnleasedReleaseAsk struct {
	checker         UnleasedReleaseChecker
	namespace, name string

	asked, allowed bool
	err            error
}

// NewUnleasedReleaseAsk prepares the question for namespace/name. A nil
// checker answers no: with no authorizer, nothing vouched for the caller.
func NewUnleasedReleaseAsk(checker UnleasedReleaseChecker, namespace, name string) *UnleasedReleaseAsk {
	return &UnleasedReleaseAsk{checker: checker, namespace: namespace, name: name}
}

// Allowed returns the answer, asking the checker the first time. An
// authorizer failure is returned as a 500 and remembered like an answer.
func (a *UnleasedReleaseAsk) Allowed(ctx context.Context) (bool, error) {
	if a.asked {
		return a.allowed, a.err
	}
	a.asked = true
	if a.checker == nil {
		return false, nil
	}
	ok, err := a.checker.CanReleaseUnleased(ctx, a.namespace, a.name)
	if err != nil {
		a.err = apierrors.NewInternalError(fmt.Errorf(
			"authorize release of unleased IPAllocation %s/%s: %w", a.namespace, a.name, err))
		return false, a.err
	}
	a.allowed = ok
	return ok, nil
}
