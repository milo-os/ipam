package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// Labels a provisioned pool carries, so teardown can find every pool a class
// made without reading each one's spec.
const (
	labelProvisionedBy = "ipam.miloapis.com/provisioned-by"
	labelScopeDigest   = "ipam.miloapis.com/scope-digest"

	// labelProvisionedFor names the consuming project a pool was provisioned
	// for, and is present ONLY on a pool whose class names the reserved project
	// role in poolPer. Its absence is meaningful: this pool is shared by every
	// consumer of its class, so a teardown selecting on it will not reach it.
	labelProvisionedFor = "ipam.miloapis.com/provisioned-for"
)

// What a claim does at one level of a class chain. Reported on
// ipam_cascade_levels_total; internal/metrics documents what each one means
// operationally.
const (
	levelReused      = "reused"
	levelProvisioned = "provisioned"
	levelLost        = "lost"
	levelError       = "error"
)

// TxBeginner is the subset of a connection pool the cascade needs. Each level
// commits separately, so it cannot take a transaction; the reads that decide
// nothing under a lock run on it directly.
//
// A transaction satisfies it too, which is how a dry-run runs the cascade
// inside its own transaction: Begin opens a savepoint, and the reads run in
// the dry-run's transaction.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	BatchQuerier
}

// BatchQuerier is a Querier that can also pipeline: a pool or a transaction.
type BatchQuerier interface {
	Querier
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// CascadeLevel is one pool the cascade must find or create.
//
// PoolKey is derived, not generated. Two racing claims must propose the same
// key for the same (class, scope), or the identity row would not identify
// anything and the winner's key would be unpredictable to the loser.
type CascadeLevel struct {
	Class *ResolvedClass

	// Tenancy is the owner/consumer pair the ScopeDigest was taken over.
	// Consumer is set exactly when this level's class names the reserved
	// project role in poolPer, and it is what makes the pool per-consumer:
	// everything else about a per-consumer level and a shared one is the same.
	Tenancy scope.PoolTenancy

	Scope       map[string]ipam.ScopeRef
	ScopeDigest string
	PoolName    string
	PoolKey     string
}

// OwnerProject is the project a level's carved space is attributed to: the
// consumer when the class carves per consumer, the class's own project
// otherwise.
//
// It is the column per-project consumption reporting reads, so a per-consumer
// carve attributed to the platform project would report every tenant's space as
// the platform's.
func (l CascadeLevel) OwnerProject() string {
	if l.Tenancy.Consumer != "" {
		return l.Tenancy.Consumer
	}
	return l.Class.Project
}

// PlanCascade computes the pools a claim needs, root-most first, writing
// nothing.
//
// Projecting the claim's scope onto each class's PoolPer is where a claim
// missing a role is caught — before any pool exists, so a claim that cannot be
// satisfied leaves no half-built chain behind.
//
// The pool OBJECT lives in the project holding the class DEFINITION, not the
// claimant's, and that is separate from what IDENTIFIES it. Two projects
// referencing one class reach one class; whether they reach one pool is what
// the class's poolPer declares. A class naming the reserved project role gets
// one pool per consumer; one that does not gets one pool shared by every
// consumer — which is the correct and safest shape for announceable public
// space, where per-consumer blocks would exhaust the aggregate after one block
// per project instead of one per location.
//
// Sharing is the default because it is the safe one to get wrong: consumers of
// a shared pool are kept apart by the exclusion constraint, whereas splitting
// space nobody meant to split exhausts an aggregate. A class that must keep
// tenants in separate address space opts in by naming the role, and cannot
// change its mind later, because poolPer is immutable.
func PlanCascade(ctx context.Context, tx Querier, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef) ([]CascadeLevel, error) {
	ancestry, err := LoadAncestry(ctx, tx, leaf)
	if err != nil {
		return nil, err
	}
	return planLevels(ctx, ancestry, claimScope)
}

// PlanScopeRangeCascade computes the pools that must exist for a class to hold
// the range its scope names, ending with the class's own pool.
//
// It is PlanCascade with the leaf included rather than excluded, and that one
// difference is the whole feature. A Block claim stops at the leaf's PARENT,
// because the leaf binds an allocation out of it. A ScopeRange claim binds the
// leaf's own pool, so the leaf is a level like any other and every level below
// the root is provisioned the same way — including the identity row that makes
// a later Block claim adopt this pool instead of provisioning a second one.
func PlanScopeRangeCascade(ctx context.Context, tx Querier, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef) ([]CascadeLevel, error) {
	ancestry, err := LoadAncestry(ctx, tx, leaf)
	if err != nil {
		return nil, err
	}
	// LoadAncestry is nearest-first, so the leaf goes on the front.
	return planLevels(ctx, append([]*ResolvedClass{leaf}, ancestry...), claimScope)
}

// planLevels turns a nearest-first chain of classes into root-first levels.
//
// Both planning paths go through here, which is what makes a level's identity
// the same whichever asked for it: the pool a ScopeRange claim provisions for a
// class and scope is the pool a Block claim of a child class carves from.
//
// The consumer comes from RequireTenant, not FromContext: an untenanted caller
// would otherwise provision into the shared identity by accident, and the
// consumer must be the same discriminator the storage key prefix uses.
func planLevels(ctx context.Context, chain []*ResolvedClass, claimScope map[string]ipam.ScopeRef) ([]CascadeLevel, error) {
	consumer, err := tenant.RequireTenant(ctx)
	if err != nil {
		return nil, err
	}

	levels := make([]CascadeLevel, len(chain))
	for i, class := range chain {
		// The reserved role is dropped before projecting: it is not a ScopeRef
		// and never arrives on a request, so a claim cannot supply it and must
		// not be asked to.
		roles, perConsumer := scope.PoolPerRoles(class.Spec.PoolPer)
		projected, err := scope.ProjectFor(claimScope, roles, "poolPer")
		if err != nil {
			return nil, err
		}
		tenancy := scope.PoolTenancy{Owner: class.Project}
		if perConsumer {
			tenancy.Consumer = consumer.Name
		}
		name := poolNameFor(tenancy, class.Name, projected)
		// Built nearest-first from the ancestry, stored root-first: a level
		// cannot be carved before the level it carves from exists.
		levels[len(chain)-1-i] = CascadeLevel{
			Class:       class,
			Tenancy:     tenancy,
			Scope:       projected,
			ScopeDigest: scope.PoolDigest(tenancy, projected),
			PoolName:    name,
			PoolKey:     tenant.Identity{Name: tenancy.Owner}.ResourceKey("ippools", name),
		}
	}
	return levels, nil
}

// ResolvePool returns the key of the pool a claim draws from, provisioning any
// missing level on the way.
//
// It takes a pool rather than a transaction because each level commits
// separately: a level that exists must be visible to every other caller
// immediately, and holding one transaction across the whole chain would make a
// herd of first claims serialise behind the slowest.
func ResolvePool(ctx context.Context, db TxBeginner, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef) (string, error) {
	return ResolvePoolFor(ctx, db, leaf, claimScope, nil)
}

// ResolvePoolFor is ResolvePool for a claim that may name its block.
//
// For a class with no parent, want narrows discovery to the offering pool that
// contains it (see DiscoverPoolFor). For a class with a parent chain it
// provisions nothing: every level must already exist, or the claim is refused
// with ErrRangeUnplaced, and the allocation then refuses the block with
// ErrAddressOutsidePool unless the leaf pool contains it.
//
// A Block claim does not steer the cascade: guessing at placement from one
// endpoint's address would carve durable infrastructure to suit one claim.
// Nor may it fall back to first-free, since the pool it placed would be in the
// way of the placement it needed. Adopting an address under a chained class
// works by placing the chain first, with a ScopeRange claim that names the
// range (PlaceScopeRange); the Block claim then finds the leaf pool holding
// its address. A pool placed before adoption, by first-free, is never moved,
// so an address outside it stays refused.
func ResolvePoolFor(ctx context.Context, db TxBeginner, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef, want *net.IPNet) (string, error) {
	chain := newChainLookup(leaf, claimScope)
	return chain.Resolve(ctx, db, want)
}

// ResolveExistingPool resolves a claim's pool without provisioning anything,
// reporting the levels that would have to be created.
//
// A claim naming its block resolves through it (existingChainFor): a pool is
// durable infrastructure that is never renumbered, so one placed by first-free
// on behalf of a claim that is then refused would stay where it landed.
//
// Every read is lock-free and decides nothing a later lock does not decide
// again, so they run on db without a transaction.
func ResolveExistingPool(ctx context.Context, db BatchQuerier, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef) (string, []CascadeLevel, error) {
	chain := newChainLookup(leaf, claimScope)
	return chain.existing(ctx, db)
}

// ErrRangeUnplaced is returned for a claim naming its block under a class
// whose chain has no pool for the claim's scope yet. Such a claim never has
// one placed for it by first-free; the chain is placed first, at the range
// holding the address, by a ScopeRange claim naming that range.
var ErrRangeUnplaced = errors.New("ipam: the range holding this address has not been placed")

// unplacedChain is the refusal for a claim naming want under a chain with
// levels pending: it names the ScopeRange claim that would place the missing
// range around want. levels is the whole chain, root first.
func unplacedChain(levels, pending []CascadeLevel, want net.IPNet) error {
	// A missing shared level cannot be placed by an addressed ScopeRange claim
	// (ErrSharedPoolUnplaced), so pointing at one would send the caller to a
	// second refusal. Nor is a claim without an address the way out: it would
	// place every per-consumer level beneath the shared one by first-free too,
	// and those never move.
	for _, level := range pending {
		if level.Tenancy.Consumer == "" {
			return fmt.Errorf("%w: %s", ErrRangeUnplaced, sharedLevelMissing(level.Class.Name))
		}
	}
	// The pool the claim draws from is the last level's: that is the range
	// a ScopeRange claim must name for the address to land inside it.
	holder := levels[len(levels)-1]
	hint := fmt.Sprintf("a ScopeRange claim of class %q", holder.Class.Name)
	if prefixLen, err := EffectivePrefixLength(holder.Class.IPClass, nil); err == nil {
		if block, err := allocation.ContainingBlock(want, prefixLen); err == nil {
			hint += fmt.Sprintf(" with spec.address %s", block.String())
		}
	}
	return fmt.Errorf("%w: class %q has no pool for this scope yet, and a claim naming an address never has one placed for it; "+
		"place the range first with %s and the same scope, then claim %s again",
		ErrRangeUnplaced, pending[0].Class.Name, hint, want.String())
}

// ensureLevel finds or creates the pool for one level, reporting whether it was
// this caller that created it:
//
//  1. Read the identity table with no lock and no transaction. Every claim
//     after the first into a scope takes this path and must cost one indexed
//     read. A committed identity row never changes, so there is nothing for a
//     transaction to hold.
//  2. Only on a miss, in a transaction of the level's own: upsert the identity
//     tuple. This is the serialisation point, deliberately BEFORE any pool row
//     is touched, so a loser waits here holding nothing. A miss decides
//     nothing; the upsert arbitrates whoever got there first.
//  3. Only on winning: carve a block from the source and write the pool.
func ensureLevel(ctx context.Context, db TxBeginner, level CascadeLevel, sourcePoolKey string) (poolKey string, provisioned bool, err error) {
	existing, err := lookupPoolIdentity(ctx, db, level.Class.Name, level.ScopeDigest)
	if err != nil {
		metrics.RecordCascadeLevel(ctx, level.Class.Name, levelError)
		return "", false, err
	}
	if existing != "" {
		metrics.RecordCascadeLevel(ctx, level.Class.Name, levelReused)
		return existing, false, nil
	}
	return provisionLevel(ctx, db, level, sourcePoolKey)
}

// provisionLevel is ensureLevel for a level whose identity row was missing
// when it was read: steps 2 and 3. The miss is only a hint, so it claims the
// identity before anything else, and a claim that loses uses the winner's
// pool.
func provisionLevel(ctx context.Context, db TxBeginner, level CascadeLevel, sourcePoolKey string) (poolKey string, provisioned bool, err error) {
	outcome := levelError
	defer func() { metrics.RecordCascadeLevel(ctx, level.Class.Name, outcome) }()

	tx, err := db.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin level transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	poolKey, won, err := claimPoolIdentity(ctx, tx, level.Class.Name, level.ScopeDigest, level.PoolKey)
	if err != nil {
		return "", false, err
	}
	if !won {
		// The winner committed the identity row and the pool object together,
		// so the pool is readable now. Nothing to undo: no pool lock was held
		// and the source was never touched.
		if err := tx.Commit(ctx); err != nil {
			return "", false, fmt.Errorf("commit lost identity race: %w", err)
		}
		committed = true
		// Every member of a first-claim herd but one loses, so a loss is an
		// outcome on the same counter as a win, not an error.
		outcome = levelLost
		klog.V(2).InfoS("Lost pool-provisioning race; using the winner's pool",
			"class", level.Class.Name, "pool", poolKey)
		return poolKey, false, nil
	}

	if err := provisionPool(ctx, tx, level, sourcePoolKey, nil, ""); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit pool provisioning: %w", err)
	}
	committed = true
	outcome = levelProvisioned
	klog.V(2).InfoS("Provisioned pool",
		"class", level.Class.Name, "pool", poolKey, "source", sourcePoolKey)
	return poolKey, true, nil
}

