package allocator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// perTenantChain seeds the shape #114 was reported against: one platform class
// carving per network, referenced by two projects, each of which names a
// network `default`. perConsumer decides which declaration the provisioning
// class makes; there is no third option, which is the point.
func perTenantChain(t *testing.T, db *pgxpool.Pool, perConsumer bool) {
	t.Helper()
	ctx := context.Background()

	poolPer := []string{"network", scope.ReservedRoleAllProjects}
	if perConsumer {
		poolPer = []string{"network", scope.ReservedRoleProject}
	}

	tx := begin(t, db)
	definition(t, tx, "platform", "tenant-ipv6", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, DefaultPrefixLength: 48, PoolPer: poolPer,
	})
	definition(t, tx, "platform", "tenant-subnet", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-ipv6", DefaultPrefixLength: 64,
	})
	// Both consumers reach the definition through a reference, which is what
	// makes ResolvedClass.Project the PLATFORM project for both of them — the
	// input that used to collapse the two identities into one.
	reference(t, tx, "projx", "tenant-subnet", "platform", "tenant-subnet", nil)
	reference(t, tx, "projy", "tenant-subnet", "platform", "tenant-subnet", nil)
	offerPool(t, tx, "platform", "root", "fd00::/32", "tenant-ipv6", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}
}

// resolveFor resolves the pool a claim from one project would draw from.
func resolveFor(t *testing.T, db *pgxpool.Pool, project string, claimScope map[string]ipam.ScopeRef) string {
	t.Helper()
	ctx := ctxIn(project)

	read := begin(t, db)
	leaf, err := LoadClass(ctx, read, "tenant-subnet")
	if err != nil {
		t.Fatalf("LoadClass in %q: %v", project, err)
	}
	_ = read.Rollback(context.Background())

	poolKey, err := ResolvePool(ctx, db, leaf, claimScope)
	if err != nil {
		t.Fatalf("ResolvePool for %q: %v", project, err)
	}
	return poolKey
}

// TestTwoConsumersOfOnePerConsumerClassGetTwoPools is #114, as a regression
// test.
//
// Two projects reference one platform class. Each names a network `default`.
// Before the fix the two claims derived one pool digest — the scope refs were
// identical and the digest folded in the DEFINING project, which is the same
// platform project for both — so the second consumer lost the
// ipam_pool_identity race, read the first's pool_key, and allocated out of it.
// One /48 backed two tenants' networks, with no error and nothing to see.
func TestTwoConsumersOfOnePerConsumerClassGetTwoPools(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	perTenantChain(t, db, true)

	claimScope := map[string]ipam.ScopeRef{"network": claimScopeRef("Network", "default")}
	x := resolveFor(t, db, "projx", claimScope)
	y := resolveFor(t, db, "projy", claimScope)

	if x == y {
		t.Fatalf("both projects resolved %q; a class naming %q in poolPer must carve per consumer",
			x, scope.ReservedRoleProject)
	}

	// Two identities, two pools, and — the part that matters to a tenant — two
	// disjoint ranges. Equal keys would have been caught above; equal CIDRs
	// under different keys would be the same failure wearing a disguise.
	var xCIDR, yCIDR string
	for _, tc := range []struct {
		key string
		out *string
	}{{x, &xCIDR}, {y, &yCIDR}} {
		if err := db.QueryRow(ctx,
			`SELECT ipam_data_to_jsonb(data) -> 'status' ->> 'allocatedCIDR'
			   FROM ipam_objects WHERE key = $1`, tc.key).Scan(tc.out); err != nil {
			t.Fatalf("read pool %q: %v", tc.key, err)
		}
	}
	if xCIDR == yCIDR {
		t.Errorf("two pools hold the same range %q", xCIDR)
	}

	// The parent's carve rows attribute the space to the consumer that caused
	// it, not to the project holding the class. This is the column per-project
	// consumption is reported from.
	for project, key := range map[string]string{"projx": x, "projy": y} {
		var owner string
		if err := db.QueryRow(ctx,
			`SELECT owner_project FROM ipam_cidr_allocations WHERE allocation_key = $1`,
			// The /48's carve sits against the root; the /64 pool below it is
			// provisioned by the leaf class, which declares no poolPer, so the
			// key here is the /48 itself.
			key).Scan(&owner); err != nil {
			t.Fatalf("read carve for %q: %v", key, err)
		}
		if owner != project {
			t.Errorf("carve for %s attributed to %q, want %q", key, owner, project)
		}
	}

	// The pool a consumer owns is selectable without decoding any spec.
	var label string
	if err := db.QueryRow(ctx,
		`SELECT ipam_data_to_jsonb(data) -> 'metadata' -> 'labels' ->> 'ipam.miloapis.com/provisioned-for'
		   FROM ipam_objects WHERE key = $1`, x).Scan(&label); err != nil {
		t.Fatalf("read provisioned-for: %v", err)
	}
	if label != "projx" {
		t.Errorf("provisioned-for = %q, want projx", label)
	}
}

