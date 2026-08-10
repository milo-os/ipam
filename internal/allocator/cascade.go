package allocator

// Pool resolution and the provisioning cascade.
//
// A claim never names a pool. Resolving one is the allocator's job, and for a
// class with a parent it may mean creating pools that do not exist yet: a claim
// in a location a network has never used creates that location's subnet, and a
// claim on a new network creates the network's prefix too.
//
// # Why each level is its own transaction
//
// The obvious implementation runs the whole cascade in one transaction. It is
// wrong in a way that does not show up until production. A cascade takes
// `FOR UPDATE` at every level it touches, and the root pool is one row
// platform-wide — so a single spanning transaction serialises all new-network
// creation behind that row. Worse, it holds that lock for the duration of a
// multi-level carve, which pins the Postgres snapshot horizon and stalls the
// xmin-cursor watch delivery for every client of the service, not just for
// claims. This service already has one live cause of that failure; it does not
// need a second.
//
// So each level commits on its own, top-down, before the leaf allocation
// begins. The consequence worth stating plainly is that **no transaction here
// ever holds more than one pool lock**, which is why there is no lock-ordering
// rule to get wrong and no cross-level deadlock to avoid. The deterministic
// lock order the design doc calls for is satisfied by there being nothing to
// order.
//
// # Why the identity row is claimed before the pool lock
//
// Within one level, two claims for the same scope both find no pool and both
// try to create one. The loser must not take the parent pool lock — if it did,
// N concurrent first-claims into one new network would mean N acquisitions of
// the root pool's lock, each waiting on the last, which is the same
// serialisation the per-level split was meant to remove.
//
// So the identity tuple is claimed first, before any pool row is touched. The
// loser blocks on a cheap `(class_name, scope_digest)` row while holding no
// pool locks, and when it wakes the winner has committed both the identity row
// and the pool object, so it simply reads the winner's pool and carries on. A
// herd of concurrent first-claims produces exactly one root-lock acquisition.
//
// The claim uses `ON CONFLICT ... DO NOTHING` and then reads the winner's key in
// a *second statement*, which is worth understanding before anyone tries to
// improve it. Both racers derive the same pool key from the same inputs, so the
// returned key cannot distinguish a win from a loss; zero rows returned is what
// says "somebody else got there".
//
// The obvious alternative, `DO UPDATE` with `RETURNING (xmax = 0) AS won`,
// settles it in one round trip and is measurably worse. It makes every loser
// take a row lock on the winner's tuple and write a new version, so the herd
// serialises on the single row it is all contending for. Measured on PG 17.10:
//
//	herd    DO NOTHING    DO UPDATE
//	  24           8ms         15ms
//	 100          17ms         79ms
//	 200          26ms        227ms
//
// The extra round trip is a constant paid in parallel; the serialisation is not.
// That was measured with no network latency, the condition most favourable to
// `DO UPDATE` — a real RTT moves the crossover down, not up. Herds of 100-200
// are the case this code exists for: a placement scaling into a location its
// network has never used.
//
// Do not collapse the two statements into a CTE. The tempting form —
// `WITH ins AS (INSERT ... DO NOTHING RETURNING ...) SELECT ... UNION ALL
// SELECT ... WHERE NOT EXISTS (SELECT 1 FROM ins)` — is one statement and so has
// one snapshot, taken before the winner committed. The fallback branch therefore
// finds nothing: measured, 95% of concurrent losers got no row at all, silently.
// Two statements work precisely because the second takes a fresh snapshot under
// READ COMMITTED.
//
// The ordering depends on the identity table's foreign key to ipam_objects
// being DEFERRABLE INITIALLY DEFERRED: the identity row names a pool_key whose
// object row is written later in the same transaction, and the constraint is
// only checked at COMMIT. That is required under either upsert form, since
// `ON CONFLICT` arbitrates only the index it names.
//
// # Why a failed claim leaves its pools behind
//
// Nothing here compensates for a claim that fails after provisioning. A pool is
// durable infrastructure triggered by a claim rather than owned by one: the
// design states that subnets "appear on first use and are never renumbered",
// and both container classes retain. Deleting a pool because the claim that
// caused it failed would renumber the next claim's subnet, which is the one
// thing the model promises not to do.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ErrNoOfferingPool is returned when no operator-authored pool offers the class
// at the root of a chain. It is the "class is backed by nothing" condition the
// class list surfaces as zero offering pools, reached at claim time.
var ErrNoOfferingPool = errors.New("ipam: no pool offers this class")

// TxBeginner is the slice of *pgxpool.Pool the cascade needs. The cascade runs
// several transactions rather than one, so unlike the rest of the allocator it
// takes a pool rather than a transaction.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CascadeLevel is one pool the cascade must find or create: the class that
// provisions it, the scope it exists for, and the storage key it will have.
//
// PoolKey is derived rather than generated. Two racing claims must propose the
// same key for the same (class, scope) or the identity row would not identify
// anything, and a generated name would make the winner's key unpredictable to
// the loser.
type CascadeLevel struct {
	Class       *ipamv1alpha1.IPClass
	Scope       map[string]ipam.ScopeRef
	ScopeDigest string
	PoolName    string
	PoolKey     string
}

// PlanCascade computes the pools a claim needs, root-most first, without
// writing anything.
//
// The plan is computed up front and in one read so the levels are consistent
// with each other. Projecting a claim's scope onto each class's PoolPer is
// where a claim missing a role is caught, and it is caught here — before any
// pool is created — so a claim that cannot be satisfied does not leave a
// half-built chain behind.
func PlanCascade(ctx context.Context, tx pgx.Tx, leaf *ipamv1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) ([]CascadeLevel, error) {
	ancestry, err := LoadAncestry(ctx, tx, leaf)
	if err != nil {
		return nil, err
	}

	levels := make([]CascadeLevel, len(ancestry))
	for i, class := range ancestry {
		projected, err := scope.ProjectFor(claimScope, class.Spec.PoolPer, "poolPer")
		if err != nil {
			return nil, err
		}
		name := poolNameFor(project, class.Name, projected)
		// Levels are built nearest-first from the ancestry and stored
		// root-first, because that is the order they must be committed in: a
		// level cannot be carved before the level it carves from exists.
		levels[len(ancestry)-1-i] = CascadeLevel{
			Class:       class,
			Scope:       projected,
			ScopeDigest: scope.PoolDigest(project, projected),
			PoolName:    name,
			PoolKey:     tenant.Identity{Name: project}.ResourceKey("ippools", name),
		}
	}
	return levels, nil
}

