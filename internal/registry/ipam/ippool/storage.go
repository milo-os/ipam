package ippool

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// IPPoolStatusStorage implements the /status subresource. The standard
// generic registry update path is reused; the only difference vs the main
// store is that the status strategy resets spec fields on update.
type IPPoolStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPPoolStatusStorage) New() runtime.Object { return &ipam.IPPool{} }
func (s *IPPoolStatusStorage) Destroy()            {}

func (s *IPPoolStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPPoolStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *IPPoolStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *IPPoolStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

// AllocatingIPPoolREST is the registered storage for IPPool. The embedded
// *genericregistry.Store handles root-pool CRUD and list/watch unchanged;
// the Create override only diverts when ParentPoolRef is set, in which case
// it runs a single allocation transaction against the parent pool. Delete
// rejects any pool that still has rows in ipam_cidr_allocations so callers
// see a deterministic 409.
type AllocatingIPPoolREST struct {
	*genericregistry.Store
	allocator allocator.PrefixAllocator
	db        *pgxpool.Pool
	strategy  ipPoolStrategy
	codec     runtime.Codec
}

// NewIPPoolStorage builds the AllocatingIPPoolREST and the matching
// /status subresource storage. alloc + db are required — synchronous child
// allocation has no usable fallback. codec serialises the in-memory IPPool
// into the wire form persisted in ipam_objects, matching what subsequent
// GETs decode.
func NewIPPoolStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool, codec runtime.Codec) (*AllocatingIPPoolREST, *IPPoolStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPPool{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPPoolList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ippools"),
		SingularQualifiedResource: v1alpha1.Resource("ippool"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ippools")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &AllocatingIPPoolREST{
		Store:     store,
		allocator: alloc,
		db:        db,
		strategy:  strategy,
		codec:     codec,
	}, &IPPoolStatusStorage{store: &statusStore}, nil
}

// Create writes a pool and everything that must exist alongside it — its
// reservations and its class offers — in one transaction.
//
// Both pool shapes go through the explicit transaction rather than only child
// pools, because a root pool now has side effects too. Its reserved positions
// are real allocations, and a pool that committed without them would hand its
// gateway address to the first claim that asked. Its class offers back the count
// behind IPClass.status.offeringPools, and a pool that committed without them
// would read as backing nothing.
func (r *AllocatingIPPoolREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	pool, ok := obj.(*ipam.IPPool)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPPool, got %T", obj)
	}

	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanPoolChildAllocate)
	defer span.End()
	span.SetAttributes(attribute.Int(tracing.AttrClaimPrefix, pool.Spec.PrefixLength))

	objectMeta, err := meta.Accessor(pool)
	if err != nil {
		return nil, fmt.Errorf("get object metadata: %w", err)
	}
	rest.FillObjectMetaSystemFields(objectMeta)

	if err := rest.BeforeCreate(r.strategy, ctx, pool); err != nil {
		return nil, err
	}
	if createValidation != nil {
		if err := createValidation(ctx, pool.DeepCopyObject()); err != nil {
			return nil, err
		}
	}

	// Pools live in the caller's project; platform callers address the root.
	id := tenant.FromContext(ctx)
	poolKey := poolStorageKey(id.Name, pool.Name)

	// A child pool's family and extent come from its parent, so both must be
	// resolved before the transaction that writes it.
	var parentKey, ipFamily string
	if pool.Spec.ParentPoolRef != nil {
		parentName := pool.Spec.ParentPoolRef.Name
		parentKey = poolStorageKey(id.Name, parentName)
		span.SetAttributes(attribute.String(tracing.AttrPoolName, parentName))

		parentObj, gerr := r.Get(ctx, parentName, &metav1.GetOptions{})
		if gerr != nil {
			return nil, apierrors.NewBadRequest("parent IPPool not found")
		}
		parentPool, ok := parentObj.(*ipam.IPPool)
		if !ok {
			return nil, fmt.Errorf("unexpected parent pool type %T", parentObj)
		}
		// IPFamily is immutable, so reading it outside the transaction is safe.
		ipFamily, err = effectiveIPFamily(parentPool)
		if err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
	} else {
		ipFamily, err = effectiveIPFamily(pool)
		if err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
	}
	span.SetAttributes(attribute.String(tracing.AttrClaimIPFamily, ipFamily))

	// Class offers are validated before the creation transaction, so a pool that
	// cannot legally back the classes it names is rejected without writing
	// anything.
	if err := r.validateClassOffers(ctx, pool, ipFamily); err != nil {
		return nil, err
	}

	// Likewise for overlap against the tenant's other root pools. Before the
	// transaction on purpose: the refusal must not depend on dry-run, so
	// `--dry-run=server` reports the same conflict the real call would.
	if err := r.validateNoRootOverlap(ctx, pool, id); err != nil {
		return nil, err
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin pool creation transaction: %w", err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	if parentKey != "" {
		cidr, aerr := r.allocator.CarveChildPool(ctx, tx, parentKey, pool.Spec.PrefixLength, allocator.PoolCarveRecord{
			// A child pool's block against its parent is identified by the
			// child's own key: the pool object is the record of the allocation,
			// and there is no separate IPAllocation for it.
			AllocationKey: poolKey,
			IPFamily:      ipFamily,
			// No ClassName. A cascade-provisioned pool records the class that
			// built it; a pool an operator authored was built by nobody, and
			// spec.classNames says which classes it *offers*, which is a
			// different fact and lives in ipam_pool_class_offer.
			//
			// The digest is recorded but does not withhold the block — purpose
			// PoolCarve does that, unconditionally, in every address space. It
			// is the universal address space rather than this pool's own digest
			// so that every carve against one parent lands in a single
			// exclusion-constraint bucket: the constraint compares rows only
			// within a (pool_key, scope_digest), so sibling carves sharing a
			// digest are checked against each other, and a pool digest here
			// would give each sibling its own bucket and check nothing.
			ScopeDigest:  scope.EmptyAddressSpaceDigest(),
			OwnerProject: id.Name,
		})
		if aerr != nil {
			rollback()
			switch {
			case errors.Is(aerr, allocator.ErrPoolExhausted):
				span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonExhausted))
			case errors.Is(aerr, allocator.ErrPoolNotFound):
				span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonPoolNotFound))
			default:
				span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonTxError))
			}
			span.SetStatus(codes.Error, aerr.Error())
			return nil, mapAllocationError(aerr)
		}

		pool.Status.AllocatedCIDR = cidr
		pool.Status.Phase = ipam.PoolReady
		pool.Status.Conditions = []metav1.Condition{{
			Type:               "Allocated",
			Status:             metav1.ConditionTrue,
			Reason:             "AllocationSucceeded",
			Message:            fmt.Sprintf("CIDR %s allocated from %s", cidr, pool.Spec.ParentPoolRef.Name),
			LastTransitionTime: metav1.Now(),
		}}
	}

	parents, err := poolRanges(pool)
	if err != nil {
		rollback()
		return nil, apierrors.NewBadRequest(err.Error())
	}
	// Reserved blocks are computed before capacity, not after the object is
	// written, because they are allocations the moment the pool exists. Seeding
	// capacity from an empty set made a pool with `leading: 2, trailing: 2`
	// report all 4096 addresses available when 4092 were, and the first reader
	// of that number is an inventory view.
	reservation := allocator.Reservation{}
	if res := pool.Spec.Reservations; res != nil {
		reservation = allocator.Reservation{
			UnitPrefixLength: int(res.UnitPrefixLength),
			Leading:          int(res.Leading),
			Trailing:         int(res.Trailing),
		}
	}
	reserved, rerr := allocation.ReservedBlocks(parents, reservation.Leading, reservation.Trailing, reservation.UnitPrefixLength)
	if rerr != nil {
		rollback()
		return nil, apierrors.NewBadRequest(rerr.Error())
	}
	setPoolStatusCapacity(pool, parents, reserved)
	pool.Status.ScopeDigest = scope.PoolDigest(id.Name, pool.Spec.Scope)

	data, err := runtime.Encode(r.codec, pool)
	if err != nil {
		rollback()
		return nil, fmt.Errorf("encode pool: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, poolKey, "IPPool", "", pool.Name, data)
	if err != nil {
		rollback()
		return nil, registryerrors.MapWriteError(err,
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ippools"},
			pool.Name, "persist pool")
	}

	// Reservations after the object row, because they reference it. Each one
	// becomes a real allocation held by the pool, so reserved space has an
	// owner, appears in utilization, and can be programmed — inventory rather
	// than an invisible hole.
	if _, rerr := allocator.ProvisionReservations(ctx, tx, poolKey, ipFamily, id.Name, parents, reservation); rerr != nil {
		rollback()
		return nil, apierrors.NewBadRequest(rerr.Error())
	}

	if err := allocator.SyncClassOffers(ctx, tx, poolKey, pool.Spec.ClassNames); err != nil {
		rollback()
		return nil, err
	}

	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(pool, uint64(rv)); err != nil {
		rollback()
		return nil, fmt.Errorf("set resource version: %w", err)
	}
	// Dry-run rolls back rather than short-circuiting. This path does not
	// delegate to Store.Create — it does the whole job itself — so nothing above
	// honoured dryRun, and a `--dry-run=server` create genuinely created the
	// pool. For a *child* pool it did worse: AllocatePrefix above carves a real
	// block out of the parent, so asking what would happen consumed the address
	// space permanently, and asking twice consumed it twice.
	//
	// Doing the work and discarding it, rather than returning early, is also the
	// better preview: the caller gets the status they would really get,
	// including the CIDR the parent would hand out and the refusals a broken
	// request would earn, with nothing left behind.
	if isDryRun(options.DryRun) {
		rollback()
		return pool, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit pool creation transaction: %w", err)
	}

	// After the commit, not before: a gauge for a pool whose transaction rolled
	// back advertises capacity that does not exist. Without this a pool was
	// invisible to the capacity dashboards until its first claim, so a pool
	// sitting at 0% and a pool that had never been created looked identical.
	allocator.PublishPoolCapacity(poolKey, ipFamily, pool.Spec.ClassRef != nil, parents, reserved)

	return pool, nil
}

// isDryRun reports whether the options request a server-side dry run.
//
// Matches the helper in the IPClaim and IPAllocation registries rather than
// testing len(DryRun) > 0: DryRunAll is the only value the API defines, and an
// unrecognised one must be rejected upstream rather than quietly treated as a
// dry run here.
func isDryRun(dryRun []string) bool {
	for _, v := range dryRun {
		if v == metav1.DryRunAll {
			return true
		}
	}
	return false
}

// poolRanges returns the ranges a pool hands out from, preferring the carved
// status.allocatedCIDR over the declared spec.cidr for the same reason the
// allocator does: a child pool's real extent is what it was given.
func poolRanges(pool *ipam.IPPool) ([]net.IPNet, error) {
	cidrStr := pool.Spec.CIDR
	if pool.Status.AllocatedCIDR != "" {
		cidrStr = pool.Status.AllocatedCIDR
	}
	if cidrStr == "" {
		return nil, fmt.Errorf("IPPool %q has no CIDR", pool.Name)
	}
	_, ipnet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return nil, fmt.Errorf("parse CIDR %q: %w", cidrStr, err)
	}
	return []net.IPNet{*ipnet}, nil
}

