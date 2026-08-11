package access

import (
	"context"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tenant"
)

type recordingAuthorizer struct {
	got      authorizer.Attributes
	decision authorizer.Decision
}

func (a *recordingAuthorizer) Authorize(_ context.Context, attrs authorizer.Attributes) (authorizer.Decision, string, error) {
	a.got = attrs
	return a.decision, "", nil
}

func callerCtx(project string) context.Context {
	return genericapirequest.WithUser(context.Background(), &user.DefaultInfo{
		Name:   "someone",
		Groups: []string{"authenticated"},
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {tenant.ParentAPIGroupProject},
			tenant.ExtraParentType:     {tenant.ParentTypeProject},
			tenant.ExtraParentName:     {project},
		},
	})
}

// Milo resolves a permission against the parent the extras name. A check that
// kept the caller's own project would ask whether they may use a class in their
// own project — a question the source project never answers.
func TestCanUseClassAsksTheSourceProject(t *testing.T) {
	authz := &recordingAuthorizer{decision: authorizer.DecisionAllow}

	allowed, err := NewClassAccessChecker(authz).CanUseClass(callerCtx("tenant-a"), "platform", "public-unicast")
	if err != nil {
		t.Fatalf("CanUseClass: %v", err)
	}
	if !allowed {
		t.Error("an allow decision came back denied")
	}

	got := authz.got
	if got.GetVerb() != "use" || got.GetAPIGroup() != "ipam.miloapis.com" ||
		got.GetResource() != "ipclasses" || got.GetName() != "public-unicast" {
		t.Errorf("checked %s %s/%s %q, want use ipam.miloapis.com/ipclasses %q",
			got.GetVerb(), got.GetAPIGroup(), got.GetResource(), got.GetName(), "public-unicast")
	}

	extra := got.GetUser().GetExtra()
	if p := extra[tenant.ExtraParentName]; len(p) != 1 || p[0] != "platform" {
		t.Errorf("parent-name = %v, want [platform] — the check must be scoped to the source project", p)
	}
	if got.GetUser().GetName() != "someone" {
		t.Errorf("subject = %q, want the original caller", got.GetUser().GetName())
	}
}

// Rewriting the extras must not mutate the identity the rest of the request
// still runs under: the caller's own project scopes every write that follows.
func TestCanUseClassLeavesTheCallersIdentityIntact(t *testing.T) {
	ctx := callerCtx("tenant-a")
	authz := &recordingAuthorizer{decision: authorizer.DecisionDeny}

	if _, err := NewClassAccessChecker(authz).CanUseClass(ctx, "platform", "public-unicast"); err != nil {
		t.Fatalf("CanUseClass: %v", err)
	}
	if got := tenant.FromContext(ctx).Name; got != "tenant-a" {
		t.Errorf("caller's project became %q, want tenant-a", got)
	}
}

func TestCanUseClassDeniesAnUnauthenticatedContext(t *testing.T) {
	authz := &recordingAuthorizer{decision: authorizer.DecisionAllow}

	allowed, err := NewClassAccessChecker(authz).CanUseClass(context.Background(), "platform", "public-unicast")
	if err != nil {
		t.Fatalf("CanUseClass: %v", err)
	}
	if allowed {
		t.Error("a context with no user was allowed")
	}
}
