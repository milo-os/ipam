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

// Create routes root pools through the embedded Store (no allocation
// required) and child pools through a single allocation transaction that
// reserves a sub-prefix from the named parent pool, populates the new pool's
// Status, and inserts the object row atomically.
func (r *AllocatingIPPoolREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	pool, ok := obj.(*ipam.IPPool)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPPool, got %T", obj)
	}

	if err := r.validateClassOffers(ctx, pool); err != nil {
		return nil, err
	}

	if pool.Spec.ParentPoolRef == nil {
		// Root pool — strategy.PrepareForCreate already populated status.
		return r.Store.Create(ctx, obj, createValidation, options)
	}

	// Child-pool creation reuses the allocation path, so trace it the same way a
	// claim is traced. ctx is rebound so the downstream spans nest under this one.
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanPoolChildAllocate)
	defer span.End()
	span.SetAttributes(
		attribute.String(tracing.AttrPoolName, pool.Spec.ParentPoolRef.Name),
		attribute.Int(tracing.AttrClaimPrefix, pool.Spec.PrefixLength),
	)

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

	// Parent and child pools both live in the caller's project (ParentPoolRef
	// carries no cross-project pointer); platform callers address the root.
	id := tenant.FromContext(ctx)
	parentName := pool.Spec.ParentPoolRef.Name
	parentKey := poolStorageKey(id.Name, parentName)
	childKey := poolStorageKey(id.Name, pool.Name)

	// Resolve the parent pool's IPFamily before entering the transaction so
	// the explicit value can be passed to AllocatePrefix. IPFamily is
	// immutable, so reading it outside the transaction is safe.
	parentObj, err := r.Get(ctx, parentName, &metav1.GetOptions{})
	if err != nil {
		return nil, apierrors.NewBadRequest("parent IPPool not found")
	}
	parentPool, ok := parentObj.(*ipam.IPPool)
	if !ok {
		return nil, fmt.Errorf("unexpected parent pool type %T", parentObj)
	}
	// Child pools inherit their family from the parent rather than setting
	// spec.ipFamily, so resolve it before allocating.
	ipFamily, err := effectiveIPFamily(parentPool)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin child-pool allocation transaction: %w", err)
	}

	span.SetAttributes(attribute.String(tracing.AttrClaimIPFamily, ipFamily))

	cidr, err := r.allocator.AllocatePrefix(ctx, tx, allocator.PrefixRequest{
		PoolKey:       parentKey,
		PrefixLen:     pool.Spec.PrefixLength,
		IPFamily:      ipFamily,
		ClaimKey:      childKey,
		AllocationKey: childKey,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		switch {
		case errors.Is(err, allocator.ErrPoolExhausted):
			span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonExhausted))
		case errors.Is(err, allocator.ErrPoolNotFound):
			span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonPoolNotFound))
		default:
			span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonTxError))
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, mapAllocationError(err)
	}

	pool.Status.AllocatedCIDR = cidr
	pool.Status.Phase = ipam.PoolReady
	pool.Status.Conditions = []metav1.Condition{{
		Type:               "Allocated",
		Status:             metav1.ConditionTrue,
		Reason:             "AllocationSucceeded",
		Message:            fmt.Sprintf("CIDR %s allocated from %s", cidr, parentName),
		LastTransitionTime: metav1.Now(),
	}}
	if _, ipnet, perr := net.ParseCIDR(cidr); perr == nil {
		// A freshly carved child pool has no sub-allocations yet; seed all
		// utilization fields from an empty allocation set.
		setPoolStatusCapacity(pool, []net.IPNet{*ipnet}, nil)
	} else {
		pool.Status.Capacity = ipam.PoolCapacity{}
	}

	data, err := runtime.Encode(r.codec, pool)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("encode pool: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, childKey, "IPPool", "", pool.Name, data)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("persist child pool: %w", err)
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(pool, uint64(rv)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("set resource version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit child-pool allocation transaction: %w", err)
	}

	return pool, nil
}

// Delete rejects any pool — root or child — that still has allocations
// recorded in ipam_cidr_allocations. For child pools with zero
// allocations the row in ipam_cidr_allocations representing the child's
// own reservation against its parent must also be released, in the same
// transaction as the object delete.
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
	var count int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ipam_cidr_allocations WHERE pool_key = $1`,
		poolKey,
	).Scan(&count); err != nil {
		return nil, false, fmt.Errorf("count active allocations for %q: %w", name, err)
	}
	if count > 0 {
		return nil, false, apierrors.NewConflict(
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ippools"},
			name,
			fmt.Errorf("cannot delete IPPool with %d active allocation(s); release all claims, retained allocations and child pools first", count),
		)
	}

	if pool.Spec.ParentPoolRef == nil {
		// Root pool with zero allocations — delegate to the standard delete.
		return r.Store.Delete(ctx, name, deleteValidation, options)
	}

	// Child pool — release its own reservation against the parent and
	// delete the object row in a single transaction.
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin child-pool delete transaction: %w", err)
	}
	if _, err := r.allocator.Release(ctx, tx, poolKey); err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, fmt.Errorf("release child-pool allocation: %w", err)
	}
	if _, err := r.allocator.DeleteObject(ctx, tx, poolKey); err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, fmt.Errorf("delete child-pool row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit child-pool delete transaction: %w", err)
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

// setPoolStatusCapacity populates the pool's capacity and utilization from its
// parent ranges and current allocations.
//
// Every figure comes from one Measure, so the counts and the percentage cannot
// disagree. The counts are exact decimal strings: an IPv6 /20 holds 2^108
// addresses, which no int64 can express.
func setPoolStatusCapacity(pool *ipam.IPPool, parents, allocations []net.IPNet) {
	m, err := allocation.Measure(parents, allocations, allocation.Reservation{})
	if err != nil {
		return
	}
	pool.Status.Capacity = ipam.PoolCapacity{
		Total:     m.Total.String(),
		Allocated: m.Consumed.String(),
		Available: m.Free.String(),
	}
	pool.Status.UtilizationPercent = m.UtilizationPercent
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
	_ rest.Storage         = (*AllocatingIPPoolREST)(nil)
	_ rest.Creater         = (*AllocatingIPPoolREST)(nil)
	_ rest.GracefulDeleter = (*AllocatingIPPoolREST)(nil)
	_ rest.Storage         = (*IPPoolStatusStorage)(nil)
)
