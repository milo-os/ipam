package ipclaim

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// conditionBound reports why a claim stored without an address could not be
// bound. A claim bound at create carries no condition: its phase says it.
const conditionBound = "Bound"

// reasonAllocationRefused is the condition reason for a refusal that carries
// no IPAM reason of its own.
const reasonAllocationRefused = "AllocationRefused"

// Update hands an update that creates the claim to Create, and binds a claim
// that is stored without an address.
//
// Server-side apply of a claim that does not exist yet is a patch, not a
// create, and the apiserver hands it to Update with forceAllowCreate set. The
// generic store would write the claim as it stands, at Pending with no
// address, and nothing would ever bind it. Building the object the patch
// describes and passing it to Create gives every way of creating a claim the
// same checks and the same allocation: kubectl create, kubectl apply
// --server-side and a Flux Kustomization end in the same state, or are refused
// with the same error. createValidation carries the create admission checks,
// quota among them, so it is passed on unchanged.
func (r *AllocatingREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	if forceAllowCreate || r.strategy.AllowCreateOnUpdate(ctx) {
		if _, err := r.Get(ctx, name, &metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return r.createOnUpdate(ctx, name, objInfo, createValidation, options)
		} else if err != nil {
			return nil, false, err
		}
	}

	obj, created, err := r.IPClaimStorage.Update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
	if err != nil {
		return obj, created, err
	}
	if claim, ok := obj.(*ipam.IPClaim); ok && isStranded(claim) && (options == nil || len(options.DryRun) == 0) {
		obj = r.bindStranded(ctx, claim)
	}
	return obj, created, nil
}

