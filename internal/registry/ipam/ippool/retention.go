package ippool

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// applyRetentionCap brings forward the deadline of every allocation the pool
// retains that its maxRetentionLease now cuts short (allocator.TightenRetention).
//
// It runs after the update has committed, in a transaction of its own under the
// pool's lock, because the generic store's write cannot join one. It is
// idempotent and reads the cap from the stored pool, so it runs on every update
// of a pool that has a cap, not only on one that lowered it. A failure is
// answered with an error the caller can retry: the retry is a no-op update
// followed by the tightening that did not happen.
func (r *AllocatingIPPoolREST) applyRetentionCap(ctx context.Context, name string, obj runtime.Object, options *metav1.UpdateOptions) error {
	pool, ok := obj.(*ipam.IPPool)
	if !ok || pool.Spec.MaxRetentionLease == nil || (options != nil && len(options.DryRun) > 0) {
		return nil
	}
	poolKey := poolStorageKey(tenant.FromContext(ctx).Name, name)
	if err := r.tightenRetention(ctx, poolKey); err != nil {
		klog.ErrorS(err, "Could not apply a pool's maxRetentionLease to the allocations it retains", "pool", name)
		return apierrors.NewInternalError(fmt.Errorf(
			"IPPool %q was updated, but its maxRetentionLease is not yet applied to the allocations it already retains; retry the update: %w", name, err))
	}
	return nil
}

func (r *AllocatingIPPoolREST) tightenRetention(ctx context.Context, poolKey string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin retention tightening transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	moved, countedPast, err := allocator.TightenRetention(ctx, tx, poolKey)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit retention tightening: %w", err)
	}
	if moved > 0 {
		metrics.RecordRetentionReleases(string(allocator.SweepObserve), countedPast)
		klog.V(2).InfoS("Brought retention deadlines forward to the pool's maxRetentionLease", "pool", poolKey, "allocations", moved)
	}
	return nil
}
