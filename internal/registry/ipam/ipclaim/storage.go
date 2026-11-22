// Package ipclaim provides REST storage for the IPClaim resource. The
// exported AllocatingREST wraps the standard storage with a synchronous
// Postgres-backed allocator: Create reserves a free sub-prefix from the
// target IPPool inside a single transaction and atomically materialises the
// resulting IPAllocation row. Delete reverses both, releasing the allocation
// and removing the IPAllocation in the same transaction as the claim.
package ipclaim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/registry/ipam/tableconvertor"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

type IPClaimStorage struct {
	*genericregistry.Store
}

type IPClaimStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPClaimStatusStorage) New() runtime.Object { return &ipam.IPClaim{} }
func (s *IPClaimStatusStorage) Destroy()            {}

func (s *IPClaimStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPClaimStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *IPClaimStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *IPClaimStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

// newInnerStorage builds the underlying generic registry-backed REST storage
// for IPClaim. NewAllocatingStorage wraps the result to add synchronous
// allocation in the request path.
func newInnerStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*IPClaimStorage, *IPClaimStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPClaim{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPClaimList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipclaims"),
		SingularQualifiedResource: v1alpha1.Resource("ipclaim"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: tableconvertor.IPClaims(),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &IPClaimStorage{store}, &IPClaimStatusStorage{store: &statusStore}, nil
}

// AllocatingREST decorates the standard claim storage with a synchronous
// allocator. On Create it begins a Postgres transaction, asks the allocator
// to reserve a sub-prefix from the target IPPool, materialises an
// IPAllocation row, and returns the claim with status fully populated. On
// Delete it releases the allocation and removes the IPAllocation in the same
// transaction as the claim deletion.
type AllocatingREST struct {
	*IPClaimStorage
	allocator   allocator.PrefixAllocator
	db          txBeginner
	strategy    ipClaimStrategy
	poolChecker access.PoolAccessChecker
	nsChecker   access.NamespaceChecker
	codec       runtime.Codec
}

// txBeginner is the minimal slice of *pgxpool.Pool the allocation handlers
// depend on. Narrowing the field to this interface lets unit tests inject a
// fake transaction without a live Postgres. QueryRow is the one read made
// outside a transaction: the check for a retained allocation every create
// makes, which is nearly always a miss.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewAllocatingStorage builds the IPClaim REST storage with synchronous
// Postgres-backed allocation. db must be the same pool the allocator commits
// against; codec is used to serialise the synchronously-allocated claim and
// the generated IPAllocation into ipam_objects so subsequent GETs return
// fully-populated objects. poolChecker may be nil; when non-nil it
// authorises cross-project claims via SubjectAccessReview before allocation.
// nsChecker may be nil, which disables the namespace-liveness check.
func NewAllocatingStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool, codec runtime.Codec, poolChecker access.PoolAccessChecker, nsChecker access.NamespaceChecker) (*AllocatingREST, *IPClaimStatusStorage, error) {
	claimStore, statusStore, err := newInnerStorage(scheme, optsGetter)
	if err != nil {
		return nil, nil, err
	}
	return &AllocatingREST{
		IPClaimStorage: claimStore,
		allocator:      alloc,
		db:             db,
		strategy:       NewStrategy(scheme),
		poolChecker:    poolChecker,
		nsChecker:      nsChecker,
		codec:          codec,
	}, statusStore, nil
}

// Create runs the standard create pipeline (system-metadata fill, strategy
// PrepareForCreate, validation), then drives the allocator inside a
// short-lived transaction. The transaction persists the claim row, the
// allocation row in ipam_cidr_allocations, and the IPAllocation API object
// together so the response body carries a CIDR that has already been
// reserved.
//
// A claim created under the name of one that was released under reclaimPolicy
// Retain takes the retained address back (see tryReclaim), rather than being
// refused or handed a second one.
//
// A claim created with metadata.generateName is named here, as the generic
// Store would name it: AllocatingREST replaces Store.Create, and BeforeCreate
// refuses an object whose name was never generated. A generated name that
// collides with a claim, or with an allocation a former claim of that name
// retained, is drawn again rather than refused.
func (r *AllocatingREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	claim, ok := obj.(*ipam.IPClaim)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPClaim, got %T", obj)
	}
	if claim.GenerateName == "" || claim.Name != "" {
		return r.create(ctx, claim, createValidation, options, false)
	}
	var err error
	for range maxNameGenerationAttempts {
		attempt := claim.DeepCopy()
		attempt.Name = r.strategy.GenerateName(attempt.GenerateName)
		var out runtime.Object
		out, err = r.create(ctx, attempt, createValidation, options, true)
		if err == nil || !isGeneratedNameCollision(err) {
			return out, err
		}
	}
	return nil, err
}

// maxNameGenerationAttempts matches the generic Store's retry budget for
// generated names.
const maxNameGenerationAttempts = 8

// isGeneratedNameCollision reports whether err refused a claim for its name
// alone, which a freshly generated name avoids.
func isGeneratedNameCollision(err error) bool {
	switch ipamerrors.ReasonFor(err) {
	case ipamerrors.ReasonClaimExists, ipamerrors.ReasonAllocationRetained:
		return true
	}
	return false
}