// lookupPoolIdentity reads the pool a class has already provisioned for a
// scope. It takes no lock: a committed identity row is immutable, because the
// pool it names is never renumbered.
func lookupPoolIdentity(ctx context.Context, tx Querier, className, scopeDigest string) (string, error) {
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

// claimPoolIdentity claims (className, scopeDigest) for proposedKey, reporting
// whether this caller is the one that creates the pool.
//
// A returned row means this transaction inserted and won. Zero rows means
// somebody else did, and by then they have committed: the INSERT blocked on
// their speculative index entry until they did, so the follow-up SELECT — a
// separate statement with its own snapshot — reads their key.
//
// DO NOTHING rather than DO UPDATE, so the losing path raises nothing and the
// transaction stays usable without a savepoint.
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
		// Unreachable: a waiter released by an aborting writer re-runs its
		// speculative insertion, finds the tuple dead, and becomes the winner.
		// The error says so rather than returning an empty key that would
		// resolve to a nonexistent pool several transactions later.
		return "", false, fmt.Errorf(
			"claim pool identity for %q: conflicted with a writer that left no row", className)
	}
	return winner, false, nil
}

// provisionPool carves this level's block out of the source pool and writes the
// pool object, in the transaction that claimed the identity row. That is what
// makes the identity and the object it names appear together: a loser waking on
// the identity row is guaranteed to find the pool.
//
// at, when set, is the block to carve instead of the first free one, and
// requesterProject is the project of the claim that named it (see poolCarve).
func provisionPool(ctx context.Context, tx pgx.Tx, level CascadeLevel, sourcePoolKey string, at *net.IPNet, requesterProject string) error {
	prefixLen, err := EffectivePrefixLength(level.Class.IPClass, nil)
	if err != nil {
		return fmt.Errorf("resolve block size for class %q: %w", level.Class.Name, err)
	}

	cidr, err := carveFromPool(ctx, tx, sourcePoolKey, prefixLen, poolCarve{
		AllocationKey:    level.PoolKey,
		ClassName:        level.Class.Name,
		ScopeDigest:      level.ScopeDigest,
		IPFamily:         string(level.Class.Spec.IPFamily),
		OwnerProject:     level.OwnerProject(),
		At:               at,
		RequesterProject: requesterProject,
	})
	if err != nil {
		return err
	}

	sourcePoolName := sourcePoolKey[strings.LastIndex(sourcePoolKey, "/")+1:]
	pool := newProvisionedPool(level, sourcePoolName, prefixLen, cidr)
	setPoolCapacityStatus(pool, []net.IPNet{*cidr}, new(big.Int))

	data, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("encode provisioned pool: %w", err)
	}
	if _, err := insertObject(ctx, tx, level.PoolKey, "IPPool", "", level.PoolName, data); err != nil {
		return fmt.Errorf("persist provisioned pool: %w", err)
	}
	// After the object row exists, because reserving updates the pool's
	// capacity and that is a write to the row.
	if err := materialiseReservations(ctx, tx, level.PoolKey, pool, []net.IPNet{*cidr}); err != nil {
		return err
	}
	return nil
}

