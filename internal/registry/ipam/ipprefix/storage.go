// Package ipprefix provides REST storage for the IPPrefix resource and the
// closely-related IPPrefixClass resource.
package ipprefix

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ----------------------------------------------------------------------------
// IPPrefixClass storage (cluster-scoped, no status subresource).
// ----------------------------------------------------------------------------

type IPPrefixClassStorage struct {
	*genericregistry.Store
}

func NewClassStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*IPPrefixClassStorage, error) {
	strategy := NewClassStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPPrefixClass{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPPrefixClassList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipprefixclasses"),
		SingularQualifiedResource: v1alpha1.Resource("ipprefixclass"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipprefixclasses")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetClassAttrs}); err != nil {
		return nil, err
	}
	return &IPPrefixClassStorage{store}, nil
}

// ----------------------------------------------------------------------------
// IPPrefix storage (cluster-scoped, with status subresource).
// ----------------------------------------------------------------------------

type IPPrefixStorage struct {
	*genericregistry.Store
	// db is used by Delete to count active allocations against this prefix
	// before letting the standard delete proceed.
	db *pgxpool.Pool
}

type IPPrefixStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPPrefixStatusStorage) New() runtime.Object { return &ipam.IPPrefix{} }
func (s *IPPrefixStatusStorage) Destroy()            {}

func (s *IPPrefixStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPPrefixStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *IPPrefixStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *IPPrefixStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

// NewPrefixStorage builds the IPPrefix REST storage.
//
// db is the pgx pool used by Delete to reject prefixes that still have
// active allocations recorded in ipam_prefix_allocations (HTTP 409
// Conflict).
func NewPrefixStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, db *pgxpool.Pool) (*IPPrefixStorage, *IPPrefixStatusStorage, error) {
	strategy := NewPrefixStrategy(scheme)
	statusStrategy := NewPrefixStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPPrefix{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPPrefixList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipprefixes"),
		SingularQualifiedResource: v1alpha1.Resource("ipprefix"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipprefixes")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetPrefixAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &IPPrefixStorage{Store: store, db: db}, &IPPrefixStatusStorage{store: &statusStore}, nil
}

// Delete rejects the request when active allocations are tracked against
// this prefix in ipam_prefix_allocations. We check up-front rather than
// cascade-delete so callers see a deterministic 409 they can react to —
// "release all claims first" — instead of finding orphaned claims after
// the fact.
func (r *IPPrefixStorage) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	poolKey := prefixPoolKey(name)
	var count int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ipam_prefix_allocations WHERE pool_key = $1`,
		poolKey,
	).Scan(&count); err != nil {
		return nil, false, fmt.Errorf("count active allocations for %q: %w", name, err)
	}
	if count > 0 {
		return nil, false, apierrors.NewConflict(
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ipprefixes"},
			name,
			fmt.Errorf("cannot delete IPPrefix with %d active allocation(s); release all claims first", count),
		)
	}
	return r.Store.Delete(ctx, name, deleteValidation, options)
}

// prefixPoolKey is the storage key for the cluster-scoped IPPrefix pool. It
// matches the key shape used by the AllocatingREST claim handlers when they
// write into ipam_prefix_allocations, so a COUNT keyed on it is a faithful
// "is anything still using this pool?" query.
func prefixPoolKey(name string) string {
	return fmt.Sprintf("/ipam.miloapis.com/ipprefixes/%s", name)
}