// create is Create for a claim whose name is already decided. generatedName
// reports that the server drew it, which matters only to a reclaim.
func (r *AllocatingREST) create(ctx context.Context, claim *ipam.IPClaim, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions, generatedName bool) (runtime.Object, error) {

	// A server-side dry-run must compute the would-be allocation but persist
	// nothing and consume no capacity (see Create body for how the transaction
	// is rolled back).
	dryRun := isDryRun(options.DryRun)
	if dryRun {
		// Nothing a dry-run changes survives it, so the metrics that count
		// changes (cascade provisioning) must not count its changes either.
		ctx = metrics.WithDryRun(ctx)
	}

	id := tenant.FromContext(ctx)
	project := id.Project()
	org := id.Org()

	ipFamily := string(claim.Spec.IPFamily)

	// Root span for the whole allocation; every downstream span (tenant resolve,
	// authorization, block search, DB calls) nests under it. ctx is rebound so
	// those calls attach. failSpan marks the span failed at the points that
	// already classify failures for metrics.
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanClaimAllocate)
	defer span.End()
	failSpan := func(reason string) {
		span.SetAttributes(attribute.String(tracing.AttrErrorReason, reason))
		span.SetStatus(codes.Error, reason)
	}

	// Record the resolved tenant scope and whether the request carried its
	// parent identity extras — the signal that tells a real project identity
	// apart from one that arrived stripped of them.
	_, resolveSpan := tracing.Tracer().Start(ctx, tracing.SpanTenantResolve)
	resolveSpan.SetAttributes(
		attribute.String(tracing.AttrProject, project),
		attribute.Bool(tracing.AttrHasParentExtras, id.APIGroup != "" || id.Kind != "" || id.Name != ""),
	)
	resolveSpan.End()

	span.SetAttributes(
		attribute.String(tracing.AttrTenantProject, project),
		attribute.String(tracing.AttrTenantOrg, org),
		attribute.String(tracing.AttrClaimIPFamily, ipFamily),
		attribute.Bool(tracing.AttrDryRun, dryRun),
	)

	metrics.AllocationAttempts.WithLabelValues("ipclaim", ipFamily, project, org).Inc()
	allocStart := time.Now()
	result := "error"
	defer func() {
		metrics.ObserveAllocationDuration("ipclaim", result, ipFamily, project, org, allocStart)
	}()
	// fail records err under the reason failureReason gives it and returns it.
	// label is reasonInternal or reasonTxError for a step whose own failure is
	// the server's; a refusal that step returns is labelled as a refusal.
	fail := func(err error, label string) error {
		metrics.RecordAllocationFailure("ipclaim", failureReason(err, label), ipFamily, project, org)
		return err
	}

	objectMeta, err := meta.Accessor(claim)
	if err != nil {
		return nil, fail(fmt.Errorf("get object metadata: %w", err), reasonInternal)
	}
	rest.FillObjectMetaSystemFields(objectMeta)

	if err := rest.BeforeCreate(r.strategy, ctx, claim); err != nil {
		return nil, fail(err, reasonInternal)
	}
	if claim.Spec.ClassName == "" && claim.Spec.IPFamily == "" {
		return nil, fail(apierrors.NewBadRequest("spec.className or spec.ipFamily is required"), reasonInvalid)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		failSpan(tracing.ReasonTxError)
		return nil, fail(fmt.Errorf("begin allocation transaction: %w", err), reasonTxError)
	}

	// A claim names a CLASS. Resolution starts in the caller's own project and
	// follows spec.source into the project holding the definition; discovery
	// then finds the pool that project offers. Cross-project sharing is the
	// reference, so there is no separate cross-project path here.
	class, err := allocator.ResolveClass(ctx, tx, claim.Spec.ClassName, v1alpha1.IPFamily(claim.Spec.IPFamily))
	_ = tx.Rollback(ctx)
	if err != nil {
		failSpan(tracing.ReasonPoolNotFound)
		switch {
		case errors.Is(err, allocator.ErrClassNotFound):
			return nil, fail(ipamerrors.New(ipamerrors.ReasonClassNotFound, err.Error()), reasonClassNotFound)
		case errors.Is(err, allocator.ErrNoDefaultClass):
			return nil, fail(ipamerrors.New(ipamerrors.ReasonNoDefaultClass, err.Error()), reasonClassNotFound)
		case errors.Is(err, tenant.ErrNoTenant):
			// A request that arrived without a project has nowhere to allocate
			// from. That is the caller's request, not a server fault, so it is
			// refused rather than reported as one.
			return nil, fail(ipamerrors.New(ipamerrors.ReasonNoProjectScope, err.Error()), reasonInvalid)
		}
		return nil, fail(fmt.Errorf("resolve class: %w", err), reasonTxError)
	}

	// spec.ipFamily is a selector for the default class, never a second opinion
	// about the class a claim names. Silently ignoring a disagreement let a
	// claim ask for IPv4 from an IPv6 class and get IPv6 back with no error.
	if claim.Spec.IPFamily != "" && string(claim.Spec.IPFamily) != string(class.Spec.IPFamily) {
		failSpan(tracing.ReasonPoolNotFound)
		return nil, fail(ipamerrors.New(ipamerrors.ReasonFamilyMismatch, fmt.Sprintf(
			"spec.ipFamily is %s but class %q hands out %s",
			claim.Spec.IPFamily, class.LocalName, class.Spec.IPFamily)), reasonInvalid)
	}
	if claim.Spec.AddressScope != "" && string(claim.Spec.AddressScope) != string(class.Spec.AddressScope) {
		failSpan(tracing.ReasonPoolNotFound)
		return nil, fail(ipamerrors.New(ipamerrors.ReasonAddressScopeMismatch, fmt.Sprintf(
			"spec.addressScope is %s but class %q has addressScope %q",
			claim.Spec.AddressScope, class.Name, class.Spec.AddressScope)), reasonInvalid)
	}

	// Default the class the same way a PersistentVolumeClaim is defaulted to
	// the cluster's StorageClass: the claim is persisted below, so the stored
	// object and the create response both name the class this resolved to
	// rather than leaving the choice implicit. spec.className is immutable, so
	// what is written here is what the claim allocated under, for good.
	claim.Spec.ClassName = class.LocalName
	claim.Spec.IPFamily = ipam.IPFamily(class.Spec.IPFamily)
	claim.Spec.AddressScope = ipam.AddressScope(class.Spec.AddressScope)
	ipFamily = string(claim.Spec.IPFamily)
	span.SetAttributes(attribute.String(tracing.AttrClaimIPFamily, ipFamily))

	if createValidation != nil {
		if err := createValidation(ctx, claim.DeepCopyObject()); err != nil {
			return nil, fail(err, reasonInternal)
		}
	}

	// Before any capacity is reserved: a namespace that is gone, or going,
	// never releases what is bound into it.
	if err := r.checkNamespaceLiveness(ctx, project, claim.Namespace); err != nil {
		failSpan(tracing.ReasonNamespaceNotLive)
		return nil, fail(err, reasonNamespaceNotLive)
	}

	// Overwrite any client-supplied ownerRef. The requestheader CA guarantees
	// the Extra headers carrying the tenant identity, so that identity is the
	// source of truth for who this claim is for.
	claim.Spec.OwnerRef = &ipam.ObjectRef{
		APIGroup: id.APIGroup,
		Kind:     id.Kind,
		Name:     id.Name,
	}

	// The address states the family a second time, and is held to the class
	// as spec.ipFamily is, whichever target the claim binds.
	if claim.Spec.Address != "" {
		if err := checkAddressFamily(claim.Spec.Address, class); err != nil {
			failSpan(tracing.ReasonPoolNotFound)
			return nil, fail(err, reasonInvalid)
		}
	}

	if claim.Spec.Target == ipam.TargetScopeRange {
		obj, outcome, err := r.createScopeRange(ctx, span, claim, class, id, dryRun)
		if err != nil {
			// The latency histogram's result is success | exhausted | error,
			// as on the block path, which alone adds reclaimed; the reason
			// goes on the failure counter.
			if failureReason(err, outcome) == reasonPoolExhausted {
				result = "exhausted"
			}
			return nil, fail(err, outcome)
		}
		result = "success"
		return obj, nil
	}

	// A CIDR-form address states its own length. Taking it as the claim's
	// prefix length, before the class default applies, is what makes
	// "address: 10.0.0.16/28" mean a /28 under a class whose default is /32;
	// the strategy has already refused a CIDR that disagrees with an explicit
	// prefixLength.
	if claim.Spec.Address != "" {
		if claim.Spec.PrefixLength == nil {
			if prefix, err := netip.ParsePrefix(claim.Spec.Address); err == nil {
				bits := int32(prefix.Bits())
				claim.Spec.PrefixLength = &bits
			}
		}
	}

	// The length the claim stated, before the class default fills it in: a
	// reclaim adopts the retained length only when the claim left it out.
	statedPrefixLength := claim.Spec.PrefixLength
	prefixLen, err := allocator.EffectivePrefixLength(class.IPClass, claim.Spec.PrefixLength)
	if err != nil {
		return nil, fail(ipamerrors.New(ipamerrors.ReasonPrefixLengthRejected, err.Error()), reasonInvalid)
	}
	reclaimPolicy := allocator.EffectiveReclaimPolicy(class.IPClass, v1alpha1.ReclaimPolicy(claim.Spec.ReclaimPolicy))

	// A Retain the class would not have chosen is allowed only under a lease.
	// The class's own lease settles it before any pool is known. When the
	// class sets none, the cap of the pool the claim lands in is the only lease
	// there could be, and it is judged under that pool's lock: by AllocatePrefix
	// for a fresh allocation, and by tryReclaim for the pool a retained one is
	// in. Checking it any earlier would judge a pool the claim might not get.
	requireRetentionCap := allocator.RetainOverridesClass(class.IPClass, v1alpha1.ReclaimPolicy(claim.Spec.ReclaimPolicy)) &&
		allocator.EffectiveLease(class.Spec.RetentionLease, nil) == nil

	// The address space this allocation must be unique in. A claim that does
	// not carry a role its class names in uniqueWithin is refused by name
	// rather than compared against a wider space: widening would look correct
	// while refusing addresses the narrow comparison was meant to allow, and
	// the operator would see a pool exhaust at a fraction of its capacity with
	// nothing to explain it.
	uniqueScope, scopeDigest, err := scope.ProjectAddressSpaceDigest(id.Name, claim.Spec.Scope, class.Spec.UniqueWithin, "uniqueWithin")
	if err != nil {
		var missing *scope.MissingRoleError
		if errors.As(err, &missing) {
			return nil, fail(ipamerrors.NewScopeRolesMissing(missing.Roles, fmt.Sprintf("%s (class %q)", missing.Error(), class.Name)), reasonInvalid)
		}
		return nil, fail(apierrors.NewBadRequest(err.Error()), reasonInvalid)
	}

	// A claim recreated under the name of one whose address was retained takes
	// that address back, before any pool is resolved: the retained row names
	// its pool, and a reclaim provisions nothing and searches nothing.
	reclaimed := r.tryReclaim(ctx, span, claim, id, reclaimTerms{
		claimName:    claim.Name,
		class:        class,
		scope:        claim.Spec.Scope,
		scopeDigest:  scopeDigest,
		prefixLength: statedPrefixLength,
		address:      claim.Spec.Address,

		requireRetentionCap: requireRetentionCap,
	}, uniqueScope, reclaimPolicy, generatedName, dryRun)
	if reclaimed.handled {
		if reclaimed.err != nil {
			failSpan(tracing.ReasonTxError)
			return nil, fail(reclaimed.err, reclaimed.outcome)
		}
		// A request-level result, so a dry-run reclaim records it too (see
		// metrics.WithDryRun); ipam_reclaims_total, which counts addresses
		// taken back, does not.
		result = "reclaimed"
		return reclaimed.obj, nil
	}

	// The block a named address asks for, resolved once the size is known. An
	// address that is not the start of its block is refused here, never
	// rounded down to the block it falls in.
	var wantBlock *net.IPNet
	if claim.Spec.Address != "" {
		block, err := allocation.RequestedBlock(claim.Spec.Address, prefixLen)
		if err != nil {
			failSpan(tracing.ReasonPoolNotFound)
			if errors.Is(err, allocation.ErrUnalignedAddress) {
				return nil, fail(ipamerrors.New(ipamerrors.ReasonAddressUnaligned, fmt.Sprintf(
					"spec.address %s: %v", claim.Spec.Address, err)), reasonInvalid)
			}
			return nil, fail(apierrors.NewBadRequest(fmt.Sprintf("spec.address %s: %v", claim.Spec.Address, err)), reasonInvalid)
		}
		wantBlock = &block
	}

	// Resolving the pool provisions any missing level of the class's chain, and
	// each level commits on its own — so it runs BEFORE the allocation
	// transaction opens, not inside it. A pool is durable infrastructure that
	// outlives the claim that caused it; holding a transaction across the
	// chain would make a herd of first claims serialise behind the slowest.
	//
	// A dry-run is the exception. A pool is never renumbered, so provisioning
	// one to answer a hypothetical would leave the hypothetical behind: the
	// whole dry-run runs in one transaction, each level in a savepoint of it,
	// and the rollback below takes the levels with it.
	var resolveDB allocator.TxBeginner = r.db
	var dryTx pgx.Tx
	if dryRun {
		dryTx, err = r.db.Begin(ctx)
		if err != nil {
			failSpan(tracing.ReasonTxError)
			return nil, fail(fmt.Errorf("begin dry-run transaction: %w", err), reasonTxError)
		}
		defer func() { _ = dryTx.Rollback(ctx) }()
		resolveDB = dryTx
	}
	poolKey, err := allocator.ResolvePoolFor(ctx, resolveDB, class, claim.Spec.Scope, wantBlock)
	if err != nil {
		failSpan(tracing.ReasonPoolNotFound)
		return nil, fail(resolvePoolError(err), reasonTxError)
	}

	if dryTx != nil {
		tx = dryTx
	} else if tx, err = r.db.Begin(ctx); err != nil {
		failSpan(tracing.ReasonTxError)
		return nil, fail(fmt.Errorf("begin allocation transaction: %w", err), reasonTxError)
	}
	poolName := poolKey[strings.LastIndex(poolKey, "/")+1:]
	claimKey := claimObjectKey(id, claim.Namespace, claim.Name)
	span.SetAttributes(attribute.String(tracing.AttrPoolName, poolName))

	// The IPAllocation's name and key are the allocation row's identity, so
	// they are resolved before the row is written rather than after.
	allocationName := allocationNameFor(claim.Namespace, claim.Name)
	allocationKey := allocationObjectKey(id, claim.Namespace, allocationName)

	cidr, err := r.allocator.AllocatePrefix(ctx, tx, allocator.PrefixRequest{
		PoolKey:       poolKey,
		PrefixLen:     prefixLen,
		IPFamily:      string(class.Spec.IPFamily),
		ClaimKey:      claimKey,
		AllocationKey: allocationKey,
		OwnerProject:  id.Name,
		ScopeDigest:   scopeDigest,
		ClassName:     class.Name,
		ClassProject:  class.Project,
		AddressScope:  class.Spec.AddressScope,
		ReclaimPolicy: reclaimPolicy,
		Address:       claim.Spec.Address,

		RequireRetentionCap: requireRetentionCap,
	})
	if err != nil {
		if isClaimNameCollision(err) || isIdentityCollision(err) {
			_ = tx.Rollback(ctx)
			failSpan(tracing.ReasonTxError)
			if isClaimNameCollision(err) {
				return nil, fail(duplicateClaimConflict(claim.Name), reasonConflict)
			}
			return nil, fail(retainedAllocationConflict(claim.Name, allocationName), reasonConflict)
		}
		_ = tx.Rollback(ctx)
		reason := allocationFailureReason(err)
		switch reason {
		case reasonPoolExhausted:
			result = "exhausted"
			failSpan(tracing.ReasonExhausted)
		case reasonPoolNotFound, reasonInvalid:
			failSpan(tracing.ReasonPoolNotFound)
		default:
			failSpan(tracing.ReasonTxError)
		}
		apiErr := mapAllocationError(err, poolName, claim.Name)
		if errors.Is(err, allocator.ErrRetentionLeaseRequired) {
			apiErr = retentionLeaseRequired(claim.Name, class, poolName)
		}
		return nil, fail(apiErr, reason)
	}

	// Populate the claim status with the computed allocation up-front so both
	// the dry-run and the persisting paths return identical bound status.
	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedCIDR = cidr
	claim.Status.Address = singleAddress(cidr)
	claim.Status.BoundAllocationRef = &ipam.LocalRef{Name: allocationName}
	claim.Status.PoolRef = &ipam.LocalRef{Name: poolName}

	// Server dry-run: the allocator has computed the real next CIDR inside the
	// transaction (SELECT … FOR UPDATE + FindFirstAvailableBlock), but we must
	// not persist anything or consume capacity. Roll the transaction back —
	// undoing the allocation row reserved by AllocatePrefix — and return the
	// claim with its would-be status. No IPAllocation row, no claim row, no
	// changelog event, no committed capacity change.
	if dryRun {
		_ = tx.Rollback(ctx)
		result = "success"
		return claim, nil
	}

	alloc := newAllocationObject(claim, allocationName, poolName, class.Name, uniqueScope, reclaimPolicy, cidr, scopeDigest)
	allocData, err := runtime.Encode(r.codec, alloc)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, fail(fmt.Errorf("encode IPAllocation: %w", err), reasonInternal)
	}
	if _, err := r.allocator.InsertObject(ctx, tx, allocationKey, "IPAllocation", claim.Namespace, allocationName, allocData); err != nil {
		_ = tx.Rollback(ctx)
		if isIdentityCollision(err) {
			return nil, fail(retainedAllocationConflict(claim.Name, allocationName), reasonConflict)
		}
		return nil, fail(fmt.Errorf("persist IPAllocation: %w", err), reasonTxError)
	}

	// Claim status was populated above (before the dry-run branch); the
	// persisted record carries the allocated CIDR + reference back to the
	// IPAllocation row. Watchers see a single ADDED event with the terminal
	// bound state.
	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, fail(fmt.Errorf("encode claim: %w", err), reasonInternal)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, claimKey, "IPClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		_ = tx.Rollback(ctx)
		// The claim row is the last write, so a collision here means the name
		// is taken by a claim holding no live allocation row of its own.
		if isIdentityCollision(err) {
			return nil, fail(duplicateClaimConflict(claim.Name), reasonConflict)
		}
		return nil, fail(fmt.Errorf("persist claim: %w", err), reasonTxError)
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(claim, uint64(rv)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fail(fmt.Errorf("set resource version: %w", err), reasonInternal)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fail(fmt.Errorf("commit allocation transaction: %w", err), reasonTxError)
	}

	result = "success"
	return claim, nil
}