// Delete rejects any pool — root or child — that still has allocations recorded
// in ipam_cidr_allocations. For a child pool with none, the row representing the
// child's own block against its parent is released in the same transaction as
// the object delete.
//
// # The step order here is load-bearing
//
// The allocation count runs first, on r.db and therefore outside any
// transaction of ours, and only then does the delete transaction open. Do not
// reorder these, and in particular do not move the object delete ahead of the
// count.
//
// A cascade provisioning a pool holds `FOR UPDATE` on its source pool's
// ipam_objects row while it inserts into ipam_cidr_allocations. Deleting the
// pool object first would take a lock on that same row, and the FK from
// ipam_cidr_allocations is ON DELETE RESTRICT, so the delete would then need
// locks on the referencing allocation rows — the exact rows a concurrent
// cascade is inserting while holding the object row this transaction wants.
// Two transactions acquiring the same two locks in opposite orders is a
// deadlock, and it would fire against every cascade rather than occasionally.
//
// Counting first, holding nothing, means the delete transaction only ever
// touches the pool once. It also means the count can be stale — a claim can
// arrive between the count and the delete — and that is handled where it should
// be, by the database: the RESTRICT constraint fails the delete rather than
// letting it orphan a live allocation. The count exists to turn the common case
// into a clear 409 rather than a constraint-violation 500.
// Update keeps the class-offer projection in step with spec.classNames.
//
// The projection is what makes IPClass.status.offeringPools a count rather than
// a scan of every pool's JSON, and zero offering pools means every claim naming
// the class fails — so a stale projection reports a class as backed by nothing,
// or as backed by a pool that stopped offering it. Neither is discoverable from
// the class.
//
// It is deliberately here and not on the status path. Status is rewritten on
// every allocation; syncing offers there would make one contended row per class
// of every pool, serialising claims that per-pool locking is designed to keep
// independent — the same mistake as a class-level utilization counter.
//
// The sync is its own short transaction after the object write rather than part
// of it, because the generic store owns its own transaction and does not expose
// one. The window between them is a projection lagging by milliseconds, and the
// table is a cache that can be rebuilt from ipam_objects at any time.
func (r *AllocatingIPPoolREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	obj, created, err := r.Store.Update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
	if err != nil {
		return obj, created, err
	}
	pool, ok := obj.(*ipam.IPPool)
	if !ok {
		return obj, created, nil
	}
	// A dry-run update must not touch anything. Store.Update already honours it
	// and persists nothing, but everything below is our own side effect and was
	// running regardless: a dry-run edit re-synced the pool's class offers
	// against the database. "Show me what would happen" is the one request that
	// must change nothing, and a caller asking it has no reason to expect
	// otherwise.
	//
	// Returning early is right here, unlike Create: the only work below is the
	// offer sync, and there is nothing in it a caller would want previewed.
	if isDryRun(options.DryRun) {
		return obj, created, nil
	}

	poolKey := poolStorageKey(tenant.FromContext(ctx).Name, name)
	tx, terr := r.db.Begin(ctx)
	if terr != nil {
		return obj, created, fmt.Errorf("begin class offer sync: %w", terr)
	}
	if serr := allocator.SyncClassOffers(ctx, tx, poolKey, pool.Spec.ClassNames); serr != nil {
		_ = tx.Rollback(ctx)
		return obj, created, serr
	}
	// Deliberately no capacity recompute here, having written one and removed it.
	// Capacity derives from the pool's ranges and its allocation rows, and an
	// update can change neither: spec.cidr is immutable (strategy.go), and
	// spec.reservations is materialised into rows only at create. So there is no
	// edit that makes status disagree with the rows, and a recompute would be a
	// guaranteed no-op on the one path it runs.
	//
	// What an edit *can* do is make the spec disagree with the rows — see the
	// task on reservation edits being silently ignored. That is a real defect and
	// a recompute does not fix it: it would faithfully re-derive the same figure
	// from the same unchanged rows and make the stale spec look confirmed.
	if cerr := tx.Commit(ctx); cerr != nil {
		return obj, created, fmt.Errorf("commit class offer sync: %w", cerr)
	}
	return obj, created, nil
}