// TestTwoConsumersOfASharedClassGetOnePool is the guard against over-correcting
// #114, and it is the case the fix must not break.
//
// A class that names `allProjects` provisions one pool for every consumer. That is what an announceable public block requires: per-consumer
// blocks would exhaust the aggregate after one block per project rather than
// one per location, and a project with one instance would burn a whole /24.
//
// Before the fix this worked by accident — the digest folded in the defining
// project, which happened to be the same for both callers. It must now work on
// purpose.
func TestTwoConsumersOfASharedClassGetOnePool(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()
	perTenantChain(t, db, false)

	claimScope := map[string]ipam.ScopeRef{"network": claimScopeRef("Network", "default")}
	x := resolveFor(t, db, "projx", claimScope)
	y := resolveFor(t, db, "projy", claimScope)

	if x != y {
		t.Fatalf("a class naming %q provisioned two pools, %q and %q",
			scope.ReservedRoleAllProjects, x, y)
	}

	// A shared pool carries no consumer label. Its absence is the fact an
	// operator reads: this pool belongs to no one consumer.
	var label *string
	if err := db.QueryRow(ctx,
		`SELECT ipam_data_to_jsonb(data) -> 'metadata' -> 'labels' ->> 'ipam.miloapis.com/provisioned-for'
		   FROM ipam_objects WHERE key = $1`, x).Scan(&label); err != nil {
		t.Fatalf("read provisioned-for: %v", err)
	}
	if label != nil {
		t.Errorf("shared pool labelled provisioned-for=%q; the label must mark per-consumer pools only", *label)
	}

	// And its carve is attributed to the project holding the class, because no
	// single consumer caused it.
	var owner string
	if err := db.QueryRow(ctx,
		`SELECT owner_project FROM ipam_cidr_allocations WHERE allocation_key = $1`, x).Scan(&owner); err != nil {
		t.Fatalf("read carve: %v", err)
	}
	if owner != "platform" {
		t.Errorf("shared carve attributed to %q, want the defining project", owner)
	}
}