func newProvisionedPool(level CascadeLevel, sourcePoolName string, prefixLen int, cidr *net.IPNet) *ipamv1alpha1.IPPool {
	labels := map[string]string{
		labelProvisionedBy: level.Class.Name,
		labelScopeDigest:   level.ScopeDigest,
	}
	// Only on a per-consumer pool. A shared pool carrying the label of whoever
	// happened to claim first would name a project with no more relation to the
	// pool than any other consumer of it.
	if level.Tenancy.Consumer != "" {
		labels[labelProvisionedFor] = level.Tenancy.Consumer
	}
	pool := &ipamv1alpha1.IPPool{
		TypeMeta: metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   level.PoolName,
			Labels: labels,
		},
		Spec: ipamv1alpha1.IPPoolSpec{
			IPFamily:      level.Class.Spec.IPFamily,
			PrefixLength:  prefixLen,
			ClassRef:      &ipamv1alpha1.LocalRef{Name: level.Class.Name},
			ParentPoolRef: &ipamv1alpha1.LocalRef{Name: sourcePoolName},
			Scope:         scopeToVersioned(level.Scope),
			Reservations:  level.Class.Spec.Reservations.DeepCopy(),
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: cidr.String(),
			IPFamily:      level.Class.Spec.IPFamily,
			ScopeDigest:   level.ScopeDigest,
		},
	}
	// A provisioned pool is written straight to the object table, so nothing
	// has stamped the fields the apiserver's create path stamps for a pool the
	// user POSTs. Without them the pool reports a zero creation timestamp and
	// no UID, and an owner reference to it cannot be made.
	rest.FillObjectMetaSystemFields(&pool.ObjectMeta)
	return pool
}

