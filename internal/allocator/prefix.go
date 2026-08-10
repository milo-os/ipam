package allocator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// Labels the allocator stamps on pools it provisions, so an operator can list
// "every pool this class made" without decoding spec.
//
// These are load-bearing for teardown, not merely convenient. Cascade cleanup
// selects on labelProvisionedBy, and a label selector that matches nothing is
// indistinguishable from a cluster with nothing to clean: the delete succeeds,
// reports success, and leaves every pool in place. Do not drop or rename either
// key without changing test/load/Taskfile.yaml's cascade-cleanup with it.
// TestProvisionedPoolCarriesTeardownLabels pins both keys and both values.
//
// spec.classRef is NOT a substitute. An operator may set it on a pool they
// authored by hand, so it identifies the class a pool draws from rather than
// the fact that the allocator created the pool. Only these labels mean "the
// cascade made this, and deleting it is undoing the cascade".
const (
	labelProvisionedBy = "ipam.miloapis.com/provisioned-by-class"
	labelScopeDigest   = "ipam.miloapis.com/scope-digest"
)

// metav1TypeMetaIPPool is stamped on pools the allocator writes directly into
// ipam_objects. The generic registry stamps it for objects that go through the
// standard path; these do not, and a stored object without apiVersion/kind does
// not decode.
var metav1TypeMetaIPPool = metav1.TypeMeta{
	APIVersion: ipamv1alpha1.SchemeGroupVersion.String(),
	Kind:       "IPPool",
}

func metav1ObjectMeta(name, namespace string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:              name,
		Namespace:         namespace,
		Labels:            labels,
		CreationTimestamp: metav1.Now(),
	}
}

func encodeObject(obj any) ([]byte, error) { return json.Marshal(obj) }

func decodeObject(data []byte, into any) error { return json.Unmarshal(data, into) }

// tenantsFromPoolKey derives (project, org) labels for utilization gauges
// from a pool key. The key encodes only the immediate parent (project for
// project-scoped pools), so org is "" today; see tenant.Identity.Org for the
// long-term plan to populate it via a forwarded org extra.
func tenantsFromPoolKey(poolKey string) (project, org string) {
	return tenant.ProjectFromKey(poolKey), ""
}

// PostgresPrefixAllocator implements PrefixAllocator atop ipam_objects and
// ipam_cidr_allocations.
//
// The pool row's lock is what serialises concurrent claims against one pool;
// the ipam_cidr_allocations rows are not individually locked, so the work is
// O(existing in scope) per allocation rather than O(pool size).
type PostgresPrefixAllocator struct{}

// NewPostgresPrefixAllocator returns a stateless allocator.
func NewPostgresPrefixAllocator() *PostgresPrefixAllocator {
	return &PostgresPrefixAllocator{}
}

// AllocateRequest is everything the allocator needs to hand out one block.
//
// It is a struct rather than a parameter list because the fields are not
// independent — ScopeDigest is derived from ClassName's UniqueWithin, and
// ReclaimPolicy from ClassName and the claim's override — and a positional call
// with six strings in it is one transposition away from writing an allocation
// into the wrong address space.
type AllocateRequest struct {
	// PoolKey is the pool to carve from, already resolved by the cascade.
	PoolKey string
	// AllocationKey is the storage key of the IPAllocation object that will
	// record this block. It is the allocation's identity, and unlike ClaimKey it
	// is never cleared.
	AllocationKey string
	// ClaimKey is the storage key of the IPClaim this block is bound to. Empty
	// only for blocks the pool holds itself.
	ClaimKey string
	// ClassName is the class the allocation was handed out under.
	ClassName string
	// ScopeDigest identifies the address space this allocation must be unique
	// within: the claim's scope projected onto the class's UniqueWithin.
	ScopeDigest string
	// PrefixLength is the requested block size in bits.
	PrefixLength int
	// Address, when set, names a specific block to bind rather than letting the
	// allocator choose. It is how an address already in use gets recorded and
	// how a specific address is handed back deliberately.
	Address string
	// IPFamily is the address family, which the class fixes.
	IPFamily string
	// ReclaimPolicy is the effective policy, resolved from the class and the
	// claim's override at bind time and recorded because the allocation
	// outlives the claim that chose it.
	ReclaimPolicy string
	// OwnerProject scopes the allocation to one tenant project for per-project
	// capacity queries. Empty for platform-scoped allocations.
	OwnerProject string
}

// AllocatePrefix reserves a block for a claim inside the supplied transaction.
//
// The sequence is the one the architecture describes, with one addition the
// class model requires: the set of allocations the new block must not overlap
// is scoped, not the whole pool. Two allocations may hold the same address if
// they differ in one of the class's UniqueWithin references — which is the
// entire reason a shared per-location IPv4 range can serve every network in
// that location at once.
func (a *PostgresPrefixAllocator) AllocatePrefix(ctx context.Context, tx pgx.Tx, req AllocateRequest) (string, error) {
	pool, err := lockAndDecodeIPPool(ctx, tx, req.PoolKey)
	if err != nil {
		return "", err
	}

	parents, err := parsePoolCIDRs(pool)
	if err != nil {
		return "", err
	}

	// One read of the pool's allocations, not two.
	//
	// The capacity recompute needs every row in the pool regardless of address
	// space. It used to be a second query issued AFTER the insert, so a pool
	// with N allocations was scanned and decoded 2N times per allocation.
	//
	// THIS READ IS STILL LINEAR IN THE POOL, AND IT IS NOW THE WHOLE OF THE
	// QUADRATIC. The search below no longer uses it — a first-fit search reads
	// only what it examines (see boundedFirstFit) — so this load exists solely
	// to recompute status.capacity and status.largestFreePrefix. Removing it
	// needs a decision about what largestFreePrefix means, not a refactor; the
	// options are in the header of writePoolCapacity. Until that lands, wiring
	// the bounded search buys correctness and instrumentation rather than
	// speed, and anyone measuring end-to-end allocation cost against this
	// commit and finding it unchanged has measured correctly.
	all, err := loadPoolAllocations(ctx, tx, req.PoolKey)
	if err != nil {
		return "", err
	}

	cidr, err := selectBlock(ctx, tx, pool, parents, all, req)
	if err != nil {
		return "", err
	}

	claimKey := &req.ClaimKey
	if req.ClaimKey == "" {
		claimKey = nil
	}
	if err := insertAllocationRow(ctx, tx, allocationRow{
		AllocationKey: req.AllocationKey,
		PoolKey:       req.PoolKey,
		CIDR:          cidr.String(),
		ClaimKey:      claimKey,
		IPFamily:      req.IPFamily,
		Purpose:       string(ipamv1alpha1.PurposeClaim),
		ClassName:     req.ClassName,
		ScopeDigest:   req.ScopeDigest,
		ReclaimPolicy: req.ReclaimPolicy,
		OwnerProject:  req.OwnerProject,
	}); err != nil {
		return "", err
	}

	// Pool capacity is a property of the pool, not of one address space in it,
	// so it is recomputed from every allocation rather than from the scoped set
	// the search used — including the block just written, appended here rather
	// than fetched back.
	if err := writePoolCapacity(ctx, tx, pool, req.PoolKey, parents, append(cidrsOf(all), *cidr)); err != nil {
		return "", err
	}

	klog.V(2).InfoS("Allocated prefix",
		"pool", req.PoolKey, "cidr", cidr.String(), "claim", req.ClaimKey,
		"class", req.ClassName, "ownerProject", req.OwnerProject)
	return cidr.String(), nil
}

