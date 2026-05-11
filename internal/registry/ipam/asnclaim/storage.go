// Package asnclaim provides REST storage for the ASNClaim resource. The
// exported AllocatingREST type wraps the standard storage with a
// synchronous Postgres-backed allocator that reserves a single ASN from
// the parent ASNPool.
package asnclaim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

type ASNClaimStorage struct {
	*genericregistry.Store
}

type ASNClaimStatusStorage struct {
	store *genericregistry.Store
}

func (s *ASNClaimStatusStorage) New() runtime.Object { return &ipam.ASNClaim{} }
func (s *ASNClaimStatusStorage) Destroy()            {}

func (s *ASNClaimStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *ASNClaimStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *ASNClaimStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *ASNClaimStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

func newInnerStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*ASNClaimStorage, *ASNClaimStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.ASNClaim{} },
		NewListFunc:               func() runtime.Object { return &ipam.ASNClaimList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("asnclaims"),
		SingularQualifiedResource: v1alpha1.Resource("asnclaim"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("asnclaims")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &ASNClaimStorage{store}, &ASNClaimStatusStorage{store: &statusStore}, nil
}

type AllocatingREST struct {
	*ASNClaimStorage
	allocator allocator.ASNAllocator
	db        *pgxpool.Pool
	strategy  asnClaimStrategy
	codec     runtime.Codec
}

func NewAllocatingStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.ASNAllocator, db *pgxpool.Pool, codec runtime.Codec) (*AllocatingREST, *ASNClaimStatusStorage, error) {
	claimStore, statusStore, err := newInnerStorage(scheme, optsGetter)
	if err != nil {
		return nil, nil, err
	}
	return &AllocatingREST{
		ASNClaimStorage: claimStore,
		allocator:       alloc,
		db:              db,
		strategy:        NewStrategy(scheme),
		codec:           codec,
	}, statusStore, nil
}