// createOnUpdate creates a claim that an update found missing, through Create.
func (r *AllocatingREST) createOnUpdate(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
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

// isStranded reports whether a stored claim holds no address and is not on
// its way out. Before create-on-update went through Create, every claim
// created by server-side apply was stored like this.
func isStranded(claim *ipam.IPClaim) bool {
	return claim.DeletionTimestamp == nil &&
		(claim.Status.Phase == ipam.ClaimPending || claim.Status.Phase == "") &&
		claim.Status.AllocatedCIDR == "" &&
		claim.Status.BoundAllocationRef == nil
}

// bindStranded binds a claim that is stored without an address, by running it
// through the same allocation a create runs: class resolution, the namespace
// and pool access checks, a reclaim of an address retained under its name,
// a named address, a scope range. The claim keeps its identity; its row is
// rewritten in the allocation's transaction rather than inserted (see
// writeClaimRow). Create admission is not run again: it ran when the claim was
// first stored.
//
// If the claim still cannot be bound, its Bound condition records the refusal
// a create would have returned, instead of the claim staying Pending with no
// explanation.
//
// Like the pool's retention cap, this runs after the update has committed, in
// transactions of its own, so it is best effort: a failure is logged and the
// claim returned as the update left it. It never runs for a dry-run.
func (r *AllocatingREST) bindStranded(ctx context.Context, claim *ipam.IPClaim) *ipam.IPClaim {
	obj, err := r.create(withHealing(ctx, claim.UID), claim.DeepCopy(), nil, &metav1.CreateOptions{}, false)
	if err == nil {
		if bound, ok := obj.(*ipam.IPClaim); ok {
			klog.V(2).InfoS("Bound a claim that was stored without an address",
				"namespace", claim.Namespace, "claim", claim.Name, "cidr", bound.Status.AllocatedCIDR)
			return bound
		}
		return claim
	}

	// A concurrent update may have bound it first, or the claim may have
	// been deleted: either way the stored claim is the answer.
	stored, gerr := r.Get(ctx, claim.Name, &metav1.GetOptions{})
	if gerr != nil {
		return claim
	}
	current, ok := stored.(*ipam.IPClaim)
	if !ok || current.UID != claim.UID || !isStranded(current) {
		if ok {
			return current
		}
		return claim
	}
	if errors.Is(err, errStaleHeal) || !isRefusal(err) {
		klog.ErrorS(err, "Could not bind a claim that was stored without an address",
			"namespace", claim.Namespace, "claim", claim.Name)
		return current
	}
	return r.recordBindFailure(ctx, current, err)
}

// isRefusal reports whether err refuses the claim, as opposed to the server
// failing to answer. Only a refusal is worth recording on the claim.
func isRefusal(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	code := status.Status().Code
	return code >= 400 && code < 500 || code == 507
}

// recordBindFailure sets a stranded claim's Bound condition to False with the
// reason a create would have been refused. Nothing is written when the
// condition already says the same thing, so a GitOps tool reapplying the claim
// does not bump its resourceVersion every time.
func (r *AllocatingREST) recordBindFailure(ctx context.Context, claim *ipam.IPClaim, cause error) *ipam.IPClaim {
	reason := string(ipamerrors.ReasonFor(cause))
	if reason == "" {
		reason = string(apierrors.ReasonForError(cause))
	}
	if reason == "" || reason == string(metav1.StatusReasonUnknown) {
		reason = reasonAllocationRefused
	}
	message := cause.Error()

	id := tenant.FromContext(ctx)
	claimKey := claimObjectKey(id, claim.Namespace, claim.Name)
	tx, err := r.db.Begin(ctx)
	if err != nil {
		klog.ErrorS(err, "Could not begin recording why a claim was not bound", "claim", claimKey)
		return claim
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, err := r.lockClaimRow(ctx, tx, claimKey)
	if err != nil {
		klog.ErrorS(err, "Could not read a claim to record why it was not bound", "claim", claimKey)
		return claim
	}
	if stored.UID != claim.UID || !isStranded(stored) {
		return stored
	}
	if c := meta.FindStatusCondition(stored.Status.Conditions, conditionBound); c != nil &&
		c.Status == metav1.ConditionFalse && c.Reason == reason && c.Message == message {
		return stored
	}
	meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
		Type:    conditionBound,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	stored.Status.Phase = ipam.ClaimPending
	data, err := runtime.Encode(r.codec, stored)
	if err != nil {
		klog.ErrorS(err, "Could not encode a claim to record why it was not bound", "claim", claimKey)
		return claim
	}
	rv, err := r.allocator.UpdateObject(ctx, tx, claimKey, data)
	if err != nil {
		klog.ErrorS(err, "Could not record why a claim was not bound", "claim", claimKey)
		return claim
	}
	if err := (storage.APIObjectVersioner{}).UpdateObject(stored, uint64(rv)); err != nil {
		return claim
	}
	if err := tx.Commit(ctx); err != nil {
		klog.ErrorS(err, "Could not commit why a claim was not bound", "claim", claimKey)
		return claim
	}
	return stored
}

// healingKey marks a create that binds a claim already stored, carrying the
// UID that claim was stored with.
type healingKey struct{}

func withHealing(ctx context.Context, uid types.UID) context.Context {
	return context.WithValue(ctx, healingKey{}, uid)
}

func healingUID(ctx context.Context) (types.UID, bool) {
	uid, ok := ctx.Value(healingKey{}).(types.UID)
	return uid, ok
}

// errStaleHeal means the claim a bind in place was for changed under it: it
// was deleted, recreated, or bound by another request.
var errStaleHeal = errors.New("the claim changed while it was being bound")

// writeClaimRow writes a claim's row in the allocation's transaction. A create
// inserts it. A bind in place rewrites the stored row, under its lock, and
// only while it is still the stranded claim the bind started from; otherwise
// it returns errStaleHeal and the transaction is rolled back with the rest of
// the allocation.
func (r *AllocatingREST) writeClaimRow(ctx context.Context, tx pgx.Tx, claimKey string, claim *ipam.IPClaim, data []byte) (int64, error) {
	uid, healing := healingUID(ctx)
	if !healing {
		return r.allocator.InsertObject(ctx, tx, claimKey, "IPClaim", claim.Namespace, claim.Name, data)
	}
	stored, err := r.lockClaimRow(ctx, tx, claimKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errStaleHeal
		}
		return 0, err
	}
	if stored.UID != uid || !isStranded(stored) {
		return 0, errStaleHeal
	}
	return r.allocator.UpdateObject(ctx, tx, claimKey, data)
}

// lockClaimRow reads and locks a claim row inside tx.
func (r *AllocatingREST) lockClaimRow(ctx context.Context, tx pgx.Tx, key string) (*ipam.IPClaim, error) {
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1 FOR UPDATE`, key).Scan(&data); err != nil {
		return nil, fmt.Errorf("lock claim row %q: %w", key, err)
	}
	obj, err := runtime.Decode(r.codec, data)
	if err != nil {
		return nil, fmt.Errorf("decode claim %q: %w", key, err)
	}
	claim, ok := obj.(*ipam.IPClaim)
	if !ok {
		return nil, fmt.Errorf("claim row %q decoded to %T", key, obj)
	}
	return claim, nil
}

var _ rest.Updater = (*AllocatingREST)(nil)
