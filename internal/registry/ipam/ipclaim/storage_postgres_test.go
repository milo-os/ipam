package ipclaim

import (
	"context"
	"encoding/json"
	"errors"
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
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

const testProject = "datum-cloud"

// claimCtx is the context a project-scoped caller arrives with.
func claimCtx(project string) context.Context {
	ctx := genericapirequest.WithUser(context.Background(), &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {project},
		},
	})
	return genericapirequest.WithNamespace(ctx, "default")
}

// newPostgresREST wires the real allocator against a real database, and seeds
// the one class and one pool a claim needs to bind.
func newPostgresREST(t *testing.T) (*AllocatingREST, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Pool(t)
	ctx := context.Background()

	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "standard"},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24,
		},
	}
	classData, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal class: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPClass',$2,$3)`,
		tenant.Identity{Name: testProject}.ResourceKey("ipclasses", "standard"), "standard", classData); err != nil {
		t.Fatalf("seed class: %v", err)
	}

	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "us-east"},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: "10.0.0.0/16", IPFamily: ipamv1alpha1.IPv4, ClassNames: []string{"standard"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "10.0.0.0/16", IPFamily: ipamv1alpha1.IPv4,
		},
	}
	poolData, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", "us-east"), "us-east", poolData); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	return &AllocatingREST{
		allocator: allocator.NewPostgresPrefixAllocator(),
		db:        db,
		strategy:  NewStrategy(scheme),
		codec:     serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion),
	}, db
}

func newClassClaim() *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"},
		Spec:       ipam.IPClaimSpec{ClassName: "standard", IPFamily: ipam.IPv4},
	}
}

func countRows(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A claim names a class and comes back bound to the pool the allocator chose.
func TestCreateBindsAClaimThroughItsClass(t *testing.T) {
	r, _ := newPostgresREST(t)

	obj, err := r.Create(claimCtx(testProject), newClassClaim(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claim := obj.(*ipam.IPClaim)

	if claim.Status.Phase != ipam.ClaimBound {
		t.Errorf("phase = %q, want %q", claim.Status.Phase, ipam.ClaimBound)
	}
	if claim.Status.AllocatedCIDR != "10.0.0.0/24" {
		t.Errorf("allocatedCIDR = %q, want the first /24 of the pool", claim.Status.AllocatedCIDR)
	}
	// The pool is resolved, not requested, so the claim reports it on status.
	if claim.Status.PoolRef == nil || claim.Status.PoolRef.Name != "us-east" {
		t.Errorf("status.poolRef = %v, want us-east", claim.Status.PoolRef)
	}
}

// Server dry-run computes the real next CIDR and persists none of it.
func TestCreateDryRunPersistsNothing(t *testing.T) {
	r, db := newPostgresREST(t)
	ctx := claimCtx(testProject)

	obj, err := r.Create(ctx, newClassClaim(), nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != "10.0.0.0/24" {
		t.Errorf("dry run returned %q, want the CIDR a real create would take", got)
	}

	if n := countRows(t, db, `SELECT count(*) FROM ipam_cidr_allocations`); n != 0 {
		t.Errorf("dry run left %d allocation rows, want 0", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE kind = 'IPClaim'`); n != 0 {
		t.Errorf("dry run left %d claim rows, want 0", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM ipam_objects WHERE kind = 'IPAllocation'`); n != 0 {
		t.Errorf("dry run left %d allocation objects, want 0", n)
	}

	// The capacity it did not consume is still there for a real claim.
	obj, err = r.Create(ctx, newClassClaim(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create after dry run: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != "10.0.0.0/24" {
		t.Errorf("after dry run, real create took %q; the dry run consumed capacity", got)
	}
}

// Every key a claim writes carries the caller's project prefix.
func TestCreateWritesOnlyIntoTheCallersProject(t *testing.T) {
	r, db := newPostgresREST(t)

	if _, err := r.Create(claimCtx(testProject), newClassClaim(), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rows, err := db.Query(context.Background(),
		`SELECT key FROM ipam_objects WHERE kind IN ('IPClaim','IPAllocation') ORDER BY key`)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	defer rows.Close()

	prefix := "project/" + testProject + "/ipam.miloapis.com/"
	var seen int
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan key: %v", err)
		}
		seen++
		if !strings.HasPrefix(key, prefix) {
			t.Errorf("wrote %q, which is outside %q", key, prefix)
		}
	}
	if seen != 2 {
		t.Errorf("wrote %d objects, want the claim and its allocation", seen)
	}
}

// A caller with no project resolves no class, rather than reaching an
// unprefixed keyspace.
func TestCreateRefusesACallerWithNoProject(t *testing.T) {
	r, _ := newPostgresREST(t)
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	if _, err := r.Create(ctx, newClassClaim(), nil, &metav1.CreateOptions{}); err == nil {
		t.Fatal("Create succeeded for a caller carrying no project")
	}
}

// Creating the same claim name twice is a name collision, not a service
// fault: the second create is refused with a 409 that names the claim and
// carries no schema detail, so a controller creating by a stable name can
// recognise it.
func TestCreatingTheSameClaimNameTwiceConflicts(t *testing.T) {
	r, _ := newPostgresREST(t)
	ctx := claimCtx(testProject)

	if _, err := r.Create(ctx, newClassClaim(), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	_, err := r.Create(ctx, newClassClaim(), nil, &metav1.CreateOptions{})
	if err == nil {
		t.Fatal("second Create succeeded; a duplicate claim name must be refused")
	}
	var statusErr *apierrors.StatusError
	if errors.As(err, &statusErr) {
		t.Logf("second create: code=%d message=%s", statusErr.ErrStatus.Code, statusErr.ErrStatus.Message)
	} else {
		t.Logf("second create: %T %v", err, err)
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("Create returned %v (%T), want a 409 Conflict", err, err)
	}
	for _, leak := range []string{"ipam_cidr_allocations", "ipam_objects", "constraint", "SQLSTATE", "23505"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("conflict message leaks schema detail %q: %v", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "claim-1") {
		t.Errorf("conflict message does not name the claim: %v", err)
	}
}