// ResolvePool returns the storage key of the pool a claim of leafClass draws
// from, provisioning any missing level of the chain on the way.
//
// db rather than a transaction: each level commits separately. See the package
// comment above for why that is the whole point rather than an implementation
// detail.
func ResolvePool(ctx context.Context, db TxBeginner, leaf *ipamv1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) (string, error) {
	planTx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin cascade planning transaction: %w", err)
	}
	levels, err := PlanCascade(ctx, planTx, leaf, claimScope, project)
	if err != nil {
		_ = planTx.Rollback(ctx)
		return "", err
	}
	// The plan reads classes and nothing else; rolling back rather than
	// committing keeps it unambiguously read-only and releases the snapshot
	// immediately, which matters because everything after this point is a
	// separate transaction and a lingering one would hold the xmin horizon.
	_ = planTx.Rollback(ctx)

	// A leaf with no ancestry — the flat IPv4 endpoint case — draws straight
	// from whichever operator-authored pool offers it. There is nothing to
	// cascade.
	if len(levels) == 0 {
		return discoverInTx(ctx, db, leaf, claimScope)
	}

	// The root-most level carves from an operator-authored pool offering its
	// class; every level below carves from the level above.
	source, err := discoverInTx(ctx, db, levels[0].Class, claimScope)
	if err != nil {
		return "", err
	}
	for i := range levels {
		poolKey, err := ensureLevel(ctx, db, levels[i], source, project)
		if err != nil {
			return "", fmt.Errorf("provision pool for class %q: %w", levels[i].Class.Name, err)
		}
		source = poolKey
	}
	return source, nil
}

// ResolveExistingPool resolves a claim's pool without provisioning anything,
// reporting the levels that would have to be created.
//
// It exists for server-side dry-run. A dry-run must consume no capacity and
// persist nothing, and a pool is emphatically something: it is durable
// infrastructure that is never renumbered afterwards, so provisioning one to
// answer a hypothetical question would leave the hypothetical behind. A dry-run
// against an established scope therefore computes the real next address, and one
// against a scope nothing has used yet reports what it would build instead of
// building it.
func ResolveExistingPool(ctx context.Context, db TxBeginner, leaf *ipamv1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) (string, []CascadeLevel, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("begin dry-run resolution transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	levels, err := PlanCascade(ctx, tx, leaf, claimScope, project)
	if err != nil {
		return "", nil, err
	}
	if len(levels) == 0 {
		poolKey, err := DiscoverPool(ctx, tx, leaf, claimScope)
		return poolKey, nil, err
	}

	// The root-most level must be backed by an operator-authored pool whether or
	// not anything below it exists, so a class backed by nothing still fails
	// here rather than looking satisfiable.
	if _, err := DiscoverPool(ctx, tx, levels[0].Class, claimScope); err != nil {
		return "", nil, err
	}

	poolKey := ""
	for i := range levels {
		existing, err := lookupPoolIdentity(ctx, tx, levels[i].Class.Name, levels[i].ScopeDigest)
		if err != nil {
			return "", nil, err
		}
		if existing == "" {
			return "", levels[i:], nil
		}
		poolKey = existing
	}
	return poolKey, nil, nil
}

// discoverInTx runs pool discovery in its own short transaction.
func discoverInTx(ctx context.Context, db TxBeginner, class *ipamv1alpha1.IPClass, claimScope map[string]ipam.ScopeRef) (string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin pool discovery transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return DiscoverPool(ctx, tx, class, claimScope)
}

// ensureLevel finds or creates the pool for one cascade level, in a single
// transaction of its own.
//
// The sequence, and the reason for each step's position, is:
//
//  1. Read the identity table with no lock. The overwhelmingly common case is
//     that the pool already exists — every claim after the first into a given
//     scope takes this path — and it must cost one indexed read and nothing
//     else.
//  2. Upsert the identity tuple. This is the serialisation point, and it is
//     deliberately *before* any pool row is touched, so a loser waits here
//     holding nothing.
//  3. Only on winning: lock the source pool, carve a block, write the pool
//     object and its allocation record, and materialise the pool's reservations.
//
// A loser returns the winner's pool key from step 2 and never reaches step 3.
func ensureLevel(ctx context.Context, db TxBeginner, level CascadeLevel, sourcePoolKey, project string) (string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin level transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Step 1 — the common path. An existing pool costs one indexed read.
	existing, err := lookupPoolIdentity(ctx, tx, level.Class.Name, level.ScopeDigest)
	if err != nil {
		return "", err
	}
	if existing != "" {
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit identity lookup: %w", err)
		}
		committed = true
		return existing, nil
	}

	// Step 2 — claim the identity before touching a pool row.
	poolKey, won, err := claimPoolIdentity(ctx, tx, level.Class.Name, level.ScopeDigest, level.PoolKey)
	if err != nil {
		return "", err
	}
	if !won {
		// Another request created this pool while we waited on the tuple. It
		// committed both the identity row and the pool object together, so the
		// pool is readable now. Nothing to undo: we hold no pool lock and never
		// touched the source pool.
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit lost identity race: %w", err)
		}
		committed = true
		metrics.RecordCascadeOutcome(level.Class.Name, "lost")
		klog.V(2).InfoS("Lost pool-provisioning race; using the winner's pool",
			"class", level.Class.Name, "pool", poolKey)
		return poolKey, nil
	}

	// Step 3 — winner only.
	if err := provisionPool(ctx, tx, level, sourcePoolKey, project); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit pool provisioning: %w", err)
	}
	committed = true
	metrics.RecordCascadeOutcome(level.Class.Name, "provisioned")
	klog.V(2).InfoS("Provisioned pool",
		"class", level.Class.Name, "pool", poolKey, "source", sourcePoolKey,
		"scope", scope.CanonicalPool(project, level.Scope))
	return poolKey, nil
}

// lookupPoolIdentity reads the pool a class has already provisioned for a
// scope. It takes no lock: a committed identity row is immutable, since the
// pool it names is never renumbered.
func lookupPoolIdentity(ctx context.Context, tx pgx.Tx, className, scopeDigest string) (string, error) {
	defer metrics.ObserveQuery("lookup_pool_identity", time.Now())
	var poolKey string
	err := tx.QueryRow(ctx,
		`SELECT pool_key FROM ipam_pool_identity
		  WHERE class_name = $1 AND scope_digest = $2`,
		className, scopeDigest,
	).Scan(&poolKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("look up pool identity for %q: %w", className, err)
	}
	return poolKey, nil
}