// checkNamespaceLiveness refuses a claim whose namespace cannot collect it. Every
// other outcome returns nil, including a failed lookup — see
// internal/access/namespace.go on failing open.
func (r *AllocatingREST) checkNamespaceLiveness(ctx context.Context, project, namespace string) error {
	if r.nsChecker == nil {
		return nil
	}
	state, err := r.nsChecker.State(ctx, project, namespace)
	if err != nil {
		access.LogUndetermined(project, namespace, err)
		return nil
	}
	return access.RefuseNamespace(state, namespace, v1alpha1.Resource("ipclaims"))
}

// isDryRun reports whether a create/delete options' DryRun slice requests a
// server-side dry-run. The apiserver only ever sets [metav1.DryRunAll], but we
// match defensively on the constant.
func isDryRun(dryRun []string) bool {
	for _, v := range dryRun {
		if v == metav1.DryRunAll {
			return true
		}
	}
	return false
}

// allocationFailureReason maps an allocator error onto the canonical reason
// label used by ipam_allocation_failures_total.
//
// A named address that is held, withheld, outside the pool or unaligned is the
// caller's request refused, so it is counted with the other refusals of that
// kind rather than as a server fault.
func allocationFailureReason(err error) string {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return reasonPoolExhausted
	case errors.Is(err, allocator.ErrPoolNotFound):
		return reasonPoolNotFound
	case errors.Is(err, allocator.ErrAddressInUse), errors.Is(err, allocator.ErrAddressReserved):
		return reasonConflict
	case errors.Is(err, allocator.ErrAddressOutsidePool), errors.Is(err, allocation.ErrUnalignedAddress),
		errors.Is(err, allocation.ErrInvalidAddress), errors.Is(err, allocation.ErrPrefixLenMismatch),
		errors.Is(err, allocator.ErrRetentionLeaseRequired):
		return reasonInvalid
	default:
		return reasonTxError
	}
}

