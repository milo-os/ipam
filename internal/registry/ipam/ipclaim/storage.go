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
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
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
		attribute.String(tracing.AttrScope, tracing.Scope(id.IsPlatform())),
		attribute.String(tracing.AttrProject, project),
		attribute.Bool(tracing.AttrHasParentExtras, id.APIGroup != "" || id.Kind != "" || id.Name != ""),
	)
	resolveSpan.End()

	span.SetAttributes(
		attribute.String(tracing.AttrTenantScope, tracing.Scope(id.IsPlatform())),
		attribute.String(tracing.AttrTenantProject, project),
		attribute.String(tracing.AttrTenantOrg, org),
		attribute.Int(tracing.AttrClaimPrefix, claim.Spec.PrefixLength),
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

	if claim.Spec.PoolRef == nil && claim.Spec.PoolSelector == nil {
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("synchronous allocation requires spec.poolRef or spec.poolSelector")
	}
	if claim.Spec.PoolRef != nil && claim.Spec.PoolSelector != nil {
		metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("spec.poolRef and spec.poolSelector are mutually exclusive")
	}

	if !id.IsPlatform() {
		// Overwrite client-supplied ownerRef — requestheader CA guarantees
		// Extra authenticity, so the tenant identity is the source of truth.
		claim.Spec.OwnerRef = &ipam.ObjectRef{
			APIGroup: id.APIGroup,
			Kind:     id.Kind,
			Name:     id.Name,
		}
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		metrics.RecordAllocationFailure("ipclaim", "tx_error", ipFamily, project, org)
		failSpan(tracing.ReasonTxError)
		return nil, fmt.Errorf("begin allocation transaction: %w", err)
	}

	// Resolve the target IPPool. spec.poolRef is a direct named lookup;
	// spec.poolSelector lists candidate pools, filters by the supplied
	// label selector, and picks the first match by storage key (see
	// allocator.ResolveIPPool). IPPool is cluster-scoped, so the storage
	// key always lives at the platform prefix regardless of the calling
	// project's tenant identity.
	isCrossProject := false
	var poolKey, poolName string
	if claim.Spec.PoolRef != nil {
		poolName = claim.Spec.PoolRef.Name
		isCrossProject = !id.IsPlatform() &&
			claim.Spec.PoolRef.ProjectRef != nil &&
			claim.Spec.PoolRef.ProjectRef.Name != id.Name
		// The pool lives in the caller's own project unless a cross-project
		// ProjectRef points it elsewhere; platform callers (empty id.Name)
		// address the platform root.
		poolProject := id.Name
		if isCrossProject {
			poolProject = claim.Spec.PoolRef.ProjectRef.Name
		}
		poolKey = poolStorageKey(poolProject, poolName)
	} else {
		// Selector lookups scan one project's pools: the caller's own, or the
		// referenced project for cross-project shared pools.
		ownerProject := id.Name
		if claim.Spec.PoolSelector.ProjectRef != nil {
			isCrossProject = !id.IsPlatform() &&
				claim.Spec.PoolSelector.ProjectRef.Name != id.Name
			if isCrossProject {
				ownerProject = claim.Spec.PoolSelector.ProjectRef.Name
			}
		}
		resolved, rerr := allocator.ResolveIPPool(ctx, tx, claim.Spec.PoolSelector.LabelSelector, ownerProject, string(claim.Spec.IPFamily))
		if rerr != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(rerr, allocator.ErrPoolNotFound) {
				metrics.RecordAllocationFailure("ipclaim", "pool_not_found", ipFamily, project, org)
				failSpan(tracing.ReasonPoolNotFound)
				return nil, apierrors.NewBadRequest("no IPPool matches spec.poolSelector")
			}
			metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
			failSpan(tracing.ReasonTxError)
			return nil, fmt.Errorf("resolve IPPool: %w", rerr)
		}
		poolKey = resolved
		poolName = poolKey[strings.LastIndex(poolKey, "/")+1:]
	}
	claimKey := claimObjectKey(id, claim.Namespace, claim.Name)
	span.SetAttributes(attribute.String(tracing.AttrPoolName, poolName))

	if isCrossProject {
		if err := r.authorizeCrossProject(ctx, tx, poolKey); err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, access.ErrCrossProjectDenied) {
				// Selector-driven lookups must not distinguish "no pool
				// matched the selector" from "a pool matched but you
				// can't use it" — that distinction is a label/existence
				// fingerprint into another project. Direct poolRef
				// lookups can return Forbidden because the caller already
				// named the pool by hand.
				if claim.Spec.PoolSelector != nil {
					metrics.RecordAllocationFailure("ipclaim", "pool_not_found", ipFamily, project, org)
					failSpan(tracing.ReasonCrossProjectDenied)
					return nil, apierrors.NewBadRequest("no IPPool matches spec.poolSelector")
				}
				metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
				failSpan(tracing.ReasonCrossProjectDenied)
				return nil, apierrors.NewForbidden(
					v1alpha1.Resource("ippools"),
					poolKey,
					fmt.Errorf("cross-project pool not accessible"),
				)
			}
			metrics.RecordAllocationFailure("ipclaim", "internal", ipFamily, project, org)
			failSpan(tracing.ReasonTxError)
			return nil, err
		}
	}

	cidr, effectiveFamily, err := r.allocator.AllocatePrefix(ctx, tx, poolKey, claim.Spec.PrefixLength, string(claim.Spec.IPFamily), claimKey, id.Name)
	if err != nil {
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
		return nil, mapAllocationError(err)
	}

	// The pool determines the address family. Default it onto the claim so the
	// persisted claim and its IPAllocation always report the resolved family,
	// even when the client omitted spec.ipFamily (it is optional).
	claim.Spec.IPFamily = ipam.IPFamily(effectiveFamily)

	// Build the IPAllocation object that records this binding. It lives in
	// the claim's namespace; its name is a stable hash of the claim
	// namespace/name so the Delete handler can recompute it deterministically.
	allocationName := allocationNameFor(claim.Namespace, claim.Name)
	allocationKey := allocationObjectKey(id, claim.Namespace, allocationName)

	// Populate the claim status with the computed allocation up-front so both
	// the dry-run and the persisting paths return identical bound status.
	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedCIDR = cidr
	claim.Status.BoundAllocationRef = &ipam.LocalRef{Name: allocationName}

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

	alloc := &ipam.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      allocationName,
			Namespace: claim.Namespace,
		},
		Spec: ipam.IPAllocationSpec{
			IPFamily: claim.Spec.IPFamily,
			PoolRef:  ipam.LocalRef{Name: poolName},
		},
		Status: ipam.IPAllocationStatus{
			Phase:         ipam.AllocationReady,
			AllocatedCIDR: cidr,
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
	case errors.Is(err, allocator.ErrFamilyMismatch):
		return "family_mismatch"
	case errors.Is(err, allocator.ErrPrefixLengthOutOfRange):
		return "prefix_length_out_of_range"
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
		attribute.String(tracing.AttrTenantScope, tracing.Scope(id.IsPlatform())),
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
	for attempt := 1; attempt <= deleteMaxAttempts; attempt++ {
		lastErr = r.releaseAndDelete(ctx, claim, claimKey)
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

	klog.V(2).InfoS("claim released and deleted", "claim", name)
	metrics.RecordRelease("ipclaim")
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

// releaseAndDelete is a single attempt of TX2: release the allocation
// row(s) for claimKey, delete the IPAllocation row recorded on the claim,
// and delete the claim row — all inside one transaction.
func (r *AllocatingREST) releaseAndDelete(ctx context.Context, claim *ipam.IPClaim, claimKey string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin release transaction: %w", err)
	}
	if err := r.allocator.Release(ctx, tx, claimKey); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("release allocation: %w", err)
	}
	if claim.Status.BoundAllocationRef != nil && claim.Status.BoundAllocationRef.Name != "" {
		allocationKey := allocationObjectKey(tenant.FromContext(ctx), claim.Namespace, claim.Status.BoundAllocationRef.Name)
		if _, err := r.allocator.DeleteObject(ctx, tx, allocationKey); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("delete IPAllocation row: %w", err)
		}
	}
	if _, err := r.allocator.DeleteObject(ctx, tx, claimKey); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("delete claim row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit release transaction: %w", err)
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