// TestAHerdFromTwoConsumersProducesTwoPools is the concurrency half: the
// per-consumer split must be decided by the identity row, not by whichever
// caller happens to arrive first.
//
// Sixteen simultaneous first claims from two projects must produce exactly two
// pools and no errors. A digest that ignored the consumer would produce one; a
// key that varied per caller would produce sixteen, or a lost-race error.
func TestAHerdFromTwoConsumersProducesTwoPools(t *testing.T) {
	db := testdb.Pool(t, testdb.MaxConns(24))
	ctx := context.Background()
	perTenantChain(t, db, true)

	claimScope := map[string]ipam.ScopeRef{"network": claimScopeRef("Network", "default")}
	projects := []string{"projx", "projy"}

	leaves := map[string]*ResolvedClass{}
	for _, project := range projects {
		read := begin(t, db)
		leaf, err := LoadClass(ctxIn(project), read, "tenant-subnet")
		if err != nil {
			t.Fatalf("LoadClass in %q: %v", project, err)
		}
		_ = read.Rollback(ctx)
		leaves[project] = leaf
	}

	const herd = 16
	keys := make([]string, herd)
	errs := make([]error, herd)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range herd {
		project := projects[i%len(projects)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys[i], errs[i] = ResolvePool(ctxIn(project), db, leaves[project], claimScope)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d (%s): %v", i, projects[i%len(projects)], err)
		}
	}
	// Every caller from one project must agree, and the two projects must
	// disagree.
	for i, k := range keys {
		if want := keys[i%len(projects)]; k != want {
			t.Errorf("caller %d resolved %q, want %q", i, k, want)
		}
	}
	if keys[0] == keys[1] {
		t.Errorf("both projects resolved %q under contention", keys[0])
	}

	var pools int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM ipam_objects
		  WHERE kind = 'IPPool'
		    AND ipam_data_to_jsonb(data) -> 'metadata' -> 'labels' ->> 'ipam.miloapis.com/provisioned-by' = 'tenant-ipv6'`,
	).Scan(&pools); err != nil {
		t.Fatalf("count provisioned pools: %v", err)
	}
	if pools != 2 {
		t.Errorf("%d callers from 2 projects provisioned %d pools, want 2", herd, pools)
	}
}

// TestOneConsumerOfTwoSameNamedClassesGetsTwoPools is the primary-key case the
// Owner half of PoolTenancy exists for.
//
// ipam_pool_identity is keyed on (class_name, scope_digest), and class names
// are project-scoped: two projects may each define a class called
// `tenant-ipv6`. Had the fix SWAPPED the defining project for the consuming one
// — the issue's literal suggestion — one consumer referencing both would
// derive one digest for both class names' rows and merge them into a single
// pool. That is a strictly worse version of the bug, so the fix adds a field.
func TestOneConsumerOfTwoSameNamedClassesGetsTwoPools(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	for _, owner := range []string{"platform-a", "platform-b"} {
		definition(t, tx, owner, "tenant-ipv6", ipamv1alpha1.IPClassSpec{
			IPFamily: ipamv1alpha1.IPv6, DefaultPrefixLength: 48,
			PoolPer: []string{"network", scope.ReservedRoleProject},
		})
		definition(t, tx, owner, "tenant-subnet", ipamv1alpha1.IPClassSpec{
			IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-ipv6", DefaultPrefixLength: 64,
		})
		offerPool(t, tx, owner, "root-"+owner, "fd00::/32", "tenant-ipv6", nil)
	}
	// One consumer, two references, one leaf name each so they can be told
	// apart from the caller's side.
	reference(t, tx, "projx", "from-a", "platform-a", "tenant-subnet", nil)
	reference(t, tx, "projx", "from-b", "platform-b", "tenant-subnet", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	claimScope := map[string]ipam.ScopeRef{"network": claimScopeRef("Network", "default")}
	keys := map[string]string{}
	for _, name := range []string{"from-a", "from-b"} {
		read := begin(t, db)
		leaf, err := LoadClass(ctxIn("projx"), read, name)
		if err != nil {
			t.Fatalf("LoadClass %q: %v", name, err)
		}
		_ = read.Rollback(ctx)

		key, err := ResolvePool(ctxIn("projx"), db, leaf, claimScope)
		if err != nil {
			t.Fatalf("ResolvePool %q: %v", name, err)
		}
		keys[name] = key
	}

	if keys["from-a"] == keys["from-b"] {
		t.Fatalf("one consumer's claims against two same-named classes merged into %q", keys["from-a"])
	}
}

// TestDryRunAgreesWithResolutionForBothShapes covers ResolveExistingPool, which
// derives the same identity by a different path and would otherwise be free to
// disagree about the consumer.
//
// Dry-run must report the pending levels before anything exists and the same
// pool afterwards, for a per-consumer class and a shared one alike. A dry-run
// that reported the shared pool for a per-consumer claim would tell a caller it
// was about to get somebody else's prefix.
func TestDryRunAgreesWithResolutionForBothShapes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		perConsumer bool
	}{
		{"per-consumer class", true},
		{"shared class", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Pool(t)
			ctx := ctxIn("projx")
			perTenantChain(t, db, tc.perConsumer)

			claimScope := map[string]ipam.ScopeRef{"network": claimScopeRef("Network", "default")}

			read := begin(t, db)
			leaf, err := LoadClass(ctx, read, "tenant-subnet")
			if err != nil {
				t.Fatalf("LoadClass: %v", err)
			}
			_ = read.Rollback(context.Background())

			// Nothing provisioned yet: the pool is unknown and every level is
			// pending.
			key, pending, err := ResolveExistingPool(ctx, db, leaf, claimScope)
			if err != nil {
				t.Fatalf("ResolveExistingPool before provisioning: %v", err)
			}
			if key != "" {
				t.Errorf("dry run reported pool %q before anything was provisioned", key)
			}
			if len(pending) == 0 {
				t.Error("dry run reported no pending levels for an unprovisioned chain")
			}
			wantKey := pending[0].PoolKey

			// And it provisioned nothing while saying so.
			var identities int
			if err := db.QueryRow(context.Background(),
				`SELECT count(*) FROM ipam_pool_identity`).Scan(&identities); err != nil {
				t.Fatalf("count identities: %v", err)
			}
			if identities != 0 {
				t.Fatalf("a dry run left %d pool identities behind", identities)
			}

			resolved := resolveFor(t, db, "projx", claimScope)

			key, pending, err = ResolveExistingPool(ctx, db, leaf, claimScope)
			if err != nil {
				t.Fatalf("ResolveExistingPool after provisioning: %v", err)
			}
			if len(pending) != 0 {
				t.Errorf("dry run still reports %d pending levels", len(pending))
			}
			if key != resolved {
				t.Errorf("dry run reports %q, resolution reached %q", key, resolved)
			}
			// The key the dry run predicted for the root level is the key that
			// level actually took: the prediction is the identity, not a guess.
			var exists bool
			if err := db.QueryRow(context.Background(),
				`SELECT EXISTS (SELECT 1 FROM ipam_pool_identity WHERE pool_key = $1)`,
				wantKey).Scan(&exists); err != nil {
				t.Fatalf("look up predicted key: %v", err)
			}
			if !exists {
				t.Errorf("dry run predicted pool key %q, which was never provisioned", wantKey)
			}
		})
	}
}

// TestPlanCascadeRefusesAnUntenantedCaller covers why the consumer comes from
// RequireTenant rather than FromContext.
//
// An untenanted caller has no project, so a per-consumer class would provision
// into the identity an empty consumer names — which is the SHARED identity, and
// therefore somebody else's pool. Refusing is the only answer that does not
// hand out address space to a request that named no tenant.
func TestPlanCascadeRefusesAnUntenantedCaller(t *testing.T) {
	db := testdb.Pool(t)
	perTenantChain(t, db, true)

	read := begin(t, db)
	leaf, err := LoadClass(ctxIn("projx"), read, "tenant-subnet")
	if err != nil {
		t.Fatalf("LoadClass: %v", err)
	}
	_ = read.Rollback(context.Background())

	claimScope := map[string]ipam.ScopeRef{"network": claimScopeRef("Network", "default")}
	if _, err := ResolvePool(context.Background(), db, leaf, claimScope); err == nil {
		t.Fatal("an untenanted caller resolved a pool")
	}

	var identities int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM ipam_pool_identity`).Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if identities != 0 {
		t.Errorf("a refused caller left %d pool identities behind", identities)
	}
}

