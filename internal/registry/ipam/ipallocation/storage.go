// Package ipallocation provides REST storage for the namespaced IPAllocation
// resource. IPAllocation rows are system-created by the ipclaim handler in the
// same transaction as the claim that produced them; this storage exposes
// standard CRUD plus a /status subresource for read paths and selectors.
package ipallocation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// IPAllocationStorage is the standard REST storage for IPAllocation.
type IPAllocationStorage struct {
	*genericregistry.Store
}

// IPAllocationStatusStorage exposes the /status subresource.
type IPAllocationStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPAllocationStatusStorage) New() runtime.Object { return &ipam.IPAllocation{} }
func (s *IPAllocationStatusStorage) Destroy()            {}

func (s *IPAllocationStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPAllocationStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *IPAllocationStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *IPAllocationStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

// ReleasingREST decorates the standard allocation storage with the release
// path a retained allocation needs.
//
// An allocation whose claim was released under reclaimPolicy Retain still
// holds its address and has no claim to delete, so deleting the allocation is
// the only way to hand the address back. That has to free the underlying
// ipam_cidr_allocations row, which the generic store knows nothing about.
type ReleasingREST struct {
	*IPAllocationStorage
	allocator allocator.PrefixAllocator
	db        txBeginner
}

// txBeginner is the minimal slice of *pgxpool.Pool the release path depends
// on, so tests can inject a fake transaction.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Delete frees a retained allocation: the ipam_cidr_allocations row and the
// object row go together, in one transaction. A bound allocation is refused —
// the same rule the strategy's ValidateDelete states for the paths that reach
// the store directly.
func (r *ReleasingREST) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	existing, err := r.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	alloc, ok := existing.(*ipam.IPAllocation)
	if !ok {
		return nil, false, fmt.Errorf("expected *ipam.IPAllocation from Get, got %T", existing)
	}
	if deleteValidation != nil {
		if err := deleteValidation(ctx, alloc.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}
	if errs := NewStrategy(nil).ValidateDelete(ctx, alloc); len(errs) > 0 {
		return nil, false, apierrors.NewInvalid(
			schema.GroupKind{Group: v1alpha1.GroupName, Kind: "IPAllocation"}, name, errs)
	}
	if options != nil && isDryRun(options.DryRun) {
		return alloc, true, nil
	}

	key := allocationObjectKey(tenant.FromContext(ctx), alloc.Namespace, alloc.Name)
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin allocation delete transaction: %w", err)
	}
	if err := r.allocator.ReleaseAllocation(ctx, tx, key); err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, fmt.Errorf("release retained allocation: %w", err)
	}
	if _, err := r.allocator.DeleteObject(ctx, tx, key); err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, fmt.Errorf("delete IPAllocation row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit allocation delete transaction: %w", err)
	}
	klog.V(2).InfoS("retained allocation released", "allocation", name)
	return alloc, true, nil
}

// DeleteCollection routes individual deletes through Delete so the allocation
// rows are freed. The embedded Store's DeleteCollection dispatches statically
// to Store.Delete, which would remove the objects and leak their addresses.
func (r *ReleasingREST) DeleteCollection(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
	listObj, err := r.List(ctx, listOptions)
	if err != nil {
		return nil, fmt.Errorf("list allocations for deletecollection: %w", err)
	}
	allocList, ok := listObj.(*ipam.IPAllocationList)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPAllocationList from List, got %T", listObj)
	}

	deleted := &ipam.IPAllocationList{}
	var errs []error
	for i := range allocList.Items {
		obj, _, err := r.Delete(ctx, allocList.Items[i].Name, deleteValidation, options.DeepCopy())
		if err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete allocation %s: %w", allocList.Items[i].Name, err))
			}
			continue
		}
		if a, ok := obj.(*ipam.IPAllocation); ok {
			deleted.Items = append(deleted.Items, *a)
		}
	}
	if len(errs) > 0 {
		return deleted, errors.Join(errs...)
	}
	return deleted, nil
}

// isDryRun reports whether the options request a server-side dry-run.
func isDryRun(dryRun []string) bool {
	for _, v := range dryRun {
		if v == metav1.DryRunAll {
			return true
		}
	}
	return false
}

// allocationObjectKey is the storage key for an IPAllocation, matching what
// the ipclaim handler writes and what ipam_cidr_allocations.allocation_key
// records.
func allocationObjectKey(id tenant.Identity, namespace, name string) string {
	return id.ApplyPrefix(fmt.Sprintf("/ipam.miloapis.com/ipallocations/%s/%s", namespace, name))
}

// NewAllocationStorage builds the IPAllocation REST storage and matching
// /status subresource. Allocations are created by the ipclaim Create handler,
// never here; the allocator and db are for the delete path, which has to free
// the address a retained allocation still holds.
func NewAllocationStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool) (*ReleasingREST, *IPAllocationStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPAllocation{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPAllocationList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipallocations"),
		SingularQualifiedResource: v1alpha1.Resource("ipallocation"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipallocations")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &ReleasingREST{
		IPAllocationStorage: &IPAllocationStorage{store},
		allocator:           alloc,
		db:                  db,
	}, &IPAllocationStatusStorage{store: &statusStore}, nil
}

// Compile-time interface assertions.
var (
	_ rest.Storage           = (*IPAllocationStorage)(nil)
	_ rest.Storage           = (*ReleasingREST)(nil)
	_ rest.GracefulDeleter   = (*ReleasingREST)(nil)
	_ rest.CollectionDeleter = (*ReleasingREST)(nil)
	_ rest.Storage           = (*IPAllocationStatusStorage)(nil)
)
