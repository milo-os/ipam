package ipclaim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// reclaimTerms is what a recreated claim asks for, resolved as far as the
// create path resolves it before choosing a pool. A retained allocation is
// handed back only on these terms.
type reclaimTerms struct {
	claimName string
	class     *allocator.ResolvedClass
	// scope is the claim's whole scope, which placement is decided on;
	// scopeDigest is its projection onto the class's uniqueWithin.
	scope       map[string]ipam.ScopeRef
	scopeDigest string
	// prefixLength is the length the claim stated, or the one its CIDR-form
	// address stated; nil leaves the size to the retained allocation.
	prefixLength *int32
	address      string
}

// reclaimMismatch is a term the retained allocation does not share, by the
// claim field that states it.
type reclaimMismatch struct {
	field   string
	message string
}

// Identity is what entitles a claim to a retained address — the allocation's
// name is derived from the claim's — but identity is not consent to different
// terms. Handing a /28 back to a claim that asked for a /29, or an address held
// under one class to a claim of another, would bind something the claim never
// asked for, and silently answering with a fresh block instead would leave the
// retained one held under a name that now names something else. So every term
// is checked, and the first that differs, in field order, is the refusal:
//
//	agreeOnClass   which class, family, address scope and address space
//	placement      the pool the claim would be placed in now (tryReclaim)
//	agreeOnBlock   the block's size and, if named, its address

// agreeOnClass checks the terms that do not depend on the pool.
//
// The class is its defining project and name, not the name alone: two projects
// can each define a class of one name, and a claim naming a reference resolves
// to the definition the reference points at. A row written before IPAM
// recorded the project leaves that half to placement (allocator.ReclaimPlacement).
//
// The address scope is compared with what the row recorded at allocation, not
// with the class alone: a class may gain one it did not have. A row that
// recorded none cannot prove it was handed out under the scope the class now
// states, and is refused.
func agreeOnClass(row *allocator.ClaimAllocation, terms reclaimTerms) *reclaimMismatch {
	class := terms.class
	if row.ClassName != class.Name || (row.ClassRecorded && row.ClassProject != class.Project) {
		held := fmt.Sprintf("class %q", row.ClassName)
		if row.ClassRecorded {
			held = fmt.Sprintf("class %q defined in project %q", row.ClassName, row.ClassProject)
		}
		return &reclaimMismatch{field: "spec.className", message: fmt.Sprintf(
			"it is held under %s and this claim resolves to class %q defined in project %q",
			held, class.Name, class.Project)}
	}
	if row.IPFamily != string(class.Spec.IPFamily) {
		return &reclaimMismatch{field: "spec.ipFamily", message: fmt.Sprintf(
			"it holds an %s block and this claim is for %s", row.IPFamily, class.Spec.IPFamily)}
	}
	if row.ClassRecorded && row.AddressScope != class.Spec.AddressScope {
		return &reclaimMismatch{field: "spec.addressScope", message: fmt.Sprintf(
			"it was handed out with addressScope %q and class %q now has addressScope %q",
			row.AddressScope, class.Name, class.Spec.AddressScope)}
	}
	if !row.ClassRecorded && class.Spec.AddressScope != "" {
		return &reclaimMismatch{field: "spec.addressScope", message: fmt.Sprintf(
			"it was allocated before IPAM recorded address scopes, and class %q now has addressScope %q",
			class.Name, class.Spec.AddressScope)}
	}
	digest := terms.scopeDigest
	if digest == "" {
		digest = scope.EmptyAddressSpaceDigest()
	}
	if row.ScopeDigest != digest {
		return &reclaimMismatch{field: "spec.scope", message: "it is held in another address space than this claim's scope names"}
	}
	return nil
}

