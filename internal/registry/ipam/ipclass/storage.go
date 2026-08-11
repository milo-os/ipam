package ipclass

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

// IPClassStorage is the standard REST storage for IPClass.
type IPClassStorage struct {
	*genericregistry.Store
}

// IPClassStatusStorage exposes the /status subresource.
type IPClassStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPClassStatusStorage) New() runtime.Object { return &ipam.IPClass{} }
func (s *IPClassStatusStorage) Destroy()            {}

func (s *IPClassStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPClassStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *IPClassStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *IPClassStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

// NewClassStorage builds the IPClass REST storage and its /status subresource.
// A class is policy, not an allocation, so this carries no allocator or db
// dependency.
func NewClassStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*IPClassStorage, *IPClassStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPClass{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPClassList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipclasses"),
		SingularQualifiedResource: v1alpha1.Resource("ipclass"),

		CreateStrategy:      strategy,
		UpdateStrategy:      strategy,
		DeleteStrategy:      strategy,
		ResetFieldsStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipclasses")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &IPClassStorage{store}, &IPClassStatusStorage{store: &statusStore}, nil
}