// poolStorageKey is the storage key for an IPPool owned by the given project
// ("" for platform scope). Although IPPool is cluster-scoped at the API layer,
// a pool created through a project control-plane is persisted under that
// project's tenant prefix — so the allocator must address it with the same
// prefix rather than at the platform root.
func poolStorageKey(project, name string) string {
	return tenant.Identity{Name: project}.ResourceKey("ippools", name)
}

// allocationNameFor generates a stable, collision-resistant name for the
// IPAllocation produced by a given claim, using a truncated SHA-256 hash of
// the claim's namespace/name. The "alloc-" prefix makes system-generated
// names obvious and lets operators distinguish them at a glance.
func allocationNameFor(namespace, name string) string {
	h := sha256.Sum256([]byte(namespace + "/" + name))
	return "alloc-" + hex.EncodeToString(h[:8])
}

// authorizeCrossProject delegates to the shared cross-project gate in
// internal/access.
func (r *AllocatingREST) authorizeCrossProject(ctx context.Context, tx pgx.Tx, poolKey string) error {
	return access.AuthorizeCrossProjectPrefix(ctx, tx, poolKey, r.poolChecker)
}

func mapAllocationError(err error) error {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return registryerrors.NewInsufficientStorage("IPPool exhausted")
	case errors.Is(err, allocator.ErrPoolNotFound):
		return apierrors.NewBadRequest("IPPool not found")
	case errors.Is(err, allocator.ErrFamilyMismatch):
		return apierrors.NewBadRequest(err.Error())
	case errors.Is(err, allocator.ErrPrefixLengthOutOfRange):
		return apierrors.NewBadRequest(err.Error())
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