// claimPoolIdentity attempts to claim (className, scopeDigest) for proposedKey,
// reporting whether this caller is the one that gets to create the pool.
//
// A returned row means this transaction inserted and has won. Zero rows means
// somebody else did — and by then they have committed, because the INSERT
// blocked on their speculative index entry until they did. So the follow-up
// SELECT, being a separate statement with its own snapshot under READ COMMITTED,
// reads their key. No savepoint is needed on the losing path: `DO NOTHING`
// raises nothing, so the transaction stays usable.
//
// A conflicting writer that *aborts* does not produce a third outcome: the
// waiter it releases re-runs its speculative insertion, finds the dead tuple,
// and inserts — becoming the winner. So the aborted transaction's pool key is
// handed to nobody, and there is no path on which both the INSERT and the SELECT
// come back empty.
//
// The returned key on a win equals proposedKey by construction; on a loss it is
// the winner's, which is also equal, since both derive from the same class and
// scope. That equality is a property to rely on rather than a check to make,
// which is exactly why row count and not key comparison decides the outcome.
//
// See the package comment for the measurements behind `DO NOTHING` and for why
// the single-statement CTE form is unsafe.
func claimPoolIdentity(ctx context.Context, tx pgx.Tx, className, scopeDigest, proposedKey string) (string, bool, error) {
	defer metrics.ObserveQuery("claim_pool_identity", time.Now())

	var poolKey string
	err := tx.QueryRow(ctx,
		`INSERT INTO ipam_pool_identity (class_name, scope_digest, pool_key)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (class_name, scope_digest) DO NOTHING
		 RETURNING pool_key`,
		className, scopeDigest, proposedKey,
	).Scan(&poolKey)
	if err == nil {
		return poolKey, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("claim pool identity for %q: %w", className, err)
	}

	winner, err := lookupPoolIdentity(ctx, tx, className, scopeDigest)
	if err != nil {
		return "", false, err
	}
	if winner == "" {
		// Unreachable, and the error says so rather than returning an empty key
		// that would resolve to a nonexistent pool several transactions later.
		//
		// The state this guards — zero rows from the INSERT and nothing from the
		// SELECT — would need a conflicting writer that aborted, and that does
		// not produce it: a waiter released by an abort re-runs its speculative
		// insertion, finds the conflicting tuple dead, and inserts successfully,
		// so it becomes the winner and gets a row back. Measured over 160 blocked
		// callers against a deliberately-aborting holder on PostgreSQL 13 and 17,
		// zero landed here. See migrations/cascade_abort_test.go, which will fail
		// loudly if a future Postgres changes that.
		return "", false, fmt.Errorf(
			"claim pool identity for %q: conflicted with a writer that left no row", className)
	}
	return winner, false, nil
}

// provisionPool carves this level's block out of the source pool and writes the
// pool object, its allocation record against the source, and its reservations.
//
// Everything here is in the caller's transaction, which is the transaction that
// claimed the identity row. That is what makes the identity and the object it
// names appear together: a loser that wakes on the identity row is guaranteed
// to find the pool.
func provisionPool(ctx context.Context, tx pgx.Tx, level CascadeLevel, sourcePoolKey, project string) error {
	prefixLen, err := EffectivePrefixLength(level.Class, nil)
	if err != nil {
		return fmt.Errorf("resolve block size for class %q: %w", level.Class.Name, err)
	}

	cidr, err := carveFromPool(ctx, tx, sourcePoolKey, prefixLen, PoolCarveRecord{
		AllocationKey: level.PoolKey,
		ClassName:     level.Class.Name,
		ScopeDigest:   level.ScopeDigest,
		IPFamily:      string(level.Class.Spec.IPFamily),
		OwnerProject:  project,
	})
	if err != nil {
		return err
	}

	// The provisioning class states the reservations its pools carry. A
	// cascade-provisioned pool has no author to write them on, so a per-subnet
	// gateway reservation can only be said on the class that builds the subnets.
	//
	// The blocks are computed before the pool object is written so the object
	// lands with its capacity already accounting for them — a pool that appeared
	// at 0% and then jumped would be a lie a watcher could observe.
	sourcePoolName := sourcePoolKey[strings.LastIndex(sourcePoolKey, "/")+1:]

	reservation := reservationFromSpec(level.Class.Spec.Reservations)
	reserved, err := allocation.ReservedBlocks(
		[]net.IPNet{*cidr}, reservation.Leading, reservation.Trailing, reservation.UnitPrefixLength)
	if err != nil {
		// A reservation the class cannot satisfy is operator misconfiguration,
		// not exhaustion. It must not reach the caller as a full pool.
		return fmt.Errorf("%w: class %q reservations do not fit the /%d it provisions: %w",
			ErrInvalidReservation, level.Class.Name, prefixLen, err)
	}

	pool := newProvisionedPool(level, sourcePoolName, prefixLen, cidr)
	// A freshly carved pool's only allocations are its reservations, so its
	// utilization seeds from exactly those — matching what the post-allocation
	// refresh will compute, so the first claim against it shows a decrease
	// rather than a jump from an unset value.
	setPoolCapacityStatus(pool, []net.IPNet{*cidr}, reserved)

	data, err := encodeObject(pool)
	if err != nil {
		return fmt.Errorf("encode provisioned pool: %w", err)
	}
	if _, err := insertObject(ctx, tx, level.PoolKey, "IPPool", "", level.PoolName, data); err != nil {
		return fmt.Errorf("persist provisioned pool: %w", err)
	}

	// Reservation rows after the object row: their pool_key foreign key is
	// immediate, unlike the identity table's, so the pool must already exist.
	if err := insertReservationRows(ctx, tx, level.PoolKey,
		string(level.Class.Spec.IPFamily), project, reserved); err != nil {
		return err
	}
	return nil
}