// Delete runs the claim teardown in two transactions so watchers can observe
// the intermediate Releasing phase before the object disappears:
//
//	TX1: UPDATE the claim row with status.phase=Releasing + MODIFIED changelog
//	TX2: Release the allocation + DeleteObject(IPAllocation) + DeleteObject(claim) + DELETED changelogs
//
// TX2 is retried up to deleteMaxAttempts times with a short backoff so a
// transient PG hiccup does not strand the claim in Releasing forever.
func (r *AllocatingREST) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	existing, err := r.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	claim, ok := existing.(*ipam.IPClaim)
	if !ok {
		return nil, false, fmt.Errorf("expected *ipam.IPClaim from Get, got %T", existing)
	}
	if deleteValidation != nil {
		if err := deleteValidation(ctx, claim.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}

	// Server dry-run: report what would be deleted without releasing the
	// allocation or removing any rows.
	if options != nil && isDryRun(options.DryRun) {
		return claim, true, nil
	}

	if claim.Spec.Target == ipam.TargetScopeRange {
		return r.deleteScopeRange(ctx, claim)
	}

	// Release span — mirrors the allocation span for the teardown path so a
	// release is traceable end-to-end. ctx is rebound so the DB spans attach.
	id := tenant.FromContext(ctx)
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanClaimRelease)
	defer span.End()
	span.SetAttributes(
		attribute.String(tracing.AttrTenantProject, id.Project()),
		attribute.String(tracing.AttrClaimIPFamily, string(claim.Spec.IPFamily)),
	)

	claimKey := claimObjectKey(id, claim.Namespace, claim.Name)

	// TX1 — publish phase=Releasing.
	releasing := claim.DeepCopy()
	releasing.Status.Phase = ipam.ClaimReleasing
	releasingData, err := runtime.Encode(r.codec, releasing)
	if err != nil {
		return nil, false, fmt.Errorf("encode releasing claim: %w", err)
	}
	tx1, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin releasing transaction: %w", err)
	}
	rv, err := r.allocator.UpdateObject(ctx, tx1, claimKey, releasingData)
	if err != nil {
		_ = tx1.Rollback(ctx)
		return nil, false, fmt.Errorf("publish releasing phase: %w", err)
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(releasing, uint64(rv)); err != nil {
		_ = tx1.Rollback(ctx)
		return nil, false, fmt.Errorf("set releasing resource version: %w", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit releasing transaction: %w", err)
	}
	klog.V(2).InfoS("claim entering Releasing phase", "claim", name)

	// TX2 — release the allocation, remove the IPAllocation row and the
	// claim row in a single transaction. Retried on transient failures.
	var lastErr error
	var retained bool
	for attempt := 1; attempt <= deleteMaxAttempts; attempt++ {
		retained, lastErr = r.releaseAndDelete(ctx, claim, claimKey)
		if lastErr == nil {
			break
		}
		klog.ErrorS(lastErr, "release-and-delete attempt failed", "claim", name, "attempt", attempt)
		if attempt < deleteMaxAttempts {
			time.Sleep(deleteRetryBackoff)
		}
	}
	if lastErr != nil {
		klog.ErrorS(lastErr, "claim stuck in Releasing after retries — manual intervention may be required", "claim", name, "attempts", deleteMaxAttempts)
		span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonTxError))
		span.SetStatus(codes.Error, "release failed after retries")
		return nil, false, fmt.Errorf("release allocation after %d attempts: %w", deleteMaxAttempts, lastErr)
	}

	klog.V(2).InfoS("claim released and deleted", "claim", name, "retained", retained)
	// The claim was released either way; releases_total counts claims. Whether
	// the address came back is a different question, and retention answers it
	// on its own counter.
	metrics.RecordRelease("ipclaim")
	if retained {
		metrics.RecordRetention("ipclaim")
	}
	return releasing, true, nil
}

