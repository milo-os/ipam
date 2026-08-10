package allocator

// Discovery under the platform-as-a-project model.
//
// Two properties meet here and pull against each other, which is why they are
// tested together rather than in separate files:
//
//   - Discovery must reach pools outside the old unprefixed root, because there
//     is no unprefixed root. Before this change a tenant could not be served by
//     a pool in the platform's project, nor by one in their own.
//   - Discovery must NOT reach every project's pools, because spec.classNames is
//     a pool volunteering itself. Without a consenting statement from the class,
//     any tenant who can create an IPPool could list a popular class name on it
//     and start receiving other tenants' claims.
//
// The first without the second is the vulnerability the old `key LIKE` predicate
// was protecting against by accident. The second without the first is the bug.

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/tenant"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// seedFlatClass writes a leaf class with no parent — the flat IPv4 endpoint
// shape — so discovery is the only thing under test and no cascade runs.
func seedFlatClass(t *testing.T, db *pgxpool.Pool, name string, backingProjects []string) *ipamv1alpha1.IPClass {
	t.Helper()
	class := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily:             ipamv1alpha1.IPv4,
			AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 32, Max: 32},
			BackingProjects:      backingProjects,
		},
	}
	seedObject(t, db, platformKey("ipclasses", name), "IPClass", name, class)
	return class
}

// seedPoolIn writes an operator-authored pool into a named project, offering
// the given class. project "" means the platform project.
func seedPoolIn(t *testing.T, db *pgxpool.Pool, project, name, cidr, className string) string {
	t.Helper()
	if project == "" {
		project = testPlatformProject
	}
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:       cidr,
			IPFamily:   ipamv1alpha1.IPv4,
			ClassNames: []string{className},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: cidr,
			IPFamily:      ipamv1alpha1.IPv4,
		},
	}
	key := tenant.Identity{Name: project}.ResourceKey("ippools", name)
	seedObject(t, db, key, "IPPool", name, pool)
	return key
}

func discover(t *testing.T, db *pgxpool.Pool, class *ipamv1alpha1.IPClass) (string, error) {
	t.Helper()
	ctx := platformCtx()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return DiscoverPool(ctx, tx, class, nil)
}

// The bug this change exists to fix. The platform's own pools live in the
// platform's project, and the old predicate — `key LIKE
// '/ipam.miloapis.com/ippools/%'` — matched only the unprefixed root, which no
// request through Milo's front gate can write to. Every claim failed with "no
// pool offers this class" while the pool sat there offering it.
func TestDiscoveryFindsAPoolInThePlatformProject(t *testing.T) {
	db := newMigratedPool(t)
	class := seedFlatClass(t, db, "flat-v4", nil)
	want := seedPoolIn(t, db, testPlatformProject, "platform-v4", "10.40.0.0/16", "flat-v4")

	got, err := discover(t, db, class)
	if err != nil {
		t.Fatalf("DiscoverPool: %v", err)
	}
	if got != want {
		t.Fatalf("discovered %q, want the platform project's pool %q", got, want)
	}
}

// The consent rule, in the direction that permits. A class naming a project in
// spec.backingProjects has said that project's pools may back it, and discovery
// must honour that — otherwise "tenants can consume out of each other's
// projects" is not expressible at all.
func TestDiscoveryFindsAPoolInAConsentingProject(t *testing.T) {
	db := newMigratedPool(t)
	class := seedFlatClass(t, db, "flat-v4", []string{"project-alpha"})
	want := seedPoolIn(t, db, "project-alpha", "alpha-v4", "10.41.0.0/16", "flat-v4")

	got, err := discover(t, db, class)
	if err != nil {
		t.Fatalf("DiscoverPool: %v", err)
	}
	if got != want {
		t.Fatalf("discovered %q, want the consenting project's pool %q", got, want)
	}
}

// The consent rule, in the direction that matters.
//
// This is the attack the old DiscoverPool comment describes, made reachable by
// removing the key predicate: a tenant creates an IPPool in their own project,
// lists a popular class name on it, and starts receiving other tenants' claims —
// learning that each claim happened, choosing the address it received, and
// holding the range it came from.
//
// Note the pool is otherwise perfectly eligible: right family, no scope
// constraints, offer row present. Only the absence of consent keeps it out, so a
// pass here cannot be explained by any of the other filters.
func TestDiscoveryIgnoresAPoolInANonConsentingProject(t *testing.T) {
	db := newMigratedPool(t)
	class := seedFlatClass(t, db, "flat-v4", nil)
	seedPoolIn(t, db, "attacker-project", "squatter-v4", "10.42.0.0/16", "flat-v4")

	// Prove the pool really did publish its offer, so this test is showing
	// consent doing the work rather than a fixture that never registered.
	if n := countRows(t, db,
		`SELECT count(*) FROM ipam_pool_class_offer WHERE class_name = 'flat-v4'`); n != 1 {
		t.Fatalf("offer rows for flat-v4 = %d, want 1: the squatting pool must be a real candidate for this test to mean anything", n)
	}

	if got, err := discover(t, db, class); err == nil {
		t.Fatalf("discovered %q: a pool in a project the class does not consent to must not back it", got)
	}
}

