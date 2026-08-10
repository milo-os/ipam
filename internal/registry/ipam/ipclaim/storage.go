// Package ipclaim provides REST storage for the IPClaim resource.
//
// A claim names a class and carries the scope it was made for. It never names a
// pool, a CIDR, or a region — resolving those is the allocator's job, and the
// AllocatingREST wrapper below is where that resolution meets the request path.
//
// Create is synchronous: by the time the response is written the address has
// been chosen, recorded, and committed, and the caller reads it out of the
// response body rather than polling for it. That property is the reason this
// service is an aggregated apiserver rather than a controller.
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
	"k8s.io/apimachinery/pkg/runtime/schema"
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
// allocator.
type AllocatingREST struct {
	*IPClaimStorage
	allocator    allocator.PrefixAllocator
	db           txBeginner
	resolver     Resolver
	classChecker access.ClassAccessChecker
	// nsChecker refuses a claim into a namespace nothing will collect. May be
	// nil, and a nil one DISABLES the check rather than denying — see
	// access.NewNamespaceChecker. That is the opposite of classChecker's nil
	// behaviour, deliberately: one is an authorization boundary and must fail
	// closed, the other is liveness and must fail open.
	nsChecker access.NamespaceChecker
	strategy  ipClaimStrategy
	codec     runtime.Codec
}

// txBeginner is the minimal slice of *pgxpool.Pool the allocation handlers
// depend on. Narrowing the field to this interface lets unit tests inject a
// fake transaction without a live Postgres.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewAllocatingStorage builds the IPClaim REST storage with synchronous
// Postgres-backed allocation. db must be the same pool the allocator commits
// against; codec serialises the allocated claim and its IPAllocation into
// ipam_objects so subsequent GETs return fully-populated objects.
// classChecker may be nil, and a nil one denies every project-scoped claim
// rather than allowing it — see access.AuthorizeClassConsumption. The class name
// is the only authorization boundary a claim crosses, so a missing checker is a
// missing boundary, not an absent requirement.
func NewAllocatingStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter, alloc allocator.PrefixAllocator, db *pgxpool.Pool, codec runtime.Codec, classChecker access.ClassAccessChecker, nsChecker access.NamespaceChecker) (*AllocatingREST, *IPClaimStatusStorage, error) {
	claimStore, statusStore, err := newInnerStorage(scheme, optsGetter)
	if err != nil {
		return nil, nil, err
	}
	return &AllocatingREST{
		IPClaimStorage: claimStore,
		allocator:      alloc,
		db:             db,
		resolver:       NewPostgresResolver(db),
		classChecker:   classChecker,
		nsChecker:      nsChecker,
		strategy:       NewStrategy(scheme),
		codec:          codec,
	}, statusStore, nil
}

