package ipclass

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/warning"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// maxClassChainDepth caps how far a class may sit from the root of its
// ancestry. The cascade walks this chain on the first claim into a new scope
// and provisions a pool at every level, so an unbounded chain is an unbounded
// amount of work — and an unbounded number of pool locks — inside one request.
// Chains in practice are three deep (network → subnet → endpoint); eight leaves
// room without leaving the bound theoretical.
const maxClassChainDepth = 8

// IPClassStatusStorage implements the /status subresource. Class health is
// computed rather than stored — status carries pool counts a reader may
// refresh, never a counter maintained during allocation, because a class-level
// counter would be one row every pool of that class contends on and would
// destroy the per-pool locking the service depends on.
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

// IPClassREST is the registered storage for IPClass. The embedded Store handles
// CRUD and list/watch unchanged; Create and Update add the validation that
// needs the rest of the catalog in hand.
//
// Those checks are deliberately at class-write time rather than claim time. A
// cycle or a family disagreement discovered while satisfying a claim would
// surface as a failed allocation on a consumer's request, long after the
// operator who could fix it stopped looking.
type IPClassREST struct {
	*genericregistry.Store
	// db backs status.offeringPools, which is derived from every pool's
	// spec.classNames and so cannot be known by anything that writes a class.
	// It is the only reason this storage touches the database at all; writing
	// a class still allocates nothing.
	db *pgxpool.Pool
}

// Get returns the class with status.offeringPools resolved.
func (r *IPClassREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	obj, err := r.Store.Get(ctx, name, options)
	if err != nil {
		return obj, err
	}
	class, ok := obj.(*ipam.IPClass)
	if !ok {
		return obj, nil
	}
	if err := r.resolveOfferingPools(ctx, []*ipam.IPClass{class}); err != nil {
		return nil, err
	}
	return class, nil
}

// List returns the catalog with status.offeringPools resolved on every item.
func (r *IPClassREST) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	obj, err := r.Store.List(ctx, options)
	if err != nil {
		return obj, err
	}
	list, ok := obj.(*ipam.IPClassList)
	if !ok {
		return obj, nil
	}
	classes := make([]*ipam.IPClass, len(list.Items))
	for i := range list.Items {
		classes[i] = &list.Items[i]
	}
	if err := r.resolveOfferingPools(ctx, classes); err != nil {
		return nil, err
	}
	return list, nil
}

// resolveOfferingPools fills status.offeringPools in place.
//
// # Why this is computed on read rather than stored
//
// The count depends on objects other than the class — every pool's
// spec.classNames — so no write to a class can know it, and the only way to
// store it would be for the pool write path to fan out across the catalog. The
// status subresource's own doc comment already stated the intent: class health
// is computed, never a counter maintained during allocation, because a
// class-level counter is one row every pool of that class contends on.
//
// The consequence, stated because it is not obvious: WATCH does not carry this
// field. A class's stored object holds whatever was last written to status, and
// no event fires when a pool changes the count, because nothing wrote to the
// class. A reader who needs it issues a GET or a LIST.
//
// # Why a failure here fails the read
//
// The alternative is leaving the count at zero, and zero is not a neutral
// value for this field: it is the specific claim "no pool backs this class,
// every claim naming it will fail". Reporting that because a query errored
// would send an operator to look for a pool that is already there. An error the
// caller can retry is the honest answer, and it costs nothing in practice —
// r.Store.Get read from this same database one line earlier, so a database that
// can serve the object can serve the count.
func (r *IPClassREST) resolveOfferingPools(ctx context.Context, classes []*ipam.IPClass) error {
	if len(classes) == 0 {
		return nil
	}
	names := make([]string, 0, len(classes))
	for _, c := range classes {
		names = append(names, c.Name)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin offering-pool count: %w", err)
	}
	// Read-only, so it is rolled back rather than committed — the same reason
	// the cascade's planning transaction is: it releases the snapshot
	// immediately instead of holding the xmin horizon the watch cursor depends
	// on.
	defer func() { _ = tx.Rollback(ctx) }()

	counts, err := allocator.OfferingPoolCounts(ctx, tx, names)
	if err != nil {
		return fmt.Errorf("count pools offering classes: %w", err)
	}
	for _, c := range classes {
		// A class the allocator's catalog does not hold keeps whatever is
		// stored rather than being told it is backed by nothing. It is
		// unreachable by claims for the same reason it is unreadable here, and
		// the honest signal for that is the claim's own "class not found" —
		// not a count that describes a class the allocator cannot see.
		if n, ok := counts[c.Name]; ok {
			c.Status.OfferingPools = n
		}
	}
	return nil
}