// DeleteCollection routes individual deletes through AllocatingREST.Delete
// so allocation rows are released when a namespace is bulk-terminated. The
// embedded Store's DeleteCollection would otherwise dispatch statically to
// Store.Delete and leak allocations.
func (r *AllocatingREST) DeleteCollection(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
	listObj, err := r.List(ctx, listOptions)
	if err != nil {
		return nil, fmt.Errorf("list claims for deletecollection: %w", err)
	}
	claimList, ok := listObj.(*ipam.IPClaimList)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPClaimList from List, got %T", listObj)
	}

	deletedList := &ipam.IPClaimList{}
	var errs []error
	for i := range claimList.Items {
		deleted, _, err := r.Delete(ctx, claimList.Items[i].Name, deleteValidation, options.DeepCopy())
		if err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete claim %s: %w", claimList.Items[i].Name, err))
			}
			continue
		}
		if c, ok := deleted.(*ipam.IPClaim); ok {
			deletedList.Items = append(deletedList.Items, *c)
		}
	}
	if len(errs) > 0 {
		return deletedList, errors.Join(errs...)
	}
	return deletedList, nil
}

// releaseAndDelete is a single attempt of TX2: dispose of the allocation
// row(s) for claimKey, dispose of the IPAllocation object the claim is bound
// to, and delete the claim row — all inside one transaction.
//
// Under reclaimPolicy Retain the allocation row survives with its claim
// cleared, and so does the IPAllocation: it is updated to drop spec.claimRef
// rather than deleted, which is what makes retention visible rather than
// inferred. The claim itself goes either way. Reports whether the address was
// retained.
func (r *AllocatingREST) releaseAndDelete(ctx context.Context, claim *ipam.IPClaim, claimKey string) (bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin release transaction: %w", err)
	}
	retained, err := r.allocator.Release(ctx, tx, claimKey)
	if err != nil {
		_ = tx.Rollback(ctx)
		return false, fmt.Errorf("release allocation: %w", err)
	}
	if claim.Status.BoundAllocationRef != nil && claim.Status.BoundAllocationRef.Name != "" {
		allocationKey := allocationObjectKey(tenant.FromContext(ctx), claim.Namespace, claim.Status.BoundAllocationRef.Name)
		i := slices.IndexFunc(retained, func(ra allocator.RetainedAllocation) bool { return ra.AllocationKey == allocationKey })
		if i >= 0 {
			if err := r.unbindAllocation(ctx, tx, retained[i]); err != nil {
				_ = tx.Rollback(ctx)
				return false, err
			}
		} else if _, err := r.allocator.DeleteObject(ctx, tx, allocationKey); err != nil {
			_ = tx.Rollback(ctx)
			return false, fmt.Errorf("delete IPAllocation row: %w", err)
		}
	}
	if _, err := r.allocator.DeleteObject(ctx, tx, claimKey); err != nil {
		_ = tx.Rollback(ctx)
		return false, fmt.Errorf("delete claim row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit release transaction: %w", err)
	}
	return len(retained) > 0, nil
}