// Create resolves the claim's class, resolves (and if necessary provisions) the
// pool chain it draws from, and allocates — returning a claim whose status
// already carries the address.
//
// The three phases are separate on purpose and in this order:
//
//  1. Class resolution and validation, in a read-only transaction. Everything
//     that can reject the claim happens before anything is written, so a claim
//     that cannot be satisfied leaves nothing behind.
//  2. Pool resolution, in transactions of the allocator's own. This is where a
//     scope nothing has used yet gets its pools built, one committed level at a
//     time. See internal/allocator/cascade.go for why it is not one transaction.
//  3. The allocation itself, in one transaction that writes the allocation row,
//     the IPAllocation object, the claim, and the pool's updated capacity
//     together — so a caller never observes a claim without its address or an
//     address without its claim.
func (r *AllocatingREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	claim, ok := obj.(*ipam.IPClaim)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPClaim, got %T", obj)
	}

	dryRun := isDryRun(options.DryRun)

	id := tenant.FromContext(ctx)
	project := id.Project()
	org := id.Org()
	ipFamily := string(claim.Spec.IPFamily)

	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanClaimAllocate)
	defer span.End()
	failSpan := func(reason string) {
		span.SetAttributes(attribute.String(tracing.AttrErrorReason, reason))
		span.SetStatus(codes.Error, reason)
	}

	span.SetAttributes(
		attribute.String(tracing.AttrTenantScope, tracing.Scope(id.IsPlatform())),
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
	fail := func(reason string) {
		metrics.RecordAllocationFailure("ipclaim", reason, ipFamily, project, org)
	}

	objectMeta, err := meta.Accessor(claim)
	if err != nil {
		fail("internal")
		return nil, fmt.Errorf("get object metadata: %w", err)
	}
	rest.FillObjectMetaSystemFields(objectMeta)

	if err := rest.BeforeCreate(r.strategy, ctx, claim); err != nil {
		fail("internal")
		return nil, err
	}
	if createValidation != nil {
		if err := createValidation(ctx, claim.DeepCopyObject()); err != nil {
			fail("internal")
			return nil, err
		}
	}

	if !id.IsPlatform() {
		// Overwrite any client-supplied ownerRef. The requestheader CA
		// guarantees the authenticity of the forwarded identity extras, so the
		// tenant identity is the source of truth for who this address is
		// attributed to — and attribution is what quota and cleanup key off.
		// The UID is the one part the caller supplies and we keep. The forwarded
		// identity extras carry a name but not a UID, and the UID answers a
		// question the name cannot: "who holds this address" after the holder has
		// been deleted and recreated under the same name, which is precisely the
		// case an operator hits mid-incident.
		//
		// Unlike ScopeRef.UID it takes no part in allocation identity and reaches
		// no digest — it is attribution, not addressing, and letting it into a
		// digest would make a recreated owner a different address space.
		var ownerUID string
		if claim.Spec.OwnerRef != nil {
			ownerUID = claim.Spec.OwnerRef.UID
		}
		claim.Spec.OwnerRef = &ipam.ObjectRef{
			APIGroup: id.APIGroup,
			Kind:     id.Kind,
			Name:     id.Name,
			UID:      ownerUID,
		}
	}

	// Phase 1 — resolve the class and check the claim against it.
	class, err := r.resolver.ResolveClass(ctx, claim.Spec.ClassName, v1alpha1.IPFamily(claim.Spec.IPFamily))
	if err != nil {
		fail(classFailureReason(err))
		failSpan(tracing.ReasonPoolNotFound)
		return nil, mapClassError(err)
	}
	span.SetAttributes(attribute.String(tracing.AttrClassName, class.Name))

	// Authorize before anything else the class implies. The class name is the
	// entire authorization surface now that no claim names a pool, so this gate
	// runs before the cascade can provision durable infrastructure on behalf of
	// a caller who may not consume the class at all.
	if err := access.AuthorizeClassConsumption(ctx, class, id.IsPlatform(), r.classChecker); err != nil {
		if errors.Is(err, access.ErrClassDenied) {
			fail("class_denied")
			failSpan(tracing.ReasonClassDenied)
			return nil, apierrors.NewForbidden(
				v1alpha1.Resource("ipclasses"), class.Name,
				fmt.Errorf("caller may not claim addresses of this class"))
		}
		fail("internal")
		failSpan(tracing.ReasonTxError)
		return nil, apierrors.NewInternalError(err)
	}

	// The namespace must still be able to collect what is bound into it (#86).
	//
	// ONLY ON CREATE, and that scoping is the safety property rather than an
	// optimisation. The namespace controller calls IPAM while deleting a
	// namespace — so it is always looking at one that is Terminating — and it
	// reaches the DELETE paths, never this one. A check placed anywhere both
	// verbs pass through would deadlock namespace teardown, which is #92: that
	// regression took a cluster-wide outage to surface and was caused by a gate
	// that did not distinguish the two.
	//
	// Placed after the class authorization so a caller who may not use the class
	// at all learns that first, and before the cascade, so nothing durable is
	// provisioned for a claim that is about to be refused.
	if r.nsChecker != nil && claim.Namespace != "" {
		state, nsErr := r.nsChecker.State(ctx, id.Name, claim.Namespace)
		switch state {
		case access.NamespaceTerminating, access.NamespaceMissing:
			fail("namespace_" + strings.ToLower(state.String()))
			failSpan(tracing.ReasonNamespaceUnusable)
			return nil, access.RefuseNamespace(state, claim.Namespace, v1alpha1.Resource("ipclaims"))
		default:
			// Unknown, including every lookup error: the claim PROCEEDS. Failing
			// closed here would put another service's availability in the hot
			// path of every allocation, turning a partial control-plane outage
			// into a total addressing outage. One orphaned allocation is the
			// cheaper failure, and it is recoverable.
			if nsErr != nil {
				access.LogUndetermined(id.Name, claim.Namespace, nsErr)
			}
		}
	}

	prefixLen, uniqueDigest, err := checkAgainstClass(id.Name, claim, class)
	if err != nil {
		fail("invalid_claim")
		failSpan(tracing.ReasonBadRequest)
		return nil, err
	}
	span.SetAttributes(attribute.Int(tracing.AttrClaimPrefix, prefixLen))

	// Phase 1a — try to recover a retained address before resolving anything.
	//
	// This is what makes Retain worth having: a replacement instance filling the
	// same slot derives the same claim name, hence the same allocation key, and
	// finds the address its predecessor held. Attempted before pool resolution
	// because a recovered address needs no pool resolved — it already knows
	// which pool it came from, and re-resolving could pick a different one.
	//
	// The probe costs one transaction and one unique-index lookup on every
	// create, and returns nothing on all but the reclaim path. That is a real
	// per-request cost and it is paid unconditionally on purpose: the cheap
	// signals that would let us skip it — this claim's reclaim policy, this
	// class's default — describe *this* claim, while whether an address is
	// waiting was decided by its predecessor's policy, which is not knowable
	// from here. A conditional probe would be right almost always and silently
	// lose an address the rest of the time.
	claimKey := claimObjectKey(id, claim.Namespace, claim.Name)
	allocationName := allocationNameFor(claim.Namespace, claim.Name)
	allocationKey := allocationObjectKey(id, claim.Namespace, allocationName)

	if !dryRun {
		reclaimed, err := r.reclaimRetained(ctx, claim, class, allocator.ReclaimRequest{
			AllocationKey: allocationKey,
			ClaimKey:      claimKey,
			ClassName:     class.Name,
			ScopeDigest:   uniqueDigest,
			PrefixLength:  prefixLen,
		}, allocationName)
		if err != nil {
			if errors.Is(err, allocator.ErrRetainedMismatch) {
				fail("retained_mismatch")
				failSpan(tracing.ReasonBadRequest)
				return nil, apierrors.NewConflict(v1alpha1.Resource("ipclaims"), claim.Name, err)
			}
			fail("tx_error")
			failSpan(tracing.ReasonTxError)
			return nil, apierrors.NewInternalError(err)
		}
		if reclaimed != nil {
			result = "reclaimed"
			return reclaimed, nil
		}
	}

	// Phase 2 — resolve the pool, provisioning the chain if this is the first
	// claim into its scope. A dry-run resolves without provisioning; see below.
	claimScope := claim.Spec.Scope
	var poolKey string
	if dryRun {
		var missing []allocator.CascadeLevel
		poolKey, missing, err = r.resolver.ResolveExistingPool(ctx, class, claimScope, id.Name)
		if err == nil && len(missing) > 0 {
			result = "success"
			return dryRunPendingProvision(claim, class, missing), nil
		}
	} else {
		poolKey, err = r.resolver.ResolvePool(ctx, class, claimScope, id.Name)
	}
	if err != nil {
		fail(resolveFailureReason(err))
		failSpan(tracing.ReasonPoolNotFound)
		return nil, mapResolveError(err)
	}
	poolName := poolKey[strings.LastIndex(poolKey, "/")+1:]
	span.SetAttributes(attribute.String(tracing.AttrPoolName, poolName))

	// Phase 3 — allocate.
	policy := effectiveReclaimPolicy(class, claim.Spec.ReclaimPolicy)

	tx, err := r.db.Begin(ctx)
	if err != nil {
		fail("tx_error")
		failSpan(tracing.ReasonTxError)
		return nil, fmt.Errorf("begin allocation transaction: %w", err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	cidr, err := r.allocator.AllocatePrefix(ctx, tx, allocator.AllocateRequest{
		PoolKey:       poolKey,
		AllocationKey: allocationKey,
		ClaimKey:      claimKey,
		ClassName:     class.Name,
		ScopeDigest:   uniqueDigest,
		PrefixLength:  prefixLen,
		Address:       claim.Spec.Address,
		IPFamily:      string(class.Spec.IPFamily),
		ReclaimPolicy: string(policy),
		OwnerProject:  id.Name,
	})
	if err != nil {
		rollback()
		reason := allocationFailureReason(err)
		fail(reason)
		switch reason {
		case "pool_exhausted":
			result = "exhausted"
			failSpan(tracing.ReasonExhausted)
		case "pool_not_found":
			failSpan(tracing.ReasonPoolNotFound)
		default:
			failSpan(tracing.ReasonTxError)
		}
		return nil, mapAllocationError(err, poolName, class.Name)
	}

	// Status is populated before the dry-run branch so both paths return the
	// identical bound object — a dry-run that reported a different shape from
	// the real thing would not be answering the question it was asked.
	address := singleAddressForm(cidr, class)
	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedCIDR = cidr
	claim.Status.Address = address
	claim.Status.PoolRef = &ipam.LocalRef{Name: poolName}
	claim.Status.BoundAllocationRef = &ipam.LocalRef{Name: allocationName}

	if dryRun {
		// The allocator computed the real next block inside the transaction,
		// under the same lock a real claim would take. Rolling back undoes the
		// reservation, so nothing is persisted and no capacity is consumed.
		rollback()
		result = "success"
		return claim, nil
	}

	alloc := &ipam.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      allocationName,
			Namespace: claim.Namespace,
		},
		Spec: ipam.IPAllocationSpec{
			IPFamily:  ipam.IPFamily(class.Spec.IPFamily),
			PoolRef:   ipam.LocalRef{Name: poolName},
			ClassName: class.Name,
			Purpose:   ipam.PurposeClaim,
			ClaimRef:  &ipam.LocalRef{Name: claim.Name},
			// The allocation records the space it is unique within, not the
			// claim's whole scope: it is the projection uniqueness was enforced
			// on, and recording anything wider would misdescribe the guarantee.
			Scope:         uniqueScope(claim, class),
			ReclaimPolicy: policy,
			// Copied from the claim rather than referenced through it, because
			// under Retain this outlives the claim and a retained address must
			// still have an attributable holder.
			// Deep-copied rather than shared: under Retain this object outlives
			// the claim, and two live objects pointing at one struct is a
			// mutation one of them never asked for.
			OwnerRef: claim.Spec.OwnerRef.DeepCopy(),
		},
		Status: ipam.IPAllocationStatus{
			Phase:         ipam.AllocationReady,
			AllocatedCIDR: cidr,
			Address:       address,
			ScopeDigest:   uniqueDigest,
		},
	}
	allocData, err := runtime.Encode(r.codec, alloc)
	if err != nil {
		rollback()
		fail("internal")
		return nil, fmt.Errorf("encode IPAllocation: %w", err)
	}
	if _, err := r.allocator.InsertObject(ctx, tx, allocationKey, "IPAllocation", claim.Namespace, allocationName, allocData); err != nil {
		rollback()
		fail("tx_error")
		return nil, registryerrors.MapWriteError(err,
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ipallocations"},
			allocationName, "persist IPAllocation")
	}

	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		rollback()
		fail("internal")
		return nil, fmt.Errorf("encode claim: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, claimKey, "IPClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		rollback()
		fail("tx_error")
		// Reachable, despite phase 1a usually catching a duplicate name first.
		// That check is for a *retained allocation* under this identity, not an
		// existence check on the claim — it happens to refuse most duplicates
		// because a live claim has an allocation bound to its key. It also runs
		// before this transaction opens, so two concurrent creates of one name
		// both pass it and one lands here. Narrow, but it is the window a
		// retrying client opens, and it answered 500 with the constraint name in
		// it.
		return nil, registryerrors.MapWriteError(err,
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ipclaims"},
			claim.Name, "persist claim")
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(claim, uint64(rv)); err != nil {
		rollback()
		fail("internal")
		return nil, fmt.Errorf("set resource version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		fail("tx_error")
		return nil, fmt.Errorf("commit allocation transaction: %w", err)
	}

	result = "success"
	return claim, nil
}

// checkAgainstClass validates a claim against the class it resolved to and
// returns the block size it gets and the digest of the address space its
// allocation must be unique within.
//
// The missing-role case is the one worth being careful about. A claim that does
// not carry a role its class names in uniqueWithin is rejected, by name, rather
// than being compared against a wider space. Widening would look correct — the
// claim succeeds — while refusing addresses the narrow comparison was meant to
// allow, and the operator would see a pool exhausting at a fraction of its
// capacity with nothing to explain it.
func checkAgainstClass(tenant string, claim *ipam.IPClaim, class *v1alpha1.IPClass) (int, string, error) {
	if claim.Spec.IPFamily != "" && string(claim.Spec.IPFamily) != string(class.Spec.IPFamily) {
		return 0, "", apierrors.NewBadRequest(fmt.Sprintf(
			"spec.ipFamily is %s but class %q hands out %s",
			claim.Spec.IPFamily, class.Name, class.Spec.IPFamily))
	}

	prefixLen, err := allocator.EffectivePrefixLength(class, claim.Spec.PrefixLength)
	if err != nil {
		return 0, "", apierrors.NewBadRequest(err.Error())
	}

	digest, err := scope.ProjectAddressSpaceDigest(tenant, claim.Spec.Scope, class.Spec.UniqueWithin, "uniqueWithin")
	if err != nil {
		var missing *scope.MissingRoleError
		if errors.As(err, &missing) {
			return 0, "", apierrors.NewBadRequest(fmt.Sprintf(
				"%s (class %q)", missing.Error(), class.Name))
		}
		return 0, "", apierrors.NewBadRequest(err.Error())
	}
	return prefixLen, digest, nil
}

// uniqueScope is the projection of a claim's scope that its allocation records.
// The projection cannot fail here: checkAgainstClass already established that
// every role is present, and it runs first.
func uniqueScope(claim *ipam.IPClaim, class *v1alpha1.IPClass) map[string]ipam.ScopeRef {
	projected, err := scope.Project(claim.Spec.Scope, class.Spec.UniqueWithin)
	if err != nil {
		return nil
	}
	return projected
}

// effectiveReclaimPolicy resolves the claim's override against the class
// default, converting between the versioned class and the internal claim.
func effectiveReclaimPolicy(class *v1alpha1.IPClass, override ipam.ReclaimPolicy) ipam.ReclaimPolicy {
	if override != "" {
		return override
	}
	if class.Spec.ReclaimPolicy != "" {
		return ipam.ReclaimPolicy(class.Spec.ReclaimPolicy)
	}
	return ipam.ReclaimDelete
}

// singleAddressForm returns the bare-address rendering of a block, for classes
// that hand out host addresses rather than blocks.
//
// It is set only when the block really is one address. A /96 IPv6 endpoint block
// has no single-address form, and reporting its first address as though it were
// the allocation would misrepresent what the holder got — the interface receives
// a block and assigns within it.
func singleAddressForm(cidr string, class *v1alpha1.IPClass) string {
	hostLen := "/32"
	if class.Spec.IPFamily == v1alpha1.IPv6 {
		hostLen = "/128"
	}
	if strings.HasSuffix(cidr, hostLen) {
		return strings.TrimSuffix(cidr, hostLen)
	}
	return ""
}

// dryRunPendingProvision answers a dry-run for a scope whose pools do not exist
// yet.
//
// It reports Pending rather than a fabricated address, and names what would be
// built. The alternative — provisioning the chain so the dry-run can compute a
// real address — would leave permanent pools behind for a request that asked
// what *would* happen, and those pools are never renumbered afterwards.
func dryRunPendingProvision(claim *ipam.IPClaim, class *v1alpha1.IPClass, missing []allocator.CascadeLevel) *ipam.IPClaim {
	names := make([]string, 0, len(missing))
	for _, level := range missing {
		names = append(names, fmt.Sprintf("%s (class %s)", level.PoolName, level.Class.Name))
	}
	claim.Status.Phase = ipam.ClaimPending
	claim.Status.Conditions = []metav1.Condition{{
		Type:               "Allocated",
		Status:             metav1.ConditionFalse,
		Reason:             "PoolProvisioningRequired",
		LastTransitionTime: metav1.Now(),
		Message: fmt.Sprintf(
			"dry-run: satisfying this claim of class %q would first provision %d pool(s): %s",
			class.Name, len(missing), strings.Join(names, ", ")),
	}}
	return claim
}

// isDryRun reports whether a create/delete options' DryRun slice requests a
// server-side dry-run.
func isDryRun(dryRun []string) bool {
	for _, v := range dryRun {
		if v == metav1.DryRunAll {
			return true
		}
	}
	return false
}

// Delete runs the claim teardown in two transactions so watchers can observe
// the intermediate Releasing phase before the object disappears:
//
//	TX1: UPDATE the claim row with status.phase=Releasing + MODIFIED changelog
//	TX2: release the allocation, reconcile the IPAllocation object, delete the
//	     claim row
//
// What TX2 does to the allocation depends on the policy recorded on it. Under
// Delete the allocation row and its object both go. Under Retain the row
// survives with its claim reference cleared, and the object survives with
// spec.claimRef nil and its ownerRef intact — so the address is continuously
// held by an identifiable party, never loose, and never in a state needing an
// operator to clear it by hand.
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

	if options != nil && isDryRun(options.DryRun) {
		return claim, true, nil
	}

	id := tenant.FromContext(ctx)
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanClaimRelease)
	defer span.End()
	span.SetAttributes(
		attribute.String(tracing.AttrTenantScope, tracing.Scope(id.IsPlatform())),
		attribute.String(tracing.AttrTenantProject, id.Project()),
		attribute.String(tracing.AttrClassName, claim.Spec.ClassName),
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

	// TX2 — release, reconcile the allocation object, delete the claim.
	var lastErr error
	for attempt := 1; attempt <= deleteMaxAttempts; attempt++ {
		lastErr = r.releaseAndDelete(ctx, claim, claimKey, id)
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

// releaseAndDelete is a single attempt of TX2.
//
// The allocator decides each allocation's fate from the policy recorded on it
// and reports back; this function's job is to keep the IPAllocation API object
// telling the same story as the row. A retained allocation is *updated*, not
// deleted and recreated: rebinding would open a window in which the address is
// loose and would need a durable identity distinct from the holder, which is
// exactly what the claim already is.
func (r *AllocatingREST) releaseAndDelete(ctx context.Context, claim *ipam.IPClaim, claimKey string, id tenant.Identity) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin release transaction: %w", err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	outcomes, err := r.allocator.Release(ctx, tx, claimKey)
	if err != nil {
		rollback()
		return fmt.Errorf("release allocation: %w", err)
	}
	// A bound claim must have had a row to release. Finding none means the claim
	// and the allocation table disagree — most plausibly because the key derived
	// here has drifted from the one Create wrote — and continuing would delete
	// the claim while the address stays held by a row nothing references. Failing
	// leaves the claim in Releasing, which is visible and recoverable; succeeding
	// would consume the address permanently and silently.
	//
	// This is the same failure the IPAllocation delete path guards, arrived at
	// from the other end. Both are instances of a release reporting success
	// having released nothing.
	if len(outcomes) == 0 && claim.Status.AllocatedCIDR != "" {
		rollback()
		return fmt.Errorf(
			"claim %q reports address %s but no allocation record was found under %q; refusing to delete the claim, because doing so would leave the address held with nothing naming it",
			claim.Name, claim.Status.AllocatedCIDR, claimKey)
	}

	if ref := claim.Status.BoundAllocationRef; ref != nil && ref.Name != "" {
		allocationKey := allocationObjectKey(id, claim.Namespace, ref.Name)
		retained := false
		for _, o := range outcomes {
			if o.AllocationKey == allocationKey && o.Retained {
				retained = true
				break
			}
		}
		if retained {
			if err := r.unbindAllocation(ctx, tx, allocationKey, claim.Namespace, ref.Name); err != nil {
				rollback()
				return err
			}
		} else if _, err := r.allocator.DeleteObject(ctx, tx, allocationKey); err != nil {
			rollback()
			return fmt.Errorf("delete IPAllocation row: %w", err)
		}
	}

	if _, err := r.allocator.DeleteObject(ctx, tx, claimKey); err != nil {
		rollback()
		return fmt.Errorf("delete claim row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit release transaction: %w", err)
	}
	return nil
}

// unbindAllocation clears spec.claimRef on a retained allocation, leaving
// everything else — the address, the class, the scope, and above all the
// ownerRef — in place.
//
// Clearing claimRef is what makes retention visible in the API rather than
// inferred from the absence of a claim. Keeping ownerRef is what makes a
// retained address still count against somebody: an address held against a
// location's public range takes that range out of service for everyone, and if
// it counted against nobody's budget nothing would pressure anyone to hand it
// back.
func (r *AllocatingREST) unbindAllocation(ctx context.Context, tx pgx.Tx, allocationKey, namespace, name string) error {
	obj, err := r.getAllocationObject(ctx, tx, allocationKey)
	if err != nil {
		// The allocation object is already gone; the row was retained but there
		// is nothing left to update. Nothing to fix here — the reconciliation
		// belongs to whatever removed the object.
		if errors.Is(err, pgx.ErrNoRows) {
			klog.V(2).InfoS("retained allocation has no object to unbind", "allocation", allocationKey)
			return nil
		}
		return err
	}
	obj.Spec.ClaimRef = nil
	obj.Status.Conditions = upsertCondition(obj.Status.Conditions, metav1.Condition{
		Type:               "Bound",
		Status:             metav1.ConditionFalse,
		Reason:             "Retained",
		Message:            "the claim was deleted under reclaimPolicy Retain; this address is still held",
		LastTransitionTime: metav1.Now(),
	})
	data, err := runtime.Encode(r.codec, obj)
	if err != nil {
		return fmt.Errorf("encode retained allocation: %w", err)
	}
	if _, err := r.allocator.UpdateObject(ctx, tx, allocationKey, data); err != nil {
		return fmt.Errorf("unbind retained allocation %q: %w", name, err)
	}
	return nil
}

// getAllocationObject reads an IPAllocation out of ipam_objects inside the
// release transaction, so the object it updates is the one the row describes.
func (r *AllocatingREST) getAllocationObject(ctx context.Context, tx pgx.Tx, allocationKey string) (*ipam.IPAllocation, error) {
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, allocationKey).Scan(&data); err != nil {
		return nil, err
	}
	obj, _, err := r.codec.Decode(data, nil, &ipam.IPAllocation{})
	if err != nil {
		return nil, fmt.Errorf("decode IPAllocation %q: %w", allocationKey, err)
	}
	alloc, ok := obj.(*ipam.IPAllocation)
	if !ok {
		return nil, fmt.Errorf("expected *ipam.IPAllocation, got %T", obj)
	}
	return alloc, nil
}

func upsertCondition(conditions []metav1.Condition, cond metav1.Condition) []metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == cond.Type {
			conditions[i] = cond
			return conditions
		}
	}
	return append(conditions, cond)
}