// Consent is revocable, and that is the whole reason the check is applied at
// read time rather than only in validateClassOffers at pool-write time.
//
// The pool here was legitimately consented to when it was written. Removing the
// project from spec.backingProjects must stop it serving immediately — under a
// write-time-only rule it would keep serving forever, because nothing revisits
// a decision made at CREATE.
func TestRevokingConsentStopsAPoolServing(t *testing.T) {
	db := newMigratedPool(t)
	class := seedFlatClass(t, db, "flat-v4", []string{"project-alpha"})
	want := seedPoolIn(t, db, "project-alpha", "alpha-v4", "10.43.0.0/16", "flat-v4")

	got, err := discover(t, db, class)
	if err != nil {
		t.Fatalf("DiscoverPool before revocation: %v", err)
	}
	if got != want {
		t.Fatalf("before revocation: discovered %q, want %q", got, want)
	}

	class.Spec.BackingProjects = nil
	reseedClass(t, db, class)

	if got, err := discover(t, db, class); err == nil {
		t.Fatalf("discovered %q after consent was revoked; the pool must stop serving", got)
	}
}

// A class the platform backs keeps working with no backingProjects at all.
//
// This is what makes the field additive rather than a flag day: every class in
// an existing catalog has an empty list and is backed by platform-authored
// pools, so requiring the platform project to be listed would break all of them
// at once.
func TestThePlatformProjectNeedNotBeListed(t *testing.T) {
	db := newMigratedPool(t)
	class := seedFlatClass(t, db, "flat-v4", []string{"project-alpha"})
	want := seedPoolIn(t, db, testPlatformProject, "platform-v4", "10.44.0.0/16", "flat-v4")
	// A consenting project exists too, and its pool sorts after the platform's,
	// so FirstFit picks the platform pool — the assertion is that the platform's
	// is a candidate at all, not which one wins.
	seedPoolIn(t, db, "project-alpha", "zzz-v4", "10.45.0.0/16", "flat-v4")

	got, err := discover(t, db, class)
	if err != nil {
		t.Fatalf("DiscoverPool: %v", err)
	}
	if got != want {
		t.Fatalf("discovered %q, want the platform project's pool %q", got, want)
	}
}

// LoadDefaultClass had no key predicate at all: it returned the first IPClass
// anywhere in the database carrying the default marker for a family.
//
// A project able to create an IPClass in its own key space could therefore
// decide what every other project's unqualified claim allocates from, and could
// win the ORDER BY by choosing a name that sorts first — which "acme" does
// against the platform project's key.
func TestDefaultClassComesFromThePlatformCatalogOnly(t *testing.T) {
	db := newMigratedPool(t)

	marked := func(name string) *ipamv1alpha1.IPClass {
		return &ipamv1alpha1.IPClass{
			TypeMeta: metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Annotations: map[string]string{ipamv1alpha1.IsDefaultClassAnnotation: "true"},
			},
			Spec: ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4},
		}
	}

	// The platform's real default.
	platformDefault := marked("platform-default-v4")
	seedObject(t, db, platformKey("ipclasses", platformDefault.Name), "IPClass", platformDefault.Name, platformDefault)

	// A tenant's own class, also marked default, in a project whose key sorts
	// before the platform's. Under the old query this one wins.
	impostor := marked("aaa-impostor-v4")
	seedObject(t, db,
		tenant.Identity{Name: "aaa-tenant"}.ResourceKey("ipclasses", impostor.Name),
		"IPClass", impostor.Name, impostor)

	ctx := platformCtx()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	got, err := LoadDefaultClass(ctx, tx, ipamv1alpha1.IPv4)
	if err != nil {
		t.Fatalf("LoadDefaultClass: %v", err)
	}
	if got.Name != platformDefault.Name {
		t.Fatalf("default class = %q, want %q: the catalog is platform policy and the query must say so",
			got.Name, platformDefault.Name)
	}
}