// selectBlock resolves the block a request gets: the one it named, or the first
// available one of the requested size.
//
// The two paths are genuinely different questions. A claim naming an address is
// asking "is this exact block free", which either holds or does not; a claim
// naming a size is asking the allocator to choose. Conflating them would make a
// specific-address request silently drift to a different address, which defeats
// the two cases that need it — recording an address already in use, and handing
// a specific one back deliberately.
func selectBlock(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, parents []net.IPNet, all []poolAllocation, req AllocateRequest) (*net.IPNet, error) {
	if req.Address != "" {
		existing := inScope(all, req.ScopeDigest)
		want, err := parseRequestedBlock(req.Address, req.PrefixLength)
		if err != nil {
			return nil, err
		}
		// IsBlockAvailable answers both questions at once, but the caller needs
		// them apart: "that address is not in this pool" points at a
		// misconfigured class, while "that address is taken" points at whoever
		// holds it. Containment is checked separately so each gets its own error.
		if !blockWithinAny(*want, parents) {
			return nil, fmt.Errorf("%w: %s is outside pool %q", ErrAddressOutOfRange, req.Address, req.PoolKey)
		}
		if !allocation.IsBlockAvailable(parents, existing, *want) {
			return nil, fmt.Errorf("%w: %s", ErrAddressTaken, req.Address)
		}
		return want, nil
	}

	strategy := allocation.Strategy(pool.Spec.Allocation.Strategy)
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanFindBlock)
	defer span.End()
	span.SetAttributes(
		attribute.String(tracing.AttrStrategy, string(strategy)),
		attribute.Int(tracing.AttrExistingCount, len(all)),
	)

	// FirstFit is the only strategy a bounded search can serve, and that is what
	// the strategies mean rather than a gap to close. BestFit wants the smallest
	// region that fits and LeastUtilized the emptiest parent; neither can be
	// known from a prefix of the allocations, so both keep the whole-set path
	// and keep its cost. They are also not the default.
	if strategyIsFirstFit(strategy) {
		block, err := boundedSelect(ctx, tx, parents, req)
		if err != nil {
			return nil, err
		}
		span.SetAttributes(attribute.String(tracing.AttrResultCIDR, block.String()))
		return block, nil
	}

	// Reservations are already materialised as rows and so are inside the set;
	// the zero Reservation here means "nothing further to withhold", not "no
	// reservations exist".
	existing := inScope(all, req.ScopeDigest)
	res, err := allocation.Allocate(parents, existing, req.PrefixLength, strategy, allocation.Reservation{})
	if err != nil {
		if errors.Is(err, allocation.ErrPoolExhausted) {
			span.SetAttributes(attribute.Bool(tracing.AttrExhausted, true))
			span.SetStatus(codes.Error, "pool exhausted")
			// The figures come from the failed search rather than from two more
			// traversals: Allocate populates them even when no block was found,
			// which is precisely the case the 507 needs them for.
			return nil, &ExhaustionError{
				PoolKey:               req.PoolKey,
				RequestedPrefixLength: req.PrefixLength,
				UtilizationPercent:    res.UtilizationPercent,
			}
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("compute next prefix: %w", err)
	}
	span.SetAttributes(attribute.String(tracing.AttrResultCIDR, res.Block.String()))
	return res.Block, nil
}

// strategyIsFirstFit reports whether the pool's strategy is the one the bounded
// search implements. The empty string is FirstFit — the library's default, and
// what an IPPool with no explicit strategy gets.
func strategyIsFirstFit(s allocation.Strategy) bool {
	return s == "" || s == allocation.FirstFit
}

// boundedSelect runs the bounded first-fit search and maintains the floor
// around it.
//
// The exhaustion figures are the one place this path pays for being bounded: a
// search that never walked the whole pool cannot say how full the pool is, so
// the 507's largestFreePrefix and utilizationPercent come from a deliberate
// whole-pool measurement taken only on the failure path. Exhaustion is rare and
// already the slow answer; paying there rather than on every success is the
// trade this whole change is about.
func boundedSelect(ctx context.Context, tx pgx.Tx, parents []net.IPNet, req AllocateRequest) (*net.IPNet, error) {
	span := trace.SpanFromContext(ctx)

	// The floor row is locked HERE, immediately after the pool row this function's
	// caller already holds, and before the search reads anything. Deferring the
	// lock to the write at the end is what produced #93: a transaction holding
	// its pool and reaching late for a floor, against one holding that floor and
	// reaching for a pool. See lockSearchFloor.
	base := parents[0].IP
	floor, err := lockSearchFloor(ctx, tx, req.PoolKey, req.ScopeDigest, base)
	if err != nil {
		return nil, err
	}

	block, nextFloor, err := boundedFirstFit(ctx, tx, req.PoolKey, req.ScopeDigest, parents, req.PrefixLength, floor)
	if err != nil {
		if errors.Is(err, allocation.ErrPoolExhausted) {
			span.SetAttributes(attribute.Bool(tracing.AttrExhausted, true))
			span.SetStatus(codes.Error, "pool exhausted")
			// A floor is NOT raised here even though the search walked to the end
			// of the pool. It found no free address, so it learned nothing about
			// where the next one will be — and writing the end of the pool would
			// make an exhausted pool permanently exhausted, surviving every later
			// release. raiseSearchFloor refuses a nil floor for the same reason;
			// this comment is here because the call site is where someone would
			// think to add one.
			exhausted, merr := measurePoolForExhaustion(ctx, tx, req.PoolKey, parents, req.ScopeDigest)
			if merr != nil {
				return nil, merr
			}
			exhausted.PoolKey = req.PoolKey
			exhausted.RequestedPrefixLength = req.PrefixLength
			return nil, exhausted
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("compute next prefix: %w", err)
	}

	if err := raiseSearchFloor(ctx, tx, req.PoolKey, req.ScopeDigest, floor, nextFloor); err != nil {
		return nil, err
	}
	return block, nil
}

// measurePoolForExhaustion builds the 507's figures from a whole-pool
// measurement. Only reached when a search failed, which is what makes its cost
// acceptable.
func measurePoolForExhaustion(ctx context.Context, tx pgx.Tx, poolKey string, parents []net.IPNet, scopeDigest string) (*ExhaustionError, error) {
	existing, err := loadAllocationsInScope(ctx, tx, poolKey, scopeDigest)
	if err != nil {
		return nil, err
	}
	m := measurePool(parents, existing)
	return &ExhaustionError{
		UtilizationPercent: m.UtilizationPercent,
	}, nil
}

// parseRequestedBlock accepts either a bare address ("198.51.100.11") or a CIDR
// ("198.51.100.0/24"). A bare address is interpreted at the requested prefix
// length, which for the host-address classes is the full family width.
func parseRequestedBlock(addr string, prefixLen int) (*net.IPNet, error) {
	if strings.Contains(addr, "/") {
		_, ipnet, err := net.ParseCIDR(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a CIDR", ErrAddressInvalid, addr)
		}
		return ipnet, nil
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return nil, fmt.Errorf("%w: %q is not an IP address", ErrAddressInvalid, addr)
	}
	bits := 128
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		bits = 32
	}
	if prefixLen <= 0 || prefixLen > bits {
		prefixLen = bits
	}
	mask := net.CIDRMask(prefixLen, bits)
	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}, nil
}

func blockWithinAny(block net.IPNet, parents []net.IPNet) bool {
	for _, parent := range parents {
		if parent.Contains(block.IP) && allocation.CIDRsOverlap(parent, block) {
			ones, _ := block.Mask.Size()
			parentOnes, _ := parent.Mask.Size()
			if ones >= parentOnes {
				return true
			}
		}
	}
	return false
}

// PoolCarveRecord describes the allocation row a child pool leaves against the
// pool it was carved from.
type PoolCarveRecord struct {
	// AllocationKey is the child pool's own object key. A carve has no separate
	// IPAllocation: the pool object is the record of it, which is also what lets
	// the delete path release the carve by naming the pool.
	AllocationKey string
	// ClassName is the provisioning class for a cascade-provisioned pool, and
	// empty for an operator-authored one, which has no class that built it.
	ClassName string
	// ScopeDigest is recorded but is not what withholds the block: purpose does
	// that. See carveFromPool.
	ScopeDigest string
	// IPFamily is the family the child pool serves, which its parent fixes.
	IPFamily string
	// OwnerProject is the project the child pool belongs to.
	OwnerProject string
}