// newProvisionedPool builds the IPPool object a cascade level writes, with
// everything that does not require the transaction.
//
// It is separate from provisionPool so the object's shape — the teardown
// labels above all — can be asserted without a database. The capacity status is
// seeded by the caller, which is the one part that depends on the reservations
// having been computed.
func newProvisionedPool(level CascadeLevel, sourcePoolName string, prefixLen int, cidr *net.IPNet) *ipamv1alpha1.IPPool {
	return &ipamv1alpha1.IPPool{
		TypeMeta: metav1TypeMetaIPPool,
		ObjectMeta: metav1ObjectMeta(level.PoolName, "", map[string]string{
			// The provisioning class and the digest are labels rather than
			// annotations because operators list pools by them: "show me every
			// subnet this class made" is the first question asked when a
			// network's addressing looks wrong — and because cascade teardown
			// deletes by label selector rather than one object at a time. See
			// the constants in prefix.go.
			labelProvisionedBy: level.Class.Name,
			labelScopeDigest:   level.ScopeDigest,
		}),
		Spec: ipamv1alpha1.IPPoolSpec{
			IPFamily:     level.Class.Spec.IPFamily,
			PrefixLength: prefixLen,
			ClassRef:     &ipamv1alpha1.LocalRef{Name: level.Class.Name},
			// The lineage is recorded on the pool, not merely implied by the
			// allocation row against the parent. Two reasons, and the second was
			// a live bug before this line existed: an operator reading a pool
			// should see what it was carved from, and IPPool deletion keys its
			// release of the carve off this field — a cascade-provisioned pool
			// without it looked like a root pool, so deleting one leaked the
			// parent's allocation row and permanently wedged the scope.
			ParentPoolRef: &ipamv1alpha1.LocalRef{Name: sourcePoolName},
			Scope:         scopeToVersioned(level.Scope),
			// Copied onto the pool rather than left implicit on the class, so
			// `kubectl get ippool -o yaml` explains why the first block is not
			// available without anyone having to find the class that built it.
			Reservations: level.Class.Spec.Reservations.DeepCopy(),
			// Allocation.Strategy is deliberately left unset, so the pool's own
			// defaulting applies (FirstFit).
			//
			// This used to copy level.Class.Spec.Strategy, which was wrong and
			// was invisible while both fields shared one Strategy type. The
			// class's strategy selects WHICH POOL serves a claim; a pool's
			// selects WHICH BLOCK is taken from inside it. Copying one to the
			// other meant a class set to LeastUtilized — "spread claims across
			// pools" — silently made every pool it provisioned use
			// LeastUtilized for block selection inside itself, which is a
			// different algorithm answering a different question. Splitting the
			// types is what surfaced it.
			Visibility: level.Class.Spec.Visibility,
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: cidr.String(),
			IPFamily:      level.Class.Spec.IPFamily,
			ScopeDigest:   level.ScopeDigest,
		},
	}
}

// reservationFromSpec converts an API reservation to the allocator's shape. A
// nil spec reserves nothing, which is the common case and must not error.
func reservationFromSpec(spec *ipamv1alpha1.ReservationSpec) Reservation {
	if spec == nil {
		return Reservation{}
	}
	return Reservation{
		UnitPrefixLength: int(spec.UnitPrefixLength),
		Leading:          int(spec.Leading),
		Trailing:         int(spec.Trailing),
	}
}

// poolNameFor derives the name of the pool a class provisions for a scope.
//
// The name must be a pure function of (tenant, class, scope): two racing
// requests derive it independently and must agree, since it is the key the
// identity row records. It must also be readable, because an operator looking
// at a list of pools needs to see which network and location each one serves.
//
// So it is the class name and the scope's values joined in sorted-role order,
// sanitised to a DNS label set, with the scope digest appended. The digest
// makes it collision-free even when sanitisation flattens two distinct scopes
// to the same text, which is what keeps readability from costing correctness.
//
// The tenant reaches the name only through the digest, deliberately. Two
// projects' pools for their own network named `default` live in different key
// spaces and could safely share a name, but they did not used to be different
// pools at all — so a shared name is the symptom this now distinguishes, and an
// operator comparing two projects' pool lists should not have to check the key
// prefix to tell them apart.
func poolNameFor(tenant, className string, projected map[string]ipam.ScopeRef) string {
	parts := make([]string, 0, len(projected)+1)
	parts = append(parts, className)
	for _, role := range scope.Roles(projected) {
		parts = append(parts, projected[role].Name)
	}
	readable := sanitizeName(strings.Join(parts, "-"))

	digest := scope.PoolDigest(tenant, projected)
	suffix := "-" + digest[:8]

	// Kubernetes names cap at 253 characters. Truncating the readable part
	// rather than the digest keeps distinct scopes distinct.
	const maxName = 253
	if len(readable)+len(suffix) > maxName {
		readable = strings.Trim(readable[:maxName-len(suffix)], "-")
	}
	return readable + suffix
}

