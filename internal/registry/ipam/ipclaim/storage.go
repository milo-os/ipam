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
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
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

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipclaims")),
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
	codec       runtime.Codec
}

// txBeginner is the minimal slice of *pgxpool.Pool the allocation handlers
// depend on. Narrowing the field to this interface lets unit tests inject a
// fake transaction without a live Postgres.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewAllocatingStorage builds the IPClaim REST storage with synchronous
// Postgres-backed allocation. db must be the same pool the allocator commits
// against; codec is used to serialise the synchronously-allocated claim and
// the generated IPAllocation into ipam_objects so subsequent GETs return
// fully-populated objects. poolChecker may be nil; when non-nil it
// authorises cross-project claims via SubjectAccessReview before allocation.
func NewAllocatingStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool, codec runtime.Codec, poolChecker access.PoolAccessChecker) (*AllocatingREST, *IPClaimStatusStorage, error) {
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
		codec:          codec,
	}, statusStore, nil
}

// Create runs the standard create pipeline (system-metadata fill, strategy
// PrepareForCreate, validation), then drives the allocator inside a
// short-lived transaction. The transaction persists the claim row, the
// allocation row in ipam_cidr_allocations, and the IPAllocation API object
// together so the response body carries a CIDR that has already been
// reserved.
func (r *AllocatingREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	claim, ok := obj.(*ipam.IPClaim)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPClaim, got %T", obj)
	}

	// A server-side dry-run must compute the would-be allocation but persist
	// nothing and consume no capacity (see Create body for how the transaction
	// is rolled back).
	dryRun := isDryRun(options.DryRun)

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

	objectMeta, err := meta.Accessor(claim)
	if err != nil {
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("get object metadata: %w", err)
	}
	rest.FillObjectMetaSystemFields(objectMeta)

	if err := rest.BeforeCreate(r.strategy, ctx, claim); err != nil {
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, err
	}
	if createValidation != nil {
		if err := createValidation(ctx, claim.DeepCopyObject()); err != nil {
			metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
			return nil, err
		}
	}

	if claim.Spec.ClassName == "" && claim.Spec.IPFamily == "" {
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("spec.className or spec.ipFamily is required")
	}

	// Overwrite any client-supplied ownerRef. The requestheader CA guarantees
	// the Extra headers carrying the tenant identity, so that identity is the
	// source of truth for who this claim is for.
	claim.Spec.OwnerRef = &ipam.ObjectRef{
		APIGroup: id.APIGroup,
		Kind:     id.Kind,
		Name:     id.Name,
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		metrics.RecordAllocationFailure("ipclaim", "tx_error", ipFamily, project, org)
		failSpan(tracing.ReasonTxError)
		return nil, fmt.Errorf("begin allocation transaction: %w", err)
	}

	// A claim names a CLASS. Resolution starts in the caller's own project and
	// follows spec.source into the project holding the definition; discovery
	// then finds the pool that project offers. Cross-project sharing is the
	// reference, so there is no separate cross-project path here.
	class, err := allocator.ResolveClass(ctx, tx, claim.Spec.ClassName, v1alpha1.IPFamily(claim.Spec.IPFamily))
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipclaim", "class_not_found", ipFamily, project, org)
		failSpan(tracing.ReasonPoolNotFound)
		if errors.Is(err, allocator.ErrClassNotFound) || errors.Is(err, allocator.ErrNoDefaultClass) {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		return nil, fmt.Errorf("resolve class: %w", err)
	}

	prefixLen, err := allocator.EffectivePrefixLength(class.IPClass, claim.Spec.PrefixLength)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest(err.Error())
	}
	reclaimPolicy := allocator.EffectiveReclaimPolicy(class.IPClass, v1alpha1.ReclaimPolicy(claim.Spec.ReclaimPolicy))

	// The address space this allocation must be unique in. A claim that does
	// not carry a role its class names in uniqueWithin is refused by name
	// rather than compared against a wider space: widening would look correct
	// while refusing addresses the narrow comparison was meant to allow, and
	// the operator would see a pool exhaust at a fraction of its capacity with
	// nothing to explain it.
	uniqueScope, scopeDigest, err := scope.ProjectAddressSpaceDigest(id.Name, claim.Spec.Scope, class.Spec.UniqueWithin, "uniqueWithin")
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		var missing *scope.MissingRoleError
		if errors.As(err, &missing) {
			return nil, apierrors.NewBadRequest(fmt.Sprintf("%s (class %q)", missing.Error(), class.Name))
		}
		return nil, apierrors.NewBadRequest(err.Error())
	}

	// Resolving the pool provisions any missing level of the class's chain, and
	// each level commits on its own — so it runs BEFORE the allocation
	// transaction opens, not inside it. A pool is durable infrastructure that
	// outlives the claim that caused it; holding this transaction across the
	// chain would make a herd of first claims serialise behind the slowest.
	_ = tx.Rollback(ctx)
	poolKey, err := allocator.ResolvePool(ctx, r.db, class, claim.Spec.Scope)
	if err != nil {
		metrics.RecordAllocationFailure("ipclaim", "pool_not_found", ipFamily, project, org)
		failSpan(tracing.ReasonPoolNotFound)
		if errors.Is(err, allocator.ErrNoOfferingPool) {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		// A scope short a role the class chain requires is a malformed request,
		// not a server fault. The error already names the missing roles and the
		// field that asked for them, which is the whole point of the type.
		var missingRole *scope.MissingRoleError
		if errors.As(err, &missingRole) {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		// Running out of space while provisioning an ancestor is the same
		// outcome as running out in the pool the claim was headed for, and gets
		// the same 507. The level that failed is a level the caller never named,
		// so the message carries the class rather than a pool name.
		if errors.Is(err, allocator.ErrPoolExhausted) {
			return nil, registryerrors.NewInsufficientStorage(err.Error())
		}
		return nil, fmt.Errorf("resolve pool: %w", err)
	}

	tx, err = r.db.Begin(ctx)
	if err != nil {
		metrics.RecordAllocationFailure("ipclaim", "tx_error", ipFamily, project, org)
		failSpan(tracing.ReasonTxError)
		return nil, fmt.Errorf("begin allocation transaction: %w", err)
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
		ReclaimPolicy: reclaimPolicy,
	})
	if err != nil {
		if isIdentityCollision(err) {
			_ = tx.Rollback(ctx)
			metrics.RecordAllocationFailure("ipclaim", "conflict", ipFamily, project, org)
			failSpan(tracing.ReasonTxError)
			return nil, retainedAllocationConflict(claim.Name, allocationName)
		}
		_ = tx.Rollback(ctx)
		reason := allocationFailureReason(err)
		metrics.RecordAllocationFailure("ipclaim", reason, ipFamily, project, org)
		switch reason {
		case "pool_exhausted":
			result = "exhausted"
			failSpan(tracing.ReasonExhausted)
		case "pool_not_found":
			failSpan(tracing.ReasonPoolNotFound)
		default:
			failSpan(tracing.ReasonTxError)
		}
		return nil, mapAllocationError(err, poolName)
	}

	// Populate the claim status with the computed allocation up-front so both
	// the dry-run and the persisting paths return identical bound status.
	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedCIDR = cidr
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

	// The allocation records what it was handed out under, not just where it
	// came from: under Retain it is read after the claim that chose those
	// values is gone.
	alloc := &ipam.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      allocationName,
			Namespace: claim.Namespace,
		},
		Spec: ipam.IPAllocationSpec{
			IPFamily:      ipam.IPFamily(class.Spec.IPFamily),
			PoolRef:       ipam.LocalRef{Name: poolName},
			ClassName:     class.Name,
			Purpose:       ipam.PurposeClaim,
			ClaimRef:      &ipam.LocalRef{Name: claim.Name},
			Scope:         uniqueScope,
			ReclaimPolicy: ipam.ReclaimPolicy(reclaimPolicy),
			OwnerRef:      claim.Spec.OwnerRef,
		},
		Status: ipam.IPAllocationStatus{
			Phase:         ipam.AllocationReady,
			AllocatedCIDR: cidr,
			ScopeDigest:   scopeDigest,
		},
	}
	allocData, err := runtime.Encode(r.codec, alloc)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("encode IPAllocation: %w", err)
	}
	if _, err := r.allocator.InsertObject(ctx, tx, allocationKey, "IPAllocation", claim.Namespace, allocationName, allocData); err != nil {
		_ = tx.Rollback(ctx)
		if isIdentityCollision(err) {
			metrics.RecordAllocationFailure("ipclaim", "conflict", ipFamily, project, org)
			return nil, retainedAllocationConflict(claim.Name, allocationName)
		}
		metrics.RecordAllocationFailure("ipclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("persist IPAllocation: %w", err)
	}

	// Claim status was populated above (before the dry-run branch); the
	// persisted record carries the allocated CIDR + reference back to the
	// IPAllocation row. Watchers see a single ADDED event with the terminal
	// bound state.
	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("encode claim: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, claimKey, "IPClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("persist claim: %w", err)
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(claim, uint64(rv)); err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("set resource version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		metrics.RecordAllocationFailure("ipclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("commit allocation transaction: %w", err)
	}

	result = "success"
	return claim, nil
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
func allocationFailureReason(err error) string {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return "pool_exhausted"
	case errors.Is(err, allocator.ErrPoolNotFound):
		return "pool_not_found"
	default:
		return "tx_error"
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
		if slices.Contains(retained, allocationKey) {
			if err := r.unbindAllocation(ctx, tx, allocationKey); err != nil {
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

// unbindAllocation clears spec.claimRef on a retained IPAllocation, in the
// transaction that unbound its row. The update emits a MODIFIED event, so a
// watcher sees the allocation become unbound rather than watching its claim
// disappear and having to infer it.
func (r *AllocatingREST) unbindAllocation(ctx context.Context, tx pgx.Tx, allocationKey string) error {
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
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		return false
	}
	switch pgErr.ConstraintName {
	case "ipam_cidr_alloc_allocation_key_key", "ipam_objects_pkey":
		return true
	default:
		return false
	}
}

const pgUniqueViolation = "23505"

func retainedAllocationConflict(claimName, allocationName string) error {
	return apierrors.NewConflict(
		v1alpha1.Resource("ipclaims"),
		claimName,
		fmt.Errorf("an allocation under this identity already exists: IPAllocation %q, retained by an earlier claim of the same name; delete it to reuse the name", allocationName),
	)
}

// mapAllocationError turns an allocator failure into the status the caller
// sees. poolName names the pool the allocation was attempted against, so an
// exhaustion 507 can say which one ran out.
func mapAllocationError(err error, poolName string) error {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return registryerrors.NewPoolExhausted(poolName)
	case errors.Is(err, allocator.ErrPoolNotFound):
		return apierrors.NewBadRequest("IPPool not found")
	default:
		return apierrors.NewInternalError(err)
	}
}

// Compile-time interface assertions.
var (
	_ rest.Storage           = (*AllocatingREST)(nil)
	_ rest.Creater           = (*AllocatingREST)(nil)
	_ rest.GracefulDeleter   = (*AllocatingREST)(nil)
	_ rest.CollectionDeleter = (*AllocatingREST)(nil)
	_ rest.Storage           = (*IPClaimStatusStorage)(nil)
)