// DeleteCollection routes individual deletes through AllocatingREST.Delete so
// allocations are released when a namespace is bulk-terminated. The embedded
// Store's DeleteCollection would otherwise dispatch statically to Store.Delete
// and leak allocations.
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

// allocationObjectKey is the storage key for an IPAllocation.
func allocationObjectKey(id tenant.Identity, namespace, name string) string {
	return id.ApplyPrefix(fmt.Sprintf("/ipam.miloapis.com/ipallocations/%s/%s", namespace, name))
}

// allocationNameFor generates a stable, collision-resistant name for the
// IPAllocation produced by a given claim, using a truncated SHA-256 hash of the
// claim's namespace/name. Determinism is what lets the Delete handler recompute
// it, and it is what makes a replacement instance filling the same slot find
// the same allocation.
func allocationNameFor(namespace, name string) string {
	h := sha256.Sum256([]byte(namespace + "/" + name))
	return "alloc-" + hex.EncodeToString(h[:8])
}

// ----------------------------------------------------------------------------
// error mapping
// ----------------------------------------------------------------------------

func classFailureReason(err error) string {
	switch {
	case errors.Is(err, allocator.ErrClassNotFound):
		return "class_not_found"
	case errors.Is(err, allocator.ErrNoDefaultClass):
		return "no_default_class"
	default:
		return "tx_error"
	}
}

