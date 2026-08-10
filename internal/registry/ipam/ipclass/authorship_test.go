package ipclass

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tenant"
)

const testPlatformProject = "milo-platform"

// identityFor builds the tenant.Identity a request from the named project
// produces. The platform project is stamped on from server configuration, so a
// context is the only way to get a value that can answer IsPlatform.
func identityFor(project string) tenant.Identity {
	ctx := tenant.WithPlatformProject(context.Background(), testPlatformProject)
	if project == "" {
		return tenant.FromContext(request.WithUser(ctx, &user.DefaultInfo{Name: "someone"}))
	}
	return tenant.FromContext(request.WithUser(ctx, &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {project},
		},
	}))
}

// The catalog is platform policy and lives in the platform project. A class
// written into a tenant's own key space would be visible to that tenant's own
// LIST and invisible to every allocator lookup — accepted, listable, and used
// by nothing.
//
// That silently-inert object is the reason this rejection exists. The write
// succeeding is worse than it failing: `kubectl get` shows the class, the
// operator believes the catalog changed, and no claim ever resolves through it.
func TestOnlyThePlatformProjectMayAuthorClasses(t *testing.T) {
	tests := []struct {
		name    string
		project string
		wantErr bool
	}{
		{
			name:    "the platform project may author classes",
			project: testPlatformProject,
			wantErr: false,
		},
		{
			name:    "a tenant project may not",
			project: "project-alpha",
			wantErr: true,
		},
		{
			// Not the platform under the current definition of IsPlatform, and
			// the case that used to be treated as the platform. It must be
			// refused like any other non-platform caller.
			name:    "a caller with no tenant may not",
			project: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateClassAuthorship(identityFor(tt.project))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("project %q must be allowed to author classes, got %v", tt.project, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("project %q must not be allowed to author classes", tt.project)
			}
			if !apierrors.IsForbidden(err) {
				t.Errorf("want a Forbidden status error, got %T: %v", err, err)
			}
		})
	}
}

// The message has to say WHERE classes live, not merely that this is refused.
// "Forbidden" alone sends an operator to read RBAC, which is not the problem —
// the permission is not missing, the object is in the wrong project.
func TestTheRejectionSaysWhereClassesLive(t *testing.T) {
	err := validateClassAuthorship(identityFor("project-alpha"))
	if err == nil {
		t.Fatal("expected a rejection")
	}
	msg := err.Error()
	for _, want := range []string{"platform project", testPlatformProject} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message should contain %q so the remedy is actionable, got: %s", want, msg)
		}
	}
}

// An unconfigured server has no platform project, so nobody may author a class.
// This is the fail-closed direction: the alternative — treating "unconfigured"
// as "anyone" — would open the catalog on exactly the server that is least
// correctly set up.
func TestNobodyMayAuthorClassesWithNoPlatformProject(t *testing.T) {
	id := tenant.FromContext(request.WithUser(context.Background(), &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {testPlatformProject},
		},
	}))
	if err := validateClassAuthorship(id); err == nil {
		t.Fatal("a class was authored on a server with no configured platform project")
	}
}
