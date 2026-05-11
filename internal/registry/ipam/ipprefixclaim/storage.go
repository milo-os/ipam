// Package ipprefixclaim provides REST storage for the IPPrefixClaim
// resource. The exported AllocatingREST type wraps the standard storage
// with a synchronous Postgres-backed allocator: when configured, Create
// resolves a free sub-prefix from the parent IPPrefix pool inside a single
// transaction so the caller's response includes the allocated CIDR.
package ipprefixclaim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

type IPPrefixClaimStorage struct {
	*genericregistry.Store
}

type IPPrefixClaimStatusStorage struct {
	store *genericregistry.Store
}

func (s *IPPrefixClaimStatusStorage) New() runtime.Object { return &ipam.IPPrefixClaim{} }
func (s *IPPrefixClaimStatusStorage) Destroy()            {}

func (s *IPPrefixClaimStatusStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

func (s *IPPrefixClaimStatusStorage) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, _ bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

func (s *IPPrefixClaimStatusStorage) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}

func (s *IPPrefixClaimStatusStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

// newInnerStorage builds the underlying genericregistry.Store-backed REST
// storage for IPPrefixClaim. NewAllocatingStorage wraps the result to add
// synchronous Postgres-backed allocation in the request path; nothing
// outside this package calls it directly.
func newInnerStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*IPPrefixClaimStorage, *IPPrefixClaimStatusStorage, error) {
	strategy := NewStrategy(scheme)
	statusStrategy := NewStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ipam.IPPrefixClaim{} },
		NewListFunc:               func() runtime.Object { return &ipam.IPPrefixClaimList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("ipprefixclaims"),
		SingularQualifiedResource: v1alpha1.Resource("ipprefixclaim"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("ipprefixclaims")),
	}

	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &IPPrefixClaimStorage{store}, &IPPrefixClaimStatusStorage{store: &statusStore}, nil
}

// AllocatingREST decorates the standard claim storage with a synchronous
// allocator. On Create it begins a Postgres transaction, asks the allocator
// to reserve a sub-prefix from the parent pool, and returns the claim with
// its status fully populated. On Delete it asks the allocator to release the
// recorded allocation in the same transaction as the claim deletion.
type AllocatingREST struct {
	*IPPrefixClaimStorage
	allocator   allocator.PrefixAllocator
	db          *pgxpool.Pool
	strategy    ipPrefixClaimStrategy
	poolChecker access.PoolAccessChecker
	// codec serialises the in-memory claim into the same wire format the
	// storage Get path expects. Internal types lack JSON tags, so json.Marshal
	// would silently drop spec/status fields when read back.
	codec runtime.Codec
}

// NewAllocatingStorage builds the IPPrefixClaim REST storage with synchronous
// Postgres-backed allocation. db must be the same pool the allocator commits
// against. codec is used to serialise the synchronously-allocated claim into
// ipam_objects so subsequent GETs return a fully-populated object.
// poolChecker may be nil; when non-nil it authorises cross-project claims
// via SubjectAccessReview before allocation.
func NewAllocatingStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool, codec runtime.Codec, poolChecker access.PoolAccessChecker) (*AllocatingREST, *IPPrefixClaimStatusStorage, error) {
	claimStore, statusStore, err := newInnerStorage(scheme, optsGetter)
	if err != nil {
		return nil, nil, err
	}
	return &AllocatingREST{
		IPPrefixClaimStorage: claimStore,
		allocator:            alloc,
		db:                   db,
		strategy:             NewStrategy(scheme),
		poolChecker:          poolChecker,
		codec:                codec,
	}, statusStore, nil
}