func mapClassError(err error) error {
	switch {
	case errors.Is(err, allocator.ErrClassNotFound):
		return apierrors.NewBadRequest(err.Error())
	case errors.Is(err, allocator.ErrNoDefaultClass):
		return apierrors.NewBadRequest(err.Error())
	default:
		return apierrors.NewInternalError(err)
	}
}

func resolveFailureReason(err error) string {
	switch {
	case errors.Is(err, allocator.ErrNoOfferingPool):
		return "no_offering_pool"
	case errors.Is(err, allocator.ErrPoolExhausted):
		return "pool_exhausted"
	case errors.Is(err, allocator.ErrChainTooDeep):
		return "chain_too_deep"
	case errors.Is(err, allocator.ErrInvalidReservation):
		return "invalid_reservation"
	case errors.Is(err, allocator.ErrAllocationExists):
		return "stale_allocation"
	default:
		return "tx_error"
	}
}

// mapResolveError translates a pool-resolution failure.
//
// A missing scope role is a bad request naming the role. Exhaustion during the
// cascade is 507 and names the level that ran out — an endpoint claim failing
// because the continent's block is full should say so, rather than reporting
// that the endpoint pool is full when it does not exist yet.
func mapResolveError(err error) error {
	var missing *scope.MissingRoleError
	if errors.As(err, &missing) {
		return apierrors.NewBadRequest(err.Error())
	}
	switch {
	case errors.Is(err, allocator.ErrNoOfferingPool):
		return apierrors.NewBadRequest(err.Error())
	case errors.Is(err, allocator.ErrPoolExhausted):
		return exhaustionError(err, "")
	case errors.Is(err, allocator.ErrChainTooDeep):
		return apierrors.NewInternalError(err)
	case errors.Is(err, allocator.ErrAllocationExists):
		// A stale allocation row under a key the cascade wants. The caller did
		// nothing wrong and can do nothing about it, so it is a 500 — but a
		// deliberate one carrying an actionable message, not a wrapped driver
		// error with a SQLSTATE and a constraint name in it.
		return apierrors.NewInternalError(fmt.Errorf(
			"a stale allocation record is blocking pool provisioning for this scope; an operator must release it: %w", err))
	case errors.Is(err, allocator.ErrClassNotFound):
		return apierrors.NewBadRequest(err.Error())
	case errors.Is(err, allocator.ErrInvalidReservation):
		// Operator misconfiguration, not exhaustion and not the caller's fault.
		// A 400 naming the class is what gets it fixed; a 507 would send someone
		// looking for capacity that is not the problem.
		return apierrors.NewBadRequest(err.Error())
	default:
		return apierrors.NewInternalError(err)
	}
}