func (r *AllocatingREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	claim, ok := obj.(*ipam.ASNClaim)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.ASNClaim, got %T", obj)
	}
	// Tenant identity is needed up front for the project / org metric labels.
	// org is "" today for project-scoped requests until Milo forwards the
	// owning org alongside the project (see tenant.Identity.Org).
	id := tenant.FromContext(ctx)
	project := id.Project()
	org := id.Org()
	// ip_family is hardcoded to "ASN" for asnclaim — autonomous system numbers
	// do not have an address family. Using "ASN" here keeps the metric label
	// set aligned with PoolUtilization{ip_family="ASN"} and ensures
	// AllocationAttempts, AllocationFailures, and the latency histogram all
	// split identically.
	const ipFamily = "ASN"
	metrics.AllocationAttempts.WithLabelValues("asnclaim", ipFamily, project, org).Inc()
	allocStart := time.Now()
	result := "error"
	defer func() {
		metrics.ObserveAllocationDuration("asnclaim", result, ipFamily, project, org, allocStart)
	}()

	objectMeta, err := meta.Accessor(claim)
	if err != nil {
		metrics.RecordAllocationFailure("asnclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("get object metadata: %w", err)
	}
	rest.FillObjectMetaSystemFields(objectMeta)

	if err := rest.BeforeCreate(r.strategy, ctx, claim); err != nil {
		metrics.RecordAllocationFailure("asnclaim", "internal", ipFamily, project, org)
		return nil, err
	}
	if createValidation != nil {
		if err := createValidation(ctx, claim.DeepCopyObject()); err != nil {
			metrics.RecordAllocationFailure("asnclaim", "internal", ipFamily, project, org)
			return nil, err
		}
	}

	if claim.Spec.PoolRef == nil && claim.Spec.ClassRef == nil {
		metrics.RecordAllocationFailure("asnclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("synchronous allocation requires spec.poolRef or spec.classRef")
	}
	if claim.Spec.PoolRef != nil && claim.Spec.ClassRef != nil {
		metrics.RecordAllocationFailure("asnclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("spec.poolRef and spec.classRef are mutually exclusive")
	}

	if !id.IsPlatform() {
		claim.Spec.OwnerRef = &ipam.ObjectRef{
			APIGroup: id.APIGroup,
			Kind:     id.Kind,
			Name:     id.Name,
		}
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		metrics.RecordAllocationFailure("asnclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("begin allocation transaction: %w", err)
	}

	// Resolve the target ASN pool. spec.poolRef is a direct named lookup;
	// spec.classRef enumerates ASNPools whose spec.classRef.name matches and
	// picks the first by storage key. Class-based selection always evaluates
	// within the caller's project scope.
	//
	// ASNClaim is intentionally LocalRef-only — there is NO ProjectRef on
	// spec.poolRef or spec.classRef, and that is a deliberate design
	// decision, not a missing feature. ASN pools are global / platform-
	// scoped infrastructure resources (ASNs are assigned to backbone
	// elements, peering routers, internal AS boundaries — not to consumer
	// workloads), so the cross-project authorization gate that
	// IPPrefixClaim and IPAddressClaim need (SubjectAccessReview against a
	// pool in another project's namespace, with the visibility=shared
	// annotation as the visibility fingerprint) does not apply. Every
	// ASNClaim resolves its pool inside the caller's own scope (or
	// platform scope for platform-scoped requests). If a future use case
	// genuinely needs cross-project ASN allocation, the ProjectRef field
	// can be added to the spec and routed through the same access.Pool*
	// gate the prefix/address claims use; until then, keeping the surface
	// LocalRef-only avoids exposing a SAR seam that the platform-scoping
	// of ASNs makes unnecessary.
	var poolKey, poolName string
	if claim.Spec.PoolRef != nil {
		poolName = claim.Spec.PoolRef.Name
		poolKey = id.ResourceKey("asnpools", poolName)
	} else {
		resolved, rerr := allocator.ResolveASNPool(ctx, tx, claim.Spec.ClassRef.Name, id.Name)
		if rerr != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(rerr, allocator.ErrPoolNotFound) {
				metrics.RecordAllocationFailure("asnclaim", "pool_not_found", ipFamily, project, org)
				return nil, apierrors.NewBadRequest(fmt.Sprintf("no ASNPool of class %q", claim.Spec.ClassRef.Name))
			}
			metrics.RecordAllocationFailure("asnclaim", "internal", ipFamily, project, org)
			return nil, fmt.Errorf("resolve asn pool: %w", rerr)
		}
		poolKey = resolved
		// Storage key tail is the pool name; needed for status.boundPoolRef.
		poolName = poolKey[strings.LastIndex(poolKey, "/")+1:]
	}
	claimKey := claimObjectKey(claim.Namespace, claim.Name)

	asn, err := r.allocator.AllocateASN(ctx, tx, poolKey, claimKey, id.Name)
	if err != nil {
		_ = tx.Rollback(ctx)
		reason := allocationFailureReason(err)
		metrics.RecordAllocationFailure("asnclaim", reason, ipFamily, project, org)
		if reason == "pool_exhausted" {
			result = "exhausted"
		}
		return nil, mapAllocationError(err)
	}

	claim.Status.Phase = ipam.ClaimBound
	claim.Status.ASN = asn
	claim.Status.BoundPoolRef = &ipam.LocalRef{Name: poolName}

	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("asnclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("encode claim: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, claimKey, "ASNClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("asnclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("persist claim: %w", err)
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(claim, uint64(rv)); err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("asnclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("set resource version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		metrics.RecordAllocationFailure("asnclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("commit allocation transaction: %w", err)
	}
	result = "success"
	return claim, nil
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
// the intermediate phase=Releasing state before the object disappears. See
// the IPPrefixClaim Delete handler for the full rationale; this is the same
// pattern adapted to ASNClaim.
func (r *AllocatingREST) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	existing, err := r.ASNClaimStorage.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	claim, ok := existing.(*ipam.ASNClaim)
	if !ok {
		return nil, false, fmt.Errorf("expected *ipam.ASNClaim from Get, got %T", existing)
	}
	if deleteValidation != nil {
		if err := deleteValidation(ctx, claim.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}
	claimKey := claimObjectKey(claim.Namespace, claim.Name)

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

	// TX2 — release the allocation and delete the object row, with retry.
	var lastErr error
	for attempt := 1; attempt <= deleteMaxAttempts; attempt++ {
		lastErr = r.releaseAndDelete(ctx, claimKey)
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
		return nil, false, fmt.Errorf("release allocation after %d attempts: %w", deleteMaxAttempts, lastErr)
	}

	klog.V(2).InfoS("claim released and deleted", "claim", name)
	metrics.RecordRelease("asnclaim")
	return releasing, true, nil
}

// releaseAndDelete is a single attempt of TX2: release the ASN allocation
// row(s) for claimKey and delete the object row, all inside one transaction.
func (r *AllocatingREST) releaseAndDelete(ctx context.Context, claimKey string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin release transaction: %w", err)
	}
	if err := r.allocator.Release(ctx, tx, claimKey); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("release allocation: %w", err)
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

// deleteMaxAttempts and deleteRetryBackoff govern the TX2 retry loop.
const (
	deleteMaxAttempts  = 3
	deleteRetryBackoff = 100 * time.Millisecond
)

func claimObjectKey(namespace, name string) string {
	return fmt.Sprintf("/ipam.miloapis.com/asnclaims/%s/%s", namespace, name)
}

func mapAllocationError(err error) error {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return registryerrors.NewInsufficientStorage("ASN pool exhausted")
	case errors.Is(err, allocator.ErrPoolNotFound):
		return apierrors.NewBadRequest("ASN pool not found")
	default:
		return apierrors.NewInternalError(err)
	}
}
