// Package asnpool provides REST storage for the cluster-scoped ASNPool and
// ASNPoolClass resources. Pool capacity status is computed asynchronously
// by a controller; the storage itself is the standard genericregistry.Store
// backed by the postgres RESTOptionsGetter.
package asnpool

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
// ASNPoolClass storage (cluster-scoped, no status subresource).
// ----------------------------------------------------------------------------

type ASNPoolClassStorage struct {
	*genericregistry.Store
}

func NewClassStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*ASNPoolClassStorage, error) {
	strategy := NewClassStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.ASNPoolClass{} },
		NewListFunc:               func() runtime.Object { return &ipam.ASNPoolClassList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("asnpoolclasses"),
		SingularQualifiedResource: v1alpha1.Resource("asnpoolclass"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("asnpoolclasses")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetClassAttrs}); err != nil {
		return nil, err
	}
	return &ASNPoolClassStorage{store}, nil
}

// ----------------------------------------------------------------------------
// ASNPool storage (cluster-scoped, with status subresource).
// ----------------------------------------------------------------------------

type ASNPoolStorage struct {
	*genericregistry.Store
	// db is used by Delete to count active ASN allocations against this
	// pool before letting the standard delete proceed. Mirrors the
	// IPPrefix deletion-protection pattern.
	db *pgxpool.Pool
}

type ASNPoolStatusStorage struct {
	store *genericregistry.Store
}

func (s *ASNPoolStatusStorage) New() runtime.Object { return &ipam.ASNPool{} }
func (s *ASNPoolStatusStorage) Destroy()            {}

func (s *ASNPoolStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *ASNPoolStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *ASNPoolStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *ASNPoolStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

// NewPoolStorage builds the ASNPool REST storage. db is the pgx pool used
// by Delete to reject pools that still have active ASN allocations
// recorded in ipam_asn_allocations (HTTP 409 Conflict). Mirrors the
// IPPrefix delete-protection pattern.
func NewPoolStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, db *pgxpool.Pool) (*ASNPoolStorage, *ASNPoolStatusStorage, error) {
	strategy := NewPoolStrategy(scheme)
	statusStrategy := NewPoolStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.ASNPool{} },
		NewListFunc:               func() runtime.Object { return &ipam.ASNPoolList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("asnpools"),
		SingularQualifiedResource: v1alpha1.Resource("asnpool"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("asnpools")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetPoolAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &ASNPoolStorage{Store: store, db: db}, &ASNPoolStatusStorage{store: &statusStore}, nil
}

// Delete rejects the request when active ASN allocations are tracked
// against this pool in ipam_asn_allocations. We check up-front rather
// than cascade-delete so callers see a deterministic 409 they can react
// to ("release all claims first") instead of finding orphaned allocation
// rows after the fact.
func (r *ASNPoolStorage) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	poolKey := poolKeyFor(name)
	var count int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM ipam_asn_allocations WHERE pool_key = $1`,
		poolKey,
	).Scan(&count); err != nil {
		return nil, false, fmt.Errorf("count active ASN allocations for %q: %w", name, err)
	}
	if count > 0 {
		return nil, false, apierrors.NewConflict(
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "asnpools"},
			name,
			fmt.Errorf("cannot delete ASNPool with %d active ASN allocation(s); release all claims first", count),
		)
	}
	return r.Store.Delete(ctx, name, deleteValidation, options)
}

// poolKeyFor is the storage key for the cluster-scoped ASNPool. It matches
// the key shape used by the AllocatingREST claim handlers when they write
// into ipam_asn_allocations, so a COUNT keyed on it is a faithful
// "is anything still using this pool?" query.
func poolKeyFor(name string) string {
	return fmt.Sprintf("/ipam.miloapis.com/asnpools/%s", name)
}
