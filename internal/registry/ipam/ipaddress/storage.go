// Package ipaddress provides REST storage for the IPAddress resource. The
// storage is the standard genericregistry.Store backed by the postgres
// RESTOptionsGetter; allocator integration lives in the ipaddressclaim
// package because IPAddress objects are materialised by the IPAddressClaim
// allocating REST or created directly by an operator for fixed assignments.
package ipaddress

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

type IPAddressStorage struct {
	*genericregistry.Store
}

type IPAddressStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPAddressStatusStorage) New() runtime.Object { return &ipam.IPAddress{} }
func (s *IPAddressStatusStorage) Destroy()            {}

func (s *IPAddressStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPAddressStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *IPAddressStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *IPAddressStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

func NewStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*IPAddressStorage, *IPAddressStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPAddress{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPAddressList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipaddresses"),
		SingularQualifiedResource: v1alpha1.Resource("ipaddress"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipaddresses")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &IPAddressStorage{store}, &IPAddressStatusStorage{store: &statusStore}, nil
}
