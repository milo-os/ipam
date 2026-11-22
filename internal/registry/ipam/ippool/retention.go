package ippool

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// tightenRetention is allocator.TightenRetention, as a variable so a test can
// make it fail.
var tightenRetention = allocator.TightenRetention

// applyRetentionCap brings forward the deadline of every allocation the pool
// retains that its maxRetentionLease now cuts short (allocator.TightenRetention),
// so the new cap shows on those allocations as soon as the update returns.
//
// It runs after the update has committed, in a transaction of its own under the
// pool's lock, because the generic store's write cannot join one. That makes it
// best effort: the update has already happened, and answering it with an error
// would tell the caller otherwise and send a retry into a conflict. A failure
// is logged and counted instead, and the retention sweeper applies every
// pool's cap on each pass, so the cap is applied within one sweep interval
// either way.
func (r *AllocatingIPPoolREST) applyRetentionCap(ctx context.Context, name string, obj runtime.Object, options *metav1.UpdateOptions) {
	pool, ok := obj.(*ipam.IPPool)
	if !ok || pool.Spec.MaxRetentionLease == nil || (options != nil && len(options.DryRun) > 0) {
		return
	}
	poolKey := poolStorageKey(tenant.FromContext(ctx).Name, name)
	moved, err := r.tightenRetentionNow(ctx, poolKey)
	if err != nil {
		metrics.RetentionCapApplyFailures.Inc()
		klog.ErrorS(err, "Could not apply a pool's maxRetentionLease to the allocations it retains; the retention sweeper will", "pool", name)
		return
	}
	if moved > 0 {
		klog.V(2).InfoS("Brought retention deadlines forward to the pool's maxRetentionLease", "pool", poolKey, "allocations", moved)
	}
}

func (r *AllocatingIPPoolREST) tightenRetentionNow(ctx context.Context, poolKey string) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin retention tightening transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	moved, err := tightenRetention(ctx, tx, poolKey)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit retention tightening: %w", err)
	}
	return moved, nil
}
