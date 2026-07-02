package ipclass

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// IPClassStorage is the REST storage for IPClass. It embeds the generic
// registry store for standard cluster-scoped CRUD and overrides Create/Update
// only to enforce the single-default-class invariant: at most one IPClass may
// carry the ipam.miloapis.com/is-default-class annotation set to "true".
type IPClassStorage struct {
	*genericregistry.Store
}

// NewIPClassStorage builds the IPClass REST storage. IPClass drives no
// allocation of its own, so — unlike the claim and pool stores — it takes no
// allocator or db: it is a plain CRUD shell over the generic registry store.
func NewIPClassStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*IPClassStorage, error) {
	strategy := NewStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPClass{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPClassList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipclasses"),
		SingularQualifiedResource: v1alpha1.Resource("ipclass"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipclasses")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, err
	}

	return &IPClassStorage{Store: store}, nil
}

// Create rejects a class marked default when another default already exists,
// then delegates to the standard store create.
func (r *IPClassStorage) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	class, ok := obj.(*ipam.IPClass)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPClass, got %T", obj)
	}
	if isDefaultClass(class) {
		if err := r.rejectIfOtherDefaultExists(ctx, class.Name); err != nil {
			return nil, err
		}
	}
	return r.Store.Create(ctx, obj, createValidation, options)
}

// Update rejects an update that would mark this class default while another
// default already exists, then delegates to the standard store update. The
// caller's transformer runs first so the single-default check sees the
// fully-resolved object (PUT body or applied patch) before the store writes.
func (r *IPClassStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	guarded := guardedUpdatedObjectInfo{
		inner: objInfo,
		guard: func(newObj runtime.Object) error {
			class, ok := newObj.(*ipam.IPClass)
			if !ok {
				return fmt.Errorf("expected *ipam.IPClass, got %T", newObj)
			}
			if isDefaultClass(class) {
				return r.rejectIfOtherDefaultExists(ctx, name)
			}
			return nil
		},
	}
	return r.Store.Update(ctx, name, guarded, createValidation, updateValidation, forceAllowCreate, options)
}

// guardedUpdatedObjectInfo runs the caller's UpdatedObjectInfo to produce the
// candidate object, then runs a guard over it, vetoing the write if the guard
// returns an error.
type guardedUpdatedObjectInfo struct {
	inner rest.UpdatedObjectInfo
	guard func(runtime.Object) error
}

func (g guardedUpdatedObjectInfo) Preconditions() *metav1.Preconditions {
	return g.inner.Preconditions()
}

func (g guardedUpdatedObjectInfo) UpdatedObject(ctx context.Context, oldObj runtime.Object) (runtime.Object, error) {
	newObj, err := g.inner.UpdatedObject(ctx, oldObj)
	if err != nil {
		return nil, err
	}
	if err := g.guard(newObj); err != nil {
		return nil, err
	}
	return newObj, nil
}

// rejectIfOtherDefaultExists lists all IPClasses and returns an invalid-object
// error if any class other than self carries the default annotation. The class
// catalog is small and cluster-scoped, so a full list is cheap.
func (r *IPClassStorage) rejectIfOtherDefaultExists(ctx context.Context, self string) error {
	list, err := r.Store.List(ctx, &metainternalversion.ListOptions{})
	if err != nil {
		return fmt.Errorf("list IPClasses to enforce single default: %w", err)
	}
	classes, ok := list.(*ipam.IPClassList)
	if !ok {
		return fmt.Errorf("expected *ipam.IPClassList from List, got %T", list)
	}
	if conflict := otherDefaultClass(classes.Items, self); conflict != "" {
		return apierrors.NewInvalid(
			ipam.Kind("IPClass"),
			self,
			field.ErrorList{field.Invalid(
				field.NewPath("metadata", "annotations").Key(ipam.IsDefaultClassAnnotation),
				"true",
				fmt.Sprintf("IPClass %q is already the default; at most one default class may exist. Remove its %s annotation first.",
					conflict, ipam.IsDefaultClassAnnotation),
			)},
		)
	}
	return nil
}

// otherDefaultClass returns the name of the first default-annotated class that
// is not self, or "" when self would be the only default. Pure so the
// single-default invariant is unit-testable without a store.
func otherDefaultClass(classes []ipam.IPClass, self string) string {
	for i := range classes {
		if classes[i].Name == self {
			continue
		}
		if isDefaultClass(&classes[i]) {
			return classes[i].Name
		}
	}
	return ""
}

// isDefaultClass reports whether an IPClass is marked as the platform default
// via the is-default-class annotation set to "true".
func isDefaultClass(c *ipam.IPClass) bool {
	return c.Annotations[ipam.IsDefaultClassAnnotation] == "true"
}

// Compile-time interface assertions.
var (
	_ rest.Storage = (*IPClassStorage)(nil)
	_ rest.Creater = (*IPClassStorage)(nil)
	_ rest.Updater = (*IPClassStorage)(nil)
	_ rest.Lister  = (*IPClassStorage)(nil)
	_ rest.Getter  = (*IPClassStorage)(nil)
)