// unbindAllocation clears spec.claimRef on a retained IPAllocation and
// publishes its retention (retainedAt, expiresAt and the Retained condition),
// in the transaction that unbound its row and fixed its lease. The update
// emits a MODIFIED event, so a watcher sees the allocation become unbound, and
// when it will be due for release, rather than watching its claim disappear
// and having to infer it.
func (r *AllocatingREST) unbindAllocation(ctx context.Context, tx pgx.Tx, retained allocator.RetainedAllocation) error {
	allocationKey := retained.AllocationKey
	data, err := r.allocator.GetObject(ctx, tx, allocationKey)
	if err != nil {
		if errors.Is(err, allocator.ErrObjectNotFound) {
			// The row it describes was retained, but there is no object left
			// to unbind. Nothing to publish.
			return nil
		}
		return fmt.Errorf("read retained IPAllocation: %w", err)
	}
	obj, err := runtime.Decode(r.codec, data)
	if err != nil {
		return fmt.Errorf("decode retained IPAllocation: %w", err)
	}
	alloc, ok := obj.(*ipam.IPAllocation)
	if !ok {
		return fmt.Errorf("expected *ipam.IPAllocation at %q, got %T", allocationKey, obj)
	}
	alloc.Spec.ClaimRef = nil
	alloc.Status.RetainedAt = retained.RetainedAtTime()
	alloc.Status.ExpiresAt = retained.ExpiresAtTime()
	meta.SetStatusCondition(&alloc.Status.Conditions, retained.RetainedCondition())
	unbound, err := runtime.Encode(r.codec, alloc)
	if err != nil {
		return fmt.Errorf("encode retained IPAllocation: %w", err)
	}
	if _, err := r.allocator.UpdateObject(ctx, tx, allocationKey, unbound); err != nil {
		return fmt.Errorf("unbind retained IPAllocation: %w", err)
	}
	return nil
}