// NewIPClassStorage builds the IPClass REST storage and its /status
// subresource. IPClass is cluster-scoped and carries no allocator dependency:
// it is policy, and nothing about writing it allocates anything. db is used on
// the read paths only, to resolve status.offeringPools.
func NewIPClassStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, db *pgxpool.Pool) (*IPClassREST, *IPClassStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

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
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &IPClassREST{Store: store, db: db}, &IPClassStatusStorage{store: &statusStore}, nil
}

// Create validates the incoming class against the existing catalog before
// delegating to the standard create path.
func (r *IPClassREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	class, ok := obj.(*ipam.IPClass)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPClass, got %T", obj)
	}
	if err := r.validateAgainstCatalog(ctx, class); err != nil {
		return nil, err
	}
	return r.Store.Create(ctx, obj, createValidation, options)
}

// Update validates the mutated class against the catalog. The immutable set —
// ipFamily, parentClassName, poolPer, uniqueWithin, provisioner — is enforced
// by the strategy's ValidateUpdate, so the only catalog-level rule that can
// newly fail here is the per-family default marker, which is an annotation and
// therefore mutable by design: moving the default from one class to another is
// a legitimate operator action.
func (r *IPClassREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	wrapped := rest.DefaultUpdatedObjectInfo(nil, func(ctx context.Context, newObj, oldObj runtime.Object) (runtime.Object, error) {
		updated, err := objInfo.UpdatedObject(ctx, oldObj)
		if err != nil {
			return nil, err
		}
		class, ok := updated.(*ipam.IPClass)
		if !ok {
			return nil, fmt.Errorf("expected *ipam.IPClass, got %T", updated)
		}
		if err := r.validateAgainstCatalog(ctx, class); err != nil {
			return nil, err
		}
		return updated, nil
	})
	return r.Store.Update(ctx, name, wrapped, createValidation, updateValidation, forceAllowCreate, options)
}

// validateClassAuthorship refuses a class written into any project but the
// configured platform one.
//
// The catalog is platform policy and lives in the platform project — that is
// where allocator.LoadClass and LoadDefaultClass read it from. A class written
// into a tenant's own key space is a silently inert object: the write succeeds,
// the tenant's own LIST shows it (this registry is tenant-scoped, like every
// other), and no allocator lookup will ever see it. `kubectl get` says the
// catalog changed and no claim resolves through it.
//
// Refusing is strictly better than accepting an object that lies about
// existing, which is the failure this whole change set has been eliminating.
//
// It is defence in depth rather than the only gate. `ipclass` ships with no
// parentResources on its ProtectedResource, so in Milo's model the create
// permission cannot be granted at a project at all. This check agrees with the
// IAM layer rather than duplicating it, and IPAM should not depend on Milo's
// role catalog for a correctness property of its own.
//
// BYOIP may want to revisit this. If a tenant brings their own address space,
// whether they need a class of their own or consume a platform-provided BYOIP
// class is genuinely open. Nothing here forecloses it — relaxing this rule
// later is additive, and would come with its own consent story the way
// IPClass.spec.backingProjects did for pools.
func validateClassAuthorship(id tenant.Identity) error {
	if id.IsPlatform() {
		return nil
	}
	// The message names where classes live, not just that this is refused. A
	// bare "forbidden" sends an operator to read RBAC, which is not the
	// problem: the permission is not missing, the object is in the wrong
	// project. When no platform project is configured there is nowhere correct
	// to point them, so say that instead of naming an empty one.
	where := "the platform project named by the apiserver's --platform-project flag"
	if platform, ok := id.PlatformProject(); ok {
		where = fmt.Sprintf("the platform project (%q)", platform)
	}
	return apierrors.NewForbidden(
		ipam.Resource("ipclasses"), "",
		fmt.Errorf("the IPClass catalog is platform policy and must be created in %s; "+
			"a class created in any other project is never read by the allocator", where))
}

// validateAgainstCatalog runs every rule that needs to see other classes:
// the parent chain (existence, family agreement, prefix lengths, cycles,
// depth) and the at-most-one-default-per-family marker. Warnings are emitted
// for conditions that are suspicious but legitimately transient.
func (r *IPClassREST) validateAgainstCatalog(ctx context.Context, class *ipam.IPClass) error {
	// First, and before listClasses: this store is tenant-scoped, so a
	// non-platform caller's catalog read would return their own key space and
	// the chain rules would be validated against the wrong catalog entirely.
	if err := validateClassAuthorship(tenant.FromContext(ctx)); err != nil {
		return err
	}

	catalog, err := r.listClasses(ctx)
	if err != nil {
		return fmt.Errorf("list IPClasses for validation: %w", err)
	}

	errs, warnings := validateClassCatalog(class, catalog)
	for _, w := range warnings {
		warning.AddWarning(ctx, "", w)
	}
	if len(errs) > 0 {
		return apierrors.NewInvalid(ipam.Kind("IPClass"), class.Name, errs)
	}

	// The chain has just been walked and found sound, so the roles fall out of
	// work already done. Computing them here rather than in a separate pass is
	// also what keeps them honest: they are derived from the same traversal the
	// claim path uses, so a client that satisfies status.requiredScopeRoles
	// cannot then be rejected for a missing role.
	class.Status.RequiredScopeRoles = requiredScopeRoles(class, catalog)
	return nil
}