func scopeToVersioned(s map[string]ipam.ScopeRef) map[string]ipamv1alpha1.ScopeRef {
	if len(s) == 0 {
		return nil
	}
	out := make(map[string]ipamv1alpha1.ScopeRef, len(s))
	for role, ref := range s {
		out[role] = ipamv1alpha1.ScopeRef{APIGroup: ref.APIGroup, Kind: ref.Kind, Name: ref.Name}
	}
	return out
}

// poolNameFor derives a pool's name from its class, its tenancy and its
// projected scope.
//
// Readable prefix plus a digest suffix: the prefix is for whoever reads
// `kubectl get ippools`, and the digest is what makes the name a function of
// (class, tenancy, scope) so two racing claims propose the same one.
//
// The consumer project appears in the readable part for a per-consumer pool,
// right after the class name, so `kubectl get ippools` distinguishes one
// tenant's pool from another's without decoding a digest. A shared pool's name
// does not mention it, because no one consumer owns it.
func poolNameFor(t scope.PoolTenancy, className string, projected map[string]ipam.ScopeRef) string {
	parts := make([]string, 0, len(projected)+2)
	parts = append(parts, className)
	if t.Consumer != "" {
		parts = append(parts, t.Consumer)
	}
	for _, role := range scope.Roles(projected) {
		parts = append(parts, projected[role].Name)
	}
	readable := sanitizeName(strings.Join(parts, "-"))

	suffix := "-" + scope.PoolDigest(t, projected)[:8]
	const maxName = 253
	if len(readable)+len(suffix) > maxName {
		readable = strings.Trim(readable[:maxName-len(suffix)], "-")
	}
	return readable + suffix
}

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