const (
	deleteMaxAttempts  = 3
	deleteRetryBackoff = 100 * time.Millisecond
)

// claimObjectKey is the storage key for an IPClaim. IPClaim is
// namespace-scoped, so the key carries the namespace segment, and the tenant
// prefix ("project/<id>/") is applied so the key matches what the generic
// registry Store reads and writes for the same project-scoped request.
func claimObjectKey(id tenant.Identity, namespace, name string) string {
	return id.ApplyPrefix(fmt.Sprintf("/ipam.miloapis.com/ipclaims/%s/%s", namespace, name))
}

// allocationObjectKey is the storage key for an IPAllocation. IPAllocation
// is namespace-scoped, so the key carries the namespace segment; the tenant
// prefix is applied for the same reason as claimObjectKey.
func allocationObjectKey(id tenant.Identity, namespace, name string) string {
	return id.ApplyPrefix(fmt.Sprintf("/ipam.miloapis.com/ipallocations/%s/%s", namespace, name))
}

// allocationNameFor generates a stable, collision-resistant name for the
// IPAllocation produced by a given claim, using a truncated SHA-256 hash of
// the claim's namespace/name. The "alloc-" prefix makes system-generated
// names obvious and lets operators distinguish them at a glance.
func allocationNameFor(namespace, name string) string {
	h := sha256.Sum256([]byte(namespace + "/" + name))
	return "alloc-" + hex.EncodeToString(h[:8])
}

// isIdentityCollision reports whether err is the unique violation a claim hits
// when something already occupies the identity its IPAllocation would take.
//
// The allocation's name is a hash of the claim's namespace and name, so a
// claim recreated under a name whose predecessor retained its address
// recomputes that address's identity. Both the object row and the allocation
// row refuse it, and neither refusal is an internal error.
func isIdentityCollision(err error) bool {
	return registryerrors.IsUniqueViolation(err, "ipam_cidr_alloc_allocation_key_key", "ipam_objects_pkey")
}

// isClaimNameCollision reports whether err is the refusal a second create
// under a name that already holds an allocation hits.
func isClaimNameCollision(err error) bool {
	return registryerrors.IsUniqueViolation(err, registryerrors.HolderConstraint)
}

// duplicateClaimConflict is the refusal a caller gets for a name it already
// owns. It carries no schema detail: a controller creating by a stable name
// keys off the 409, not off a constraint.
func duplicateClaimConflict(claimName string) error {
	return ipamerrors.NewClaimExists(
		v1alpha1.Resource("ipclaims"),
		claimName,
		"an IPClaim of this name already holds an allocation; delete it to release the address, or claim under a different name",
	)
}

func retainedAllocationConflict(claimName, allocationName string) error {
	return ipamerrors.NewRetainedAllocation(
		v1alpha1.Resource("ipclaims"),
		claimName,
		allocationName,
		fmt.Sprintf("an allocation under this identity already exists: IPAllocation %q, retained by an earlier claim of the same name; delete it to reuse the name", allocationName),
	)
}