// allocationFailureReason maps an allocator error onto the canonical reason
// label used by ipam_allocation_failures_total.
func allocationFailureReason(err error) string {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return "pool_exhausted"
	case errors.Is(err, allocator.ErrPoolNotFound):
		return "pool_not_found"
	case errors.Is(err, allocator.ErrAddressTaken),
		errors.Is(err, allocator.ErrAddressOutOfRange),
		errors.Is(err, allocator.ErrAddressInvalid):
		return "address_unavailable"
	default:
		return "tx_error"
	}
}

// exhaustionError turns an allocator exhaustion into the 507 a client can act
// on.
//
// The detail is the point. Nobody names a pool on the way in any more, so a bare
// "exhausted" leaves a caller unable to say which pool filled up, and unable to
// find out — listing the pools that offer the class fans out and misses
// cascade-provisioned pools entirely. The largest free prefix is what separates
// "add capacity" from "this pool is fragmented and adding capacity may not
// help".
func exhaustionError(err error, className string) error {
	var detail *allocator.ExhaustionError
	if !errors.As(err, &detail) {
		// Exhaustion reported without detail — possible only if a future code
		// path returns the bare sentinel. Still a 507; the client just gets less.
		return registryerrors.NewInsufficientStorage(err.Error())
	}
	poolName := detail.PoolKey[strings.LastIndex(detail.PoolKey, "/")+1:]
	return registryerrors.NewPoolExhausted(
		className,
		poolName,
		int32(detail.RequestedPrefixLength),
		detail.UtilizationPercent,
		detail.Error(),
	)
}