// sanitizeName reduces arbitrary reference names to the lowercase alphanumeric
// and dash set a Kubernetes object name allows. Distinctness is not this
// function's job — the digest suffix handles that — so collapsing is fine.
func sanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// DiscoverPool picks an operator-authored pool that offers the class, applying
// the doc's three filters in order: the pool must offer the class, it must
// share the class's address family, and it must not declare a scope the claim
// disagrees with.
//
// # Discovery searches consenting projects only, and that is a security
// property rather than an implementation detail
//
// This used to read `key LIKE '/ipam.miloapis.com/ippools/%'` — the unprefixed
// root — and the comment here said that predicate must never be relaxed to
// search project-scoped pools. The mechanism is obsolete, because there is no
// unprefixed root any more: the platform is a project, its pools carry a
// project prefix like everyone else's, and with the old predicate a tenant
// could not be served even by a pool in their own project.
//
// The reasoning behind the old predicate is not obsolete, and it is why
// `backingKeyPatterns` exists. `spec.classNames` is a pool *volunteering*
// itself to a class: it is written by the pool's owner, and it was safe only
// because authoring a pool at the platform root was an operator action. Once
// any project's pools are searchable, a tenant able to create an IPPool in their
// own project could list a popular class name on it and have another tenant's
// claims allocated out of space they control — they would learn that the claim
// happened, choose the address it received, and hold the range it came from.
//
// So the volunteer needs a matching consent from the class, which is the thing
// being consumed: IPClass.spec.backingProjects. The platform project is always
// permitted and need not be listed, so a catalog written before that field
// existed behaves exactly as it did.
//
// The consent check is applied here, at read time, and not only in
// validateClassOffers at pool-write time. Write-time is a snapshot: consent is
// revocable, and a pool that passed validation before a project was removed
// from backingProjects would otherwise keep serving forever. The offer table is
// also documented as a rebuildable cache, and a rebuild reconstructs rows from
// spec.classNames alone — it knows nothing about which offers were consented
// to, so a write-time-only rule would be laundered away by a maintenance
// operation.
//
// # Why the join, and not a scan
//
// `ipam_pool_class_offer` is a projection of every pool's spec.classNames,
// indexed on class_name, maintained on pool spec writes and garbage-collected
// by ON DELETE CASCADE. It answers "which pools offer this class" as an index
// lookup instead of a JSONB containment scan over every pool row in the
// database — which is what dropping the key predicate would otherwise have
// turned this into. It carries no policy at all, so every filter that follows —
// family, scope, preference — is unchanged and still applied below.
//
// Cascade-provisioned pools remain reachable only through `ipam_pool_identity`,
// keyed by (provisioning class, scope digest). That path cannot be volunteered
// into: a pool appears there because the allocator put it there for one
// specific scope. The two mechanisms do different jobs and must not be merged —
// discovery answers "what capacity was published to this class", the identity
// table answers "which pool is this scope's".
func DiscoverPool(ctx context.Context, tx pgx.Tx, class *ipamv1alpha1.IPClass, claimScope map[string]ipam.ScopeRef) (string, error) {
	defer metrics.ObserveQuery("discover_pool", time.Now())
	offered, err := offeringPools(ctx, tx, class)
	if err != nil {
		return "", err
	}

	var candidates []offeringPool
	for _, c := range offered {
		// The one filter the count in CountOfferingPools cannot apply, because
		// it depends on the claim rather than on the class.
		if !poolServesScope(&c.pool, claimScope) {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("%w: class %q", ErrNoOfferingPool, class.Name)
	}

	best := 0
	for i := 1; i < len(candidates); i++ {
		if preferPool(class.Spec.Strategy, &candidates[i].pool, &candidates[best].pool) {
			best = i
		}
	}
	return candidates[best].key, nil
}

// offeringPool is one pool published to a class: the storage key and the
// decoded object the filters below need.
type offeringPool struct {
	key  string
	pool ipamv1alpha1.IPPool
}

// offeringPools lists the pools published to a class, filtered by everything
// that depends only on the CLASS: the projects it consents to be backed by, and
// the address family it hands out.
//
// It is factored out of DiscoverPool because CountOfferingPools needs the same
// set, and two queries answering "which pools offer this class" would be free to
// disagree. The one that reported a count would then contradict the one that
// satisfies claims — a class reading `offeringPools: 3` whose every claim fails,
// or the reverse — and neither would error. Both go through here so the only
// difference between them is the single claim-dependent filter DiscoverPool adds
// on top, which is stated at its call site.
func offeringPools(ctx context.Context, tx pgx.Tx, class *ipamv1alpha1.IPClass) ([]offeringPool, error) {
	patterns, err := backingKeyPatterns(ctx, class)
	if err != nil {
		return nil, err
	}
	backingPrefixes, err := backingKeyPrefixes(ctx, class)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT o.pool_key, obj.data
		   FROM ipam_pool_class_offer o
		   JOIN ipam_objects obj ON obj.key = o.pool_key
		  WHERE o.class_name = $1
		    AND obj.kind = 'IPPool'
		    AND obj.key LIKE ANY ($2::text[])
		  ORDER BY o.pool_key`,
		class.Name, patterns,
	)
	if err != nil {
		return nil, fmt.Errorf("list pools offering class %q: %w", class.Name, err)
	}
	defer rows.Close()

	var out []offeringPool
	for rows.Next() {
		var key string
		var data []byte
		if err := rows.Scan(&key, &data); err != nil {
			return nil, fmt.Errorf("scan pool row: %w", err)
		}
		var pool ipamv1alpha1.IPPool
		if err := decodeObject(data, &pool); err != nil {
			return nil, fmt.Errorf("decode pool %q: %w", key, err)
		}
		if !offerEligible(class, backingPrefixes, key, &pool) {
			continue
		}
		out = append(out, offeringPool{key: key, pool: pool})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pool rows: %w", err)
	}
	return out, nil
}

// CountOfferingPools reports how many pools back a class, for
// IPClass.status.offeringPools.
//
// # It counts the root of the chain, not the class itself
//
// This is the whole difficulty, and counting the class named is the wrong
// answer for most classes in a catalog. Only the root-most class in a chain is
// discovered through ipam_pool_class_offer; every class below it gets its space
// carved from the level above through ipam_pool_identity, and no operator ever
// offers a pool to it. So a leaf class with a fully working cascade offers
// exactly zero pools, and reporting that as the count would say "every claim
// naming this class fails" about a class whose claims all succeed. That is
// worse than the field being absent, which is the state this replaces.
//
// Counting the chain root instead makes the field's documented meaning true for
// every class: if nothing offers the root, ResolvePool's very first step fails
// with ErrNoOfferingPool, and every claim naming any class in that chain fails
// with it. For a class with no parent the root is the class itself, so the two
// readings coincide exactly where they should.
//
// # What it is an upper bound on
//
// DiscoverPool applies one further filter that this cannot: poolServesScope,
// which depends on the claim's scope and not on the class. A pool pinned to
// spec.scope.location = us-central-1 is counted here and is unreachable from
// every other location. So a non-zero count does not promise any particular
// claim will succeed; a zero count does promise that none will. That asymmetry
// is the one the field exists for and is worth preserving in that direction —
// a false "backed" is a claim that fails with a specific error, while a false
// "unbacked" sends an operator hunting for a pool that is already there.
//
// # It is computed on read and never stored
//
// The value depends on objects other than the class — every pool's
// spec.classNames — so nothing that writes a class can know it, and writing it
// from the pool path would mean fanning a pool write out across the catalog.
// The consequence to know is that WATCH does not carry it: a class's stored
// object has whatever was last written to status, and no event fires when a
// pool changes the count. Read it with GET or LIST.
func CountOfferingPools(ctx context.Context, tx pgx.Tx, leaf *ipamv1alpha1.IPClass) (int32, error) {
	root, err := chainRoot(ctx, tx, leaf)
	if err != nil {
		return 0, err
	}
	offered, err := offeringPools(ctx, tx, root)
	if err != nil {
		return 0, err
	}
	return int32(len(offered)), nil
}

// OfferingPoolCounts resolves status.offeringPools for a set of classes by
// name, which is what a LIST of the catalog needs.
//
// Work is shared by chain root rather than by class, because that is where the
// sharing actually is: a three-deep chain is three classes with one root, and a
// catalog is a handful of chains. Counting per class would re-run the same
// ancestry walk and the same offer query once per level.
//
// A class the catalog does not hold is omitted from the result rather than
// counted as zero. The caller decides what to do about it — reporting zero for
// a class nobody can read is the exact "wrong is worse than absent" failure
// this field is being fixed for.
func OfferingPoolCounts(ctx context.Context, tx pgx.Tx, classNames []string) (map[string]int32, error) {
	counts := make(map[string]int32, len(classNames))
	if len(classNames) == 0 {
		return counts, nil
	}

	// One read of the catalog, not one per class plus one per ancestor.
	catalog, err := loadCatalog(ctx, tx)
	if err != nil {
		return nil, err
	}

	// Resolve every class to its chain root in memory. No queries.
	rootFor := make(map[string]*ipamv1alpha1.IPClass, len(classNames))
	rootNames := make([]string, 0, len(classNames))
	for _, name := range classNames {
		if _, done := rootFor[name]; done {
			continue
		}
		class, ok := catalog[name]
		if !ok {
			// Not in the platform catalog: unreachable by claims for the same
			// reason it is unreadable here, and the caller leaves whatever is
			// stored rather than asserting zero.
			continue
		}
		root, err := chainRootIn(catalog, class)
		if err != nil {
			return nil, err
		}
		rootFor[name] = root
		if !slices.Contains(rootNames, root.Name) {
			rootNames = append(rootNames, root.Name)
		}
	}
	if len(rootNames) == 0 {
		return counts, nil
	}

	// One offers query for every root on the page.
	//
	// It cannot carry the key-prefix filter the single-class query uses, because
	// consent is per class and one statement serves several. The filter is
	// applied per root below, through the same predicate DiscoverPool uses.
	offers, err := offersForClasses(ctx, tx, rootNames)
	if err != nil {
		return nil, err
	}

	byRoot := make(map[string]int32, len(rootNames))
	for _, root := range rootNames {
		class := catalog[root]
		prefixes, err := backingKeyPrefixes(ctx, class)
		if err != nil {
			return nil, err
		}
		var n int32
		for _, o := range offers[root] {
			if offerEligible(class, prefixes, o.key, &o.pool) {
				n++
			}
		}
		byRoot[root] = n
	}

	for name, root := range rootFor {
		counts[name] = byRoot[root.Name]
	}
	return counts, nil
}

// offersForClasses reads the pools offered to any of the named classes in one
// query, grouped by class.
func offersForClasses(ctx context.Context, tx pgx.Tx, classNames []string) (map[string][]offeringPool, error) {
	defer metrics.ObserveQuery("offers_for_classes", time.Now())
	rows, err := tx.Query(ctx,
		`SELECT o.class_name, o.pool_key, obj.data
		   FROM ipam_pool_class_offer o
		   JOIN ipam_objects obj ON obj.key = o.pool_key
		  WHERE o.class_name = ANY ($1::text[])
		    AND obj.kind = 'IPPool'
		  ORDER BY o.class_name, o.pool_key`, classNames)
	if err != nil {
		return nil, fmt.Errorf("list pools offering %d classes: %w", len(classNames), err)
	}
	defer rows.Close()

	out := map[string][]offeringPool{}
	for rows.Next() {
		var className, key string
		var data []byte
		if err := rows.Scan(&className, &key, &data); err != nil {
			return nil, fmt.Errorf("scan offer row: %w", err)
		}
		var pool ipamv1alpha1.IPPool
		if err := decodeObject(data, &pool); err != nil {
			return nil, fmt.Errorf("decode pool %q: %w", key, err)
		}
		out[className] = append(out[className], offeringPool{key: key, pool: pool})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate offer rows: %w", err)
	}
	return out, nil
}

// chainRoot returns the root-most class of leaf's chain, which is leaf itself
// when it has no parent.
//
// LoadAncestry walks parents nearest-first and excludes leaf, so the last entry
// is the root — the same class ResolvePool hands to DiscoverPool as levels[0],
// since PlanCascade stores the levels reversed.
func chainRoot(ctx context.Context, tx pgx.Tx, leaf *ipamv1alpha1.IPClass) (*ipamv1alpha1.IPClass, error) {
	ancestry, err := LoadAncestry(ctx, tx, leaf)
	if err != nil {
		return nil, err
	}
	if len(ancestry) == 0 {
		return leaf, nil
	}
	return ancestry[len(ancestry)-1], nil
}

// offerEligible applies every filter that depends only on the CLASS, and is the
// one place those filters live.
//
// Two callers reach it: offeringPools, which pre-filters the key in SQL and so
// re-checks the prefix redundantly, and the batched count below, which cannot
// pre-filter because one query serves many classes with different consent
// lists. Sharing the predicate is what stops them drifting — the whole reason
// the lister was factored out in #57, and drift here would mean a class
// reporting pools no claim can reach, or the reverse, with nothing erroring.
//
// The key-prefix term is NOT redundant in the batched caller, and must not be
// "simplified" away on the grounds that the SQL already does it: for the batch
// the SQL does not.
func offerEligible(class *ipamv1alpha1.IPClass, backingPrefixes []string, key string, pool *ipamv1alpha1.IPPool) bool {
	if effectivePoolFamily(pool) != string(class.Spec.IPFamily) {
		return false
	}
	for _, prefix := range backingPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// backingKeyPrefixes is backingKeyPatterns without the LIKE metacharacters, for
// comparison in Go rather than in SQL. Kept beside it so the two cannot name
// different project sets.
func backingKeyPrefixes(ctx context.Context, class *ipamv1alpha1.IPClass) ([]string, error) {
	platformID, err := tenant.PlatformIdentity(ctx)
	if err != nil {
		return nil, err
	}
	projects := []string{platformID.Name}
	for _, p := range class.Spec.BackingProjects {
		if p != "" && !slices.Contains(projects, p) {
			projects = append(projects, p)
		}
	}
	prefixes := make([]string, 0, len(projects))
	for _, p := range projects {
		prefixes = append(prefixes, tenant.Identity{Name: p}.ResourceKey("ippools", ""))
	}
	return prefixes, nil
}

// loadCatalog reads every IPClass in the platform catalog in ONE query.
//
// The catalog is operator-authored and small — the same premise
// ipclass.listClasses already relies on — so a full read is cheaper than the
// per-class chain walk it replaces, and it gives the ancestry resolution a
// consistent view instead of one assembled from N reads.
func loadCatalog(ctx context.Context, tx pgx.Tx) (map[string]*ipamv1alpha1.IPClass, error) {
	defer metrics.ObserveQuery("load_class_catalog", time.Now())
	prefix, err := classKeyPrefix(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT data FROM ipam_objects WHERE kind = 'IPClass' AND key LIKE $1`, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("load class catalog: %w", err)
	}
	defer rows.Close()

	out := map[string]*ipamv1alpha1.IPClass{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scan class: %w", err)
		}
		var class ipamv1alpha1.IPClass
		if err := decodeObject(data, &class); err != nil {
			return nil, fmt.Errorf("decode class: %w", err)
		}
		out[class.Name] = &class
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate classes: %w", err)
	}
	return out, nil
}

