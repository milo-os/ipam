package allocator

// These run against a real Postgres: the property under test is which key a
// name lands under, which a fake transaction cannot show.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestMain(m *testing.M) { testdb.TestMain(m) }

// ctxIn builds the request context a caller in the named project arrives with.
func ctxIn(project string) context.Context {
	return request.WithUser(context.Background(), &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {project},
		},
	})
}

// definition writes a class that holds policy of its own.
func definition(t *testing.T, tx pgx.Tx, project, name string, spec ipamv1alpha1.IPClassSpec) {
	t.Helper()
	writeClass(t, tx, project, &ipamv1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	})
}

// reference writes a class that points at one in another project.
func reference(t *testing.T, tx pgx.Tx, project, name, srcProject, srcName string, annotations map[string]string) {
	t.Helper()
	writeClass(t, tx, project, &ipamv1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec:       ipamv1alpha1.IPClassSpec{Source: &ipamv1alpha1.ClassSourceRef{Project: srcProject, Name: srcName}},
	})
}

func writeClass(t *testing.T, tx pgx.Tx, project string, class *ipamv1alpha1.IPClass) {
	t.Helper()
	data, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal class %q: %v", class.Name, err)
	}
	_, err = tx.Exec(context.Background(),
		`INSERT INTO ipam_objects (key, kind, name, data)
		 VALUES ($1, 'IPClass', $2, $3)`,
		classStorageKey(project, class.Name), class.Name, data,
	)
	if err != nil {
		t.Fatalf("insert class %q in %q: %v", class.Name, project, err)
	}
}

func begin(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

const v6 = ipamv1alpha1.IPv6

// Nothing but the sender selects between them.
func TestOneNameInTwoProjectsIsTwoClasses(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "alpha", "standard", ipamv1alpha1.IPClassSpec{IPFamily: v6, DefaultPrefixLength: 64})
	definition(t, tx, "beta", "standard", ipamv1alpha1.IPClassSpec{IPFamily: v6, DefaultPrefixLength: 96})

	for project, want := range map[string]int32{"alpha": 64, "beta": 96} {
		got, err := LoadClass(ctxIn(project), tx, "standard")
		if err != nil {
			t.Fatalf("LoadClass in %q: %v", project, err)
		}
		if got.Project != project {
			t.Errorf("resolved into project %q, want %q", got.Project, project)
		}
		if got.Spec.DefaultPrefixLength != want {
			t.Errorf("in %q got the class with defaultPrefixLength %d, want %d",
				project, got.Spec.DefaultPrefixLength, want)
		}
	}
}

// A reference reports the definition's project, which is what makes two
// projects consuming one class share its pool and address space.
func TestAReferenceResolvesToTheDefinitionsProject(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "public-unicast", ipamv1alpha1.IPClassSpec{IPFamily: v6, DefaultPrefixLength: 64})
	reference(t, tx, "alpha", "our-addresses", "platform", "public-unicast", nil)
	reference(t, tx, "beta", "public-unicast", "platform", "public-unicast", nil)

	first, err := LoadClass(ctxIn("alpha"), tx, "our-addresses")
	if err != nil {
		t.Fatalf("LoadClass in alpha: %v", err)
	}
	second, err := LoadClass(ctxIn("beta"), tx, "public-unicast")
	if err != nil {
		t.Fatalf("LoadClass in beta: %v", err)
	}

	// Different names in each project, one identity.
	for _, got := range []*ResolvedClass{first, second} {
		if got.Project != "platform" || got.Name != "public-unicast" {
			t.Errorf("resolved to %q in %q, want %q in %q",
				got.Name, got.Project, "public-unicast", "platform")
		}
		if got.Spec.DefaultPrefixLength != 64 {
			t.Errorf("defaultPrefixLength = %d, want the definition's 64", got.Spec.DefaultPrefixLength)
		}
	}
}

// A chain would let the project at the far end change which class a claim
// allocates under.
func TestAReferenceToAReferenceIsRefused(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "public-unicast", ipamv1alpha1.IPClassSpec{IPFamily: v6})
	reference(t, tx, "beta", "public-unicast", "platform", "public-unicast", nil)
	reference(t, tx, "alpha", "public-unicast", "beta", "public-unicast", nil)

	_, err := LoadClass(ctxIn("alpha"), tx, "public-unicast")
	if !errors.Is(err, ErrChainedReference) {
		t.Fatalf("LoadClass() error = %v, want ErrChainedReference", err)
	}
}

func TestAReferenceToAMissingClassIsNotFound(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	reference(t, tx, "alpha", "our-addresses", "platform", "public-unicast", nil)

	_, err := LoadClass(ctxIn("alpha"), tx, "our-addresses")
	if !errors.Is(err, ErrClassNotFound) {
		t.Fatalf("LoadClass() error = %v, want ErrClassNotFound", err)
	}
}

// A caller with no project resolves nothing, rather than seeing every class.
func TestAnUntenantedCallerResolvesNothing(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "public-unicast", ipamv1alpha1.IPClassSpec{IPFamily: v6})

	ctx := request.WithUser(context.Background(), &user.DefaultInfo{Name: "someone"})
	if _, err := LoadClass(ctx, tx, "public-unicast"); !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("LoadClass() error = %v, want ErrNoTenant", err)
	}
}