func mapAllocationError(err error, poolName, className string) error {
	switch {
	case errors.Is(err, allocator.ErrPoolExhausted):
		return exhaustionError(err, className)
	case errors.Is(err, allocator.ErrPoolNotFound):
		return apierrors.NewBadRequest(fmt.Sprintf("IPPool %q not found", poolName))
	case errors.Is(err, allocator.ErrAddressTaken):
		// Conflict rather than 507: the pool may have plenty of room, and the
		// caller asked for one specific address that somebody else holds.
		return apierrors.NewConflict(v1alpha1.Resource("ipclaims"), "", err)
	case errors.Is(err, allocator.ErrAddressOutOfRange), errors.Is(err, allocator.ErrAddressInvalid):
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

// reclaimRetained attempts to recover the address this claim's predecessor
// retained, returning the bound claim on success and nil when there is nothing
// to recover.
//
// The whole transaction is one: the row is re-bound, the surviving IPAllocation
// object is reconciled, and the claim is written together. A caller must never
// observe a claim bound to an address whose allocation object still says it
// belongs to nobody.
//
// Note what this does *not* do: resolve a pool. A recovered address already
// knows which pool it came from, and re-resolving could pick a different one —
// which for a cascade class would mean provisioning a pool the claim then does
// not draw from.
func (r *AllocatingREST) reclaimRetained(ctx context.Context, claim *ipam.IPClaim, class *v1alpha1.IPClass, req allocator.ReclaimRequest, allocationName string) (*ipam.IPClaim, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin reclaim transaction: %w", err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	cidr, reclaimed, err := r.allocator.ReclaimRetained(ctx, tx, req)
	if err != nil {
		rollback()
		return nil, err
	}
	if !reclaimed {
		rollback()
		return nil, nil
	}

	alloc, err := r.getAllocationObject(ctx, tx, req.AllocationKey)
	if err != nil {
		rollback()
		if errors.Is(err, pgx.ErrNoRows) {
			// The row survived but its object did not. Refused rather than
			// papered over: the address is genuinely held, and returning a bound
			// claim whose allocation cannot be found would be a claim pointing
			// at nothing.
			return nil, fmt.Errorf(
				"allocation record %q holds %s but its IPAllocation object is missing; an operator must reconcile it",
				req.AllocationKey, cidr)
		}
		return nil, err
	}

	// The allocation is bound again: it names its new claim, it is Ready rather
	// than expiring, and any lease warning it accumulated is withdrawn — the
	// clock has stopped, and migration 004's trigger has already cleared the
	// retention timestamp behind this.
	alloc.Spec.ClaimRef = &ipam.LocalRef{Name: claim.Name}
	alloc.Spec.OwnerRef = claim.Spec.OwnerRef.DeepCopy()
	alloc.Status.Phase = ipam.AllocationReady
	alloc.Status.Conditions = withoutCondition(alloc.Status.Conditions, "Expiring")
	alloc.Status.Conditions = upsertCondition(alloc.Status.Conditions, metav1.Condition{
		Type:               "Bound",
		Status:             metav1.ConditionTrue,
		Reason:             "Reclaimed",
		Message:            fmt.Sprintf("recovered by replacement claim %q", claim.Name),
		LastTransitionTime: metav1.Now(),
	})

	allocData, err := runtime.Encode(r.codec, alloc)
	if err != nil {
		rollback()
		return nil, fmt.Errorf("encode reclaimed allocation: %w", err)
	}
	if _, err := r.allocator.UpdateObject(ctx, tx, req.AllocationKey, allocData); err != nil {
		rollback()
		return nil, fmt.Errorf("rebind IPAllocation object: %w", err)
	}

	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedCIDR = cidr
	claim.Status.Address = singleAddressForm(cidr, class)
	claim.Status.PoolRef = &ipam.LocalRef{Name: alloc.Spec.PoolRef.Name}
	claim.Status.BoundAllocationRef = &ipam.LocalRef{Name: allocationName}

	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		rollback()
		return nil, fmt.Errorf("encode reclaiming claim: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, req.ClaimKey, "IPClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		rollback()
		return nil, registryerrors.MapWriteError(err,
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ipclaims"},
			claim.Name, "persist reclaiming claim")
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(claim, uint64(rv)); err != nil {
		rollback()
		return nil, fmt.Errorf("set resource version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit reclaim transaction: %w", err)
	}

	klog.V(2).InfoS("claim reclaimed its retained address",
		"claim", claim.Name, "cidr", cidr, "class", class.Name)
	return claim, nil
}

// withoutCondition removes a condition by type, so a withdrawn warning
// disappears rather than lingering as False — an `Expiring` condition on an
// allocation that is bound again would be a warning about something that is no
// longer happening.
func withoutCondition(conditions []metav1.Condition, condType string) []metav1.Condition {
	out := conditions[:0]
	for _, c := range conditions {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	return out
}