// agreeOnBlock checks the block itself and returns the prefix length the claim
// binds at.
//
// The size is the one term a claim may leave out. A claim that states no
// length takes the retained one, if its class still allows it; a fresh claim
// would take the class default, but the retained block is the claim's own.
func agreeOnBlock(row *allocator.ClaimAllocation, terms reclaimTerms) (int, *reclaimMismatch) {
	_, block, err := net.ParseCIDR(row.CIDR)
	if err != nil {
		return 0, &reclaimMismatch{field: "spec.address", message: fmt.Sprintf("the retained block %q does not parse", row.CIDR)}
	}
	retainedLen, _ := block.Mask.Size()

	length := retainedLen
	if terms.prefixLength != nil {
		if int(*terms.prefixLength) != retainedLen {
			return 0, &reclaimMismatch{field: "spec.prefixLength", message: fmt.Sprintf(
				"it holds a /%d and this claim asks for a /%d", retainedLen, *terms.prefixLength)}
		}
	} else {
		want := int32(retainedLen)
		if _, err := allocator.EffectivePrefixLength(terms.class.IPClass, &want); err != nil {
			return 0, &reclaimMismatch{field: "spec.prefixLength", message: fmt.Sprintf(
				"it holds a /%d, which class %q no longer allows: %v", retainedLen, terms.class.Name, err)}
		}
	}

	if terms.address != "" {
		asked, err := allocation.RequestedBlock(terms.address, length)
		if err != nil || asked.String() != block.String() {
			return 0, &reclaimMismatch{field: "spec.address", message: fmt.Sprintf(
				"it holds %s and this claim names %s", block, terms.address)}
		}
	}
	return length, nil
}

// reclaimResult is what tryReclaim reports back to create.
type reclaimResult struct {
	// handled is false when there is no retained allocation under the claim's
	// identity, and create carries on to allocate.
	handled bool
	obj     runtime.Object
	// outcome is the failure-metric reason when err is set.
	outcome string
	err     error
}

