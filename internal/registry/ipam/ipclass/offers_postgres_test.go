package ipclass

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// A pool may name a class that does not exist yet, so the disagreement can
// arrive from the class's side. Creating the second class into a pool that
// already serves a differently divided one is the same hazard reached by
// writing the objects in the other order.
func TestClassJoiningADisagreeingPoolIsRejected(t *testing.T) {
	s := newPostgresClassStorage(t, &fakeChecker{allow: true})
	db := storageDB(t, s)
	seedClass(t, db, callerProject, "flat", nil)
	seedPool(t, db, callerProject, "shared", "flat", "per-network")

	_, err := create(t, s, classCtx(callerProject), definition("per-network", func(c *ipam.IPClass) {
		c.Spec.UniqueWithin = []string{"network"}
	}))
	if !apierrors.IsInvalid(err) {
		t.Fatalf("Create returned %v, want Invalid", err)
	}
	for _, want := range []string{"shared", "flat", "per-network", "the whole pool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The same write, into a pool whose other class divides its space the same way,
// is what the rule is meant to allow.
func TestClassJoiningAnAgreeingPoolIsAccepted(t *testing.T) {
	s := newPostgresClassStorage(t, &fakeChecker{allow: true})
	db := storageDB(t, s)
	seedClass(t, db, callerProject, "public", []string{"network"})
	seedPool(t, db, callerProject, "shared", "public", "private")

	if _, err := create(t, s, classCtx(callerProject), definition("private", func(c *ipam.IPClass) {
		c.Spec.UniqueWithin = []string{"network"}
	})); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// Discovery restricts a pool to the project holding the class definition, so a
// pool in another project never backs this class and its offers say nothing
// about what this class may declare.
func TestPoolInAnotherProjectDoesNotConstrainTheClass(t *testing.T) {
	s := newPostgresClassStorage(t, &fakeChecker{allow: true})
	db := storageDB(t, s)
	seedClass(t, db, sourceProject, "flat", nil)
	seedPool(t, db, sourceProject, "shared", "flat", "per-network")

	if _, err := create(t, s, classCtx(callerProject), definition("per-network", func(c *ipam.IPClass) {
		c.Spec.UniqueWithin = []string{"network"}
	})); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func storageDB(t *testing.T, s *IPClassStorage) *pgxpool.Pool {
	t.Helper()
	if s.db == nil {
		t.Fatal("class storage has no database handle")
	}
	return s.db
}

func seedClass(t *testing.T, db *pgxpool.Pool, project, name string, uniqueWithin []string) {
	t.Helper()
	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv6, UniqueWithin: uniqueWithin},
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

func seedPool(t *testing.T, db *pgxpool.Pool, project, name string, classNames ...string) {
	t.Helper()
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: "2001:db8::/40", IPFamily: ipamv1alpha1.IPv6, ClassNames: classNames,
		},
	}
	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool %q: %v", name, err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		tenant.Identity{Name: project}.ResourceKey("ippools", name), name, data,
	); err != nil {
		t.Fatalf("seed pool %q: %v", name, err)
	}
}