// The sub-case above cannot reach, and the one a fresh cluster is actually in.
//
// TestDefaultClassComesFromThePlatformCatalogOnly seeds a platform default and
// an impostor and asserts the platform one wins. That is a test about ORDER BY
// resolving a contest, and it leaves the state where there is no contest
// untested: **a cluster with no default-annotated IPClass at all**. Every fresh
// cluster is in that state until an operator marks one, and it is the state the
// exploit for #56 was actually demonstrated in — with no platform candidate,
// ORDER BY is never consulted and a tenant's class is the only row the query
// can return.
//
// So the two tests fail against different defects. A query that scoped nothing
// but ordered correctly passes here and fails above; a query that scoped
// nothing would pass above if the platform project's name happened to sort
// first, and fails here regardless of names. The property this one asserts is
// the stronger of the two: no tenant object is a candidate, rather than no
// tenant object outranks the platform's.
//
// The correct answer is ErrNoDefaultClass. "This family has no default class"
// is a claim the caller can act on; being handed a stranger's class is not
// something they can even see.
func TestNoPlatformDefaultMeansNoDefaultEvenIfATenantMarkedOne(t *testing.T) {
	db := newMigratedPool(t)

	// Deliberately no platform default. The only default-annotated IPv4 class
	// in the database belongs to a tenant.
	impostor := &ipamv1alpha1.IPClass{
		TypeMeta: metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "audit-evil-default-v4",
			Annotations: map[string]string{ipamv1alpha1.IsDefaultClassAnnotation: "true"},
		},
		Spec: ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4},
	}
	seedObject(t, db,
		tenant.Identity{Name: "acme"}.ResourceKey("ipclasses", impostor.Name),
		"IPClass", impostor.Name, impostor)

	// Positive control on the fixture. Without this the test passes against a
	// database where the seed silently did nothing, which is indistinguishable
	// from the guard working.
	var marked int
	if err := db.QueryRow(platformCtx(),
		`SELECT count(*) FROM ipam_objects WHERE kind = 'IPClass'`).Scan(&marked); err != nil {
		t.Fatalf("count classes: %v", err)
	}
	if marked != 1 {
		t.Fatalf("seeded classes = %d, want 1: the tenant's default-marked class must exist, "+
			"or ErrNoDefaultClass below proves nothing", marked)
	}

	ctx := platformCtx()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	got, err := LoadDefaultClass(ctx, tx, ipamv1alpha1.IPv4)
	if err == nil {
		t.Fatalf("LoadDefaultClass returned %q, a class in project \"acme\"; with no platform "+
			"default the answer is ErrNoDefaultClass, not whatever a tenant marked. This is the "+
			"state every fresh cluster is in, so a missing key predicate is reachable there "+
			"without any tenant having to win an ORDER BY", got.Name)
	}
	if !errors.Is(err, ErrNoDefaultClass) {
		t.Fatalf("error = %v, want ErrNoDefaultClass: the caller has to be able to tell "+
			"\"no default for this family\" from a lookup failure", err)
	}
}

// A class named by a tenant must resolve to the platform's copy, not to one the
// tenant wrote in their own key space under the same name.
//
// LoadClass reads by exact key, so this is really a test that the key is built
// from the platform project rather than from the caller — but the failure it
// guards against is a tenant shadowing a platform class name and having their
// own policy applied to their claims.
func TestClassLookupIgnoresATenantsSameNamedClass(t *testing.T) {
	db := newMigratedPool(t)

	platformCopy := seedFlatClass(t, db, "shadowed-v4", nil)
	platformCopy.Spec.DefaultPrefixLength = 32

	shadow := &ipamv1alpha1.IPClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "shadowed-v4"},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily: ipamv1alpha1.IPv4,
			// A visibility the platform copy does not grant, so a lookup that
			// returned this one would be handing out different policy.
			Visibility:      ipamv1alpha1.VisibilityShared,
			BackingProjects: []string{"acme"},
		},
	}
	seedObject(t, db,
		tenant.Identity{Name: "acme"}.ResourceKey("ipclasses", shadow.Name),
		"IPClass", shadow.Name, shadow)

	ctx := platformCtx()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	got, err := LoadClass(ctx, tx, "shadowed-v4")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	if got.Spec.Visibility == ipamv1alpha1.VisibilityShared || len(got.Spec.BackingProjects) != 0 {
		t.Fatalf("LoadClass returned a tenant's shadowing copy: visibility=%q backingProjects=%v",
			got.Spec.Visibility, got.Spec.BackingProjects)
	}
}

// Without a configured platform project there is nowhere for the catalog to
// live, so every platform-owned lookup fails with a message naming the flag
// rather than silently reading an empty keyspace.
func TestLookupsFailClosedWithNoPlatformProject(t *testing.T) {
	db := newMigratedPool(t)
	class := seedFlatClass(t, db, "flat-v4", nil)
	seedPoolIn(t, db, testPlatformProject, "platform-v4", "10.46.0.0/16", "flat-v4")

	// Deliberately not platformCtx(): this is a server started without the flag.
	ctx := t.Context()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := LoadClass(ctx, tx, "flat-v4"); err == nil {
		t.Error("LoadClass succeeded with no platform project configured")
	}
	if _, err := LoadDefaultClass(ctx, tx, ipamv1alpha1.IPv4); err == nil {
		t.Error("LoadDefaultClass succeeded with no platform project configured")
	}
	if _, err := DiscoverPool(ctx, tx, class, nil); err == nil {
		t.Error("DiscoverPool succeeded with no platform project configured")
	}
}