// CarveChildPool reserves a block for a child pool out of parentPoolKey and
// returns its CIDR.
//
// Both routes to a child pool land here: the cascade's provisionPool, and the
// IPPool registry's Create when spec.parentPoolRef is set. They are the same
// operation seen from the parent — real space leaving the pool for a sub-pool —
// and differ only in who authored the child. The registry used to call
// AllocatePrefix for it instead, which is a claim against an address space, and
// got two things wrong: the row was recorded as purpose Claim, and the
// free-space search was scoped to one address space so the carve could be
// placed on top of a live claim in another. See carveFromPool.
func (a *PostgresPrefixAllocator) CarveChildPool(ctx context.Context, tx pgx.Tx, parentPoolKey string, prefixLen int, rec PoolCarveRecord) (string, error) {
	cidr, err := carveFromPool(ctx, tx, parentPoolKey, prefixLen, rec)
	if err != nil {
		return "", err
	}
	klog.V(2).InfoS("Carved child pool block",
		"parent", parentPoolKey, "cidr", cidr.String(), "child", rec.AllocationKey,
		"class", rec.ClassName, "ownerProject", rec.OwnerProject)
	return cidr.String(), nil
}

// carveFromPool reserves a block for a child pool out of sourcePoolKey.
//
// A child pool's block is recorded with purpose PoolCarve.
//
// It shares the property that matters to the search with a Reservation: neither
// belongs to an address space, so both are excluded from every scope carved from
// the pool rather than only from the one whose claim triggered them. The search
// therefore asks `purpose <> 'Claim'` and needs no third case.
//
// The two are nonetheless distinct, and the delete guard is what needs them to
// be: a pool's own edge reservations go away with the pool, while a carve means a
// child pool still exists and the delete must be refused. Recording both as
// Reservation made every pool carrying a reservation permanently undeletable,
// with an error telling the operator to release claims that did not exist.
func carveFromPool(ctx context.Context, tx pgx.Tx, sourcePoolKey string, prefixLen int, rec PoolCarveRecord) (*net.IPNet, error) {
	pool, err := lockAndDecodeIPPool(ctx, tx, sourcePoolKey)
	if err != nil {
		return nil, err
	}
	parents, err := parsePoolCIDRs(pool)
	if err != nil {
		return nil, err
	}
	// Every allocation in the source pool blocks a child pool's block, whatever
	// address space it belongs to: a sub-pool is real space, not a view of it.
	existing, err := loadAllAllocations(ctx, tx, sourcePoolKey)
	if err != nil {
		return nil, err
	}

	// The carve searches every allocation in the source pool, not a scoped
	// subset — so the state Allocate reports alongside the block describes the
	// whole pool, and the capacity recompute below can use it directly instead
	// of traversing the same free space twice more.
	res, err := allocation.Allocate(parents, existing, prefixLen,
		allocation.Strategy(pool.Spec.Allocation.Strategy), allocation.Reservation{})
	if err != nil {
		if errors.Is(err, allocation.ErrPoolExhausted) {
			// The error names the level that ran out rather than the level that
			// was asked for — an endpoint claim failing because the continent's
			// block is full should say so, and the caller has no other way to
			// learn which level it was.
			return nil, &ExhaustionError{
				PoolKey:               sourcePoolKey,
				RequestedPrefixLength: prefixLen,
				UtilizationPercent:    res.UtilizationPercent,
			}
		}
		return nil, fmt.Errorf("carve /%d from %q: %w", prefixLen, sourcePoolKey, err)
	}
	cidr := res.Block

	if err := insertAllocationRow(ctx, tx, allocationRow{
		AllocationKey: rec.AllocationKey,
		PoolKey:       sourcePoolKey,
		CIDR:          cidr.String(),
		ClaimKey:      nil,
		IPFamily:      rec.IPFamily,
		Purpose:       string(ipamv1alpha1.PurposePoolCarve),
		ClassName:     rec.ClassName,
		ScopeDigest:   rec.ScopeDigest,
		ReclaimPolicy: string(ipamv1alpha1.ReclaimRetain),
		OwnerProject:  rec.OwnerProject,
	}); err != nil {
		return nil, err
	}

	// The figures Allocate already reported describe this pool after the carve,
	// so the capacity write needs no further traversal and no re-read.
	if err := writePoolCapacityFrom(ctx, tx, pool, sourcePoolKey, parents,
		res.UtilizationPercent, append(existing, *cidr)); err != nil {
		return nil, err
	}
	return cidr, nil
}

// ----------------------------------------------------------------------------
// allocation rows
// ----------------------------------------------------------------------------

// allocationRow is one row of ipam_cidr_allocations.
//
// ClaimKey is a pointer because NULL is meaningful and distinct from empty: a
// row with no claim is a reservation or a retained allocation, and both are
// held rather than free.
type allocationRow struct {
	AllocationKey string
	PoolKey       string
	CIDR          string
	ClaimKey      *string
	IPFamily      string
	Purpose       string
	ClassName     string
	ScopeDigest   string
	ReclaimPolicy string
	OwnerProject  string
}

// insertAllocationRow records an allocation.
//
// Note reclaim_policy is a parameter. It was previously the SQL string literal
// 'Delete', with no way for a caller to pass anything else, so every allocation
// was recorded as Delete whatever the claim or class asked for and Retain was
// silently ignored — the release path then deleted the row unconditionally and
// the address was handed to the next claimant.
func insertAllocationRow(ctx context.Context, tx pgx.Tx, row allocationRow) error {
	defer metrics.ObserveQuery("insert_allocation", time.Now())
	_, err := tx.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (allocation_key, pool_key, allocated_cidr, claim_key, ip_family,
		     purpose, class_name, scope_digest, reclaim_policy, owner_project)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		row.AllocationKey, row.PoolKey, row.CIDR, row.ClaimKey, row.IPFamily,
		row.Purpose, row.ClassName, row.ScopeDigest, row.ReclaimPolicy, row.OwnerProject,
	)
	if err != nil {
		// A unique violation here means an allocation row already exists under
		// this key — always a server-side bookkeeping fault rather than anything
		// the caller did, since the caller never chooses an allocation key. It is
		// translated so the driver's SQLSTATE and constraint name do not reach an
		// API client, who can neither act on them nor should learn them.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return fmt.Errorf("%w: an allocation already exists for %q in pool %q",
				ErrAllocationExists, row.AllocationKey, row.PoolKey)
		}
		return fmt.Errorf("insert allocation %q: %w", row.AllocationKey, err)
	}
	return nil
}

// pgUniqueViolation is SQLSTATE 23505.
const pgUniqueViolation = "23505"

// poolAllocation is one row of a pool's allocations, carrying just enough to
// answer both questions asked of it: which address space it belongs to, and
// whether it belongs to one at all.
type poolAllocation struct {
	cidr        net.IPNet
	scopeDigest string
	purpose     string
}