// chainRootIn resolves a class's root against an in-memory catalog, with the
// same cycle and depth guards LoadAncestry applies.
//
// The guards are not optional here even though the catalog is validated at
// write time: this walks whatever the database holds, and a cycle would spin
// forever inside a read request rather than returning an error.
func chainRootIn(catalog map[string]*ipamv1alpha1.IPClass, leaf *ipamv1alpha1.IPClass) (*ipamv1alpha1.IPClass, error) {
	seen := map[string]bool{leaf.Name: true}
	current := leaf
	for depth := 0; depth <= MaxClassChainDepth; depth++ {
		parentName := current.Spec.ParentClassName
		if parentName == "" {
			return current, nil
		}
		if seen[parentName] {
			return nil, fmt.Errorf("%w: cycle at class %q", ErrChainTooDeep, parentName)
		}
		seen[parentName] = true
		parent, ok := catalog[parentName]
		if !ok {
			return nil, fmt.Errorf("resolve parent of %q: %w: %q", current.Name, ErrClassNotFound, parentName)
		}
		current = parent
	}
	return nil, fmt.Errorf("%w: class %q exceeds %d levels", ErrChainTooDeep, leaf.Name, MaxClassChainDepth)
}

// backingKeyPatterns returns the LIKE patterns matching the pools a class
// consents to be backed by: the platform project's, plus one per project named
// in spec.backingProjects.
//
// The platform project is always included and is deliberately not required to
// appear in backingProjects. Every class in an existing catalog has an empty
// list, and every one of them is backed by platform-authored pools — so
// requiring the field would turn this change from "consent is now expressible"
// into "every class stops working until it is edited".
//
// Duplicates are dropped rather than rejected: a class naming the platform
// project explicitly is saying something true, and one LIKE pattern per
// distinct project keeps the array the size the operator would expect.
func backingKeyPatterns(ctx context.Context, class *ipamv1alpha1.IPClass) ([]string, error) {
	platformID, err := tenant.PlatformIdentity(ctx)
	if err != nil {
		return nil, err
	}
	projects := []string{platformID.Name}
	for _, p := range class.Spec.BackingProjects {
		if p != "" && !slices.Contains(projects, p) {
			projects = append(projects, p)
		}
	}
	patterns := make([]string, 0, len(projects))
	for _, p := range projects {
		patterns = append(patterns, resourceKeyPrefixFor(p, "ippools")+"%")
	}
	return patterns, nil
}