// tryReclaim binds a claim to the allocation retained under its identity, if
// there is one: the claim was released under reclaimPolicy Retain, and this
// is the claim recreated under the same name, so the address is its own.
//
// It runs before the pool is resolved. The retained row names its pool, so
// nothing is provisioned or re-resolved, and nothing is searched: a pool with
// no free block left still hands back a block it is already holding.
//
// One transaction: lock the row's pool, then the row; refuse a row still
// bound, or one held on terms the claim does not ask for, including a pool the
// claim would not be placed in now; rebind the row by compare-and-set; point
// the IPAllocation at the claim; write the claim as Bound. Capacity does not
// move, because the address never left. A dry run runs all of it and rolls
// back.
//
// A generated name that lands on a retained identity was not chosen by
// anyone, so it is refused as a collision and drawn again rather than handed
// the address an unrelated claim left behind.
func (r *AllocatingREST) tryReclaim(
	ctx context.Context,
	span trace.Span,
	claim *ipam.IPClaim,
	id tenant.Identity,
	terms reclaimTerms,
	uniqueScope map[string]ipam.ScopeRef,
	reclaimPolicy v1alpha1.ReclaimPolicy,
	generatedName, dryRun bool,
) reclaimResult {
	claimKey := claimObjectKey(id, claim.Namespace, claim.Name)
	allocationName := allocationNameFor(claim.Namespace, claim.Name)
	allocationKey := allocationObjectKey(id, claim.Namespace, allocationName)

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return reclaimResult{handled: true, outcome: "tx_error", err: fmt.Errorf("begin reclaim transaction: %w", err)}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	row, err := r.allocator.LockClaimAllocation(ctx, tx, allocationKey)
	if err != nil {
		return reclaimResult{handled: true, outcome: "tx_error", err: fmt.Errorf("read allocation under the claim's identity: %w", err)}
	}
	if row == nil {
		// Nothing retained under this identity, or it was released while this
		// request waited on the pool. Either way the claim allocates afresh.
		return reclaimResult{}
	}
	if row.ClaimKey != "" {
		return reclaimResult{handled: true, outcome: "conflict", err: duplicateClaimConflict(claim.Name)}
	}
	if generatedName {
		return reclaimResult{handled: true, outcome: "conflict", err: retainedAllocationConflict(claim.Name, allocationName)}
	}

	refuse := func(m *reclaimMismatch) reclaimResult {
		if !dryRun {
			metrics.RecordReclaim("mismatch")
		}
		return reclaimResult{handled: true, outcome: "conflict", err: retainedMismatchConflict(claim.Name, allocationName, m)}
	}
	if m := agreeOnClass(row, terms); m != nil {
		return refuse(m)
	}
	// Read under the pool's lock, so the pool judged is the pool bound into.
	placement, err := allocator.ReclaimPlacement(ctx, tx, terms.class, terms.scope, row)
	if err != nil {
		var missing *scope.MissingRoleError
		if errors.As(err, &missing) {
			return reclaimResult{handled: true, outcome: "internal", err: ipamerrors.NewScopeRolesMissing(missing.Roles,
				fmt.Sprintf("%s (class %q)", missing.Error(), terms.class.Name))}
		}
		return reclaimResult{handled: true, outcome: "tx_error", err: fmt.Errorf("check the retained allocation's placement: %w", err)}
	}
	if placement != nil {
		return refuse(&reclaimMismatch{field: placement.Field, message: placement.Reason})
	}
	prefixLen, mismatch := agreeOnBlock(row, terms)
	if mismatch != nil {
		return refuse(mismatch)
	}

	if err := r.allocator.ReclaimAllocation(ctx, tx, allocationKey, claimKey, reclaimPolicy); err != nil {
		if isClaimNameCollision(err) {
			return reclaimResult{handled: true, outcome: "conflict", err: duplicateClaimConflict(claim.Name)}
		}
		return reclaimResult{handled: true, outcome: "tx_error", err: fmt.Errorf("reclaim allocation: %w", err)}
	}

	poolName := row.PoolKey[strings.LastIndex(row.PoolKey, "/")+1:]
	span.SetAttributes(
		attribute.String(tracing.AttrPoolName, poolName),
		attribute.Bool(tracing.AttrReclaimed, true),
	)

	if claim.Spec.PrefixLength == nil {
		bits := int32(prefixLen)
		claim.Spec.PrefixLength = &bits
	}
	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedCIDR = row.CIDR
	claim.Status.Address = singleAddress(row.CIDR)
	claim.Status.BoundAllocationRef = &ipam.LocalRef{Name: allocationName}
	claim.Status.PoolRef = &ipam.LocalRef{Name: poolName}

	if err := r.bindRetainedAllocation(ctx, tx, claim, row, allocationKey, allocationName, poolName, uniqueScope, reclaimPolicy); err != nil {
		return reclaimResult{handled: true, outcome: "tx_error", err: err}
	}

	if dryRun {
		return reclaimResult{handled: true, obj: claim}
	}

	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		return reclaimResult{handled: true, outcome: "internal", err: fmt.Errorf("encode claim: %w", err)}
	}
	rv, err := r.allocator.InsertObject(ctx, tx, claimKey, "IPClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		if isIdentityCollision(err) {
			return reclaimResult{handled: true, outcome: "conflict", err: duplicateClaimConflict(claim.Name)}
		}
		return reclaimResult{handled: true, outcome: "tx_error", err: fmt.Errorf("persist claim: %w", err)}
	}
	if err := (storage.APIObjectVersioner{}).UpdateObject(claim, uint64(rv)); err != nil {
		return reclaimResult{handled: true, outcome: "internal", err: fmt.Errorf("set resource version: %w", err)}
	}
	if err := tx.Commit(ctx); err != nil {
		return reclaimResult{handled: true, outcome: "tx_error", err: fmt.Errorf("commit reclaim transaction: %w", err)}
	}
	committed = true

	metrics.RecordReclaim("reclaimed")
	klog.V(2).InfoS("Reclaimed retained allocation", "claim", claimKey, "allocation", allocationName,
		"cidr", row.CIDR, "reclaimPolicy", reclaimPolicy)
	return reclaimResult{handled: true, obj: claim}
}

