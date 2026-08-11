package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// offerPool writes a pool into a project. Publishing it to the class named in
// spec.classNames is the database's job, so nothing here writes the offer.
func offerPool(t *testing.T, tx pgx.Tx, project, name, cidr, className string, scope map[string]ipamv1alpha1.ScopeRef) string {
	t.Helper()
	family := ipamv1alpha1.IPv4
	if cidr[0] == 'f' {
		family = ipamv1alpha1.IPv6
	}
	pool := &ipamv1alpha1.IPPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR: cidr, IPFamily: family, ClassNames: []string{className}, Scope: scope,
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: family,
		},
	}
	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	key := tenant.Identity{Name: project}.ResourceKey("ippools", name)
	ctx := context.Background()
	if _, err := tx.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,'IPPool',$2,$3)`,
		key, name, data); err != nil {
		t.Fatalf("seed pool %q: %v", key, err)
	}
	return key
}

func scopeRef(kind, name string) ipamv1alpha1.ScopeRef {
	return ipamv1alpha1.ScopeRef{APIGroup: "resourcemanager.miloapis.com", Kind: kind, Name: name}
}

func claimScopeRef(kind, name string) ipam.ScopeRef {
	return ipam.ScopeRef{APIGroup: "resourcemanager.miloapis.com", Kind: kind, Name: name}
}

// A pool may only back a class in the project holding that class's definition.
// The offer table keys on class name alone, so a pool elsewhere publishing
// itself to the same name must not serve these claims.
func TestAPoolInAnotherProjectCannotBackTheClass(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "backbone", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4})
	// Same class name, a pool in somebody else's project.
	attacker := offerPool(t, tx, "attacker", "theirs", "10.9.0.0/16", "backbone", nil)

	class, err := LoadClass(ctxIn("platform"), tx, "backbone")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_, err = DiscoverPool(context.Background(), tx, class, nil)
	if !errors.Is(err, ErrNoOfferingPool) {
		t.Fatalf("DiscoverPool() error = %v, want ErrNoOfferingPool; %s must not back this class", err, attacker)
	}

	// The same pool in the definition's project does back it.
	want := offerPool(t, tx, "platform", "ours", "10.8.0.0/16", "backbone", nil)
	got, err := DiscoverPool(context.Background(), tx, class, nil)
	if err != nil {
		t.Fatalf("DiscoverPool: %v", err)
	}
	if got != want {
		t.Errorf("DiscoverPool() = %q, want %q", got, want)
	}
}

// Pools back the root of the chain, not the leaf, so an operator backs a chain
// once at the top.
func TestPoolsBackTheRootOfTheChain(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "backbone", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4})
	definition(t, tx, "platform", "subnets", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv4, ParentClassName: "backbone",
	})
	want := offerPool(t, tx, "platform", "root-pool", "10.10.0.0/16", "backbone", nil)

	leaf, err := LoadClass(ctxIn("platform"), tx, "subnets")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	got, err := DiscoverPool(context.Background(), tx, leaf, nil)
	if err != nil {
		t.Fatalf("DiscoverPool: %v", err)
	}
	if got != want {
		t.Errorf("DiscoverPool() = %q, want the pool backing the root %q", got, want)
	}
}

// A pool states the roles it is specific to; a claim must name the same object
// for each. Roles the pool leaves unstated are unconstrained.
func TestAPoolServesOnlyTheScopeItDeclares(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "endpoints", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4})
	lon := offerPool(t, tx, "platform", "lon1", "10.20.0.0/16", "endpoints",
		map[string]ipamv1alpha1.ScopeRef{"location": scopeRef("Location", "lon1")})

	class, err := LoadClass(ctxIn("platform"), tx, "endpoints")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}

	for _, tc := range []struct {
		name  string
		scope map[string]ipam.ScopeRef
		want  string
	}{
		{"matching location", map[string]ipam.ScopeRef{"location": claimScopeRef("Location", "lon1")}, lon},
		{"another location", map[string]ipam.ScopeRef{"location": claimScopeRef("Location", "fra1")}, ""},
		{"no location at all", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DiscoverPool(context.Background(), tx, class, tc.scope)
			if tc.want == "" {
				if !errors.Is(err, ErrNoOfferingPool) {
					t.Fatalf("DiscoverPool() = %q, %v; want ErrNoOfferingPool", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DiscoverPool: %v", err)
			}
			if got != tc.want {
				t.Errorf("DiscoverPool() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A pool of the wrong family never serves the class, however it is scoped.
func TestAPoolOfTheWrongFamilyDoesNotBackTheClass(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "v6-only", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv6})
	offerPool(t, tx, "platform", "a-v4-pool", "10.30.0.0/16", "v6-only", nil)

	class, err := LoadClass(ctxIn("platform"), tx, "v6-only")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	if got, err := DiscoverPool(context.Background(), tx, class, nil); !errors.Is(err, ErrNoOfferingPool) {
		t.Fatalf("DiscoverPool() = %q, %v; an IPv4 pool must not back an IPv6 class", got, err)
	}
}

// Two projects referencing one class share its pool, because a reference
// resolves to the definition and the definition's project is what backs it.
func TestTwoProjectsReferencingOneClassShareItsPool(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)

	definition(t, tx, "platform", "public-unicast", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4})
	want := offerPool(t, tx, "platform", "shared", "10.40.0.0/16", "public-unicast", nil)
	reference(t, tx, "alpha", "ours", "platform", "public-unicast", nil)
	reference(t, tx, "beta", "theirs", "platform", "public-unicast", nil)

	for project, name := range map[string]string{"alpha": "ours", "beta": "theirs"} {
		class, err := LoadClass(ctxIn(project), tx, name)
		if err != nil {
			t.Fatalf("LoadClass in %q: %v", project, err)
		}
		got, err := DiscoverPool(context.Background(), tx, class, nil)
		if err != nil {
			t.Fatalf("DiscoverPool in %q: %v", project, err)
		}
		if got != want {
			t.Errorf("project %q discovered %q, want the definition's pool %q", project, got, want)
		}
	}
}

// spec.classNames is what publishes a pool to a class, and the database keeps
// the offer table in step with it. A pool arrives by several routes — the
// registry's own Create, the generic store, the cascade — so a hook on one of
// them is a hook missing from the others.
func TestEditingClassNamesRepublishesThePool(t *testing.T) {
	pool := testdb.Pool(t)
	tx := begin(t, pool)
	ctx := context.Background()

	definition(t, tx, "platform", "first", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4})
	definition(t, tx, "platform", "second", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4})
	key := offerPool(t, tx, "platform", "movable", "10.50.0.0/16", "first", nil)

	offeredTo := func() []string {
		rows, err := tx.Query(ctx,
			`SELECT class_name FROM ipam_pool_class_offer WHERE pool_key = $1 ORDER BY class_name`, key)
		if err != nil {
			t.Fatalf("read offers: %v", err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan offer: %v", err)
			}
			names = append(names, n)
		}
		return names
	}

	if got := offeredTo(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("after create, offered to %v, want [first]", got)
	}

	// Re-point the pool at the other class, the way an update to spec does.
	if _, err := tx.Exec(ctx,
		`UPDATE ipam_objects
		    SET data = jsonb_set(ipam_data_to_jsonb(data), '{spec,classNames}', '["second"]')::text::bytea
		  WHERE key = $1`, key); err != nil {
		t.Fatalf("update classNames: %v", err)
	}

	if got := offeredTo(); len(got) != 1 || got[0] != "second" {
		t.Errorf("after update, offered to %v, want [second]; the old offer must not survive", got)
	}
}