// poolServesScope reports whether a pool is eligible for a claim carrying
// claimScope.
//
// A pool's spec.scope declares its own constraints: a pool with
// scope.location = us-central-1 serves only claims from that location. A pool
// declaring nothing serves everywhere. The rule generalises past location
// without the allocator learning what a location is — every role the pool
// declares must be matched exactly by the claim.
//
// Note the asymmetry, which is deliberate: a claim carrying roles the pool does
// not declare is fine (the pool is simply less specific), but a pool declaring
// a role the claim does not carry is not. An unlocated ancestor is never
// eligible in its located child's place, and a claim that cannot say where it
// is must not be handed located space by default.
func poolServesScope(pool *ipamv1alpha1.IPPool, claimScope map[string]ipam.ScopeRef) bool {
	for role, declared := range pool.Spec.Scope {
		got, ok := claimScope[role]
		if !ok {
			return false
		}
		if got.Name != declared.Name || got.Kind != declared.Kind || got.APIGroup != declared.APIGroup {
			return false
		}
		// A pool pinned to a specific instance of a reference serves only that
		// instance; a pool naming it by name alone serves whatever currently
		// holds the name.
		if declared.UID != "" && got.UID != declared.UID {
			return false
		}
	}
	return true
}

// preferPool reports whether candidate should displace best under the class's
// strategy.
//
// LeastUtilized spreads claims across pools, which is what an operator wants
// when several pools back one class in one location and no single one should
// fill first. FirstFit — the default — takes the first by storage key, which is
// deterministic across callers and lets an operator steer allocation by naming
// pools in the order they want them filled.
//
// BestFit was removed with status.largestFreePrefix. It preferred the pool with
// the least contiguous headroom that still had some, and that is the one
// question this status no longer answers: utilizationPercent measures VOLUME
// consumed, not the size of the largest remaining block, and a pool can be
// lightly utilized and badly fragmented. Reimplementing it against utilization
// would keep the name while selecting on a different property.
//
// It could not be salvaged by measuring at selection time either. That is the
// whole-pool traversal removing largestFreePrefix exists to avoid, and it would
// run per candidate pool per claim — strictly worse than the cost that was
// removed.
//
// Note this selection has no failover: the chosen pool is the one the claim
// uses, and a pool that cannot satisfy it produces a 507 rather than a retry
// elsewhere. So a strategy selecting on a proxy for headroom would turn a
// mis-estimate into a failed claim, which is why an approximation was rejected
// rather than shipped under the old name.
func preferPool(strategy ipamv1alpha1.PoolSelectionStrategy, candidate, best *ipamv1alpha1.IPPool) bool {
	switch strategy {
	case ipamv1alpha1.PoolLeastUtilized:
		return candidate.Status.UtilizationPercent < best.Status.UtilizationPercent
	default:
		return false
	}
}

// Reservation restates internal/allocation's reservation shape at the
// allocator's boundary, so a caller can describe one without importing the
// allocation library and without either registry converting its API type.
type Reservation struct {
	// UnitPrefixLength is the block size one reserved position occupies. A pool
	// cannot infer it, since pools serve classes of differing allocation sizes.
	UnitPrefixLength int
	// Leading is the number of positions withheld at the start of the pool.
	Leading int
	// Trailing is the number withheld at the end.
	Trailing int
}