// The family lives on the definition, because a reference states none.
func TestTheDefaultClassMatchesFamilyThroughAReference(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "v4-endpoints", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4})
	definition(t, tx, "platform", "v6-subnets", ipamv1alpha1.IPClassSpec{IPFamily: v6})
	isDefault := map[string]string{ipamv1alpha1.IsDefaultClassAnnotation: "true"}
	// The IPv4 class sorts first, so the family filter must skip it.
	reference(t, tx, "alpha", "a-legacy", "platform", "v4-endpoints", isDefault)
	reference(t, tx, "alpha", "z-standard", "platform", "v6-subnets", isDefault)

	got, err := LoadDefaultClass(ctxIn("alpha"), tx, v6)
	if err != nil {
		t.Fatalf("LoadDefaultClass: %v", err)
	}
	if got.Name != "v6-subnets" || got.Project != "platform" {
		t.Errorf("default = %q in %q, want %q in %q", got.Name, got.Project, "v6-subnets", "platform")
	}
}

func TestADefaultInAnotherProjectIsNotThisProjectsDefault(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "aaa-attacker", "mine", ipamv1alpha1.IPClassSpec{IPFamily: v6})
	writeClass(t, tx, "aaa-attacker", &ipamv1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "mine-default",
			Annotations: map[string]string{ipamv1alpha1.IsDefaultClassAnnotation: "true"},
		},
		Spec: ipamv1alpha1.IPClassSpec{IPFamily: v6},
	})

	_, err := LoadDefaultClass(ctxIn("alpha"), tx, v6)
	if !errors.Is(err, ErrNoDefaultClass) {
		t.Fatalf("LoadDefaultClass() error = %v, want ErrNoDefaultClass", err)
	}
}

// The chain continues in the definition's project, not the caller's.
func TestAncestryWalksTheDefinitionsProjectNotTheCallers(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "backbone", ipamv1alpha1.IPClassSpec{IPFamily: v6, DefaultPrefixLength: 48})
	definition(t, tx, "platform", "subnets", ipamv1alpha1.IPClassSpec{
		IPFamily: v6, ParentClassName: "backbone", DefaultPrefixLength: 64,
	})
	reference(t, tx, "alpha", "subnets", "platform", "subnets", nil)
	// A decoy: the real parent's name, in the consuming project.
	definition(t, tx, "alpha", "backbone", ipamv1alpha1.IPClassSpec{IPFamily: v6, DefaultPrefixLength: 120})

	leaf, err := LoadClass(ctxIn("alpha"), tx, "subnets")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	chain, err := LoadAncestry(ctxIn("alpha"), tx, leaf)
	if err != nil {
		t.Fatalf("LoadAncestry: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("ancestry has %d levels, want 1", len(chain))
	}
	if chain[0].Project != "platform" {
		t.Errorf("parent resolved in project %q, want %q", chain[0].Project, "platform")
	}
	if chain[0].Spec.DefaultPrefixLength != 48 {
		t.Errorf("parent defaultPrefixLength = %d, want the platform class's 48 (the decoy is 120)",
			chain[0].Spec.DefaultPrefixLength)
	}
}

func TestAClassWithNoParentHasNoAncestry(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "alpha", "flat", ipamv1alpha1.IPClassSpec{IPFamily: v6})

	leaf, err := LoadClass(ctxIn("alpha"), tx, "flat")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	chain, err := LoadAncestry(ctxIn("alpha"), tx, leaf)
	if err != nil {
		t.Fatalf("LoadAncestry: %v", err)
	}
	if len(chain) != 0 {
		t.Errorf("ancestry has %d levels, want 0", len(chain))
	}
}

// The registry rejects a cycle at write time; this is the backstop.
func TestACycleInTheParentChainTerminates(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "alpha", "a", ipamv1alpha1.IPClassSpec{IPFamily: v6, ParentClassName: "b"})
	definition(t, tx, "alpha", "b", ipamv1alpha1.IPClassSpec{IPFamily: v6, ParentClassName: "a"})

	leaf, err := LoadClass(ctxIn("alpha"), tx, "a")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	if _, err := LoadAncestry(ctxIn("alpha"), tx, leaf); !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("LoadAncestry() error = %v, want ErrChainTooDeep", err)
	}
}

func TestTheSameNameInTwoProjectsIsNotACycle(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "space", ipamv1alpha1.IPClassSpec{IPFamily: v6, DefaultPrefixLength: 48})
	definition(t, tx, "alpha", "space", ipamv1alpha1.IPClassSpec{IPFamily: v6, ParentClassName: "parent"})
	reference(t, tx, "alpha", "parent", "platform", "space", nil)

	leaf, err := LoadClass(ctxIn("alpha"), tx, "space")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	chain, err := LoadAncestry(ctxIn("alpha"), tx, leaf)
	if err != nil {
		t.Fatalf("LoadAncestry: %v", err)
	}
	if len(chain) != 1 || chain[0].Project != "platform" {
		t.Fatalf("ancestry = %d levels ending in %v, want 1 level in platform", len(chain), chain)
	}
}
