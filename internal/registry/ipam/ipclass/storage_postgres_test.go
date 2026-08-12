package ipclass

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/access"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

const (
	callerProject = "tenant-a"
	sourceProject = "platform"
)

// fakeChecker records the class a create asked about and answers with a fixed
// decision.
type fakeChecker struct {
	allow      bool
	err        error
	gotProject string
	gotName    string
}

func (f *fakeChecker) CanUseClass(_ context.Context, project, name string) (bool, error) {
	f.gotProject, f.gotName = project, name
	return f.allow, f.err
}

// classCtx is the context a project-scoped caller arrives with.
func classCtx(project string) context.Context {
	ctx := genericapirequest.WithUser(context.Background(), &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {tenant.ParentAPIGroupProject},
			tenant.ExtraParentType:     {tenant.ParentTypeProject},
			tenant.ExtraParentName:     {project},
		},
	})
	// IPClass is cluster-scoped, but the generic store still reads the
	// namespace out of the context the endpoint handler would have set.
	return genericapirequest.WithNamespace(ctx, metav1.NamespaceNone)
}

func newPostgresClassStorage(t *testing.T, checker access.ClassAccessChecker) *IPClassStorage {
	t.Helper()
	db := testdb.Pool(t)

	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)

	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion))

	store, _, err := NewClassStorage(scheme, getter, checker, db)
	if err != nil {
		t.Fatalf("class storage: %v", err)
	}
	t.Cleanup(store.Destroy)
	return store
}

func create(t *testing.T, s *IPClassStorage, ctx context.Context, c *ipam.IPClass) (runtime.Object, error) {
	t.Helper()
	return s.Create(ctx, c, nil, &metav1.CreateOptions{})
}

// The grant is checked against the source class, in the project that holds it.
func TestCrossProjectReferenceIsCreatedWhenTheSourceAdmitsIt(t *testing.T) {
	checker := &fakeChecker{allow: true}
	s := newPostgresClassStorage(t, checker)

	obj, err := create(t, s, classCtx(callerProject), reference("ours", sourceProject, "public-unicast"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := obj.(*ipam.IPClass).Name; got != "ours" {
		t.Errorf("created %q, want %q", got, "ours")
	}
	if checker.gotProject != sourceProject || checker.gotName != "public-unicast" {
		t.Errorf("checked %q in %q, want %q in %q — the grant lives on the source class",
			checker.gotName, checker.gotProject, "public-unicast", sourceProject)
	}
}

// Without the grant the reference is refused, so no claim ever resolves through
// it into the source project's address space.
func TestCrossProjectReferenceIsRefusedWithoutTheGrant(t *testing.T) {
	s := newPostgresClassStorage(t, &fakeChecker{allow: false})

	_, err := create(t, s, classCtx(callerProject), reference("ours", sourceProject, "public-unicast"))
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Create returned %v, want Forbidden", err)
	}
	if !strings.Contains(err.Error(), "ipclasses.use") {
		t.Errorf("error %q does not name the permission the caller needs", err)
	}

	if _, err := s.Get(classCtx(callerProject), "ours", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("refused reference was persisted anyway: %v", err)
	}
}

// No authorizer means nothing can vouch for the caller, and admitting the
// reference would hand over the source project's address space regardless.
func TestCrossProjectReferenceIsRefusedWithNoChecker(t *testing.T) {
	s := newPostgresClassStorage(t, nil)

	_, err := create(t, s, classCtx(callerProject), reference("ours", sourceProject, "public-unicast"))
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Create returned %v, want Forbidden", err)
	}
}

// An authorizer that fails is not a denial: answering "no" would make an outage
// look like a revoked grant.
func TestCheckerFailureIsNotADenial(t *testing.T) {
	s := newPostgresClassStorage(t, &fakeChecker{err: errors.New("webhook unreachable")})

	_, err := create(t, s, classCtx(callerProject), reference("ours", sourceProject, "public-unicast"))
	if !apierrors.IsInternalError(err) {
		t.Fatalf("Create returned %v, want InternalError", err)
	}
}

// A class pointing at one in the caller's own project reaches nothing new, so
// it needs no grant.
func TestSameProjectReferenceNeedsNoGrant(t *testing.T) {
	checker := &fakeChecker{allow: false}
	s := newPostgresClassStorage(t, checker)

	if _, err := create(t, s, classCtx(callerProject), reference("ours", callerProject, "local")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if checker.gotName != "" {
		t.Errorf("a same-project reference issued a SubjectAccessReview for %q", checker.gotName)
	}
}

// A definition names no source and so has nothing to be authorized against.
func TestDefinitionNeedsNoGrant(t *testing.T) {
	checker := &fakeChecker{allow: false}
	s := newPostgresClassStorage(t, checker)

	if _, err := create(t, s, classCtx(callerProject), definition("ours")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if checker.gotName != "" {
		t.Errorf("a definition issued a SubjectAccessReview for %q", checker.gotName)
	}
}
