package access

import (
	"context"
	"testing"

	"k8s.io/apiserver/pkg/authorization/authorizer"

	"go.miloapis.com/ipam/internal/tenant"
)

// The check asks about the allocation being released, in the caller's own
// project, under the verb the ProtectedResource declares and only ipam-admin
// grants.
func TestCanReleaseUnleasedAsksAboutTheAllocationInTheCallersProject(t *testing.T) {
	for _, tc := range []struct {
		decision authorizer.Decision
		want     bool
	}{
		{authorizer.DecisionAllow, true},
		{authorizer.DecisionDeny, false},
		{authorizer.DecisionNoOpinion, false},
	} {
		authz := &recordingAuthorizer{decision: tc.decision}
		allowed, err := NewUnleasedReleaseChecker(authz).CanReleaseUnleased(callerCtx("tenant-a"), "default", "vpc-48")
		if err != nil {
			t.Fatalf("CanReleaseUnleased: %v", err)
		}
		if allowed != tc.want {
			t.Errorf("decision %v: allowed = %v, want %v", tc.decision, allowed, tc.want)
		}

		got := authz.got
		if got.GetVerb() != VerbReleaseUnleased || got.GetAPIGroup() != "ipam.miloapis.com" ||
			got.GetResource() != "ipallocations" || got.GetNamespace() != "default" ||
			got.GetName() != "vpc-48" || !got.IsResourceRequest() {
			t.Errorf("checked %s %s/%s %s/%q, want %s ipam.miloapis.com/ipallocations default/%q",
				got.GetVerb(), got.GetAPIGroup(), got.GetResource(), got.GetNamespace(), got.GetName(),
				VerbReleaseUnleased, "vpc-48")
		}
		if p := got.GetUser().GetExtra()[tenant.ExtraParentName]; len(p) != 1 || p[0] != "tenant-a" {
			t.Errorf("parent-name = %v, want [tenant-a]: the release is in the caller's own project", p)
		}
	}
}

// No user on the context is a denial, not an error.
func TestCanReleaseUnleasedWithNoUserIsDenied(t *testing.T) {
	authz := &recordingAuthorizer{decision: authorizer.DecisionAllow}
	allowed, err := NewUnleasedReleaseChecker(authz).CanReleaseUnleased(context.Background(), "default", "vpc-48")
	if err != nil || allowed {
		t.Errorf("CanReleaseUnleased with no user = (%v, %v), want (false, nil)", allowed, err)
	}
}
