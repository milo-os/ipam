package access

import (
	"context"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tenant"
)

// ClassAccessChecker checks whether the caller may reference an IPClass that
// lives in another project.
//
// A reference resolves into the source project and allocates from its pools, so
// the source project has to admit the caller. project and name identify the
// class the reference points at.
type ClassAccessChecker interface {
	CanUseClass(ctx context.Context, project, name string) (bool, error)
}

// NewClassAccessChecker wraps an authorizer.Authorizer so the IPClass registry
// can ask "may the caller use this class?" before admitting a reference to it.
func NewClassAccessChecker(authz authorizer.Authorizer) ClassAccessChecker {
	return &sarChecker{authz: authz}
}

// CanUseClass runs a "use" authorization check against ipclasses/<name> in the
// source project. Returns false without an error when the context carries no
// user; callers treat that as a denial, not a system failure.
func (c *sarChecker) CanUseClass(ctx context.Context, project, name string) (bool, error) {
	u, ok := request.UserFrom(ctx)
	if !ok {
		return false, nil
	}

	attrs := authorizer.AttributesRecord{
		User:            scopeUserToProject(u, project),
		Verb:            "use",
		APIGroup:        "ipam.miloapis.com",
		Resource:        "ipclasses",
		Name:            name,
		ResourceRequest: true,
	}
	decision, _, err := c.authz.Authorize(ctx, attrs)
	return decision == authorizer.DecisionAllow, err
}

// scopeUserToProject copies u with its parent extras rewritten to name project.
//
// Milo resolves an authorization request against the parent the extras name, so
// a SAR carrying the caller's own project asks whether the caller may use a
// class in their own project — always the wrong question for a reference, and
// one a project admin answers yes to for themselves.
func scopeUserToProject(u user.Info, project string) user.Info {
	extra := map[string][]string{}
	for k, v := range u.GetExtra() {
		extra[k] = v
	}
	extra[tenant.ExtraParentAPIGroup] = []string{tenant.ParentAPIGroupProject}
	extra[tenant.ExtraParentType] = []string{tenant.ParentTypeProject}
	extra[tenant.ExtraParentName] = []string{project}

	return &user.DefaultInfo{
		Name:   u.GetName(),
		UID:    u.GetUID(),
		Groups: u.GetGroups(),
		Extra:  extra,
	}
}
