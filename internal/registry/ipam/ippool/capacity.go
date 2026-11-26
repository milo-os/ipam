package ippool

import (
	"context"
	"fmt"

	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// A pool's allocated and available counts and its utilization are not stored
// with the pool. Every claim would have to rewrite the pool row to keep them
// current, which serializes claims on that row and publishes a MODIFIED event
// for the pool on each one. They are derived instead, whenever a pool is
// returned, from the running consumption total the allocator keeps in
// ipam_pool_consumption and the pool's address range.
//
// A watcher therefore sees current figures on every event it receives, but
// receives no event when only the figures change.

// Get returns the pool with its capacity derived from current consumption.
func (r *AllocatingIPPoolREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	obj, err := r.Store.Get(ctx, name, options)
	if err != nil {
		return nil, err
	}
	return obj, r.applyCapacity(ctx, obj)
}

// List returns the pools with their capacity derived from current consumption,
// read for the whole page in one query.
func (r *AllocatingIPPoolREST) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	obj, err := r.Store.List(ctx, options)
	if err != nil {
		return nil, err
	}
	return obj, r.applyCapacity(ctx, obj)
}

// Watch derives each delivered pool's capacity from current consumption. A
// failed read leaves the event's stored figures in place rather than ending
// the watch.
func (r *AllocatingIPPoolREST) Watch(ctx context.Context, options *metainternalversion.ListOptions) (watch.Interface, error) {
	w, err := r.Store.Watch(ctx, options)
	if err != nil {
		return nil, err
	}
	return watch.Filter(w, func(ev watch.Event) (watch.Event, bool) {
		if ev.Type == watch.Added || ev.Type == watch.Modified {
			if err := r.applyCapacity(ctx, ev.Object); err != nil {
				klog.ErrorS(err, "Could not derive pool capacity for a watch event; sending stored figures")
			}
		}
		return ev, true
	}), nil
}

// applyCapacity sets the derived capacity figures on a pool or a pool list.
func (r *AllocatingIPPoolREST) applyCapacity(ctx context.Context, obj runtime.Object) error {
	var pools []*ipam.IPPool
	switch o := obj.(type) {
	case *ipam.IPPool:
		pools = []*ipam.IPPool{o}
	case *ipam.IPPoolList:
		for i := range o.Items {
			pools = append(pools, &o.Items[i])
		}
	default:
		return nil
	}
	if len(pools) == 0 {
		return nil
	}

	project := tenant.FromContext(ctx).Name
	keys := make([]string, len(pools))
	for i, p := range pools {
		keys[i] = poolStorageKey(project, p.Name)
	}
	totals, err := allocator.ReadConsumedTotals(ctx, r.db, keys)
	if err != nil {
		return fmt.Errorf("derive pool capacity: %w", err)
	}
	for i, p := range pools {
		consumed, ok := totals[keys[i]]
		if !ok {
			continue // nothing allocated yet: the stored figures are current
		}
		cidr := p.Spec.CIDR
		if p.Status.AllocatedCIDR != "" {
			cidr = p.Status.AllocatedCIDR
		}
		total, allocated, available, utilization, ok := allocator.PoolCapacityFromConsumed(cidr, consumed)
		if !ok {
			continue
		}
		p.Status.Capacity = ipam.PoolCapacity{Total: total, Allocated: allocated, Available: available}
		p.Status.UtilizationPercent = utilization
	}
	return nil
}