// DeleteCollection routes each pool through Delete above.
//
// Without this, `kubectl delete ippool --all` dispatches to the embedded Store's
// DeleteCollection, which calls Store.Delete statically and never reaches the
// override — so every safeguard on the single-pool path is skipped at once. The
// blocking count never runs, so a pool with live claims is deleted; the carve
// against the parent is never released, so it orphans; and the pool's own
// reservations survive it.
//
// The orphaned carve is the one that does lasting damage, and it is worth
// naming: nothing in the schema prevents it. The foreign key on
// ipam_cidr_allocations.pool_key protects a pool against having allocations
// deleted out from under it, but a carve's *allocation_key* names the child pool
// and carries no constraint at all. So deleting a child pool by any path that
// skips this one leaves a row pointing at an object that no longer exists — and
// because pool names are a pure function of the scope, the next claim into that
// scope collides with it and wedges the scope permanently.
//
// Deletes proceed rather than aborting on the first refusal: a bulk delete over
// a mixed set should remove what it can and report what it could not, and the
// pools that refuse are precisely the ones still holding somebody's addresses.
func (r *AllocatingIPPoolREST) DeleteCollection(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
	listObj, err := r.List(ctx, listOptions)
	if err != nil {
		return nil, fmt.Errorf("list pools for deletecollection: %w", err)
	}
	poolList, ok := listObj.(*ipam.IPPoolList)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPPoolList from List, got %T", listObj)
	}

	deletedList := &ipam.IPPoolList{}
	var errs []error
	for i := range poolList.Items {
		deleted, _, derr := r.Delete(ctx, poolList.Items[i].Name, deleteValidation, options.DeepCopy())
		if derr != nil {
			if !apierrors.IsNotFound(derr) {
				errs = append(errs, fmt.Errorf("delete pool %s: %w", poolList.Items[i].Name, derr))
			}
			continue
		}
		if p, ok := deleted.(*ipam.IPPool); ok {
			deletedList.Items = append(deletedList.Items, *p)
		}
	}
	if len(errs) > 0 {
		return deletedList, errors.Join(errs...)
	}
	return deletedList, nil
}

