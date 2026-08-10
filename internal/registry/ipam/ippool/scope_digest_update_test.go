package ippool

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// digestTestPlatformProject is what --platform-project is set to for these
// tests. It has to be a real project name: the whole point of the bug under
// test is that the platform's tenant string changed from "" to a project.
const digestTestPlatformProject = "milo-platform"

// requestFrom builds the context a request from the named project arrives on,
// carrying the server's configured platform project the way cmd/ipam's
// platformProjectFilter puts it there.
func requestFrom(project string) context.Context {
	ctx := tenant.WithPlatformProject(context.Background(), digestTestPlatformProject)
	return request.WithUser(ctx, &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {project},
		},
	})
}

func poolWithScope(digest string, s map[string]ipam.ScopeRef) *ipam.IPPool {
	return &ipam.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       ipam.IPPoolSpec{CIDR: "10.0.0.0/16", IPFamily: ipam.IPv4, Scope: s},
		Status:     ipam.IPPoolStatus{Phase: ipam.PoolReady, ScopeDigest: digest},
	}
}

// status.scopeDigest is derived from (tenant, spec.scope), and PrepareForUpdate
// copies the old status wholesale — so before this fix an update carried the
// digest forward unchanged whatever happened to either input.
//
// The assertion is against `scope.PoolDigest(...)` recomputed from the object's
// own inputs, not against a literal. A test that only checked "Update writes
// some digest" would pass against a hardcoded constant, and one that pinned a
// literal would have to be edited every time the canonical encoding moves —
// which is exactly when it most needs to keep working.
func TestUpdateRecomputesScopeDigest(t *testing.T) {
	strategy := NewStrategy(nil)

	located := map[string]ipam.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	}
	elsewhere := map[string]ipam.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "eu-west-1"},
	}

	tests := []struct {
		name       string
		project    string
		storedOn   *ipam.IPPool // the object as stored, with whatever digest it carries
		incoming   *ipam.IPPool // the object as submitted
		wantScope  map[string]ipam.ScopeRef
		wantTenant string
	}{
		{
			// The #65 re-homing case. The stored digest was computed when the
			// platform's tenant string was "" — migration 005's default,
			// 6139457f… — and the key rewrite could not recompute it, because a
			// digest is a SHA-256 over a canonical form the schema never stores.
			// A spec write must heal it.
			name:       "a re-homed platform pool heals its stale digest",
			project:    digestTestPlatformProject,
			storedOn:   poolWithScope(scope.PoolDigest("", nil), nil),
			incoming:   poolWithScope(scope.PoolDigest("", nil), nil),
			wantScope:  nil,
			wantTenant: digestTestPlatformProject,
		},
		{
			// The sharper case, and the one that is not about the cutover at
			// all: spec.scope is NOT in the immutable set (see ValidateUpdate),
			// so an operator can legitimately edit it. Before this fix the
			// digest went on describing the scope the pool used to have.
			name:       "editing spec.scope moves the digest with it",
			project:    "acme",
			storedOn:   poolWithScope(scope.PoolDigest("acme", located), located),
			incoming:   poolWithScope(scope.PoolDigest("acme", located), elsewhere),
			wantScope:  elsewhere,
			wantTenant: "acme",
		},
		{
			// The no-op. An ordinary update to an already-correct pool must not
			// perturb the value, or every update would look like a change.
			name:       "an already-correct digest is left where it is",
			project:    "acme",
			storedOn:   poolWithScope(scope.PoolDigest("acme", located), located),
			incoming:   poolWithScope(scope.PoolDigest("acme", located), located),
			wantScope:  located,
			wantTenant: "acme",
		},
		{
			// A client cannot talk the server into a digest of its choosing:
			// status is server-derived, and the submitted value is ignored in
			// favour of a recompute from the object's real inputs.
			name:       "a client-supplied digest is overwritten, not trusted",
			project:    "acme",
			storedOn:   poolWithScope(scope.PoolDigest("acme", located), located),
			incoming:   poolWithScope("deadbeef", located),
			wantScope:  located,
			wantTenant: "acme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.incoming.DeepCopy()
			strategy.PrepareForUpdate(requestFrom(tt.project), got, tt.storedOn)

			want := scope.PoolDigest(tt.wantTenant, tt.wantScope)
			if got.Status.ScopeDigest != want {
				t.Fatalf("status.scopeDigest = %q, want %q (recomputed from tenant %q and the object's own spec.scope)",
					got.Status.ScopeDigest, want, tt.wantTenant)
			}
			// And the rest of status must still come from the stored object —
			// PrepareForUpdate's existing job. A recompute that also let a
			// client write phase or capacity would be a worse bug than the one
			// being fixed.
			if got.Status.Phase != tt.storedOn.Status.Phase {
				t.Errorf("status.phase = %q, want the stored %q: only the digest is re-derived",
					got.Status.Phase, tt.storedOn.Status.Phase)
			}
		})
	}
}

// The digest a spec update derives must be the same one Create would have
// written for the same inputs, or a pool's value would depend on which path
// last touched it. Both go through scope.PoolDigest(id.Name, spec.Scope); this
// pins that they agree rather than merely that each is self-consistent.
func TestUpdateAndCreateDeriveTheSameDigest(t *testing.T) {
	const project = "acme"
	s := map[string]ipam.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	}

	// What the Create path assigns (storage.go, after the allocation transaction).
	createSide := scope.PoolDigest(tenant.FromContext(requestFrom(project)).Name, s)

	// What an update now derives.
	updated := poolWithScope("stale", s)
	NewStrategy(nil).PrepareForUpdate(requestFrom(project), updated, poolWithScope("stale", s))

	if updated.Status.ScopeDigest != createSide {
		t.Fatalf("update derived %q but create would write %q; the two paths must agree",
			updated.Status.ScopeDigest, createSide)
	}
}