// loadPoolAllocations reads every allocation held against a pool, once.
//
// Both consumers are served from this: the overlap search, which wants the rows
// in one address space, and the capacity recompute, which wants all of them.
// Splitting it into two queries meant scanning and decoding the same rows twice
// per allocation, which is half of what made the cost quadratic in pool
// occupancy.
//
// Deliberately no ORDER BY, having tried it. internal/allocation sorts what it
// is given and a pre-sorted input is that sort's cheap case, so ordering here
// looks free. It is not: no index carries allocated_cidr as a key, so EXPLAIN
// puts an explicit Sort above the scan — quicksort, 298kB at 2000 rows, ~0.9ms
// of a 1.07ms query — once per allocation, on a path already quadratic in pool
// occupancy.
//
// READ THIS BEFORE BENCHMARKING THIS PATH. The plan is not stable within a
// single run: autoanalyze fires partway through and flips later calls between a
// seq scan and an index scan, at a point that moves run to run. alloc-lib
// measured 16 to 1784 seq scans out of ~4000 identical calls. Any baseline
// taken here will wander for that reason, and it looks exactly like noise. Take
// pg_stat_user_tables deltas per run if you need the numbers to mean anything.
//
// That instability is a real hazard, but it does not disorder these rows.
// Measured on a 2000-row table under both plans: zero descents each. The seq
// scan returns heap order, and the index scan is on pool_key alone, so within
// one pool it returns heap order too. A sequential run writes ascending, so
// both hand back ascending.
//
// Churn could break that — reclaimed heap slots stop tracking address order —
// and if it ever does, the fix is a (pool_key, allocated_cidr) btree, which
// yields ordered rows with no sort node and pins the plan at the same time. A
// per-call sort relocates the variance rather than removing it.
func loadPoolAllocations(ctx context.Context, tx pgx.Tx, poolKey string) ([]poolAllocation, error) {
	defer metrics.ObserveQuery("load_pool_allocations", time.Now())
	rows, err := tx.Query(ctx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr), scope_digest, purpose
		   FROM ipam_cidr_allocations
		  WHERE pool_key = $1`, poolKey)
	if err != nil {
		return nil, fmt.Errorf("load allocations for %q: %w", poolKey, err)
	}
	defer rows.Close()

	var out []poolAllocation
	for rows.Next() {
		var cidrStr string
		var a poolAllocation
		if err := rows.Scan(&cidrStr, &a.scopeDigest, &a.purpose); err != nil {
			return nil, fmt.Errorf("scan allocation row: %w", err)
		}
		_, ipnet, perr := net.ParseCIDR(cidrStr)
		if perr != nil {
			return nil, fmt.Errorf("parse stored cidr %q: %w", cidrStr, perr)
		}
		a.cidr = *ipnet
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate allocation rows: %w", err)
	}
	return out, nil
}

// inScope narrows a pool's allocations to those a new allocation in scopeDigest
// must not overlap.
//
// This is the class model's central rule, and it used to be a SQL predicate:
// a Claim row belongs to one address space and only blocks allocations in that
// space, so two networks may both hold 10.128.0.2 out of one shared range.
// Everything else the pool holds — its reservations, and the carves backing
// child pools — blocks *every* space, because that space is really gone.
//
// Getting it wrong in either direction is quiet. Scoping the reservations too
// would hand the subnet gateway to the second network that asked; not scoping
// the claims would refuse addresses the model was designed to allow, surfacing
// as unexplained exhaustion.
func inScope(all []poolAllocation, scopeDigest string) []net.IPNet {
	out := make([]net.IPNet, 0, len(all))
	for _, a := range all {
		if a.purpose != string(ipamv1alpha1.PurposeClaim) || a.scopeDigest == scopeDigest {
			out = append(out, a.cidr)
		}
	}
	return out
}

// cidrsOf drops the scope metadata, for the consumers that want every block.
func cidrsOf(all []poolAllocation) []net.IPNet {
	out := make([]net.IPNet, len(all))
	for i, a := range all {
		out[i] = a.cidr
	}
	return out
}

// loadAllocationsInScope returns the blocks a new allocation in scopeDigest
// must not overlap.
//
// The predicate is the class model's central rule expressed in SQL. A Claim row
// belongs to one address space and only blocks allocations in that space, so
// two networks may both hold 10.128.0.2 out of one shared per-location range.
// Everything else the pool holds — its reservations, and the blocks carved out
// of it for child pools — blocks *every* space, because that space is really
// gone. The design states it directly: the parent holds the reservation and
// excludes it from every space carved from that parent, whatever uniqueWithin
// says.
//
// Getting this backwards in either direction is quiet rather than loud. Scoping
// the reservations too would hand out the subnet gateway to the second network
// that asked; not scoping the claims would refuse addresses the model was
// designed to allow, and surface as unexplained exhaustion.
//
// # Do not add an owner_project term to this predicate
//
// It looks like it is missing one. Two projects drawing from one shared
// platform pool are two address spaces and the predicate names no owner, which
// reads as the tenancy hole it used to be. It is not: the tenant is INSIDE the
// digest (see internal/scope), so `scope_digest = $2` already means "this
// tenant's space" and an owner term would be redundant here and wrong next
// door. The exclusion constraint on (pool_key, scope_digest, allocated_cidr) is
// what actually enforces non-overlap, and it cannot grow an owner column
// without being dropped and rebuilt over every allocation in the service — so
// the two would disagree, with the search narrower than the constraint. That
// disagreement presents as spurious exhaustion, not as an error.
func loadAllocationsInScope(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string) ([]net.IPNet, error) {
	defer metrics.ObserveQuery("load_allocations_in_scope", time.Now())
	return queryAllocationCIDRs(ctx, tx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE pool_key = $1
		    AND (purpose <> 'Claim' OR scope_digest = $2)`,
		poolKey, scopeDigest)
}

