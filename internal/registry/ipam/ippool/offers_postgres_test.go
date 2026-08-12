package ippool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/internal/allocator"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

const offerProject = "tenant-a"

func TestMain(m *testing.M) { testdb.TestMain(m) }

// poolCtx is the context a project-scoped caller arrives with. IPPool is
// cluster-scoped, but the generic store still reads the namespace the endpoint
// handler would have set.
func poolCtx(project string) context.Context {
	ctx := genericapirequest.WithUser(context.Background(), &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {tenant.ParentAPIGroupProject},
			tenant.ExtraParentType:     {tenant.ParentTypeProject},
			tenant.ExtraParentName:     {project},
		},
	})
	return genericapirequest.WithNamespace(ctx, metav1.NamespaceNone)
}

func newPostgresPoolStorage(t *testing.T) (*AllocatingIPPoolREST, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Pool(t)

	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion)

	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(codec)

	store, _, err := NewIPPoolStorage(scheme, getter, allocator.NewPostgresPrefixAllocator(), db, codec)
	if err != nil {
		t.Fatalf("pool storage: %v", err)
	}
	t.Cleanup(store.Destroy)
	return store, db
}

// seedClass writes a class definition straight to storage: these tests are
// about the pool's admission, not the class registry's.
func seedClass(t *testing.T, db *pgxpool.Pool, project, name string, uniqueWithin []string) {
	t.Helper()
	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4, UniqueWithin: uniqueWithin},
	}
	data, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal class %q: %v", name, err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPClass',$2,$3)`,
		tenant.Identity{Name: project}.ResourceKey("ipclasses", name), name, data,
	); err != nil {
		t.Fatalf("seed class %q: %v", name, err)
	}
}

func rootPool(name string, classNames ...string) *ipam.IPPool {
	return &ipam.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipam.IPPoolSpec{
			CIDR:       "10.0.0.0/16",
			IPFamily:   ipam.IPv4,
			ClassNames: classNames,
		},
	}
}

// Two classes that divide their space the same way can share a pool: every
// claim drawing from it derives the same address-space digest, so the exclusion
// constraint sees them all.
func TestPoolOfferedToAgreeingClassesIsAccepted(t *testing.T) {
	store, db := newPostgresPoolStorage(t)
	seedClass(t, db, offerProject, "public", []string{"network"})
	seedClass(t, db, offerProject, "private", []string{"network"})

	if _, err := store.Create(poolCtx(offerProject), rootPool("shared", "public", "private"), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// The failure this rule exists for: one pool split into two address spaces that
// cannot see each other, so the second class hands out addresses the first
// already holds with nothing to report the collision.
func TestPoolOfferedToDisagreeingClassesIsRejected(t *testing.T) {
	store, db := newPostgresPoolStorage(t)
	seedClass(t, db, offerProject, "flat", nil)
	seedClass(t, db, offerProject, "per-network", []string{"network"})

	_, err := store.Create(poolCtx(offerProject), rootPool("shared", "flat", "per-network"), nil, &metav1.CreateOptions{})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("Create returned %v, want Invalid", err)
	}
	for _, want := range []string{"flat", "per-network", "the whole pool", "network"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	if _, err := store.Get(poolCtx(offerProject), "shared", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("refused pool was persisted anyway: %v", err)
	}
}

// Ordering differences are not disagreement: uniqueWithin is a set, and
// [network, location] is the same address space as [location, network].
func TestRoleOrderIsNotDisagreement(t *testing.T) {
	store, db := newPostgresPoolStorage(t)
	seedClass(t, db, offerProject, "a", []string{"network", "location"})
	seedClass(t, db, offerProject, "b", []string{"location", "network"})

	if _, err := store.Create(poolCtx(offerProject), rootPool("shared", "a", "b"), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// A pool with one class has nothing to disagree with, whatever that class says.
func TestPoolOfferedToOneClassIsUnaffected(t *testing.T) {
	store, db := newPostgresPoolStorage(t)
	seedClass(t, db, offerProject, "per-network", []string{"network"})

	if _, err := store.Create(poolCtx(offerProject), rootPool("solo", "per-network"), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// A pool written before this rule existed is held to it on its next edit
// rather than grandfathered: an update that leaves the two classes in place
// republishes the hazard.
func TestUpdateOfAPreExistingViolationIsRejected(t *testing.T) {
	store, db := newPostgresPoolStorage(t)
	seedClass(t, db, offerProject, "flat", nil)
	seedClass(t, db, offerProject, "per-network", []string{"network"})
	seedPool(t, db, offerProject, "legacy", "flat", "per-network")

	_, _, err := updatePool(store, poolCtx(offerProject), "legacy", func(p *ipam.IPPool) {
		p.Labels = map[string]string{"team": "network"}
	})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("Update returned %v, want Invalid", err)
	}
	if !strings.Contains(err.Error(), "per-network") {
		t.Errorf("error %q does not name the class to reconcile", err)
	}
}

// Nothing is stranded by that: the offending field is mutable, and dropping one
// class lets the same edit through.
func TestUpdateThatResolvesTheViolationIsAccepted(t *testing.T) {
	store, db := newPostgresPoolStorage(t)
	seedClass(t, db, offerProject, "flat", nil)
	seedClass(t, db, offerProject, "per-network", []string{"network"})
	seedPool(t, db, offerProject, "legacy", "flat", "per-network")

	obj, _, err := updatePool(store, poolCtx(offerProject), "legacy", func(p *ipam.IPPool) {
		p.Labels = map[string]string{"team": "network"}
		p.Spec.ClassNames = []string{"flat"}
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := obj.(*ipam.IPPool).Spec.ClassNames; len(got) != 1 || got[0] != "flat" {
		t.Errorf("classNames after update: %v, want [flat]", got)
	}
}

// seedPool writes a pool straight to storage, standing in for one created
// before the agreement rule existed.
func seedPool(t *testing.T, db *pgxpool.Pool, project, name string, classNames ...string) {
	t.Helper()
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: "10.0.0.0/16", IPFamily: ipamv1alpha1.IPv4, ClassNames: classNames,
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "10.0.0.0/16", IPFamily: ipamv1alpha1.IPv4,
		},
	}
	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool %q: %v", name, err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		poolStorageKey(project, name), name, data,
	); err != nil {
		t.Fatalf("seed pool %q: %v", name, err)
	}
}

func updatePool(store *AllocatingIPPoolREST, ctx context.Context, name string, mutate func(*ipam.IPPool)) (runtime.Object, bool, error) {
	current, err := store.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	updated := current.(*ipam.IPPool).DeepCopy()
	mutate(updated)
	return store.Update(ctx, name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, &metav1.UpdateOptions{})
}
