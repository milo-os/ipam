package postgres

import (
	"context"
	"strconv"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/storage"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// withProject mirrors the tenant extras Milo's front gate forwards for a
// project-scoped request so tenant.FromContext resolves the given project.
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

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := startEphemeralPostgres(t)
	codec := newTestCodec(t)
	s := &Store{
		db:        db,
		codec:     codec,
		versioner: storage.APIObjectVersioner{},
	}
	return s
}

// pruneChangelog deletes every changelog row, reproducing the state of a
// long-lived database whose cleanup loop has aged out all entries past the
// retention window. The durable ipam_objects rows are left intact.
func pruneChangelog(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `DELETE FROM ipam_changelog`); err != nil {
		t.Fatalf("prune changelog: %v", err)
	}
}

// TestGetList_ResourceVersionAfterChangelogPruned: once the changelog has been
// pruned (the steady state of a quiet, long-lived database), a list must still
// report the durable version from the object rows and include those objects,
// rather than collapsing to the empty-database value.
func TestGetList_ResourceVersionAfterChangelogPruned(t *testing.T) {
	cases := []struct {
		name string
		ctx  func() context.Context
		key  string
	}{
		{
			name: "platform scope",
			ctx:  func() context.Context { return context.Background() },
			key:  "/ipam.miloapis.com/ippools",
		},
		{
			name: "project scope",
			ctx:  func() context.Context { return withProject(context.Background(), "datum-cloud") },
			key:  "/ipam.miloapis.com/ippools",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := tc.ctx()

			// Create a pool, then advance the sequence with a couple more
			// objects so the pool's RV is comfortably above 1.
			for _, name := range []string{"alpha", "beta", "gamma"} {
				out := newIPPool(name)
				if err := s.Create(ctx, "/ipam.miloapis.com/ippools/"+name, newIPPool(name), out, 0); err != nil {
					t.Fatalf("create %s: %v", name, err)
				}
			}

			// Capture the durable max RV before pruning.
			wantRV, err := s.currentResourceVersion(ctx)
			if err != nil {
				t.Fatalf("currentResourceVersion: %v", err)
			}
			if wantRV <= 1 {
				t.Fatalf("expected durable RV > 1, got %d", wantRV)
			}

			// Simulate the long-lived-DB steady state: changelog aged out.
			pruneChangelog(t, s)

			list := &v1alpha1.IPPoolList{}
			if err := s.GetList(ctx, tc.key, storage.ListOptions{Recursive: true, Predicate: storage.Everything}, list); err != nil {
				t.Fatalf("GetList: %v", err)
			}

			if got := len(list.Items); got != 3 {
				t.Fatalf("GetList returned %d items, want 3", got)
			}

			gotRV, err := strconv.ParseInt(list.ResourceVersion, 10, 64)
			if err != nil {
				t.Fatalf("parse list RV %q: %v", list.ResourceVersion, err)
			}
			if gotRV == 0 {
				t.Fatal("list ResourceVersion is 0 — apiserver rejects this as 'illegal resource version from storage: 0'")
			}
			if gotRV < wantRV {
				t.Errorf("list ResourceVersion = %d, want >= durable max %d (changelog-only RV regression)", gotRV, wantRV)
			}
		})
	}
}

// TestCurrentResourceVersion_AnchoredOnObjects asserts currentResourceVersion
// reflects ipam_objects even when the changelog is empty.
func TestCurrentResourceVersion_AnchoredOnObjects(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Several creates so the surviving object's RV is well above the
	// fallback value of 1 — this is what distinguishes "anchored on
	// ipam_objects" from "changelog empty, collapsed to 1".
	for _, name := range []string{"a", "b", "c", "d"} {
		out := newIPPool(name)
		if err := s.Create(ctx, "/ipam.miloapis.com/ippools/"+name, newIPPool(name), out, 0); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	withChangelog, err := s.currentResourceVersion(ctx)
	if err != nil {
		t.Fatalf("currentResourceVersion (with changelog): %v", err)
	}
	if withChangelog <= 1 {
		t.Fatalf("expected durable RV > 1 before prune, got %d", withChangelog)
	}

	pruneChangelog(t, s)

	pruned, err := s.currentResourceVersion(ctx)
	if err != nil {
		t.Fatalf("currentResourceVersion (pruned): %v", err)
	}

	if pruned != withChangelog {
		t.Errorf("currentResourceVersion changed after changelog prune: with=%d pruned=%d (must stay anchored on ipam_objects)", withChangelog, pruned)
	}
	if pruned <= 1 {
		t.Errorf("currentResourceVersion = %d after prune, want the objects' durable RV (> 1); changelog-only RV would collapse to 1", pruned)
	}
}
