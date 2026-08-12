package ippool

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclass"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

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

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	return scheme
}

func newRESTOptionsGetter(t *testing.T, scheme *runtime.Scheme, db *pgxpool.Pool) *pgstore.RESTOptionsGetter {
	t.Helper()
	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion))
	return getter
}

func newPostgresPoolStorage(t *testing.T, db *pgxpool.Pool) *AllocatingIPPoolREST {
	t.Helper()
	scheme := newTestScheme()
	getter := newRESTOptionsGetter(t, scheme, db)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion)

	store, _, err := NewIPPoolStorage(scheme, getter, allocator.NewPostgresPrefixAllocator(), db, codec)
	if err != nil {
		t.Fatalf("pool storage: %v", err)
	}
	t.Cleanup(store.Destroy)
	return store
}

func rootPoolObj(name, cidr string, family ipam.IPFamily) *ipam.IPPool {
	return &ipam.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ipam.IPPoolSpec{CIDR: cidr, IPFamily: family},
	}
}

func createPool(t *testing.T, s *AllocatingIPPoolREST, ctx context.Context, p *ipam.IPPool) error {
	t.Helper()
	_, err := s.Create(ctx, p, nil, &metav1.CreateOptions{})
	return err
}

// A second root pool over space the first already holds would hand the same
// address to unrelated claims, because IPAM enforces uniqueness within a pool.
func TestOverlappingRootPoolsAreRefusedWithinAProject(t *testing.T) {
	s := newPostgresPoolStorage(t, testdb.Pool(t))
	ctx := poolCtx("tenant-a")

	if err := createPool(t, s, ctx, rootPoolObj("wide", "10.171.0.0/16", ipam.IPv4)); err != nil {
		t.Fatalf("create first root pool: %v", err)
	}

	err := createPool(t, s, ctx, rootPoolObj("narrow", "10.171.0.0/24", ipam.IPv4))
	if !apierrors.IsConflict(err) {
		t.Fatalf("Create returned %v, want Conflict", err)
	}
	if !strings.Contains(err.Error(), "wide") {
		t.Errorf("error %q does not name the pool that was collided with", err)
	}

	if _, err := s.Get(ctx, "narrow", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("refused pool was persisted anyway: %v", err)
	}
}

// Containment is symmetric: the wider range is refused against the narrower one
// already held, not only the other way round.
func TestARootPoolContainingAnExistingOneIsRefused(t *testing.T) {
	s := newPostgresPoolStorage(t, testdb.Pool(t))
	ctx := poolCtx("tenant-a")

	if err := createPool(t, s, ctx, rootPoolObj("narrow", "10.171.0.0/24", ipam.IPv4)); err != nil {
		t.Fatalf("create first root pool: %v", err)
	}
	if err := createPool(t, s, ctx, rootPoolObj("wide", "10.171.0.0/16", ipam.IPv4)); !apierrors.IsConflict(err) {
		t.Fatalf("Create returned %v, want Conflict", err)
	}
}

// Adjacent ranges share no address, and a project carving its space into
// several root pools is the ordinary case.
func TestNonOverlappingRootPoolsAreAccepted(t *testing.T) {
	s := newPostgresPoolStorage(t, testdb.Pool(t))
	ctx := poolCtx("tenant-a")

	if err := createPool(t, s, ctx, rootPoolObj("first", "10.171.0.0/24", ipam.IPv4)); err != nil {
		t.Fatalf("create first root pool: %v", err)
	}
	if err := createPool(t, s, ctx, rootPoolObj("second", "10.171.1.0/24", ipam.IPv4)); err != nil {
		t.Fatalf("create adjacent root pool: %v", err)
	}
	if err := createPool(t, s, ctx, rootPoolObj("v6", "2001:db8::/40", ipam.IPv6)); err != nil {
		t.Fatalf("create IPv6 root pool: %v", err)
	}
}

// Private space is tenant-scoped: two projects both holding 10.0.0.0/8 are
// separate address spaces, and refusing the second would be wrong.
func TestTwoProjectsMayHoldOverlappingRootPools(t *testing.T) {
	s := newPostgresPoolStorage(t, testdb.Pool(t))

	if err := createPool(t, s, poolCtx("tenant-a"), rootPoolObj("private", "10.0.0.0/8", ipam.IPv4)); err != nil {
		t.Fatalf("create in tenant-a: %v", err)
	}
	if err := createPool(t, s, poolCtx("tenant-b"), rootPoolObj("private", "10.0.0.0/8", ipam.IPv4)); err != nil {
		t.Fatalf("create in tenant-b: %v", err)
	}
}

// Re-creating a pool is the far more common mistake, and reporting an overlap
// with itself would send the operator looking for a second pool.
func TestRecreatingARootPoolReportsAlreadyExists(t *testing.T) {
	s := newPostgresPoolStorage(t, testdb.Pool(t))
	ctx := poolCtx("tenant-a")

	if err := createPool(t, s, ctx, rootPoolObj("only", "10.171.0.0/16", ipam.IPv4)); err != nil {
		t.Fatalf("create root pool: %v", err)
	}
	if err := createPool(t, s, ctx, rootPoolObj("only", "10.171.0.0/16", ipam.IPv4)); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Create returned %v, want AlreadyExists", err)
	}
}

// A pool and the class that provisions pools must refuse the same reservations.
// A rule enforced on one alone lets a class hold a reservation that its own
// pools reject.
func TestTheSameBadReservationIsRefusedOnAPoolAndOnAClass(t *testing.T) {
	db := testdb.Pool(t)
	scheme := newTestScheme()
	getter := newRESTOptionsGetter(t, scheme, db)

	poolStore := newPostgresPoolStorage(t, db)
	classStore, _, err := ipclass.NewClassStorage(scheme, getter, nil)
	if err != nil {
		t.Fatalf("class storage: %v", err)
	}
	t.Cleanup(classStore.Destroy)

	ctx := poolCtx("tenant-a")

	for _, tc := range []struct {
		name string
		res  ipam.ReservationSpec
		want string
	}{
		{
			name: "positions reserved without a unit size",
			res:  ipam.ReservationSpec{Leading: 1},
			want: "unitPrefixLength",
		},
		{
			name: "more positions than one transaction should materialise",
			res:  ipam.ReservationSpec{Leading: 2000, UnitPrefixLength: 24},
			want: "leading + trailing",
		},
		{
			name: "unit size outside the family",
			res:  ipam.ReservationSpec{Leading: 1, UnitPrefixLength: 96},
			want: "must be in [1, 32]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.res

			pool := rootPoolObj("reserving-pool", "10.0.0.0/16", ipam.IPv4)
			pool.Spec.Reservations = &res
			poolErr := createPool(t, poolStore, ctx, pool)

			class := &ipam.IPClass{
				ObjectMeta: metav1.ObjectMeta{Name: "reserving-class"},
				Spec: ipam.IPClassSpec{
					IPFamily:     ipam.IPv4,
					PoolPer:      []string{"project"},
					Reservations: &res,
				},
			}
			_, classErr := classStore.Create(ctx, class, nil, &metav1.CreateOptions{})

			if !apierrors.IsInvalid(poolErr) {
				t.Fatalf("pool create returned %v, want Invalid", poolErr)
			}
			if !apierrors.IsInvalid(classErr) {
				t.Fatalf("class create returned %v, want Invalid", classErr)
			}
			for _, err := range []error{poolErr, classErr} {
				if !strings.Contains(err.Error(), "spec.reservations") {
					t.Errorf("error %q does not point at spec.reservations", err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error %q does not explain %q", err, tc.want)
				}
			}
		})
	}
}
