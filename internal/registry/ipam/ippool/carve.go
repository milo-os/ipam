package ippool

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// The Allocated condition reports whether a pool holds its range. A root pool
// holds it from the moment it is created; a child pool holds it once it has
// been carved out of its parent.
const (
	conditionAllocated = "Allocated"

	reasonAllocationSucceeded = "AllocationSucceeded"
	// reasonParentNotFound: the pool named in spec.parentPoolRef does not exist.
	reasonParentNotFound = "ParentNotFound"
	// reasonParentNotReady: the parent exists but has no range of its own yet,
	// so there is nothing to carve from.
	reasonParentNotReady = "ParentNotReady"
	// reasonParentExhausted: the parent has no free block of spec.prefixLength.
	reasonParentExhausted = "ParentExhausted"
)

// createOnUpdate creates a pool that an update found missing.
//
// Server-side apply of an object that does not exist yet is a patch, not a
// create, and the apiserver hands it to Update with forceAllowCreate set. The
// embedded store would write the object as it stands, so a child pool would be
// stored without being carved. Building the object the patch describes and
// passing it to Create gives every way of creating a pool the same checks and
// the same carve: kubectl create, kubectl apply --server-side and a Flux
// Kustomization end in the same state, or are refused with the same error.
func (r *AllocatingIPPoolREST) createOnUpdate(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	// A precondition names an object that exists. With none here, the request
	// cannot be met, which is what the generic store reports too.
	if p := objInfo.Preconditions(); p != nil && (p.UID != nil || p.ResourceVersion != nil) {
		return nil, false, apierrors.NewNotFound(r.DefaultQualifiedResource, name)
	}

	obj, err := objInfo.UpdatedObject(ctx, r.NewFunc())
	if err != nil {
		return nil, false, err
	}
	objectMeta, err := meta.Accessor(obj)
	if err != nil {
		return nil, false, fmt.Errorf("get object metadata: %w", err)
	}
	if objectMeta.GetName() != name {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("the name of the object (%s) does not match the name on the URL (%s)", objectMeta.GetName(), name))
	}
	// Match what the generic store does for a create through update, so the
	// object Create sees is the one a POST would have handed it.
	rest.WipeObjectMetaSystemFields(objectMeta)
	objectMeta.SetResourceVersion("")

	createOptions := &metav1.CreateOptions{}
	if options != nil {
		createOptions.DryRun = options.DryRun
		createOptions.FieldManager = options.FieldManager
		createOptions.FieldValidation = options.FieldValidation
	}
	out, err := r.Create(ctx, obj, createValidation, createOptions)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// carveStranded carves a child pool that is stored but holds no range.