// bindRetainedAllocation points the IPAllocation behind a reclaimed row at the
// claim taking it back, in the reclaim's transaction. The spec is immutable to
// clients; the binding is the server's to change, exactly as retention cleared
// it. Everything the allocation was handed out under stays as it was, because
// the reclaim was refused unless the claim agreed with it.
//
// An allocation row with no object over it is not something IPAM writes, but
// the row is what holds the address, so the object is written afresh rather
// than the reclaim refused.
func (r *AllocatingREST) bindRetainedAllocation(
	ctx context.Context,
	tx pgx.Tx,
	claim *ipam.IPClaim,
	row *allocator.ClaimAllocation,
	allocationKey, allocationName, poolName string,
	uniqueScope map[string]ipam.ScopeRef,
	reclaimPolicy v1alpha1.ReclaimPolicy,
) error {
	var alloc *ipam.IPAllocation
	data, err := r.allocator.GetObject(ctx, tx, allocationKey)
	switch {
	case errors.Is(err, allocator.ErrObjectNotFound):
		alloc = newAllocationObject(claim, allocationName, poolName, row.ClassName, uniqueScope, reclaimPolicy, row.CIDR, row.ScopeDigest)
	case err != nil:
		return fmt.Errorf("read retained IPAllocation: %w", err)
	default:
		obj, err := runtime.Decode(r.codec, data)
		if err != nil {
			return fmt.Errorf("decode retained IPAllocation: %w", err)
		}
		var ok bool
		if alloc, ok = obj.(*ipam.IPAllocation); !ok {
			return fmt.Errorf("expected *ipam.IPAllocation at %q, got %T", allocationKey, obj)
		}
	}

	alloc.Spec.ClaimRef = &ipam.LocalRef{Name: claim.Name}
	alloc.Spec.ReclaimPolicy = ipam.ReclaimPolicy(reclaimPolicy)
	alloc.Spec.OwnerRef = claim.Spec.OwnerRef
	alloc.Status.Phase = ipam.AllocationReady

	encoded, err := runtime.Encode(r.codec, alloc)
	if err != nil {
		return fmt.Errorf("encode reclaimed IPAllocation: %w", err)
	}
	if data == nil {
		if _, err := r.allocator.InsertObject(ctx, tx, allocationKey, "IPAllocation", claim.Namespace, allocationName, encoded); err != nil {
			return fmt.Errorf("persist reclaimed IPAllocation: %w", err)
		}
		return nil
	}
	if _, err := r.allocator.UpdateObject(ctx, tx, allocationKey, encoded); err != nil {
		return fmt.Errorf("bind reclaimed IPAllocation: %w", err)
	}
	return nil
}

// newAllocationObject is the IPAllocation a claim's allocation is recorded
// as. The allocation records what it was handed out under, not just where it
// came from: under Retain it is read after the claim that chose those values
// is gone.
func newAllocationObject(
	claim *ipam.IPClaim,
	allocationName, poolName, className string,
	uniqueScope map[string]ipam.ScopeRef,
	reclaimPolicy v1alpha1.ReclaimPolicy,
	cidr, scopeDigest string,
) *ipam.IPAllocation {
	alloc := &ipam.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      allocationName,
			Namespace: claim.Namespace,
		},
		Spec: ipam.IPAllocationSpec{
			IPFamily:      claim.Spec.IPFamily,
			PoolRef:       ipam.LocalRef{Name: poolName},
			ClassName:     className,
			Purpose:       ipam.PurposeClaim,
			ClaimRef:      &ipam.LocalRef{Name: claim.Name},
			Scope:         uniqueScope,
			ReclaimPolicy: ipam.ReclaimPolicy(reclaimPolicy),
			OwnerRef:      claim.Spec.OwnerRef,
		},
		Status: ipam.IPAllocationStatus{
			Phase:         ipam.AllocationReady,
			AllocatedCIDR: cidr,
			Address:       singleAddress(cidr),
			ScopeDigest:   scopeDigest,
		},
	}
	// The allocation never passes through a create handler of its own, so the
	// system fields a POSTed object gets are stamped here instead. Otherwise it
	// reports a zero creation timestamp and no UID.
	rest.FillObjectMetaSystemFields(&alloc.ObjectMeta)
	return alloc
}

// retainedMismatchConflict is the refusal for a recreated claim that asks for
// something its retained allocation does not hold. It says both ways out: ask
// again on the retained terms, or release the allocation and claim afresh.
func retainedMismatchConflict(claimName, allocationName string, m *reclaimMismatch) error {
	return ipamerrors.NewRetainedAllocationMismatch(
		v1alpha1.Resource("ipclaims"),
		claimName,
		allocationName,
		m.field,
		fmt.Sprintf("IPAllocation %q, retained by an earlier claim of this name, cannot be reclaimed: %s (%s differs). "+
			"Recreate the claim on the retained terms to take the address back, or delete the IPAllocation to release it and claim afresh",
			allocationName, m.message, m.field),
	)
}