func (r *AllocatingIPPoolREST) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	existing, err := r.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	pool, ok := existing.(*ipam.IPPool)
	if !ok {
		return nil, false, fmt.Errorf("expected *ipam.IPPool from Get, got %T", existing)
	}
	if deleteValidation != nil {
		if err := deleteValidation(ctx, pool.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}

	poolKey := poolStorageKey(tenant.FromContext(ctx).Name, name)
	// Count what somebody else holds in this pool: claims, and the carves that
	// back child pools. The pool's *own* edge reservations are excluded — they
	// are this pool's bookkeeping, they go away with it, and counting them made a
	// pool with any reservation permanently undeletable with a message telling
	// the operator to release claims that do not exist.
	//
	// This is the one place the Claim / Reservation / PoolCarve distinction is
	// read. The allocator's search collapses the last two, because neither
	// belongs to an address space; only deletion cares which of them a row is.
	var count int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ipam_cidr_allocations
		  WHERE pool_key = $1 AND purpose <> $2`,
		poolKey, string(v1alpha1.PurposeReservation),
	).Scan(&count); err != nil {
		return nil, false, fmt.Errorf("count active allocations for %q: %w", name, err)
	}
	if count > 0 {
		return nil, false, apierrors.NewConflict(
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ippools"},
			name,
			fmt.Errorf("cannot delete IPPool with %d active allocation(s); release all claims and child pools first", count),
		)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin pool delete transaction: %w", err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	// A child pool's block against its parent is recorded under the child's own
	// key, so releasing it is a force-release by allocation key rather than by
	// claim: it never had a claim to release through.
	//
	// Called unconditionally rather than only when ParentPoolRef is set. It is a
	// no-op for a pool that has no such row, and gating it on the field is how
	// this leaked before: cascade-provisioned pools were not recording a parent
	// reference, so they looked like root pools, the carve survived the delete,
	// and — because the pool name is a pure function of the scope — the next
	// claim into that scope collided with the orphaned row and wedged the scope
	// permanently. The field is set now, and this no longer depends on it.
	// The bool is deliberately discarded here: a root pool has no carve against
	// any parent, so "no row" is the expected answer for one and there is nothing
	// to distinguish it from a drifted key at this layer.
	if _, err := r.allocator.ForceRelease(ctx, tx, poolKey); err != nil {
		rollback()
		return nil, false, fmt.Errorf("release child-pool allocation: %w", err)
	}
	// The pool's own edge reservations go with it. They were excluded from the
	// blocking count above for the same reason they are deleted here: they are
	// the pool's bookkeeping, not a holder's claim on it.
	if err := allocator.ReleasePoolReservations(ctx, tx, poolKey); err != nil {
		rollback()
		return nil, false, err
	}
	// Class offers and the pool identity row both cascade with the object row
	// (ON DELETE CASCADE), so neither needs clearing here. The identity in
	// particular is a pointer rather than a claim on the pool: deleting the pool
	// retires it, and the next claim for that scope provisions a fresh one.
	if _, err := r.allocator.DeleteObject(ctx, tx, poolKey); err != nil {
		rollback()
		return nil, false, fmt.Errorf("delete pool row: %w", err)
	}
	// Dry-run discards the transaction, for the same reason Create does and with
	// more at stake. This path never delegated to the embedded Store, so nothing
	// honoured dryRun: `kubectl delete ippool --dry-run=server` really deleted
	// the pool, released its carve against the parent, and dropped its
	// reservations. DeleteCollection routes through here, so `--all
	// --dry-run=server` did it to every pool at once.
	//
	// Rolling back rather than returning early keeps the preview honest: the
	// blocking count above still runs, so a pool somebody is still holding
	// addresses in is refused in dry-run exactly as it would be for real. A
	// dry run that reports success where the real call would fail is worse than
	// no dry run at all.
	if isDryRun(options.DryRun) {
		rollback()
		return pool, true, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit pool delete transaction: %w", err)
	}

	return pool, true, nil
}

