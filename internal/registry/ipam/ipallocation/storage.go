// Package ipallocation provides REST storage for the namespaced IPAllocation
// resource. IPAllocation rows are system-created by the ipclaim handler in the
// same transaction as the claim that produced them; this storage exposes
// standard CRUD plus a /status subresource for read paths and selectors.
package ipallocation

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

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

// NewAllocationStorage builds the IPAllocation REST storage and matching
// /status subresource. IPAllocation rows are always system-created (by the
// ipclaim Create handler) so this storage carries no allocator or db
// dependency — it is a thin CRUD shell on top of the generic registry store.
func NewAllocationStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*IPAllocationStorage, *IPAllocationStatusStorage, error) {
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

	return &IPAllocationStorage{store}, &IPAllocationStatusStorage{store: &statusStore}, nil
}

// Compile-time interface assertions.
var (
	_ rest.Storage = (*IPAllocationStorage)(nil)
	_ rest.Storage = (*IPAllocationStatusStorage)(nil)
)