// requiredScopeRoles is the resolved set of roles a claim of this class must
// supply: the class's own UniqueWithin, unioned with the PoolPer of every class
// along its parent chain.
//
// Both halves are needed and for different reasons. UniqueWithin decides which
// allocations the new one must not collide with; each ancestor's PoolPer decides
// which pool that level provisions. A claim short either is rejected — the
// allocator never widens — so the two sets together are exactly what a caller
// has to carry.
//
// Returned sorted so the field is stable across writes and a diff of two class
// objects shows a real change rather than a map iteration.
func requiredScopeRoles(class *ipam.IPClass, catalog []ipam.IPClass) []string {
	byName := make(map[string]*ipam.IPClass, len(catalog))
	for i := range catalog {
		byName[catalog[i].Name] = &catalog[i]
	}
	byName[class.Name] = class

	seen := map[string]bool{}
	var roles []string
	add := func(rs []string) {
		for _, r := range rs {
			if r != "" && !seen[r] {
				seen[r] = true
				roles = append(roles, r)
			}
		}
	}

	add(class.Spec.UniqueWithin)

	// The chain is already known acyclic and within depth — validateParentChain
	// ran first — but the visited set is kept anyway so this cannot become the
	// thing that hangs a request if that ever stops being true.
	visited := map[string]bool{class.Name: true}
	for current := class; current.Spec.ParentClassName != ""; {
		parent, ok := byName[current.Spec.ParentClassName]
		if !ok || visited[parent.Name] {
			break
		}
		visited[parent.Name] = true
		add(parent.Spec.PoolPer)
		current = parent
	}

	sort.Strings(roles)
	return roles
}

// listClasses reads the whole class catalog. The catalog is operator-authored
// and small — a handful of classes per family — so a full list is cheaper than
// the two indexed lookups (parent chain walk, default-marker scan) it replaces,
// and it gives the cycle check a consistent view rather than one assembled from
// several reads.
func (r *IPClassREST) listClasses(ctx context.Context) ([]ipam.IPClass, error) {
	obj, err := r.Store.List(ctx, &metainternalversion.ListOptions{
		LabelSelector: labels.Everything(),
	})
	if err != nil {
		return nil, err
	}
	list, ok := obj.(*ipam.IPClassList)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPClassList, got %T", obj)
	}
	return list.Items, nil
}

// validateClassCatalog is the pure core of the catalog-level rules, split out
// from the storage so it can be tested without a store.
//
// `class` is the object being written; `catalog` is every class currently
// stored, which for an update still holds the pre-update copy of `class`
// itself. Entries matching the incoming name are therefore skipped everywhere
// the rule is about *other* classes.
func validateClassCatalog(class *ipam.IPClass, catalog []ipam.IPClass) (field.ErrorList, []string) {
	var errs field.ErrorList
	var warnings []string

	byName := make(map[string]*ipam.IPClass, len(catalog)+1)
	for i := range catalog {
		byName[catalog[i].Name] = &catalog[i]
	}
	// The incoming version wins over the stored one for the chain walk, so an
	// update is validated against what it will be, not what it was.
	byName[class.Name] = class

	if class.Spec.ParentClassName != "" {
		errs = append(errs, validateParentChain(class, byName)...)
	}

	// poolPer only means anything on a class some other class names as its
	// parent: a leaf class binds allocations directly and provisions nothing.
	// This is a warning rather than an error because the parent is necessarily
	// authored before its children — rejecting it would make the catalog
	// impossible to write in a valid order.
	if len(class.Spec.PoolPer) > 0 && !isNamedAsParent(class.Name, catalog) {
		warnings = append(warnings, fmt.Sprintf(
			"spec.poolPer has no effect yet: no IPClass names %q as its parentClassName, and only a parent provisions pools",
			class.Name))
	}

	if isDefaultClass(class) {
		for i := range catalog {
			other := &catalog[i]
			if other.Name == class.Name {
				continue
			}
			if other.Spec.IPFamily != class.Spec.IPFamily {
				continue
			}
			if isDefaultClass(other) {
				errs = append(errs, field.Duplicate(
					field.NewPath("metadata", "annotations").Key(ipam.IsDefaultClassAnnotation),
					fmt.Sprintf("IPClass %q is already the default for %s", other.Name, class.Spec.IPFamily)))
				break
			}
		}
	}

	// A class nobody can reach is worth saying out loud at write time. A leaf
	// class with no parent draws from the pools that offer it via
	// spec.classNames; if no pool does, every claim naming it fails on the
	// first request rather than at authoring time.
	if class.Spec.ParentClassName == "" && len(class.Spec.PoolPer) == 0 {
		warnings = append(warnings, fmt.Sprintf(
			"IPClass %q has no parentClassName, so it draws only from pools listing it in spec.classNames; claims naming it fail until such a pool exists",
			class.Name))
	}

	return errs, warnings
}