// loadAllAllocations returns every block held against a pool, in any address
// space. Used where the question is how much of the pool is really gone —
// capacity, and carving space for a child pool.
func loadAllAllocations(ctx context.Context, tx pgx.Tx, poolKey string) ([]net.IPNet, error) {
	defer metrics.ObserveQuery("load_existing_allocations", time.Now())
	return queryAllocationCIDRs(ctx, tx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE pool_key = $1`,
		poolKey)
}

func queryAllocationCIDRs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]net.IPNet, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("load allocations: %w", err)
	}
	defer rows.Close()

	var existing []net.IPNet
	for rows.Next() {
		var cidrStr string
		if err := rows.Scan(&cidrStr); err != nil {
			return nil, fmt.Errorf("scan allocation row: %w", err)
		}
		_, ipnet, err := net.ParseCIDR(cidrStr)
		if err != nil {
			return nil, fmt.Errorf("parse stored cidr %q: %w", cidrStr, err)
		}
		existing = append(existing, *ipnet)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate allocation rows: %w", err)
	}
	return existing, nil
}

// ----------------------------------------------------------------------------
// release
// ----------------------------------------------------------------------------

// ReleaseOutcome reports what became of one allocation when its claim was
// released. The caller needs it to keep the IPAllocation API object in step:
// a deleted allocation's object is removed, a retained one's object survives
// with its claim reference cleared.
type ReleaseOutcome struct {
	// AllocationKey is the storage key of the IPAllocation object.
	AllocationKey string
	// PoolKey is the pool the block came from.
	PoolKey string
	// CIDR is the block.
	CIDR string
	// Retained reports that the allocation survived: still held, still counted
	// against its holder, and still recorded — but no longer bound to a claim.
	Retained bool
}

// Release disposes of the allocations bound to claimKey according to the policy
// each was recorded with.
//
// Retention works by *not unbinding*, which is the property the whole design
// turns on. The row survives with claim_key cleared and its owner intact, so
// the address is continuously held: there is no window in which it is loose,
// nothing has to reconstruct who used to hold it, and it never lands in the
// state the storage model calls Released and needs an operator to clear by
// hand. Implementing retention as release-then-rebind would produce all three
// of those problems.
//
// Before this, reclaim policy was ignored entirely: rows were written with a
// hardcoded 'Delete' and this method issued an unconditional DELETE, so a claim
// asking for Retain lost its address to the next claimant with nothing in the
// API to show it had happened.
func (a *PostgresPrefixAllocator) Release(ctx context.Context, tx pgx.Tx, claimKey string) ([]ReleaseOutcome, error) {
	defer metrics.ObserveQuery("release_allocation", time.Now())

	rows, err := tx.Query(ctx,
		`SELECT allocation_key, pool_key,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr),
		        ip_family, reclaim_policy
		   FROM ipam_cidr_allocations
		  WHERE claim_key = $1`,
		claimKey,
	)
	if err != nil {
		return nil, fmt.Errorf("load allocations for release: %w", err)
	}
	type bound struct {
		allocationKey string
		poolKey       string
		cidr          string
		ipFamily      string
		policy        string
	}
	var found []bound
	for rows.Next() {
		var b bound
		if err := rows.Scan(&b.allocationKey, &b.poolKey, &b.cidr, &b.ipFamily, &b.policy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan allocation for release: %w", err)
		}
		found = append(found, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate allocations for release: %w", err)
	}

	// LOCK THE POOLS BEFORE TOUCHING THEIR ALLOCATIONS, and in a deterministic
	// order. Both halves of that are load-bearing and neither is obvious.
	//
	// ORDER AGAINST THE ALLOCATOR. AllocatePrefix locks the pool row first and
	// writes ipam_pool_search_floor later; deleting an allocation fires the
	// trigger from migration 009, which UPDATEs that same floor row. If the
	// deletes ran first, release would hold the floor row and want the pool
	// while allocate held the pool and wanted the floor — opposite orders, and
	// Postgres resolves that by killing one of them with a 40P01 the caller
	// sees as a 500. Taking the pool lock first makes both paths pool-then-floor.
	//
	// ORDER AMONG THEMSELVES. A claim can hold allocations in more than one
	// pool, so two concurrent releases could take the same two pool locks in
	// opposite orders. Sorting the keys gives every caller one order.
	//
	// This also reads as more correct independently of deadlocks: the pool row
	// lock is what serialises mutations to a pool, and its allocations are part
	// of the pool's state.
	poolsToLock := make([]string, 0, len(found))
	seenPool := make(map[string]bool, len(found))
	for _, b := range found {
		if b.policy == string(ipamv1alpha1.ReclaimRetain) || seenPool[b.poolKey] {
			continue
		}
		seenPool[b.poolKey] = true
		poolsToLock = append(poolsToLock, b.poolKey)
	}
	sort.Strings(poolsToLock)
	lockedPools := make(map[string]*ipamv1alpha1.IPPool, len(poolsToLock))
	for _, poolKey := range poolsToLock {
		pool, perr := lockAndDecodeIPPool(ctx, tx, poolKey)
		if perr != nil {
			// Pool already gone (cascading delete); nothing to lock or publish.
			if errors.Is(perr, ErrPoolNotFound) {
				continue
			}
			return nil, fmt.Errorf("lock pool before release: %w", perr)
		}
		lockedPools[poolKey] = pool
	}

	outcomes := make([]ReleaseOutcome, 0, len(found))
	touchedPools := make(map[string]string, len(found))
	for _, b := range found {
		retain := b.policy == string(ipamv1alpha1.ReclaimRetain)
		if retain {
			// Unbind without releasing. owner_project is deliberately left
			// alone: a retained address must keep an attributable holder, or it
			// counts against nobody's budget and nothing pressures anyone to
			// hand it back.
			if _, err := tx.Exec(ctx,
				`UPDATE ipam_cidr_allocations SET claim_key = NULL WHERE allocation_key = $1`,
				b.allocationKey,
			); err != nil {
				return nil, fmt.Errorf("retain allocation %q: %w", b.allocationKey, err)
			}
			metrics.RecordReclaim("Retain")
		} else {
			if _, err := tx.Exec(ctx,
				`DELETE FROM ipam_cidr_allocations WHERE allocation_key = $1`,
				b.allocationKey,
			); err != nil {
				return nil, fmt.Errorf("release allocation %q: %w", b.allocationKey, err)
			}
			// Only a delete returns capacity; a retained block is still held,
			// so the pool's utilization must not move.
			touchedPools[b.poolKey] = b.ipFamily
			metrics.RecordReclaim("Delete")
		}
		outcomes = append(outcomes, ReleaseOutcome{
			AllocationKey: b.allocationKey,
			PoolKey:       b.poolKey,
			CIDR:          b.cidr,
			Retained:      retain,
		})
	}

	// The pools are already locked and decoded above; re-locking here would be
	// harmless but re-reading would not, because the deletes have happened since.
	for poolKey := range touchedPools {
		pool, ok := lockedPools[poolKey]
		if !ok {
			// Pool vanished between the two passes, or was never lockable.
			continue
		}
		parents, perr := parsePoolCIDRs(pool)
		if perr != nil {
			return nil, fmt.Errorf("parse pool cidr after release: %w", perr)
		}
		if perr := refreshPoolCapacity(ctx, tx, pool, poolKey, parents); perr != nil {
			return nil, perr
		}
	}
	return outcomes, nil
}

// ForceRelease releases an allocation by its own key rather than through a
// claim, which is what a retained allocation needs: it has no claim left to
// release it through.
//
// The design requires a retained address to return to a claimable state without
// an operator in the path for the ordinary case, and to be force-releasable
// with an audit record for the extraordinary one. This is the mechanism for
// both; the audit record is the DELETED changelog entry the caller writes for
// the IPAllocation object.
func (a *PostgresPrefixAllocator) ForceRelease(ctx context.Context, tx pgx.Tx, allocationKey string) (bool, error) {
	defer metrics.ObserveQuery("force_release_allocation", time.Now())

	// The pool row is locked BEFORE the delete, for the reason spelled out in
	// Release: deleting an allocation fires migration 009's trigger, which
	// updates ipam_pool_search_floor, and AllocatePrefix takes the pool lock
	// first and the floor second. Deleting first would give this path the
	// opposite order and deadlock against a concurrent claim on the same pool.
	//
	// It costs an extra read to find the pool the allocation belongs to, which
	// the DELETE ... RETURNING used to supply for free. That is the price of a
	// consistent lock order and it is worth paying on a path that is already
	// the rare one.
	var poolKey, ipFamily string
	if err := tx.QueryRow(ctx,
		`SELECT pool_key, ip_family FROM ipam_cidr_allocations WHERE allocation_key = $1`,
		allocationKey,
	).Scan(&poolKey, &ipFamily); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("look up allocation %q for force release: %w", allocationKey, err)
	}
	if _, perr := lockAndDecodeIPPool(ctx, tx, poolKey); perr != nil && !errors.Is(perr, ErrPoolNotFound) {
		return false, fmt.Errorf("lock pool before force release: %w", perr)
	}

	err := tx.QueryRow(ctx,
		`DELETE FROM ipam_cidr_allocations WHERE allocation_key = $1
		 RETURNING pool_key, ip_family`,
		allocationKey,
	).Scan(&poolKey, &ipFamily)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not an error — a root pool has no carve, and a double delete is
			// idempotent. But it is reported, because for a caller that expected
			// a row this is the failure mode that hides: a mistyped or drifted
			// allocation key makes the release a no-op, the delete succeeds, and
			// the address stays consumed with nothing naming it. Callers that
			// know a row should exist must say so rather than discard this.
			return false, nil
		}
		return false, fmt.Errorf("force-release allocation %q: %w", allocationKey, err)
	}

	pool, perr := lockAndDecodeIPPool(ctx, tx, poolKey)
	if perr != nil {
		if errors.Is(perr, ErrPoolNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("reload pool after force release: %w", perr)
	}
	parents, perr := parsePoolCIDRs(pool)
	if perr != nil {
		return false, fmt.Errorf("parse pool cidr after force release: %w", perr)
	}
	if perr := refreshPoolCapacity(ctx, tx, pool, poolKey, parents); perr != nil {
		return false, perr
	}
	return true, nil
}

// ----------------------------------------------------------------------------
// capacity
// ----------------------------------------------------------------------------

// refreshPoolCapacity recomputes the pool's utilization from every allocation
// held against it and writes the updated object back (+ MODIFIED changelog)
// inside the current transaction.
//
// It re-reads the allocations rather than adjusting the previous figure by the
// block just written, because capacity counts every address space at once while
// the caller only has the one it was working in. Deriving it from a partial set
// would drift a little on every claim.
func refreshPoolCapacity(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, poolKey string, parents []net.IPNet) error {
	all, err := loadAllAllocations(ctx, tx, poolKey)
	if err != nil {
		return fmt.Errorf("reload allocations for capacity: %w", err)
	}
	return writePoolCapacity(ctx, tx, pool, poolKey, parents, all)
}

// writePoolCapacity is refreshPoolCapacity for a caller that already holds the
// allocation set — which the allocation path does, having just loaded it and
// appended the block it wrote. Split out so that path does not re-read rows it
// has in hand.
func writePoolCapacity(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, poolKey string, parents, all []net.IPNet) error {
	setPoolCapacityStatus(pool, parents, all)
	return persistPoolCapacity(ctx, tx, pool, poolKey, parents, all)
}

// writePoolCapacityFrom is writePoolCapacity for a caller that also already
// holds the two computed figures — which the carve path does, because its search
// covers the whole pool and allocation.Allocate reports them alongside the block
// it chose.
//
// The address counts still come from the allocation set, because those are a
// different question: how much space exists and how much is spoken for, which
// the free-space traversal does not answer.
func writePoolCapacityFrom(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, poolKey string, parents []net.IPNet, utilization float64, all []net.IPNet) error {
	total, allocated, available := CapacityFor(parents, all)
	pool.Status.Capacity = ipamv1alpha1.PoolCapacity{Total: total, Allocated: allocated, Available: available}
	pool.Status.UtilizationPercent = utilization
	pool.Status.IPFamily = ipamv1alpha1.IPFamily(effectivePoolFamily(pool))
	return persistPoolCapacity(ctx, tx, pool, poolKey, parents, all)
}

// persistPoolCapacity writes the pool object back and refreshes the gauge, for
// callers that have already populated its status.
func persistPoolCapacity(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, poolKey string, parents, all []net.IPNet) error {
	data, err := encodeObject(pool)
	if err != nil {
		return fmt.Errorf("marshal pool: %w", err)
	}
	if _, err := updateObject(ctx, tx, poolKey, data); err != nil {
		return fmt.Errorf("write pool: %w", err)
	}
	// Status is written for every pool; the gauge is not. See
	// publishPrefixUtilization.
	publishPrefixUtilization(pool, poolKey, effectivePoolFamily(pool), parents, all)
	return nil
}

// setPoolCapacityStatus populates every utilization field on the pool's status
// from the parent ranges and current allocations. The Capacity counts are EXACT
// decimal strings — they used to be int64s that saturated at MaxInt64, which is
// a ceiling rather than a count — and UtilizationPercent is computed with
// arbitrary-precision arithmetic from the same measurement, so the four figures
// cannot disagree.
func setPoolCapacityStatus(pool *ipamv1alpha1.IPPool, parents, allocations []net.IPNet) {
	v := PoolCapacityFor(parents, allocations)
	pool.Status.Capacity = ipamv1alpha1.PoolCapacity{
		Total:     v.Total,
		Allocated: v.Allocated,
		Available: v.Available,
	}

	pool.Status.UtilizationPercent = v.UtilizationPercent
	pool.Status.IPFamily = ipamv1alpha1.IPFamily(effectivePoolFamily(pool))
}

// effectivePoolFamily returns a pool's address family: spec.ipFamily when set,
// otherwise derived from the carved status.allocatedCIDR. Returns the empty
// string when neither is resolvable.
func effectivePoolFamily(pool *ipamv1alpha1.IPPool) string {
	if pool.Spec.IPFamily != "" {
		return string(pool.Spec.IPFamily)
	}
	cidr := pool.Status.AllocatedCIDR
	if cidr == "" {
		cidr = pool.Spec.CIDR
	}
	if cidr == "" {
		return ""
	}
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	if ip.To4() != nil {
		return string(ipamv1alpha1.IPv4)
	}
	return string(ipamv1alpha1.IPv6)
}

// ----------------------------------------------------------------------------
// object helpers
// ----------------------------------------------------------------------------

// InsertObject implements PrefixAllocator.InsertObject.
func (a *PostgresPrefixAllocator) InsertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error) {
	return insertObject(ctx, tx, key, kind, namespace, name, data)
}

// insertObject writes an API object row and its ADDED changelog entry in one
// transaction. The RETURNING clause hands back the rv the sequence default
// assigned, so the caller can stamp it on the in-memory object before
// responding to the API client.
func insertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error) {
	defer metrics.ObserveQuery("insert_object", time.Now())
	labels := labelsFromData(data)
	var rv int64
	err := tx.QueryRow(ctx,
		`INSERT INTO ipam_objects (key, kind, namespace, name, data, labels)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING resource_version`,
		key, kind, namespace, name, data, labels,
	).Scan(&rv)
	if err != nil {
		// A unique violation on ipam_objects_pkey means an object of this name
		// already exists in this project. Translated here, at the boundary, so no
		// call site can leak the driver's SQLSTATE and constraint name to an API
		// client — see ErrObjectExists.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return 0, fmt.Errorf("%w: %s %q", ErrObjectExists, kind, name)
		}
		return 0, fmt.Errorf("insert object %q: %w", key, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ipam_changelog (key, resource_version, event_type, data)
		 VALUES ($1, $2, 'ADDED', $3)`,
		key, rv, data,
	); err != nil {
		return 0, fmt.Errorf("insert changelog for %q: %w", key, err)
	}
	return rv, nil
}

