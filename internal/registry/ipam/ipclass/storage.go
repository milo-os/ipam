package ipclass

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// IPClassStorage is the standard REST storage for IPClass.
type IPClassStorage struct {
	*genericregistry.Store

	classChecker access.ClassAccessChecker
	db           *pgxpool.Pool
}

// Create admits a class, requiring the "use" permission on the source class
// when the class is a reference into another project.
//
// The check runs here rather than on each allocation because spec.source is
// immutable: the reference is the grant being exercised, and it is established
// exactly once. A permission revoked later does not invalidate a reference
// already created — deleting the reference is what withdraws the access.
func (s *IPClassStorage) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	if err := s.authorizeSource(ctx, obj); err != nil {
		return nil, err
	}
	if err := s.validateOfferAgreement(ctx, obj); err != nil {
		return nil, err
	}
	return s.Store.Create(ctx, obj, createValidation, options)
}

func (s *IPClassStorage) authorizeSource(ctx context.Context, obj runtime.Object) error {
	class, ok := obj.(*ipam.IPClass)
	if !ok || class.Spec.Source == nil {
		return nil
	}
	src := *class.Spec.Source

	// A class naming a source in the caller's own project reaches nothing the
	// caller cannot already reach.
	if src.Project == tenant.FromContext(ctx).Name {
		return nil
	}

	forbidden := func(detail string) error {
		return apierrors.NewForbidden(v1alpha1.Resource("ipclasses"), class.Name,
			fmt.Errorf("%s: referencing class %q in project %q requires the "+
				"ipam.miloapis.com/ipclasses.use permission on it", detail, src.Name, src.Project))
	}

	// No checker means no authorizer was configured. Admitting the reference
	// would grant the source project's address space to a caller nothing
	// vouched for.
	if s.classChecker == nil {
		return forbidden("cross-project class references are unavailable")
	}

	allowed, err := s.classChecker.CanUseClass(ctx, src.Project, src.Name)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("authorize class reference: %w", err))
	}
	if !allowed {
		return forbidden("not authorized to use the source class")
	}
	return nil
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
// A class is policy, not an allocation, so this carries no allocator.
//
// classChecker authorises references into another project. A nil checker
// refuses them. db is required, because admitting a class means reading the
// IPPools that already offer themselves to its name.
func NewClassStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, classChecker access.ClassAccessChecker, db *pgxpool.Pool) (*IPClassStorage, *IPClassStatusStorage, error) {
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

	return &IPClassStorage{Store: store, classChecker: classChecker, db: db}, &IPClassStatusStorage{store: &statusStore}, nil
}