// poolCarve is the allocation row a provisioned pool leaves against its source.
type poolCarve struct {
	AllocationKey string
	ClassName     string
	ScopeDigest   string
	IPFamily      string

	// OwnerProject attributes the carved space. See CascadeLevel.OwnerProject:
	// the consumer for a per-consumer level, the defining project otherwise.
	OwnerProject string

	// At is the block to carve, for a level a ScopeRange claim places at the
	// range it names. Nil carves the first free block. A named block is
	// carved there or refused, never moved: it is checked by the same
	// classification a named claim gets, so a collision reads as AddressInUse
	// or AddressReserved rather than as exhaustion.
	At *net.IPNet

	// RequesterProject is the project of the claim that named At. It decides
	// how much a refusal may say about what is in the way.
	RequesterProject string
}

// carveFromPool takes a block out of the source pool for a child pool to own.
//
// Recorded as purpose 'PoolCarve', not 'Claim'. A claim's block leaves only its
// own address space occupied; a carve is space that has LEFT the pool entirely,
// so it must block every space. searchFilter reads exactly that distinction, so
// a carve recorded as a claim would let another address space allocate inside a
// child pool's range.
func carveFromPool(ctx context.Context, tx pgx.Tx, sourcePoolKey string, prefixLen int, carve poolCarve) (*net.IPNet, error) {
	pool, err := lockAndDecodeIPPool(ctx, tx, sourcePoolKey)
	if err != nil {
		return nil, err
	}
	parents, err := parsePoolCIDR(pool)
	if err != nil {
		return nil, err
	}

	if err := materialiseReservations(ctx, tx, sourcePoolKey, pool, parents); err != nil {
		return nil, err
	}

	// spaceAll, not carve.ScopeDigest — which is a POOL digest and identifies
	// nothing the search understands. The block leaves the pool, so it must be
	// free in every space, not in one of them.
	var block *net.IPNet
	if carve.At != nil {
		if err := checkRequestedBlock(ctx, tx, sourcePoolKey, spaceAll, parents, *carve.At, carve.RequesterProject); err != nil {
			return nil, err
		}
		at := *carve.At
		block = &at
	} else {
		block, err = findBlock(ctx, tx, sourcePoolKey, spaceAll, parents, prefixLen, allocation.Strategy(pool.Spec.Allocation.Strategy))
		if err != nil {
			if errors.Is(err, allocation.ErrPoolExhausted) {
				return nil, ErrPoolExhausted
			}
			return nil, fmt.Errorf("carve /%d out of %q: %w", prefixLen, sourcePoolKey, err)
		}
	}

	consumed, err := consumptionAfterAllocate(ctx, tx, sourcePoolKey, parents, *block)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family,
		     purpose, class_name, scope_digest, reclaim_policy, owner_project)
		 VALUES ($1, $2, $3, $3, $4, 'PoolCarve', $5, $6, 'Delete', $7)`,
		sourcePoolKey, block.String(), carve.AllocationKey, carve.IPFamily,
		carve.ClassName, carve.ScopeDigest, carve.OwnerProject,
	); err != nil {
		if carve.At != nil && isOverlapViolation(err) {
			return nil, &AddressInUseError{Address: block.String()}
		}
		return nil, fmt.Errorf("record pool carve against %q: %w", sourcePoolKey, err)
	}

	if err := writeConsumed(ctx, tx, sourcePoolKey, consumed); err != nil {
		return nil, err
	}
	if err := persistPoolCapacity(ctx, tx, pool, sourcePoolKey, parents, consumed); err != nil {
		return nil, fmt.Errorf("update source pool capacity after carve: %w", err)
	}
	return block, nil
}