// Create runs the standard create pipeline (system-metadata fill, strategy
// PrepareForCreate, validation), then drives the allocator inside a
// short-lived transaction. The allocator is expected to persist the claim
// row, the allocation row, and (when ChildPrefixTemplate is set) the child
// IPPrefix object inside that transaction.
func (r *AllocatingREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	claim, ok := obj.(*ipam.IPPrefixClaim)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPPrefixClaim, got %T", obj)
	}

	// Tenant identity is needed up front so the project / org metric labels
	// are available to AllocationAttempts and the deferred AllocationDuration
	// observation. project / org come from the iam.miloapis.com/parent-* extras
	// via tenant.Identity helpers; both are "" for platform-scoped requests
	// (and org is "" today for project-scoped requests until Milo forwards
	// the owning org alongside the project).
	id := tenant.FromContext(ctx)
	project := id.Project()
	org := id.Org()

	// ipFamily is derived from the claim spec up front so it can label
	// AllocationAttempts (counted immediately below) and AllocationFailures
	// (recorded throughout the handler) identically with the latency
	// histogram. claim.Spec.IPFamily is set on every valid claim; pre-spec
	// failures land in the empty-string family, distinguishable from
	// family-tagged successes.
	ipFamily := string(claim.Spec.IPFamily)
	// Counted at the top of the synchronous path so failures (validation,
	// auth, allocation, encode, commit) all show up against attempts and
	// success ratios survive partial flow-through.
	metrics.AllocationAttempts.WithLabelValues("ipprefixclaim", ipFamily, project, org).Inc()
	// Track latency for every synchronous attempt under (resource, result,
	// ip_family, project, org). `result` defaults to "error" and is
	// overwritten by the success branch just before commit. The deferred
	// Observe runs after every return so the histogram count tracks
	// AllocationAttempts 1:1.
	allocStart := time.Now()
	result := "error"
	defer func() {
		metrics.ObserveAllocationDuration("ipprefixclaim", result, ipFamily, project, org, allocStart)
	}()

	objectMeta, err := meta.Accessor(claim)
	if err != nil {
		metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("get object metadata: %w", err)
	}
	rest.FillObjectMetaSystemFields(objectMeta)

	if err := rest.BeforeCreate(r.strategy, ctx, claim); err != nil {
		metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
		return nil, err
	}
	if createValidation != nil {
		if err := createValidation(ctx, claim.DeepCopyObject()); err != nil {
			metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
			return nil, err
		}
	}

	if claim.Spec.PrefixRef == nil && claim.Spec.PrefixSelector == nil {
		metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("synchronous allocation requires spec.prefixRef or spec.prefixSelector")
	}
	if claim.Spec.PrefixRef != nil && claim.Spec.PrefixSelector != nil {
		metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
		return nil, apierrors.NewBadRequest("spec.prefixRef and spec.prefixSelector are mutually exclusive")
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
		metrics.RecordAllocationFailure("ipprefixclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("begin allocation transaction: %w", err)
	}

	// Resolve the target pool. With spec.prefixRef this is a direct named
	// lookup; with spec.prefixSelector we list candidate pools, filter by
	// the supplied label selector, and pick the first match by storage key
	// (see allocator.ResolvePrefixPool for why first-match is the chosen
	// strategy). Cross-project routing is only supported through
	// spec.prefixRef.projectRef; selectors evaluate within the caller's
	// project scope unless they carry an explicit projectRef.
	isCrossProject := false
	var poolKey, poolName string
	if claim.Spec.PrefixRef != nil {
		poolName = claim.Spec.PrefixRef.Name
		isCrossProject = !id.IsPlatform() &&
			claim.Spec.PrefixRef.ProjectRef != nil &&
			claim.Spec.PrefixRef.ProjectRef.Name != id.Name
		if isCrossProject {
			poolKey = tenant.Identity{Name: claim.Spec.PrefixRef.ProjectRef.Name}.ResourceKey("ipprefixes", poolName)
		} else {
			poolKey = id.ResourceKey("ipprefixes", poolName)
		}
	} else {
		// PrefixSelector path. The selector's optional ProjectRef lets a
		// claim target a specific project's pools; absent that, scope to
		// the caller's own project (or platform).
		ownerProject := id.Name
		if claim.Spec.PrefixSelector.ProjectRef != nil {
			ownerProject = claim.Spec.PrefixSelector.ProjectRef.Name
			isCrossProject = !id.IsPlatform() && ownerProject != id.Name
		}
		resolved, rerr := allocator.ResolvePrefixPool(ctx, tx, claim.Spec.PrefixSelector.LabelSelector, ownerProject, string(claim.Spec.IPFamily))
		if rerr != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(rerr, allocator.ErrPoolNotFound) {
				metrics.RecordAllocationFailure("ipprefixclaim", "pool_not_found", ipFamily, project, org)
				return nil, apierrors.NewBadRequest("no IPPrefix pool matches spec.prefixSelector")
			}
			metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
			return nil, fmt.Errorf("resolve prefix pool: %w", rerr)
		}
		poolKey = resolved
		// Storage key has the form "/ipam.miloapis.com/ipprefixes/<name>" or
		// "project/<id>/ipam.miloapis.com/ipprefixes/<name>"; the last
		// segment after the final '/' is the pool name. We need it for
		// status.boundPrefixRef and (when ChildPrefixTemplate is set) the
		// child's ParentRef.
		poolName = poolKey[strings.LastIndex(poolKey, "/")+1:]
	}
	claimKey := claimObjectKey(claim.Namespace, claim.Name)

	if isCrossProject {
		if err := r.authorizeCrossProject(ctx, tx, poolKey); err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, access.ErrCrossProjectDenied) {
				// Selector-driven lookups must not distinguish "no pool
				// matched the selector" from "a pool matched but you can't
				// use it" — that distinction is a label/existence
				// fingerprint into another project (audit finding H1).
				// Direct prefixRef lookups can return Forbidden because
				// the caller already named the pool by hand.
				if claim.Spec.PrefixSelector != nil {
					metrics.RecordAllocationFailure("ipprefixclaim", "pool_not_found", ipFamily, project, org)
					return nil, apierrors.NewBadRequest("no IPPrefix pool matches spec.prefixSelector")
				}
				metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
				return nil, apierrors.NewForbidden(
					v1alpha1.Resource("ipprefixes"),
					poolKey,
					fmt.Errorf("cross-project pool not accessible"),
				)
			}
			metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
			return nil, err
		}
	}

	cidr, err := r.allocator.AllocatePrefix(ctx, tx, poolKey, claim.Spec.PrefixLength, string(claim.Spec.IPFamily), claimKey, id.Name)
	if err != nil {
		_ = tx.Rollback(ctx)
		reason := allocationFailureReason(err)
		metrics.RecordAllocationFailure("ipprefixclaim", reason, ipFamily, project, org)
		if reason == "pool_exhausted" {
			result = "exhausted"
		}
		return nil, mapAllocationError(err)
	}

	// Populate status synchronously so the persisted row already reflects
	// the bound state and the CREATE response carries the allocated CIDR.
	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedCIDR = cidr
	claim.Status.BoundPrefixRef = &ipam.LocalRef{Name: poolName}

	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("encode claim: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, claimKey, "IPPrefixClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipprefixclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("persist claim: %w", err)
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(claim, uint64(rv)); err != nil {
		_ = tx.Rollback(ctx)
		metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
		return nil, fmt.Errorf("set resource version: %w", err)
	}

	if claim.Spec.ChildPrefixTemplate != nil {
		child := &ipam.IPPrefix{
			ObjectMeta: claim.Spec.ChildPrefixTemplate.Metadata,
			Spec:       claim.Spec.ChildPrefixTemplate.Spec,
		}
		// IPPrefix is cluster-scoped; drop any namespace the template may
		// have carried over from older configurations.
		child.Namespace = ""
		child.Spec.CIDR = cidr
		// Inherit ipFamily from the claim when the template did not set it
		// — otherwise the child lands with spec.ipFamily="" and downstream
		// validation/allocation has no way to recover it.
		if child.Spec.IPFamily == "" {
			child.Spec.IPFamily = claim.Spec.IPFamily
		}
		child.Spec.ParentRef = &ipam.ObjectRef{
			APIGroup: v1alpha1.GroupName,
			Kind:     "IPPrefix",
			Name:     poolName,
		}
		// Children skip the standard create path so PrepareForCreate never
		// runs on them. Mirror the full Status block PrepareForCreate would
		// have set (phase + canonical CIDR + capacity + Ready condition) so
		// the prefix-hierarchy e2e suite — which asserts on all four — does
		// not have to wait for a follow-up status update that never comes.
		if _, ipnet, parseErr := net.ParseCIDR(cidr); parseErr == nil {
			child.Status = ipam.IPPrefixStatus{
				Phase:    ipam.PrefixReady,
				CIDR:     ipnet.String(),
				Capacity: ipam.PrefixCapacity{Total: allocation.CountAddresses(*ipnet)},
				Conditions: []metav1.Condition{{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "PrefixReady",
					Message:            "IPPrefix is ready for allocation",
					LastTransitionTime: metav1.Now(),
				}},
			}
		}
		childKey := childPrefixObjectKey(child.Namespace, child.Name)
		childData, err := runtime.Encode(r.codec, child)
		if err != nil {
			_ = tx.Rollback(ctx)
			metrics.RecordAllocationFailure("ipprefixclaim", "internal", ipFamily, project, org)
			return nil, fmt.Errorf("encode child prefix: %w", err)
		}
		if err := r.allocator.InsertChildPrefix(ctx, tx, childKey, child.Namespace, child.Name, childData); err != nil {
			_ = tx.Rollback(ctx)
			metrics.RecordAllocationFailure("ipprefixclaim", "tx_error", ipFamily, project, org)
			return nil, fmt.Errorf("insert child prefix: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		metrics.RecordAllocationFailure("ipprefixclaim", "tx_error", ipFamily, project, org)
		return nil, fmt.Errorf("commit allocation transaction: %w", err)
	}

	result = "success"
	return claim, nil
}

// allocationFailureReason maps an allocator error onto the canonical reason
// label set used by ipam_allocation_failures_total. The histogram's `result`
// label uses a coarser bucketing — pool exhaustion is its own outcome, every
// other failure rolls up to "error" — so the two metrics intentionally do
// not share a label set.
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
// the intermediate phase=Releasing state before the object disappears:
//
//	TX1: UPDATE the claim row with status.phase=Releasing + MODIFIED changelog
//	TX2: Release the allocation + DeleteObject + DELETED changelog
//
// TX2 is retried up to deleteMaxAttempts times with a short backoff because a
// transient failure between the two transactions would leave the claim
// stranded in Releasing. After the retries are exhausted the claim stays in
// Releasing and is visible to operators — the allocation may have been
// released by an aborted attempt, but no allocation is leaked because Release
// is idempotent on the claim_key.
func (r *AllocatingREST) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	existing, err := r.IPPrefixClaimStorage.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	claim, ok := existing.(*ipam.IPPrefixClaim)
	if !ok {
		return nil, false, fmt.Errorf("expected *ipam.IPPrefixClaim from Get, got %T", existing)
	}
	if deleteValidation != nil {
		if err := deleteValidation(ctx, claim.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}

	claimKey := claimObjectKey(claim.Namespace, claim.Name)

	// TX1 — publish phase=Releasing. Deep-copy first so the in-memory claim
	// returned to the caller carries the Releasing phase without mutating the
	// object the storage layer cached.
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

	// TX2 — release the allocation and delete the object row. Retry on
	// transient failures so a brief PG hiccup does not leave the claim
	// stranded in Releasing forever; the user-facing Delete contract is
	// "Releasing is observable, then the object disappears".
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
	metrics.RecordRelease("ipprefixclaim")
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

// deleteMaxAttempts and deleteRetryBackoff govern the TX2 retry loop. Three
// attempts at 100ms covers the common transient failure (brief connection
// loss) without holding the request open for more than a few hundred
// milliseconds; persistent failures surface as a 500 with the claim still
// observable in Releasing.
const (
	deleteMaxAttempts  = 3
	deleteRetryBackoff = 100 * time.Millisecond
)

func claimObjectKey(namespace, name string) string {
	return fmt.Sprintf("/ipam.miloapis.com/ipprefixclaims/%s/%s", namespace, name)
}

// childPrefixObjectKey is the storage key for a child IPPrefix materialised
// from a claim's ChildPrefixTemplate. IPPrefix is cluster-scoped, so
// the namespace argument from the template is ignored at the key layer.
func childPrefixObjectKey(_, name string) string {
	return fmt.Sprintf("/ipam.miloapis.com/ipprefixes/%s", name)
}

// authorizeCrossProject delegates to the shared cross-project gate in
// internal/access. Kept as a thin method so the call site reads naturally;
// the same gate is used by ipaddressclaim's Create handler so the policy
// (fail-closed when no checker, visibility=shared check, SAR, single
// sentinel for all denial paths) lives in exactly one place.
func (r *AllocatingREST) authorizeCrossProject(ctx context.Context, tx pgx.Tx, poolKey string) error {
	return access.AuthorizeCrossProjectPrefix(ctx, tx, poolKey, r.poolChecker)
}

func mapAllocationError(err error) error {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return registryerrors.NewInsufficientStorage("prefix pool exhausted")
	case errors.Is(err, allocator.ErrPoolNotFound):
		return apierrors.NewBadRequest("prefix pool not found")
	default:
		return apierrors.NewInternalError(err)
	}
}