// validateParentChain walks upward from class, enforcing the three rules the
// allocator relies on at every hop — the parent exists, shares the address
// family, and hands out blocks strictly wider than its child's — plus the two
// structural bounds the cascade needs: no cycles, bounded depth.
func validateParentChain(class *ipam.IPClass, byName map[string]*ipam.IPClass) field.ErrorList {
	var errs field.ErrorList
	parentPath := field.NewPath("spec", "parentClassName")

	seen := map[string]bool{class.Name: true}
	child := class
	for depth := 1; ; depth++ {
		parentName := child.Spec.ParentClassName
		if parentName == "" {
			return errs
		}
		if seen[parentName] {
			return append(errs, field.Invalid(parentPath, class.Spec.ParentClassName,
				fmt.Sprintf("parent chain contains a cycle at %q", parentName)))
		}
		if depth > maxClassChainDepth {
			return append(errs, field.Invalid(parentPath, class.Spec.ParentClassName,
				fmt.Sprintf("parent chain exceeds the maximum depth of %d", maxClassChainDepth)))
		}
		seen[parentName] = true

		parent, ok := byName[parentName]
		if !ok {
			// Only report the missing link for the class being written; a gap
			// higher up the chain is the ancestor's own problem and was
			// rejected when that ancestor was written.
			if child == class {
				errs = append(errs, field.Invalid(parentPath, parentName,
					fmt.Sprintf("IPClass %q does not exist", parentName)))
			}
			return errs
		}

		if child == class {
			errs = append(errs, validateParentAgreement(class, parent)...)
		}
		child = parent
	}
}

// validateParentAgreement checks the two rules that bind a class to its
// immediate parent.
func validateParentAgreement(class, parent *ipam.IPClass) field.ErrorList {
	var errs field.ErrorList
	parentPath := field.NewPath("spec", "parentClassName")

	if parent.Spec.IPFamily != class.Spec.IPFamily {
		errs = append(errs, field.Invalid(parentPath, parent.Name,
			fmt.Sprintf("parent IPClass %q hands out %s but this class hands out %s; a class and its parent share an address family",
				parent.Name, parent.Spec.IPFamily, class.Spec.IPFamily)))
	}

	// A child carves from blocks its parent handed out, so every size the
	// child may request must be strictly longer than every size the parent
	// hands out. Comparing the child's shortest against the parent's longest
	// is the whole rule: if that holds, every pairing does.
	childR, parentR := class.Spec.AllowedPrefixLengths, parent.Spec.AllowedPrefixLengths
	if childR != nil && parentR != nil && childR.Min <= parentR.Max {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "allowedPrefixLengths", "min"), childR.Min,
			fmt.Sprintf("must be greater than parent IPClass %q allowedPrefixLengths.max (%d): a class carves from its parent and its blocks are strictly smaller",
				parent.Name, parentR.Max)))
	}
	return errs
}

// isNamedAsParent reports whether any class in the catalog carves from name.
func isNamedAsParent(name string, catalog []ipam.IPClass) bool {
	for i := range catalog {
		if catalog[i].Name != name && catalog[i].Spec.ParentClassName == name {
			return true
		}
	}
	return false
}

// isDefaultClass reports whether the class carries the default marker for its
// family. The annotation is the marker rather than a spec field because the
// default moves between classes over a class's life, and moving it is not a
// change to what either class means.
func isDefaultClass(c *ipam.IPClass) bool {
	return c.Annotations[ipam.IsDefaultClassAnnotation] == "true"
}

// Compile-time interface assertions to catch storage contract drift.
var (
	_ rest.Storage = (*IPClassREST)(nil)
	_ rest.Creater = (*IPClassREST)(nil)
	_ rest.Updater = (*IPClassREST)(nil)
	_ rest.Lister  = (*IPClassREST)(nil)
	_ rest.Storage = (*IPClassStatusStorage)(nil)
)