// poolStorageKey is the canonical ipam_objects key for an IPPool owned by the
// given project ("" for platform scope). Matches the key shape used by
// allocator.AllocatePrefix and the FOR UPDATE lock on the pool row. Although
// IPPool is cluster-scoped at the API layer, a pool created through a project
// control-plane is persisted under that project's tenant prefix, so the key
// must carry the same prefix.
func poolStorageKey(project, name string) string {
	return tenant.Identity{Name: project}.ResourceKey("ippools", name)
}

// setPoolStatusCapacity populates every utilization field on the pool's status.
//
// The arithmetic lives in internal/allocator so this and the cascade cannot
// disagree — they had separate copies, and only one of them was fixed when the
// int64 saturation turned an empty IPv6 pool into a full one.
func setPoolStatusCapacity(pool *ipam.IPPool, parents, allocations []net.IPNet) {
	v := allocator.PoolCapacityFor(parents, allocations)
	pool.Status.Capacity = ipam.PoolCapacity{
		Total:     v.Total,
		Allocated: v.Allocated,
		Available: v.Available,
	}

	pool.Status.UtilizationPercent = v.UtilizationPercent
	if fam, err := effectiveIPFamily(pool); err == nil {
		pool.Status.IPFamily = ipam.IPFamily(fam)
	}
}