// retentionLeaseRequired is the refusal for a claim whose Retain override
// would hold its address with no lease, under class in pool poolName. It names
// both objects a lease could be set on, and the way out that needs neither.
func retentionLeaseRequired(claimName string, class *allocator.ResolvedClass, poolName string) error {
	named := fmt.Sprintf("class %q", class.LocalName)
	if class.LocalName != class.Name {
		named = fmt.Sprintf("class %q (defined as %q in project %q)", class.LocalName, class.Name, class.Project)
	}
	return ipamerrors.NewRetentionLeaseRequired(v1alpha1.Resource("ipclaims"), claimName, fmt.Sprintf(
		"%s does not allow retaining addresses indefinitely: spec.reclaimPolicy Retain overrides its default of Delete, "+
			"and neither the class's retentionLease nor pool %q's maxRetentionLease is set. "+
			"Set retentionLease on the class or maxRetentionLease on the pool, or leave spec.reclaimPolicy unset",
		named, poolName))
}

// resolvePoolError turns a failure to resolve a Block claim's pool into the
// status the caller sees. An error it leaves as a plain error is a server
// fault.
func resolvePoolError(err error) error {
	switch {
	case errors.Is(err, allocator.ErrRangeUnplaced):
		return ipamerrors.New(ipamerrors.ReasonRangeUnplaced, err.Error())
	case errors.Is(err, allocator.ErrAddressOutsidePool):
		return ipamerrors.New(ipamerrors.ReasonAddressOutsidePool, err.Error())
	case errors.Is(err, allocator.ErrNoOfferingPool):
		return ipamerrors.New(ipamerrors.ReasonNoOfferingPool, err.Error())
	case errors.Is(err, allocator.ErrPoolExhausted):
		// Running out of space while provisioning an ancestor is the same
		// outcome as running out in the pool the claim was headed for, and
		// gets the same 507. The level that failed is a level the caller never
		// named, so the message carries the class rather than a pool name.
		return registryerrors.NewInsufficientStorage(err.Error())
	}
	// A scope short a role the class chain requires is a malformed request,
	// not a server fault. The error already names the missing roles and the
	// field that asked for them, which is the whole point of the type.
	var missingRole *scope.MissingRoleError
	if errors.As(err, &missingRole) {
		return ipamerrors.NewScopeRolesMissing(missingRole.Roles, err.Error())
	}
	return fmt.Errorf("resolve pool: %w", err)
}

// mapAllocationError turns an allocator failure into the status the caller
// sees. poolName names the pool the allocation was attempted against, so an
// exhaustion 507 can say which one ran out.
func mapAllocationError(err error, poolName, claimName string) error {
	var inUse *allocator.AddressInUseError
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return registryerrors.NewPoolExhausted(poolName)
	case errors.Is(err, allocator.ErrPoolNotFound):
		return apierrors.NewBadRequest("IPPool not found")
	case errors.As(err, &inUse):
		return ipamerrors.NewAddressInUse(v1alpha1.Resource("ipclaims"), claimName, ipamerrors.Holder{
			Namespace: inUse.AllocationNamespace,
			Name:      inUse.AllocationName,
			Retained:  inUse.Retained,
		}, inUse.Error())
	case errors.Is(err, allocator.ErrAddressReserved):
		return ipamerrors.NewAddressReserved(v1alpha1.Resource("ipclaims"), claimName, err.Error())
	case errors.Is(err, allocator.ErrAddressOutsidePool):
		return ipamerrors.New(ipamerrors.ReasonAddressOutsidePool, err.Error())
	case errors.Is(err, allocation.ErrUnalignedAddress):
		return ipamerrors.New(ipamerrors.ReasonAddressUnaligned, err.Error())
	case errors.Is(err, allocation.ErrInvalidAddress), errors.Is(err, allocation.ErrPrefixLenMismatch):
		return apierrors.NewBadRequest(err.Error())
	default:
		return apierrors.NewInternalError(err)
	}
}

// checkAddressFamily refuses a named address of the other family from the
// class. spec.ipFamily is already held to the class; the address is a second
// statement of the family and is held to it the same way.
func checkAddressFamily(address string, class *allocator.ResolvedClass) error {
	host := address
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// The strategy refuses a malformed address before create gets here.
		return apierrors.NewBadRequest(fmt.Sprintf("spec.address %q is not an IP address", address))
	}
	family := v1alpha1.IPv6
	if ip.Is4() {
		family = v1alpha1.IPv4
	}
	if family != class.Spec.IPFamily {
		return ipamerrors.New(ipamerrors.ReasonFamilyMismatch, fmt.Sprintf(
			"spec.address %s is %s but class %q hands out %s",
			address, family, class.LocalName, class.Spec.IPFamily))
	}
	return nil
}

// singleAddress returns the bare address a single-address block holds, or ""
// for a block of more than one. Every claim reports it, not only claims that
// named an address: a consumer of a /32 or /128 wants the address, not a CIDR
// it has to strip.
func singleAddress(cidr string) string {
	_, block, err := net.ParseCIDR(cidr)
	if err != nil || !allocation.IsSingleAddress(*block) {
		return ""
	}
	return block.IP.String()
}

// Compile-time interface assertions.
var (
	_ rest.Storage           = (*AllocatingREST)(nil)
	_ rest.Creater           = (*AllocatingREST)(nil)
	_ rest.GracefulDeleter   = (*AllocatingREST)(nil)
	_ rest.CollectionDeleter = (*AllocatingREST)(nil)
	_ rest.Storage           = (*IPClaimStatusStorage)(nil)
)
