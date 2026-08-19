package ipclaim

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// createScopeRange binds the range a class holds for the claim's scope,
// provisioning it and everything above it if this is the first ask.
//
// It carves nothing out of that range and writes no IPAllocation. The range IS
// the object being held — the pool the cascade would have built under the first
// block claim, built now instead — so an allocation inside it would be a second
// thing, and the block nobody asked for is exactly what this endpoint exists to
// stop callers having to create.
//
// Returns the object, the reason label for the failure metric (empty when the
// call succeeded), and the error.
func (r *AllocatingREST) createScopeRange(
	ctx context.Context,
	span trace.Span,
	claim *ipam.IPClaim,
	class *allocator.ResolvedClass,
	id tenant.Identity,
	dryRun bool,
) (runtime.Object, string, error) {
	poolKey, err := allocator.ResolveScopeRange(ctx, r.db, class, claim.Spec.Scope)
	if err != nil {
		span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonPoolNotFound))
		span.SetStatus(codes.Error, "resolve scope range")
		switch {
		case errors.Is(err, allocator.ErrClassHoldsNoRange):
			return nil, "class_holds_no_range", apierrors.NewBadRequest(err.Error())
		case errors.Is(err, allocator.ErrNoOfferingPool):
			return nil, "pool_not_found", ipamerrors.New(ipamerrors.ReasonNoOfferingPool, err.Error())
		case errors.Is(err, allocator.ErrPoolExhausted):
			return nil, "pool_exhausted", registryerrors.NewInsufficientStorage(err.Error())
		}
		var missingRole *scope.MissingRoleError
		if errors.As(err, &missingRole) {
			return nil, "pool_not_found", ipamerrors.NewScopeRolesMissing(missingRole.Roles, err.Error())
		}
		return nil, "tx_error", fmt.Errorf("resolve scope range: %w", err)
	}

	poolName := poolKey[strings.LastIndex(poolKey, "/")+1:]
	span.SetAttributes(attribute.String(tracing.AttrPoolName, poolName))
	claimKey := claimObjectKey(id, claim.Namespace, claim.Name)

	tx, err := r.db.Begin(ctx)
	if err != nil {
		span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonTxError))
		return nil, "tx_error", fmt.Errorf("begin scope-range transaction: %w", err)
	}

	cidr, err := allocator.BindScopeRange(ctx, tx, poolKey, claimKey)
	if err != nil {
		_ = tx.Rollback(ctx)
		span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonTxError))
		if errors.Is(err, allocator.ErrScopeRangeHeld) {
			return nil, "conflict", apierrors.NewConflict(
				v1alpha1.Resource("ipclaims"), claim.Name,
				fmt.Errorf("the range %s holds is already held by another claim; delete that claim to release it", poolName))
		}
		if errors.Is(err, allocator.ErrScopeRangeCarveMissing) {
			return nil, "conflict", apierrors.NewConflict(
				v1alpha1.Resource("ipclaims"), claim.Name,
				fmt.Errorf("IPPool %q was authored rather than provisioned for this scope, so no claim holds it", poolName))
		}
		return nil, "tx_error", err
	}

	claim.Status.Phase = ipam.ClaimBound
	claim.Status.AllocatedCIDR = cidr
	claim.Status.PoolRef = &ipam.LocalRef{Name: poolName}

	// A range claim binds no IPAllocation. BoundAllocationRef stays empty
	// rather than pointing at the pool: an IPAllocation is a block handed out
	// of a pool, and this claim holds the pool.

	if dryRun {
		_ = tx.Rollback(ctx)
		return claim, "", nil
	}

	claimData, err := runtime.Encode(r.codec, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, "internal", fmt.Errorf("encode scope-range claim: %w", err)
	}
	rv, err := r.allocator.InsertObject(ctx, tx, claimKey, "IPClaim", claim.Namespace, claim.Name, claimData)
	if err != nil {
		_ = tx.Rollback(ctx)
		if isIdentityCollision(err) {
			return nil, "conflict", duplicateClaimConflict(claim.Name)
		}
		return nil, "tx_error", fmt.Errorf("persist scope-range claim: %w", err)
	}
	versioner := storage.APIObjectVersioner{}
	if err := versioner.UpdateObject(claim, uint64(rv)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, "internal", fmt.Errorf("set resource version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "tx_error", fmt.Errorf("commit scope-range transaction: %w", err)
	}
	return claim, "", nil
}

// deleteScopeRange releases the range a claim holds and deletes the claim, in
// one transaction.
//
// One transaction, and no intermediate Releasing phase, because the release can
// be REFUSED: a range with subnets or endpoints still inside it is not freed,
// and a refusal must leave the claim exactly as it was rather than parked in a
// phase that describes something that is not happening. The block path can
// publish Releasing because nothing it does afterwards can decline.
func (r *AllocatingREST) deleteScopeRange(ctx context.Context, claim *ipam.IPClaim) (runtime.Object, bool, error) {
	id := tenant.FromContext(ctx)
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanClaimRelease)
	defer span.End()
	span.SetAttributes(
		attribute.String(tracing.AttrTenantProject, id.Project()),
		attribute.String(tracing.AttrClaimIPFamily, string(claim.Spec.IPFamily)),
	)

	claimKey := claimObjectKey(id, claim.Namespace, claim.Name)

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin scope-range release transaction: %w", err)
	}

	poolKey, err := allocator.FindScopeRange(ctx, tx, claimKey)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, err
	}
	// An empty key means the claim holds no range — it was already released, or
	// something released the pool out from under it. Either way the claim row
	// is the only thing left to remove.
	if poolKey != "" {
		if err := allocator.ReleaseScopeRange(ctx, tx, r.allocator, poolKey); err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, allocator.ErrScopeRangeOccupied) {
				span.SetAttributes(attribute.String(tracing.AttrErrorReason, tracing.ReasonTxError))
				return nil, false, apierrors.NewConflict(
					schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ipclaims"},
					claim.Name,
					fmt.Errorf("%s; release everything allocated inside this range first", err.Error()),
				)
			}
			return nil, false, err
		}
	}

	if _, err := r.allocator.DeleteObject(ctx, tx, claimKey); err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, fmt.Errorf("delete scope-range claim row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit scope-range release transaction: %w", err)
	}

	klog.V(2).InfoS("scope range released", "claim", claim.Name, "pool", poolKey)
	metrics.RecordRelease("ipclaim")

	released := claim.DeepCopy()
	released.Status.Phase = ipam.ClaimReleasing
	return released, true, nil
}