// effectiveIPFamily returns a pool's address family. Root pools set it in
// spec.ipFamily; child pools leave that empty and carry the family in their
// carved status.allocatedCIDR. Both are one hop away, so no chain walk.
func effectiveIPFamily(pool *ipam.IPPool) (string, error) {
	if pool.Spec.IPFamily != "" {
		return string(pool.Spec.IPFamily), nil
	}
	cidr := pool.Status.AllocatedCIDR
	if cidr == "" {
		// Not yet provisioned: no family to inherit.
		return "", fmt.Errorf("parent IPPool %q has no resolved IP family", pool.Name)
	}
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse parent allocated CIDR %q: %w", cidr, err)
	}
	if ip.To4() != nil {
		return string(ipam.IPv4), nil
	}
	return string(ipam.IPv6), nil
}

// mapAllocationError translates allocator sentinel errors into the matching
// HTTP-shaped registry errors. Pool exhaustion is HTTP 507; unknown pool is
// a client error (the named parent does not exist).
func mapAllocationError(err error) error {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return registryerrors.NewInsufficientStorage("parent pool exhausted")
	case errors.Is(err, allocator.ErrPoolNotFound):
		return apierrors.NewBadRequest("parent IPPool not found")
	default:
		return apierrors.NewInternalError(err)
	}
}

// Compile-time interface assertions to catch storage contract drift.
var (
	_ rest.Storage           = (*AllocatingIPPoolREST)(nil)
	_ rest.Creater           = (*AllocatingIPPoolREST)(nil)
	_ rest.Updater           = (*AllocatingIPPoolREST)(nil)
	_ rest.CollectionDeleter = (*AllocatingIPPoolREST)(nil)
	_ rest.GracefulDeleter   = (*AllocatingIPPoolREST)(nil)
	_ rest.Storage           = (*IPPoolStatusStorage)(nil)
)