// SyncClassOffers records which classes a pool offers itself to.
//
// The projection exists so IPClass.status.offeringPools is a count rather than a
// scan of every pool's JSON. Zero offering pools means every claim naming the
// class fails, which is worth surfacing to an operator before a consumer
// discovers it — so it has to be cheap enough to compute on a list.
//
// It must be called on pool *spec* writes only. Status writes happen on every
// allocation, and rewriting offer rows there would make one contended row per
// class of every pool — serialising claims that per-pool locking deliberately
// keeps independent, which is the same mistake as a class-level utilization
// counter.
func SyncClassOffers(ctx context.Context, tx pgx.Tx, poolKey string, classNames []string) error {
	defer metrics.ObserveQuery("sync_class_offers", time.Now())
	if _, err := tx.Exec(ctx,
		`SELECT ipam_sync_pool_class_offers($1, $2)`,
		poolKey, classNames,
	); err != nil {
		return fmt.Errorf("sync class offers for pool %q: %w", poolKey, err)
	}
	return nil
}

// ProvisionReservations materialises a pool's reserved edge positions as real
// allocations.
//
// The design is explicit that a reservation is inventory rather than an
// invisible hole: each reserved position "becomes a real allocation held by the
// parent, so reserved space has an owner, appears in utilization, and can be
// programmed". Holding them as rows rather than as a rule applied at search
// time also means the existing overlap logic excludes them with no second code
// path, and an operator can see what is reserved by listing allocations.
//
// Reservations belong to the pool and not to any address space carved from it:
// one reservation per pool, not one per network. That is why the rows carry no
// scope digest of their own and are excluded from every scope's search — see
// loadAllocationsInScope.
// It takes the reservation as three integers rather than an API type because
// both callers hold a different one — the pool registry an internal IPPool, the
// cascade a versioned one — and neither should have to convert to ask this.
func ProvisionReservations(ctx context.Context, tx pgx.Tx, poolKey, ipFamily, ownerProject string, parents []net.IPNet, res Reservation) ([]net.IPNet, error) {
	if res.Leading == 0 && res.Trailing == 0 {
		return nil, nil
	}
	blocks, err := allocation.ReservedBlocks(parents, res.Leading, res.Trailing, res.UnitPrefixLength)
	if err != nil {
		return nil, fmt.Errorf("%w: pool %q: %w", ErrInvalidReservation, poolKey, err)
	}
	if err := insertReservationRows(ctx, tx, poolKey, ipFamily, ownerProject, blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// insertReservationRows writes one allocation row per reserved block.
//
// The rows carry the empty scope digest and are excluded from every address
// space's search by purpose rather than by digest — see loadAllocationsInScope.
// That is the rule the design states directly: the parent holds the reservation
// and excludes it from every space carved from that parent, whatever
// uniqueWithin says. One reservation per pool, not one per network.
func insertReservationRows(ctx context.Context, tx pgx.Tx, poolKey, ipFamily, ownerProject string, blocks []net.IPNet) error {
	for i, block := range blocks {
		// The allocation key is derived from the pool and the position so a
		// reservation is idempotent: re-running against the same pool proposes
		// the same keys and conflicts rather than duplicating. ReservedBlocks is
		// documented to be deterministic across parent orderings, which is what
		// makes the index a stable identity rather than an accident of row order.
		allocKey := fmt.Sprintf("%s%d", reservationKeyPrefix(poolKey), i)
		if err := insertAllocationRow(ctx, tx, allocationRow{
			AllocationKey: allocKey,
			PoolKey:       poolKey,
			CIDR:          block.String(),
			ClaimKey:      nil,
			IPFamily:      ipFamily,
			Purpose:       string(ipamv1alpha1.PurposeReservation),
			ClassName:     "",
			// The space no refs separate, which is the honest value for a row
			// that belongs to no tenant's space: a reservation is the pool's
			// own, held against everyone.
			//
			// The search excludes reservations by purpose and never reads this
			// (`purpose <> 'Claim'`), so what it buys is the exclusion
			// constraint, which does not filter by purpose:
			// (pool_key, scope_digest, allocated_cidr) now compares a
			// reservation against every `uniqueWithin: []` claim in the pool,
			// whoever made it. 002's header says reserved rows participate in
			// that constraint precisely so a held address is capacity nobody
			// else can use; carrying the owning tenant's pool digest is what
			// stopped them participating for anyone else.
			//
			// It cannot wrongly refuse a claim: the search already excludes
			// every reservation from every space, so no legitimate allocation
			// overlaps one, and a violation here means a bug upstream chose a
			// block it was shown.
			ScopeDigest: scope.EmptyAddressSpaceDigest(),
			// Retain, because a reservation is never released by a claim going
			// away — it has no claim. Only deleting the pool or an explicit
			// force-release returns the space.
			ReclaimPolicy: string(ipamv1alpha1.ReclaimRetain),
			OwnerProject:  ownerProject,
		}); err != nil {
			return fmt.Errorf("record reservation %d for pool %q: %w", i, poolKey, err)
		}
	}
	return nil
}

// reservationKeyPrefix names a pool's own edge reservations deterministically, so
// re-running against the same pool proposes the same keys and conflicts rather
// than duplicating.
//
// It is an identity convention and nothing more. It used to carry meaning too —
// the delete guard read it to tell a pool's reservations from its children's
// carves, because `purpose` recorded both as Reservation. That is the PoolCarve
// value's job now, which is where it belongs: semantics in a string prefix
// survive exactly until someone writes a pool key containing a '#'.
func reservationKeyPrefix(poolKey string) string { return poolKey + "#reservation/" }

// ReleasePoolReservations deletes the edge reservations a pool holds for itself.
//
// They are not released by any claim — they never had one — and they must not
// outlive the pool: a leaked reservation row keeps a foreign key onto a deleted
// pool and, because reservation keys are derived from the pool key, collides
// with the next pool created under the same name.
func ReleasePoolReservations(ctx context.Context, tx pgx.Tx, poolKey string) error {
	defer metrics.ObserveQuery("release_pool_reservations", time.Now())
	if _, err := tx.Exec(ctx,
		`DELETE FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND purpose = $2`,
		poolKey, string(ipamv1alpha1.PurposeReservation),
	); err != nil {
		return fmt.Errorf("release reservations for pool %q: %w", poolKey, err)
	}
	return nil
}
