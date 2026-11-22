// Package ipallocation provides REST storage for the namespaced IPAllocation
// resource. IPAllocation rows are system-created by the ipclaim handler in the
// same transaction as the claim that produced them; this storage exposes
// reads, metadata updates and release (delete), plus a /status subresource,
// and no create.
package ipallocation

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/tableconvertor"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// IPAllocationStatusStorage exposes the /status subresource, read-only.
//
// An allocation's status is the server's record of what it holds: its
// address, and while retained, when it was retained and when its lease runs
// out. The retention sweeper acts on the row the status mirrors, not on the
// status, so a client-written expiresAt would be a deadline that nothing
// honours. Every write comes from the claim, retention and sweep paths, in the
// transactions that change the row; the API offers get only.
type IPAllocationStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPAllocationStatusStorage) New() runtime.Object { return &ipam.IPAllocation{} }
func (s *IPAllocationStatusStorage) Destroy()            {}

func (s *IPAllocationStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPAllocationStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

// ReleasingREST decorates the standard allocation storage with the release
// path a retained allocation needs, and without create.
//
// An allocation whose claim was released under reclaimPolicy Retain still
// holds its address and has no claim to delete, so deleting the allocation is
// the only way to hand the address back. That has to free the underlying
// ipam_cidr_allocations row, which the generic store knows nothing about.
//
// Allocations are written by the claim that makes them, in the transaction
// that writes their row. One created through the API would have no row behind
// it and could take the name a future claim's allocation needs, so the
// endpoint does not offer create at all.
//
// Deleting an IPAllocation is authorized by the apiserver as any delete is,
// and the ipam-consumer role grants it. That is enough for an address whose
// lease will run out anyway. One retained with no lease deadline is held on
// purpose, often by something outside IPAM that still uses it, so releasing it
// also needs the release-unleased permission, which only ipam-admin grants and
// which releaseChecker asks about. A nil releaseChecker refuses every such
// release: with no authorizer to ask, nothing has vouched for the caller.
type ReleasingREST struct {
	allocationStore
	allocator      allocator.PrefixAllocator
	db             txBeginner
	releaseChecker access.UnleasedReleaseChecker
}

// allocationStore is the part of the generic store the IPAllocation endpoint
// serves. It is an interface so that the store's Create, and the corrupt-object
// deleter that would skip the row release, are not promoted onto ReleasingREST.
type allocationStore interface {
	rest.Storage
	rest.Scoper
	rest.SingularNameProvider
	rest.StorageVersionProvider
	rest.ResetFieldsStrategy
	rest.Getter
	rest.Lister
	rest.Watcher
	rest.Updater
	rest.MayReturnFullObjectDeleter
}

// txBeginner is the minimal slice of *pgxpool.Pool the release path depends
// on, so tests can inject a fake transaction. QueryRow is the one read made
// outside the transaction: whether the row has a lease deadline, read before
// the authorizer is asked, so a slow authorizer holds no connection.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Delete frees a retained allocation: the ipam_cidr_allocations row and the
// object row go together, in one transaction. A bound allocation is refused
// with 409 Conflict naming its claim.
//
// The object's spec.claimRef is checked first, so the common refusal costs no
// transaction, but it is not what decides: the release itself deletes only a
// row bound to no claim, and refuses the same way if the row says otherwise.
//
// The object is deleted only if it is still the one read here. Between that
// read and this transaction another release can free the row and a recreated
// claim can write a new, bound IPAllocation under the same name; deleting by
// name alone would remove that one. A dry run runs the same checks and rolls
// back.
//
// An allocation retained with no finite lease deadline is released only for a
// caller with the release-unleased permission, or deletecollection as the
// namespace controller has, and refused with 403 otherwise. The allocator
// decides that from the row under the locks it deletes it under. When an
// unlocked read made before the transaction already shows no deadline, the
// permission is asked about then, so the authorizer's round trip holds no
// connection and no lock, and a caller without it is refused at once rather
// than queueing on the pool. It is asked under the locks only if the row lost
// its deadline in between.
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
	if alloc.Spec.ClaimRef != nil && alloc.Spec.ClaimRef.Name != "" {
		return nil, false, boundConflict(name, alloc.Spec.ClaimRef.Name)
	}
	readVersion, err := strconv.ParseInt(alloc.ResourceVersion, 10, 64)
	if err != nil {
		return nil, false, fmt.Errorf("parse resourceVersion %q of IPAllocation %q: %w", alloc.ResourceVersion, name, err)
	}

	key := allocationObjectKey(tenant.FromContext(ctx), alloc.Namespace, alloc.Name)
	ask := access.NewUnleasedReleaseAsk(r.releaseChecker, alloc.Namespace, name)
	unleased, err := allocator.RetainedWithoutDeadline(ctx, r.db, key)
	if err != nil {
		return nil, false, err
	}
	if unleased {
		allowed, err := ask.Allowed(ctx)
		if err != nil {
			return nil, false, err
		}
		if !allowed {
			return nil, false, unleasedForbidden(name)
		}
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin allocation delete transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := r.allocator.ReleaseRetainedAllocation(ctx, tx, key, ask.Allowed); err != nil {
		var unleased *allocator.UnleasedReleaseError
		if errors.As(err, &unleased) {
			return nil, false, unleasedForbidden(name)
		}
		var bound *allocator.AllocationBoundError
		if errors.As(err, &bound) {
			claimName := ""
			if bound.ClaimKey != "" {
				claimName = path.Base(bound.ClaimKey)
			}
			return nil, false, boundConflict(name, claimName)
		}
		return nil, false, fmt.Errorf("release retained allocation: %w", err)
	}

	// After the release, so the order is pool, then object, as on create.
	var currentVersion int64
	err = tx.QueryRow(ctx,
		`SELECT resource_version FROM ipam_objects WHERE key = $1 FOR UPDATE`, key,
	).Scan(&currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, apierrors.NewNotFound(v1alpha1.Resource("ipallocations"), name)
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock IPAllocation row: %w", err)
	}
	if currentVersion != readVersion {
		return nil, false, apierrors.NewConflict(v1alpha1.Resource("ipallocations"), name, fmt.Errorf(
			"IPAllocation changed while it was being released; read it again and retry"))
	}

	if options != nil && isDryRun(options.DryRun) {
		return alloc, true, nil
	}
	if _, err := r.allocator.DeleteObject(ctx, tx, key); err != nil {
		return nil, false, fmt.Errorf("delete IPAllocation row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit allocation delete transaction: %w", err)
	}
	committed = true
	klog.V(2).InfoS("retained allocation released", "allocation", name)
	return alloc, true, nil
}

// unleasedForbidden is the refusal to release an allocation retained with no
// lease deadline to a caller without the release-unleased permission.
func unleasedForbidden(name string) error {
	return apierrors.NewForbidden(v1alpha1.Resource("ipallocations"), name, fmt.Errorf(
		"the retained address has no lease deadline, so whatever held it may still be using it; "+
			"only an IPAM admin can release it (permission ipam.miloapis.com/ipallocations.%s), "+
			"or deleting its namespace",
		access.VerbReleaseUnleased))
}

// boundConflict is the refusal to release an allocation something still
// holds. It names the claim, because deleting that claim is what the caller
// has to do instead. With no claim to name, the row is a reservation or a pool
// carve, which no IPAllocation release frees.
func boundConflict(name, claimName string) error {
	if claimName == "" {
		return apierrors.NewConflict(v1alpha1.Resource("ipallocations"), name, fmt.Errorf(
			"IPAllocation's address is held by its pool, not by a claim, and cannot be released here"))
	}
	return apierrors.NewConflict(v1alpha1.Resource("ipallocations"), name, fmt.Errorf(
		"IPAllocation is bound to IPClaim %q; delete the claim instead", claimName))
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
// never here, and the storage offers no create; the allocator and db are for
// the delete path, which has to free the address a retained allocation still
// holds. releaseChecker authorizes releasing one with no lease deadline; nil
// refuses every such release.
func NewAllocationStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool, releaseChecker access.UnleasedReleaseChecker) (*ReleasingREST, *IPAllocationStatusStorage, error) {
	strategy := NewStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPAllocation{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPAllocationList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipallocations"),
		SingularQualifiedResource: v1alpha1.Resource("ipallocation"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: tableconvertor.IPAllocations(),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	return &ReleasingREST{
		allocationStore: store,
		allocator:       alloc,
		db:              db,
		releaseChecker:  releaseChecker,
	}, &IPAllocationStatusStorage{store: store}, nil
}

// Compile-time interface assertions.
var (
	_ rest.Storage           = (*ReleasingREST)(nil)
	_ rest.GracefulDeleter   = (*ReleasingREST)(nil)
	_ rest.CollectionDeleter = (*ReleasingREST)(nil)
	_ rest.Storage           = (*IPAllocationStatusStorage)(nil)
	_ rest.Getter            = (*IPAllocationStatusStorage)(nil)
)
