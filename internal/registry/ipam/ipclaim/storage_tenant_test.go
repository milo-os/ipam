package ipclaim

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tenant"
)

// withProject returns a context carrying the tenant extras Milo's front gate
// forwards for a project-scoped request, so tenant.FromContext resolves the
// given project ID.
func withProject(ctx context.Context, project string) context.Context {
	return genericapirequest.WithUser(ctx, &user.DefaultInfo{
		Name: "tester",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {project},
		},
	})
}

// TestAllocatingREST_Create_AppliesTenantPrefix is the regression guard for the
// project-scoping bug: a claim created through a project control-plane must
// address its pool, claim, and allocation rows under the "project/<id>/" tenant
// prefix — matching where the generic registry Store persists the same objects.
// Before the fix the allocator looked the pool up at the platform root and every
// project-scoped allocation failed with "IPPool not found".
func TestAllocatingREST_Create_AppliesTenantPrefix(t *testing.T) {
	const project = "datum-cloud"

	t.Run("project-scoped request prefixes every key", func(t *testing.T) {
		r, fa, _ := newTestREST()
		ctx := genericapirequest.WithNamespace(withProject(context.Background(), project), "default")

		if _, err := r.Create(ctx, newClaim(), nil, &metav1.CreateOptions{}); err != nil {
			t.Fatalf("Create returned error: %v", err)
		}

		wantPool := "project/" + project + "/ipam.miloapis.com/ippools/us-east"
		if fa.gotPoolKey != wantPool {
			t.Errorf("pool key = %q, want %q", fa.gotPoolKey, wantPool)
		}
		if fa.gotOwnerProject != project {
			t.Errorf("owner project passed to allocator = %q, want %q", fa.gotOwnerProject, project)
		}
		wantPrefix := "project/" + project + "/ipam.miloapis.com/"
		for _, k := range fa.gotInsertKeys {
			if !strings.HasPrefix(k, wantPrefix) {
				t.Errorf("inserted key %q missing tenant prefix %q", k, wantPrefix)
			}
		}
	})

	t.Run("platform request keeps the platform root", func(t *testing.T) {
		r, fa, _ := newTestREST()
		ctx := genericapirequest.WithNamespace(context.Background(), "default")

		if _, err := r.Create(ctx, newClaim(), nil, &metav1.CreateOptions{}); err != nil {
			t.Fatalf("Create returned error: %v", err)
		}

		if want := "/ipam.miloapis.com/ippools/us-east"; fa.gotPoolKey != want {
			t.Errorf("pool key = %q, want %q", fa.gotPoolKey, want)
		}
		if fa.gotOwnerProject != "" {
			t.Errorf("owner project = %q, want empty for platform scope", fa.gotOwnerProject)
		}
		for _, k := range fa.gotInsertKeys {
			if !strings.HasPrefix(k, "/ipam.miloapis.com/") {
				t.Errorf("inserted key %q is not a platform key", k)
			}
		}
	})
}

// Cross-project pool resolution (poolRef.projectRef pointing at another
// project's shared pool) exercises a DB read inside the authorization gate,
// so it is covered by the chainsaw e2e suite rather than these fakes. The key
// construction it relies on is the same poolStorageKey(project, name) path
// asserted above.
