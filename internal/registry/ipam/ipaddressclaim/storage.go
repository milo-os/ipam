// Package ipaddressclaim provides REST storage for the IPAddressClaim
// resource. The exported AllocatingREST type wraps the standard storage
// with a synchronous Postgres-backed allocator that reserves a single
// host IP address from the parent IPPrefix pool.
package ipaddressclaim

import (
	"context"
	"errors"
	"fmt"
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

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

type IPAddressClaimStorage struct {
	*genericregistry.Store
}

type IPAddressClaimStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPAddressClaimStatusStorage) New() runtime.Object { return &ipam.IPAddressClaim{} }
func (s *IPAddressClaimStatusStorage) Destroy()            {}

func (s *IPAddressClaimStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPAddressClaimStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *IPAddressClaimStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *IPAddressClaimStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

func newInnerStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*IPAddressClaimStorage, *IPAddressClaimStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPAddressClaim{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPAddressClaimList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipaddressclaims"),
		SingularQualifiedResource: v1alpha1.Resource("ipaddressclaim"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipaddressclaims")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &IPAddressClaimStorage{store}, &IPAddressClaimStatusStorage{store: &statusStore}, nil
}

type AllocatingREST struct {
	*IPAddressClaimStorage
	allocator   allocator.PrefixAllocator
	db          *pgxpool.Pool
	strategy    ipAddressClaimStrategy
	poolChecker access.PoolAccessChecker
	codec       runtime.Codec
}

// NewAllocatingStorage builds the IPAddressClaim REST storage with
// synchronous Postgres-backed allocation. poolChecker may be nil; when
// non-nil it authorises cross-project claims (prefixSelector.projectRef
// targeting another project) via SubjectAccessReview before allocation.
// When nil, cross-project allocation fails closed — the visibility=shared
// marker on the IPPrefixClass is intent-only and never sufficient on its
// own. Mirrors the IPPrefixClaim auth pattern (audit findings H1/H6,
// task #20).
func NewAllocatingStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool, codec runtime.Codec, poolChecker access.PoolAccessChecker) (*AllocatingREST, *IPAddressClaimStatusStorage, error) {
	claimStore, statusStore, err := newInnerStorage(scheme, optsGetter)
	if err != nil {
		return nil, nil, err
	}
	return &AllocatingREST{
		IPAddressClaimStorage: claimStore,
		allocator:             alloc,
		db:                    db,
		strategy:              NewStrategy(scheme),
		poolChecker:           poolChecker,
		codec:                 codec,
	}, statusStore, nil
}

func (r *AllocatingREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	claim, ok := obj.(*ipam.IPAddressClaim)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPAddressClaim, got %T", obj)
	}
	// Extract tenant identity up front so the project / org labels are
	// available to AllocationAttempts and the deferred AllocationDuration
	// observation. project / org come from tenant.Identity helpers
	// (iam.miloapis.com/parent-* extras); both are "" for platform-scoped
	// requests, and org is "" today for project-scoped requests until Milo
	// forwards the owning org alongside the project.
	id := tenant.FromContext(ctx)
	project := id.Project()
	org := id.Org()
	// ip_family is sourced from claim.Spec.IPFamily before any metric is
	// recorded so AllocationAttempts, AllocationFailures, and the latency
	// histogram all split identically. claim.Spec.IPFamily is set on every
	// valid IPAddressClaim ("IPv4" or "IPv6"); pre-spec failures land in the
	// empty-string family and are clearly distinguishable from the
	// family-tagged successes.
	ipFamily := string(claim.Spec.IPFamily)
	metrics.AllocationAttempts.WithLabelValues("ipaddressclaim", ipFamily, project, org).Inc()
	allocStart := time.Now()
	result := "error"
	defer func() {
		metrics.ObserveAllocationDuration("ipaddressclaim", result, ipFamily, project, org, allocStart)
	}()

	objectMeta, err := meta.Accessor(claim)
	if err != nil {
		metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("get object metadata: %w", err)
	}
	rest.FillObjectMetaSystemFields(objectMeta)

	if err := rest.BeforeCreate(r.strategy, ctx, claim); err != nil {
		metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
		return nil, err
	}
	if createValidation != nil {
		if err := createValidation(ctx, claim.DeepCopyObject()); err != nil {
			metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
			return nil, err
		}
	}

	if claim.Spec.PrefixRef == nil && claim.Spec.PrefixSelector == nil {
		metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("synchronous allocation requires spec.prefixRef or spec.prefixSelector")
	}
	if claim.Spec.PrefixRef != nil && claim.Spec.PrefixSelector != nil {
		metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("spec.prefixRef and spec.prefixSelector are mutually exclusive")
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
		metrics.RecordAllocationFailure("ipaddressclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("begin allocation transaction: %w", err)
	}

	// Resolve the target prefix pool. spec.prefixRef is a direct named
	// lookup; spec.prefixSelector lists candidates and picks the first
	// match (allocator.ResolvePrefixPool documents the strategy). Both
	// paths support an optional cross-project ProjectRef pointing at a
	// foreign project's pool; that branch sets isCrossProject so we can
	// run the same SAR + visibility=shared gate as IPPrefixClaim before
	// allocating (audit findings H1/H6 — task #20).
	isCrossProject := false
	var poolKey string
	if claim.Spec.PrefixRef != nil {
		isCrossProject = !id.IsPlatform() &&
			claim.Spec.PrefixRef.ProjectRef != nil &&
			claim.Spec.PrefixRef.ProjectRef.Name != id.Name
		if isCrossProject {
			poolKey = tenant.Identity{Name: claim.Spec.PrefixRef.ProjectRef.Name}.ResourceKey("ipprefixes", claim.Spec.PrefixRef.Name)
		} else {
			poolKey = id.ResourceKey("ipprefixes", claim.Spec.PrefixRef.Name)
		}
	} else {
		ownerProject := id.Name
		if claim.Spec.PrefixSelector.ProjectRef != nil {
			ownerProject = claim.Spec.PrefixSelector.ProjectRef.Name
			isCrossProject = !id.IsPlatform() && ownerProject != id.Name
		}
		resolved, rerr := allocator.ResolvePrefixPool(ctx, tx, claim.Spec.PrefixSelector.LabelSelector, ownerProject, string(claim.Spec.IPFamily))
		if rerr != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(rerr, allocator.ErrPoolNotFound) {
				metrics.RecordAllocationFailure("ipaddressclaim", "pool_not_found", ipFamily, project, org)
				return nil, apierrors.NewBadRequest("no IPPrefix pool matches spec.prefixSelector")
			}
			metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
			return nil, fmt.Errorf("resolve prefix pool: %w", rerr)
		}
		poolKey = resolved
	}
	claimKey := claimObjectKey(claim.Namespace, claim.Name)

	if isCrossProject {
		if err := access.AuthorizeCrossProjectPrefix(ctx, tx, poolKey, r.poolChecker); err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, access.ErrCrossProjectDenied) {
				// Mask the failure so the selector path can't be used to
				// fingerprint another project's pools by trial labels —
				// the response must be indistinguishable from "no pool
				// matched the selector". Direct prefixRef lookups can
				// return Forbidden because the caller already named the
				// pool by hand, so revealing forbidden-vs-not-found
				// reveals nothing new.
				if claim.Spec.PrefixSelector != nil {
					metrics.RecordAllocationFailure("ipaddressclaim", "pool_not_found", ipFamily, project, org)
					return nil, apierrors.NewBadRequest("no IPPrefix pool matches spec.prefixSelector")
				}
				metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
				return nil, apierrors.NewForbidden(
					v1alpha1.Resource("ipprefixes"),
					poolKey,
					fmt.Errorf("cross-project pool not accessible"),
				)
			}
			metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
			return nil, err
		}
	}

	addr, err := r.allocator.AllocateSingleAddress(ctx, tx, poolKey, string(claim.Spec.IPFamily), claimKey, id.Name)
	if err != nil {
		_ = tx.Rollback(ctx)
		reason := allocationFailureReason(err)
		metrics.RecordAllocationFailure("ipaddressclaim", reason, ipFamily, project, org)
		if reason == "pool_exhausted" {
			result = "exhausted"
		}
		return nil, mapAllocationError(err)
	}

	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedIP = addr

	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("encode claim: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, claimKey, "IPAddressClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipaddressclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("persist claim: %w", err)
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(claim, uint64(rv)); err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipaddressclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("set resource version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		metrics.RecordAllocationFailure("ipaddressclaim", "tx_error", ipFamily, project, org)
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
// pattern adapted to IPAddressClaim.
func (r *AllocatingREST) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	existing, err := r.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	claim, ok := existing.(*ipam.IPAddressClaim)
	if !ok {
		return nil, false, fmt.Errorf("expected *ipam.IPAddressClaim from Get, got %T", existing)
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
	metrics.RecordRelease("ipaddressclaim")
	return releasing, true, nil
}

// releaseAndDelete is a single attempt of TX2: release the allocation row(s)
// for claimKey and delete the object row, all inside one transaction.
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
	return fmt.Sprintf("/ipam.miloapis.com/ipaddressclaims/%s/%s", namespace, name)
}

func mapAllocationError(err error) error {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return registryerrors.NewInsufficientStorage("address pool exhausted")
	case errors.Is(err, allocator.ErrPoolNotFound):
		return apierrors.NewBadRequest("address pool not found")
	default:
		return apierrors.NewInternalError(err)
	}
}
