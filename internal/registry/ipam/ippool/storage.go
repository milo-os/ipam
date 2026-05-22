package ippool

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5/pgxpool"
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
// rejects any pool that still has rows in ipam_prefix_allocations so callers
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

	if pool.Spec.ParentPoolRef == nil {
		// Root pool — strategy.PrepareForCreate already populated status.
		return r.Store.Create(ctx, obj, createValidation, options)
	}

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

	parentName := pool.Spec.ParentPoolRef.Name
	parentKey := poolStorageKey(parentName)
	childKey := poolStorageKey(pool.Name)

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
	ipFamily := string(parentPool.Spec.IPFamily)

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin child-pool allocation transaction: %w", err)
	}

	cidr, err := r.allocator.AllocatePrefix(ctx, tx, parentKey, pool.Spec.PrefixLength, ipFamily, childKey, "")
	if err != nil {
		_ = tx.Rollback(ctx)
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
		total := allocation.CountAddresses(*ipnet)
		pool.Status.Capacity = ipam.PoolCapacity{Total: total, Available: total}
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
// recorded in ipam_prefix_allocations. For child pools with zero
// allocations the row in ipam_prefix_allocations representing the child's
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

	poolKey := poolStorageKey(name)
	var count int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ipam_prefix_allocations WHERE pool_key = $1`,
		poolKey,
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
	if err := r.allocator.Release(ctx, tx, poolKey); err != nil {
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

// poolStorageKey is the canonical ipam_objects key for a cluster-scoped
// IPPool. Matches the key shape used by allocator.AllocatePrefix and the
// FOR UPDATE lock on the pool row.
func poolStorageKey(name string) string {
	return fmt.Sprintf("/ipam.miloapis.com/ippools/%s", name)
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
