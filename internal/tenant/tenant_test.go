package tenant

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
)

// ctxFor builds a request context carrying the parent extras Milo's front gate
// forwards. An empty parentName produces the shape an unscoped kubeconfig does:
// an authenticated user with no parent extras at all.
func ctxFor(parentName string) context.Context {
	ctx := context.Background()
	if parentName == "" {
		return request.WithUser(ctx, &user.DefaultInfo{Name: "someone"})
	}
	return request.WithUser(ctx, &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			ExtraParentType:     {"Project"},
			ExtraParentName:     {parentName},
		},
	})
}

func TestRequireTenantRefusesACallerWithNoProject(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"authenticated but no parent extras", ctxFor("")},
		{"no user info at all", context.Background()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RequireTenant(tc.ctx); !errors.Is(err, ErrNoTenant) {
				t.Fatalf("RequireTenant() error = %v, want ErrNoTenant", err)
			}
		})
	}
}

func TestRequireTenantAcceptsAProjectScopedCaller(t *testing.T) {
	id, err := RequireTenant(ctxFor("alpha"))
	if err != nil {
		t.Fatalf("RequireTenant() error = %v, want nil", err)
	}
	if got := id.Project(); got != "alpha" {
		t.Errorf("Project() = %q, want %q", got, "alpha")
	}
}

// Reads take FromContext rather than RequireTenant: an untenanted read returns
// nothing, which is the tenancy model working. Only writes need refusing.
func TestFromContextIsEmptyRatherThanAnErrorWhenUntenanted(t *testing.T) {
	if got := FromContext(ctxFor("")).Name; got != "" {
		t.Errorf("Name = %q, want empty", got)
	}
}

// Every key a tenanted caller produces is prefixed, so an untenanted caller
// cannot address the same keyspace by accident.
func TestKeysAreProjectPrefixed(t *testing.T) {
	id, err := RequireTenant(ctxFor("alpha"))
	if err != nil {
		t.Fatalf("RequireTenant() error = %v", err)
	}
	key := id.ResourceKey("ipclasses", "public-unicast-ipv4")
	want := "project/alpha/ipam.miloapis.com/ipclasses/public-unicast-ipv4"
	if key != want {
		t.Errorf("ResourceKey() = %q, want %q", key, want)
	}
	if got := ProjectFromKey(key); got != "alpha" {
		t.Errorf("ProjectFromKey(%q) = %q, want %q", key, got, "alpha")
	}
}