// TestAnUndeclaredClassProvisionsNothing is the other half of the fix: the half
// that makes undeclared sharing impossible rather than merely discouraged.
//
// Validation refuses a provisioning class that says neither `project` nor
// `allProjects`, but validation runs on write and a class is read. One written
// before the rule existed looks exactly like the fixture here, and spec.poolPer
// is immutable, so it cannot be corrected — it has to be replaced. Until it is,
// its claims are refused rather than being quietly handed the shared identity,
// which is the behaviour #114 reported.
func TestAnUndeclaredClassProvisionsNothing(t *testing.T) {
	db := testdb.Pool(t)
	ctx := context.Background()

	tx := begin(t, db)
	definition(t, tx, "platform", "tenant-ipv6", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, DefaultPrefixLength: 48, PoolPer: []string{"network"},
	})
	definition(t, tx, "platform", "tenant-subnet", ipamv1alpha1.IPClassSpec{
		IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-ipv6", DefaultPrefixLength: 64,
	})
	reference(t, tx, "projx", "tenant-subnet", "platform", "tenant-subnet", nil)
	reference(t, tx, "projy", "tenant-subnet", "platform", "tenant-subnet", nil)
	offerPool(t, tx, "platform", "root", "fd00::/32", "tenant-ipv6", nil)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}

	claimScope := map[string]ipam.ScopeRef{"network": claimScopeRef("Network", "default")}
	for _, project := range []string{"projx", "projy"} {
		read := begin(t, db)
		leaf, err := LoadClass(ctxIn(project), read, "tenant-subnet")
		if err != nil {
			t.Fatalf("LoadClass in %q: %v", project, err)
		}
		_ = read.Rollback(ctx)

		_, err = ResolvePool(ctxIn(project), db, leaf, claimScope)
		if !errors.Is(err, scope.ErrPoolPerUndeclared) {
			t.Fatalf("ResolvePool for %q returned %v, want ErrPoolPerUndeclared", project, err)
		}
		// The error has to say what to write, since the class cannot be edited.
		for _, want := range []string{"tenant-ipv6", scope.ReservedRoleProject, scope.ReservedRoleAllProjects} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	}

	var identities int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ipam_pool_identity`).Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if identities != 0 {
		t.Errorf("an undeclared class provisioned %d pools", identities)
	}
}
