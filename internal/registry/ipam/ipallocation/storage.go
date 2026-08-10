// Package ipallocation provides REST storage for the namespaced IPAllocation
// resource. Allocations are system-created by the ipclaim handler in the same
// transaction as the claim that produced them; this storage exposes read paths,
// selectors, a /status subresource, and one deliberate write: deletion.
//
// Deletion is not plain CRUD, because an IPAllocation object is not the whole
// allocation. The address is held by a row in ipam_cidr_allocations, and that
// row has no foreign key naming the object — the constraint runs the other way,
// protecting the pool. So deleting the object through the generic store removed
// the only visible record of an address that stayed consumed: the invisible hole
// the design says a held address must never become, reachable with one
// `kubectl delete ipallocation`.
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
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// IPAllocationStorage is the REST storage for IPAllocation.
type IPAllocationStorage struct {
	*genericregistry.Store
	allocator allocator.PrefixAllocator
	db        txBeginner
}

// txBeginner is the slice of *pgxpool.Pool the delete path needs.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
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

// NewAllocationStorage builds the IPAllocation REST storage and matching
// /status subresource.
//
// IPAllocation rows are always system-created, by the ipclaim Create handler,
// in the same transaction as the address they hold. That was a comment here
// asserting an invariant nothing enforced; Create now refuses and the spec is
// frozen on update, so it is a property of the code rather than a description
// of how it happened to be used.
func NewAllocationStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool) (*IPAllocationStorage, *IPAllocationStatusStorage, error) {
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

	return &IPAllocationStorage{Store: store, allocator: alloc, db: db},
		&IPAllocationStatusStorage{store: &statusStore}, nil
}

// Create refuses direct creation of an IPAllocation.
//
// The declared invariant is that IPAllocation rows are always system-created by
// the ipclaim Create handler, which inserts the object and the
// ipam_cidr_allocations row that holds the address in ONE transaction. Nothing
// enforced it: the generic store's Create was exposed and the shipped ipam-admin
// role granted ipallocations.create.
//
// A hand-created IPAllocation produces the object without the allocation row.
// The result names an address, holds nothing, blocks nothing, and is invisible
// to the exclusion constraint that guarantees two holders never share an
// address. It does not wedge its pool — the delete guard counts
// ipam_cidr_allocations rows rather than objects, so a phantom is correctly
// ignored there — but it is a visible object asserting a holder for an address
// that is in fact free. To anyone auditing, that reads as a double allocation.
//
// Delete is guarded for the mirror-image reason: it refuses to release an
// address out from under a live claim. Create is the same gesture in the other
// direction and was the unguarded half.
//
// The system's own path does not come through here — it calls
// allocator.InsertObject inside the claim transaction — so this closes the
// client route without touching the one that is supposed to work.
func (r *IPAllocationStorage) Create(_ context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	name := ""
	if alloc, ok := obj.(*ipam.IPAllocation); ok {
		name = alloc.Name
	}
	return nil, apierrors.NewForbidden(
		v1alpha1.Resource("ipallocations"), name,
		errors.New("IPAllocation is created by the server when an IPClaim binds; "+
			"create an IPClaim instead. A directly-created allocation would hold no "+
			"address and would misreport one as taken"),
	)
}

// Delete releases the address an allocation holds and removes the object, in one
// transaction.
//
// It is the operator gesture the design calls for: a retained address "can be
// force-released by an operator with an audit record". Deleting the allocation
// is that gesture, and the DELETED changelog entry is the audit record — so an
// address that outlived its claim returns to circulation through the API rather
// than needing someone in the database.
//
// A still-bound allocation is refused. Releasing an address out from under a
// live claim would leave the claim pointing at nothing while the workload keeps
// using the address, and the claim is the durable identity here — deleting it is
// the supported way to release, and reclaimPolicy then decides what happens.
func (r *IPAllocationStorage) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
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

	if err := refuseIfBound(alloc, name); err != nil {
		return nil, false, err
	}

	if options != nil && isDryRun(options.DryRun) {
		return alloc, true, nil
	}

	id := tenant.FromContext(ctx)
	allocationKey := allocationObjectKey(id, alloc.Namespace, name)

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin allocation delete transaction: %w", err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	// The row first, then the object. Both in one transaction, so there is no
	// instant at which the address is held by a row nothing names.
	released, err := r.allocator.ForceRelease(ctx, tx, allocationKey)
	if err != nil {
		rollback()
		return nil, false, fmt.Errorf("release allocation %q: %w", name, err)
	}
	// An allocation that reported an address must have had a row. Finding none
	// means the object and the row disagree — most likely because the key this
	// path derives has drifted from the one the claim handler wrote — and the
	// consequence is silent and permanent: the delete succeeds, the object goes,
	// and the address stays consumed with nothing naming it. Refusing is the only
	// response that keeps it findable.
	if !released && alloc.Status.AllocatedCIDR != "" {
		rollback()
		return nil, false, apierrors.NewInternalError(fmt.Errorf(
			"IPAllocation %q reports address %s but no allocation record was found under %q; refusing to delete the object, because doing so would leave the address held with nothing naming it",
			name, alloc.Status.AllocatedCIDR, allocationKey))
	}
	if _, err := r.allocator.DeleteObject(ctx, tx, allocationKey); err != nil {
		rollback()
		return nil, false, fmt.Errorf("delete allocation object %q: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit allocation delete transaction: %w", err)
	}
	return alloc, true, nil
}

// refuseIfBound rejects deletion of an allocation a claim still holds.
//
// Split out from Delete so the rule is testable on its own: it is the one place
// that decides whether an address may be taken away from something using it, and
// it should not need a live storage backend to exercise.
func refuseIfBound(alloc *ipam.IPAllocation, name string) error {
	ref := alloc.Spec.ClaimRef
	if ref == nil || ref.Name == "" {
		return nil
	}
	return apierrors.NewConflict(
		v1alpha1.Resource("ipallocations"), name,
		fmt.Errorf("this allocation is bound to IPClaim %q; delete the claim to release the address, and its reclaimPolicy decides whether the address is freed or retained", ref.Name))
}

// DeleteCollection routes each allocation through Delete above.
//
// The embedded Store's DeleteCollection dispatches to Store.Delete statically,
// so without this the bulk verb reverts to the generic path and leaks every
// address it touches. Every override on a delete path needs its collection form
// written alongside it; this service has now been bitten by that twice.
func (r *IPAllocationStorage) DeleteCollection(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
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
		obj, _, derr := r.Delete(ctx, allocList.Items[i].Name, deleteValidation, options.DeepCopy())
		if derr != nil {
			if !apierrors.IsNotFound(derr) {
				errs = append(errs, fmt.Errorf("delete allocation %s: %w", allocList.Items[i].Name, derr))
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

// allocationObjectKey is the storage key for an IPAllocation. It must match what
// the ipclaim handler wrote, or the release would find no row.
func allocationObjectKey(id tenant.Identity, namespace, name string) string {
	return id.ApplyPrefix(fmt.Sprintf("/ipam.miloapis.com/ipallocations/%s/%s", namespace, name))
}

func isDryRun(dryRun []string) bool {
	for _, v := range dryRun {
		if v == metav1.DryRunAll {
			return true
		}
	}
	return false
}

// Compile-time interface assertions.
var (
	_ rest.Storage           = (*IPAllocationStorage)(nil)
	_ rest.GracefulDeleter   = (*IPAllocationStorage)(nil)
	_ rest.CollectionDeleter = (*IPAllocationStorage)(nil)
	_ rest.Storage           = (*IPAllocationStatusStorage)(nil)
)