// labelsFromData extracts metadata.labels from a JSON-encoded API object.
func labelsFromData(data []byte) []byte {
	var obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &obj); err != nil || len(obj.Metadata.Labels) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(obj.Metadata.Labels)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// DeleteObject implements PrefixAllocator.DeleteObject.
func (a *PostgresPrefixAllocator) DeleteObject(ctx context.Context, tx pgx.Tx, key string) (int64, error) {
	return deleteObject(ctx, tx, key)
}

// UpdateObject implements PrefixAllocator.UpdateObject.
func (a *PostgresPrefixAllocator) UpdateObject(ctx context.Context, tx pgx.Tx, key string, data []byte) (int64, error) {
	return updateObject(ctx, tx, key, data)
}

// updateObject rewrites the data column for an existing ipam_objects row,
// allocates a fresh resource_version, and inserts a MODIFIED changelog entry in
// the same transaction so watchers observe the update.
func updateObject(ctx context.Context, tx pgx.Tx, key string, data []byte) (int64, error) {
	defer metrics.ObserveQuery("update_object", time.Now())
	labels := labelsFromData(data)
	var rv int64
	err := tx.QueryRow(ctx,
		`UPDATE ipam_objects
		    SET resource_version = nextval('ipam_resource_version_seq'),
		        data = $1,
		        labels = $2,
		        updated_at = NOW()
		  WHERE key = $3
		  RETURNING resource_version`,
		data, labels, key,
	).Scan(&rv)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("update object %q: not found", key)
		}
		return 0, fmt.Errorf("update object %q: %w", key, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ipam_changelog (key, resource_version, event_type, data)
		 VALUES ($1, $2, 'MODIFIED', $3)`,
		key, rv, data,
	); err != nil {
		return 0, fmt.Errorf("insert changelog for %q: %w", key, err)
	}
	return rv, nil
}

// deleteObject removes the ipam_objects row and inserts a DELETED changelog
// entry carrying the pre-delete payload as a tombstone, atomically. An object
// that was already gone yields no rows and returns (0, nil) so callers can
// distinguish "already deleted" from a real failure.
func deleteObject(ctx context.Context, tx pgx.Tx, key string) (int64, error) {
	var rv int64
	err := tx.QueryRow(ctx,
		`WITH deleted AS (
		     DELETE FROM ipam_objects WHERE key = $1 RETURNING data
		 )
		 INSERT INTO ipam_changelog (key, resource_version, event_type, data)
		 SELECT $1, nextval('ipam_resource_version_seq'), 'DELETED', data FROM deleted
		 RETURNING resource_version`,
		key,
	).Scan(&rv)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("delete object %q: %w", key, err)
	}
	return rv, nil
}

// ----------------------------------------------------------------------------
// pool helpers
// ----------------------------------------------------------------------------

// lockAndDecodeIPPool acquires a row-level lock on the pool row and decodes it.
//
// Locking one row per pool regardless of how full the pool is, is the property
// the whole allocation path rests on: the lock cost is O(1) in pool size, and
// there is no phantom-read window for a concurrent claim to slip an overlapping
// block through.
func lockAndDecodeIPPool(ctx context.Context, tx pgx.Tx, poolKey string) (*ipamv1alpha1.IPPool, error) {
	defer metrics.ObserveQuery("select_pool_for_update", time.Now())
	var data []byte
	err := tx.QueryRow(ctx,
		`SELECT data FROM ipam_objects WHERE key = $1 FOR UPDATE`,
		poolKey,
	).Scan(&data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPoolNotFound
		}
		return nil, fmt.Errorf("lock pool row: %w", err)
	}

	var pool ipamv1alpha1.IPPool
	if err := decodeObject(data, &pool); err != nil {
		return nil, fmt.Errorf("decode pool object: %w", err)
	}
	return &pool, nil
}

// parsePoolCIDRs returns the ranges a pool hands out from, as the slice shape
// the allocation library takes.
//
// It is a slice because FindFirstAvailableBlock searches across several ranges
// and the multi-range case is real — a pool assembling capacity from more than
// one block is the natural way to add space to a class without renumbering. The
// API cannot express it today: IPPoolSpec carries a single `cidr`, so this
// always returns one element. That is a limit of the type rather than of the
// search, and the shape here is what lets the type gain a `cidrs` field without
// touching the allocation path.
//
// A carved child pool's real extent is what it was given, so
// status.allocatedCIDR wins over whatever the spec asked for.
func parsePoolCIDRs(pool *ipamv1alpha1.IPPool) ([]net.IPNet, error) {
	var sources []string
	if pool.Status.AllocatedCIDR != "" {
		sources = append(sources, pool.Status.AllocatedCIDR)
	} else if pool.Spec.CIDR != "" {
		sources = append(sources, pool.Spec.CIDR)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("pool has no CIDR")
	}

	parents := make([]net.IPNet, 0, len(sources))
	for _, s := range sources {
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("parse pool CIDR %q: %w", s, err)
		}
		parents = append(parents, *ipnet)
	}
	return normalizeParents(parents)
}

// normalizeParents enforces the two invariants every consumer of a parent slice
// depends on, in the one place the slice is built.
//
// **Sorted ascending by address.** FindFirstAvailableBlock scans parents in
// slice order rather than address order — there is no sort inside it — so
// FirstFit over ["10.0.2.0/24", "10.0.0.0/24"] hands out 10.0.2.0/26 while the
// lower range sits empty. ReservedBlocks, by contrast, orders by address: it
// counts `leading` from the start of the lowest range. Unsorted, the two
// disagree, and a reservation lands at the head of a range the search will not
// reach until everything above it is full. Sorting here makes them agree by
// construction rather than by each caller remembering.
//
// **Non-overlapping.** Free regions are computed per parent, so an overlapping
// pair offers the same block twice and two claims can be handed the same space.
// The search does not detect this; ReservedBlocks rejects it, which means an
// overlapping pool would fail only if it happened to carry a reservation. It is
// rejected here so the failure does not depend on that.
//
// Both are enforced here rather than at each call site because a parent slice
// that is wrong is wrong for every consumer, and the invariant is cheap to hold
// at construction and impossible to remember at use.
func normalizeParents(parents []net.IPNet) ([]net.IPNet, error) {
	if len(parents) < 2 {
		return parents, nil
	}
	sort.Slice(parents, func(i, j int) bool {
		return bytes.Compare(parents[i].IP.To16(), parents[j].IP.To16()) < 0
	})
	for i := 1; i < len(parents); i++ {
		if allocation.CIDRsOverlap(parents[i-1], parents[i]) {
			return nil, fmt.Errorf("pool ranges %s and %s overlap: overlapping ranges would offer the same block to two claims",
				parents[i-1].String(), parents[i].String())
		}
	}
	return parents, nil
}

// publishPrefixUtilization updates the per-pool capacity gauges — but only for
// operator-authored pools.
//
// # Why cascade-provisioned pools are excluded
//
// `pool_key` is a gauge label, so every pool that reaches this becomes a
// permanent time series. Operator-authored pools are a bounded set: a human
// writes each one. Cascade-provisioned pools are not — there is one per (class,
// scope combination), so the series count grows with the number of networks and
// locations a platform serves, without bound and without ever being reclaimed. A
// deleted pool's series outlives it for the process lifetime.
//
// Excluding them costs nothing that was ever being used, because the alerts
// built on these gauges are about capacity a *platform operator* acts on —
// "coordinate with the pool owner to expand ranges". Nobody pages the platform
// because one tenant's /64 subnet filled up. That condition is already covered,
// with bounded labels, by ipam_allocation_failures_total{reason="pool_exhausted"}
// keyed on class and project, and exactly per-pool by the API itself: every pool
// carries utilizationPercent and largestFreePrefix in its status, which this
// function's caller has just written.
//
// That split is the same reasoning the design applies to class health — computed
// at query time, never maintained during allocation — arriving at the metrics
// layer. A per-pool gauge over an unbounded pool set is the monitoring-side
// version of the counter the design rejects.
//
// Both sums are computed in big.Int so very large IPv6 pools do not overflow
// int64; the final division is converted to float64 for the gauge. A
// zero-capacity pool publishes 0 rather than NaN — the gauge is documented as a
// ratio in [0, 1] and dashboards assume a finite value.
func publishPrefixUtilization(pool *ipamv1alpha1.IPPool, poolKey, ipFamily string, parents, allocated []net.IPNet) {
	PublishPoolCapacity(poolKey, ipFamily, pool != nil && pool.Spec.ClassRef != nil, parents, allocated)
}

// PublishPoolCapacity refreshes the per-pool capacity gauges.
//
// Exported because a pool's capacity is written from two places that cannot
// share code: the allocation path here, which works on the versioned type, and
// the IPPool registry, which works on the internal one. The arithmetic was
// centralised into PoolCapacityFor for exactly that reason — and the
// *publication* was left duplicated in the same shape it warned about, so a
// pool created and never claimed carried a correct status and no series at all.
// A pool sitting at 0% was invisible rather than visibly empty.
//
// Both writers now call this. It takes primitives rather than a pool object
// precisely so it can serve both types, and `cascadeProvisioned` is a bool
// rather than a re-derived spec check so neither caller can accidentally
// disagree with the other about what the exclusion means.
//
// Call it only from a path that has committed, or is about to. It is not safe
// on a dry-run or validation path: moving a gauge because someone asked what
// *would* happen is a worse defect than the missing series it fixes.
func PublishPoolCapacity(poolKey, ipFamily string, cascadeProvisioned bool, parents, allocated []net.IPNet) {
	if cascadeProvisioned {
		// The allocator built this pool for one scope, and there is no bound on
		// how many such scopes exist.
		metrics.RecordProvisionedPoolCapacitySkipped()
		return
	}

	project, org := tenantsFromPoolKey(poolKey)
	// Free-derived, matching status.capacity — the gauge is what the exhaustion
	// alerts fire on, so it must not be the one place that still double-counts
	// overlapping allocations from different address spaces.
	m := measurePool(parents, allocated)
	total, used := m.Total, m.Consumed

	usedF, _ := new(big.Float).SetInt(used).Float64()
	totalF, _ := new(big.Float).SetInt(total).Float64()
	// Absolute counters are published alongside the ratio so dashboards can
	// distinguish small/full pools from large/half-full pools at a glance.
	// Float64 rather than int64: a /48 IPv6 pool has 2^80 addresses.
	metrics.SetPoolCapacity(poolKey, ipFamily, "ippools", project, org, totalF, usedF)
	if total.Sign() == 0 {
		metrics.SetPoolUtilization(poolKey, ipFamily, "ippools", project, org, 0)
		return
	}
	metrics.SetPoolUtilization(poolKey, ipFamily, "ippools", project, org, usedF/totalF)
}

// sumAddresses is deliberately gone. It added up allocation sizes, which is the
// arithmetic that reported a /28 shared by eight networks as half full — see
// PoolCapacityFor. Every figure now comes from allocation.Measure, and there is
// no summing helper left in THIS package for a future caller to reach for.
//
// "This package" is load-bearing and was read as "anywhere" once, which produced
// a task to delete something that must not be deleted. allocation.UtilizationPercent
// still exists and still sums. It is retained on purpose: it is the reference the
// benchmarks measure against, and the contrast that proves the fix — a test
// asserting Measure says 6% where summing says 50% is only meaningful while
// something still computes the 50%. It carries its own prohibition; read its doc
// before touching it.

// newExhaustionError assembles the exhaustion report from state already in hand.
//
// The two figures are computed rather than read from the pool's stored status
// because the status reflects the last committed write, while this describes the
// pool as the failing search saw it — inside the lock, including anything this
// transaction has already written. A caller told "0% utilized, and also full"
// would reasonably conclude the service was lying.
func newExhaustionError(poolKey string, prefixLen int, parents, existing []net.IPNet) error {
	m := measurePool(parents, existing)
	return &ExhaustionError{
		PoolKey:               poolKey,
		RequestedPrefixLength: prefixLen,
		UtilizationPercent:    m.UtilizationPercent,
	}
}

// CapacityFor computes the address-count view of a pool.
//
// Exported and returning plain integers so the IPPool registry can use the same
// arithmetic on the internal type without either package converting the other's
// struct — the two had diverged copies of this, and one of them was wrong.
//
// The counts are exact whenever the pool's total fits in an int64, which covers
// every IPv4 pool and most IPv6 ones. Beyond that the field cannot hold the
// truth, and the interesting question is what to report instead.
//
// The previous answer — saturate each sum independently at MaxInt64 — produced
// the worst possible output. A /20 IPv6 pool with a single /48 carved out has an
// allocated count of 2^80, which saturated to the same MaxInt64 as the total, so
// the pool reported `available: 0`: a completely empty pool rendered as
// completely full. Anything reading capacity (an inventory view, a "worst
// location" alert) would page someone about a pool at 0% utilization.
//
// So when the total overflows, both figures are scaled by the same factor to
// land total on MaxInt64. The counts stop being address counts and become a
// proportion — which is all they could have been at that magnitude — but they
// stay mutually consistent, allocated + available still equals total, and an
// empty pool reads as empty. UtilizationPercent and LargestFreePrefix alongside
// are computed with arbitrary precision and remain exactly right; they are the
// fields to trust for wide IPv6 space, and this one no longer contradicts them.
func CapacityFor(parents, allocations []net.IPNet) (total, allocated, available string) {
	v := PoolCapacityFor(parents, allocations)
	return v.Total, v.Allocated, v.Available
}

// PoolCapacityView is every capacity figure a pool's status carries, all read
// off one measurement so they cannot disagree with each other.
type PoolCapacityView struct {
	// Exact decimal counts. See ipamv1alpha1.PoolCapacity for why they are
	// strings rather than int64.
	Total              string
	Allocated          string
	Available          string
	UtilizationPercent float64
}

// PoolCapacityFor measures how much of a pool is actually consumed.
//
// Consumption is derived from the pool's remaining free space — total minus
// what is still allocatable — rather than by summing the allocation rows. For a
// pool whose rows are disjoint and inside the parents the two agree. Where they
// differ, only this one can be right, and the class model makes them differ
// routinely.
//
// A pool serving a class with `uniqueWithin: [network]` holds allocations from
// many address spaces at once, and rows in different spaces are *supposed* to
// overlap: eight networks may each hold 10.71.0.0/32 out of one shared range,
// and exactly one address is gone. Summing counted that address eight times and
// reported a /28 as 50% full. The error scales with tenant count, so it is
// worst on precisely the shared per-location IPv4 ranges an operator most needs
// to trust — and it reads as a pool filling up, which is the shape of an alert.
//
// The contradiction was visible inside a single status block: largestFreePrefix
// is computed from free space and said /29, while utilizationPercent said 50%.
// The free-space machinery already knew the truth; the counts disagreed with
// it. All four figures now come from that same measurement.
//
// The counts are exact whenever the pool's total fits in an int64, which covers
// every IPv4 pool and most IPv6 ones. Beyond that the field cannot hold the
// truth, and the interesting question is what to report instead.
//
// The previous answer — saturate each sum independently at MaxInt64 — produced
// the worst possible output. A /20 IPv6 pool with a single /48 carved out has an
// allocated count of 2^80, which saturated to the same MaxInt64 as the total, so
// the pool reported `available: 0`: a completely empty pool rendered as
// completely full. Anything reading capacity (an inventory view, a "worst
// location" alert) would page someone about a pool at 0% utilization.
//
// So when the total overflows, both figures are scaled by the same factor to
// land total on MaxInt64. The counts stop being address counts and become a
// proportion — which is all they could have been at that magnitude — but they
// stay mutually consistent, allocated + available still equals total, and an
// empty pool reads as empty. UtilizationPercent and LargestFreePrefix alongside
// are computed with arbitrary precision and remain exactly right; they are the
// fields to trust for wide IPv6 space, and this one no longer contradicts them.
func PoolCapacityFor(parents, allocations []net.IPNet) PoolCapacityView {
	return capacityFrom(measurePool(parents, allocations))
}

// measurePool is allocation.Measure for a caller with no unmaterialised
// reservations, which is every caller here: a pool's reservations are written
// as allocation rows when it is provisioned, so they arrive in allocations.
//
// Measure's only error path is a malformed Reservation, and the zero
// Reservation passed here cannot be malformed. Logged rather than swallowed
// anyway, because the alternative to a real measurement is a pool reporting
// itself empty, and that must not pass silently.
func measurePool(parents, allocations []net.IPNet) allocation.PoolMeasurement {
	m, err := allocation.Measure(parents, allocations, allocation.Reservation{})
	if err != nil {
		klog.ErrorS(err, "Measuring pool free space; capacity will read as empty")
		return allocation.PoolMeasurement{
			Total:    new(big.Int),
			Consumed: new(big.Int),
			Free:     new(big.Int),
		}
	}
	return m
}

// capacityFrom reads an exact measurement into the status view.
//
// The measurement is the single source: the counts and the percentage are two
// readings of it rather than two computations, so they cannot drift apart.
//
// Nothing is clamped any more. These fields used to be int64 and saturated at
// MaxInt64, which meant a wide IPv6 pool reported a ceiling rather than a
// count — and the ratio had to be preserved by scaling consumed down against
// the same ceiling, so both numbers were fictions that happened to divide
// correctly. They are decimal strings now and carry the real values.
func capacityFrom(m allocation.PoolMeasurement) PoolCapacityView {
	totalBig, consumedBig := m.Total, m.Consumed
	if totalBig == nil || consumedBig == nil || totalBig.Sign() == 0 {
		return PoolCapacityView{}
	}
	if consumedBig.Cmp(totalBig) > 0 {
		// More consumed than exists means overlapping allocations were counted
		// against a parent set that does not contain them. Clamp rather than
		// report a negative remainder.
		consumedBig = totalBig
	}
	return PoolCapacityView{
		Total:              totalBig.String(),
		Allocated:          consumedBig.String(),
		Available:          new(big.Int).Sub(totalBig, consumedBig).String(),
		UtilizationPercent: min(max(m.UtilizationPercent, 0), 100),
	}
}
