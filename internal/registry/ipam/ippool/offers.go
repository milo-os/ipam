package ippool

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// Update applies the same offer check as Create to the object the update
// produces.
//
// A pool that already breaks the rule is not grandfathered. The classes are
// compared as the pool will read after the write, so an update that leaves two
// disagreeing classes in place is republishing the hazard rather than
// inheriting it. Nothing is stranded by that: spec.classNames is mutable, the
// error names the two classes to reconcile, and the fix can ride along in the
// same request as whatever else the pool was being edited for.
func (r *AllocatingIPPoolREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	validate := func(ctx context.Context, obj, old runtime.Object) error {
		if updateValidation != nil {
			if err := updateValidation(ctx, obj, old); err != nil {
				return err
			}
		}
		pool, ok := obj.(*ipam.IPPool)
		if !ok {
			return fmt.Errorf("expected *ipam.IPPool, got %T", obj)
		}
		return r.validateClassOffers(ctx, pool)
	}
	return r.Store.Update(ctx, name, objInfo, createValidation, validate, forceAllowCreate, options)
}

// validateClassOffers rejects a pool offered to two classes that disagree about
// what makes one address space.
//
// Non-overlap is enforced per address space: the exclusion constraint compares
// allocations sharing a (pool_key, scope_digest), and the allocator's search
// reads only the allocations in the claim's own space. Both are correct only
// while every claim drawing from a pool derives that digest the same way. Two
// classes with different uniqueWithin break that premise — the pool holds one
// class's allocations under one digest and the other's under another, so
// neither the constraint nor the search ever compares them, and the second
// class hands out addresses the first already holds. It surfaces as two holders
// of one address with no constraint violation and nothing logged, which is the
// worst shape a fault in this service can take.
//
// It is checked here rather than during allocation because the pool is where
// the two classes meet, and because a claim cannot report a conflict it is
// structurally unable to see.
//
// It lives outside the strategy because the rule needs the class catalog, and a
// strategy has no store to read.
func (r *AllocatingIPPoolREST) validateClassOffers(ctx context.Context, pool *ipam.IPPool) error {
	if len(pool.Spec.ClassNames) < 2 {
		return nil
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin class offer validation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	spaces, err := allocator.OfferedSpaces(ctx, tx, tenant.FromContext(ctx).Name, pool.Spec.ClassNames)
	if err != nil {
		return fmt.Errorf("resolve the classes IPPool %q offers itself to: %w", pool.Name, err)
	}
	d := allocator.FirstDisagreement(spaces)
	if d == nil {
		return nil
	}
	return apierrors.NewInvalid(ipam.Kind("IPPool"), pool.Name, field.ErrorList{
		field.Invalid(field.NewPath("spec", "classNames"), pool.Spec.ClassNames, d.Error()),
	})
}