//
// Before create-on-update went through Create, a child pool created by
// server-side apply was stored at Pending with no allocatedCIDR, and nothing
// would ever carve it. The next update of such a pool carves it now. If it
// still cannot be carved, the pool records why in its Allocated condition
// rather than staying Pending with no explanation.
//
// Like applyRetentionCap, this runs after the update has committed, in a
// transaction of its own, because the generic store's write cannot join one.
// It is best effort: a failure is logged and the pool returned as the update
// left it. It never runs for a dry-run.
func (r *AllocatingIPPoolREST) carveStranded(ctx context.Context, pool *ipam.IPPool) *ipam.IPPool {
	parentName := pool.Spec.ParentPoolRef.Name
	id := tenant.FromContext(ctx)
	childKey := poolStorageKey(id.Name, pool.Name)
	parentKey := poolStorageKey(id.Name, parentName)

	parentObj, err := r.Get(ctx, parentName, &metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.recordCarveFailure(ctx, pool, childKey, reasonParentNotFound,
				fmt.Sprintf("parent IPPool %q not found", parentName))
		}
		klog.ErrorS(err, "Could not read the parent of a child pool that holds no range", "pool", childKey, "parent", parentName)
		return pool
	}
	parentPool, ok := parentObj.(*ipam.IPPool)
	if !ok {
		return pool
	}
	ipFamily, err := effectiveIPFamily(parentPool)
	if err != nil {
		return r.recordCarveFailure(ctx, pool, childKey, reasonParentNotReady,
			fmt.Sprintf("parent IPPool %q has no range to carve from", parentName))
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		klog.ErrorS(err, "Could not begin the carve of a child pool that holds no range", "pool", childKey)
		return pool
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// AllocatePrefix locks the parent first; the child's row is locked after
	// it, which is the ancestor-first order every multi-pool writer takes.
	cidr, err := r.allocator.AllocatePrefix(ctx, tx, allocator.PrefixRequest{
		PoolKey:       parentKey,
		PrefixLen:     pool.Spec.PrefixLength,
		IPFamily:      ipFamily,
		ClaimKey:      childKey,
		AllocationKey: childKey,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		switch {
		case registryerrors.IsUniqueViolation(err, registryerrors.HolderConstraint):
			// The pool already holds a carve: a concurrent update carved it.
			return r.reread(ctx, pool)
		case errors.Is(err, allocator.ErrPoolExhausted):
			return r.recordCarveFailure(ctx, pool, childKey, reasonParentExhausted,
				fmt.Sprintf("parent IPPool %q has no free /%d", parentName, pool.Spec.PrefixLength))
		case errors.Is(err, allocator.ErrPoolNotFound):
			return r.recordCarveFailure(ctx, pool, childKey, reasonParentNotFound,
				fmt.Sprintf("parent IPPool %q not found", parentName))
		}
		klog.ErrorS(err, "Could not carve a child pool that holds no range", "pool", childKey, "parent", parentName)
		return pool
	}

	stored, err := r.lockStored(ctx, tx, childKey)
	if err != nil {
		klog.ErrorS(err, "Could not read a child pool to record its carve", "pool", childKey)
		return pool
	}
	if stored.UID != pool.UID || stored.Status.AllocatedCIDR != "" {
		// Deleted and recreated, or carved, since the update returned.
		return stored
	}
	setCarvedStatus(stored, cidr, parentName)
	if err := r.writeStored(ctx, tx, childKey, stored); err != nil {
		klog.ErrorS(err, "Could not record the carve of a child pool", "pool", childKey)
		return pool
	}
	if err := tx.Commit(ctx); err != nil {
		klog.ErrorS(err, "Could not commit the carve of a child pool", "pool", childKey)
		return pool
	}
	klog.V(2).InfoS("Carved a child pool that was stored without a range", "pool", childKey, "cidr", cidr)
	return stored
}

// recordCarveFailure sets a stranded child pool's Allocated condition to False
// with the reason it could not be carved. The pool stays Pending: the next
// update tries the carve again. Nothing is written when the condition already
// says the same thing, so a GitOps tool reapplying the pool does not bump its
// resourceVersion every time.
func (r *AllocatingIPPoolREST) recordCarveFailure(ctx context.Context, pool *ipam.IPPool, childKey, reason, message string) *ipam.IPPool {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		klog.ErrorS(err, "Could not begin recording why a child pool was not carved", "pool", childKey)
		return pool
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, err := r.lockStored(ctx, tx, childKey)
	if err != nil {
		klog.ErrorS(err, "Could not read a child pool to record why it was not carved", "pool", childKey)
		return pool
	}
	if stored.UID != pool.UID || stored.Status.AllocatedCIDR != "" {
		return stored
	}
	if c := meta.FindStatusCondition(stored.Status.Conditions, conditionAllocated); c != nil &&
		c.Status == metav1.ConditionFalse && c.Reason == reason && c.Message == message {
		return stored
	}
	meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
		Type:    conditionAllocated,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	stored.Status.Phase = ipam.PoolPending
	if err := r.writeStored(ctx, tx, childKey, stored); err != nil {
		klog.ErrorS(err, "Could not record why a child pool was not carved", "pool", childKey)
		return pool
	}
	if err := tx.Commit(ctx); err != nil {
		klog.ErrorS(err, "Could not commit why a child pool was not carved", "pool", childKey)
		return pool
	}
	return stored
}

// lockStored reads and locks a pool row inside tx.
func (r *AllocatingIPPoolREST) lockStored(ctx context.Context, tx pgx.Tx, key string) (*ipam.IPPool, error) {
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1 FOR UPDATE`, key).Scan(&data); err != nil {
		return nil, fmt.Errorf("lock pool row %q: %w", key, err)
	}
	obj, err := runtime.Decode(r.codec, data)
	if err != nil {
		return nil, fmt.Errorf("decode pool %q: %w", key, err)
	}
	pool, ok := obj.(*ipam.IPPool)
	if !ok {
		return nil, fmt.Errorf("pool row %q decoded to %T", key, obj)
	}
	return pool, nil
}

// writeStored writes pool back to its row inside tx and stamps the new
// resourceVersion on it.
func (r *AllocatingIPPoolREST) writeStored(ctx context.Context, tx pgx.Tx, key string, pool *ipam.IPPool) error {
	data, err := runtime.Encode(r.codec, pool)
	if err != nil {
		return fmt.Errorf("encode pool: %w", err)
	}
	rv, err := r.allocator.UpdateObject(ctx, tx, key, data)
	if err != nil {
		return err
	}
	return storage.APIObjectVersioner{}.UpdateObject(pool, uint64(rv))
}

// reread returns the pool as stored now, or pool if it cannot be read.
func (r *AllocatingIPPoolREST) reread(ctx context.Context, pool *ipam.IPPool) *ipam.IPPool {
	obj, err := r.Get(ctx, pool.Name, &metav1.GetOptions{})
	if err != nil {
		return pool
	}
	if p, ok := obj.(*ipam.IPPool); ok {
		return p
	}
	return pool
}
