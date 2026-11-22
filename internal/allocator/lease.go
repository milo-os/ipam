package allocator

// Retention leases.
//
// A retained address is capacity nobody else can use, so retention may carry a
// lease. The lease is resolved once, when the claim is deleted, and stored on
// the row as expires_at; everything after that reads the stored deadline. See
// migrations/008_retention_expiry.sql for the column's three states.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// LeaseState is what is known about a retained allocation's lease. The values
// are the lease label of ipam_retained_allocations.
type LeaseState string

const (
	// LeaseFinite: the lease runs out at ExpiresAt.
	LeaseFinite LeaseState = "finite"
	// LeaseNone: neither the class nor the pool sets a lease.
	LeaseNone LeaseState = "none"
	// LeaseUnevaluated: the lease could not be resolved yet.
	LeaseUnevaluated LeaseState = "unevaluated"
)

// RetainedAllocation is what retention left of a claim allocation: the row,
// unbound, and the lease it is held under.
type RetainedAllocation struct {
	AllocationKey string
	RetainedAt    time.Time
	Lease         LeaseState
	// ExpiresAt is set when Lease is LeaseFinite.
	ExpiresAt time.Time
}

// EffectiveLease is how long an allocation of a class with classLease, drawn
// from a pool capped at poolCap, may stay retained: the shorter of the two.
// Nil means no expiry, which is what both unset gives.
//
// The pool caps even a class that states no lease. The pool is the thing that
// runs out, so an operator hardening a scarce range does not have to find and
// edit every class that draws from it.
//
// A lease that is not positive counts as unset. Validation refuses one now,
// but an object stored before it did may still hold one, and reading "0s" or
// "-1h" as a lease would make every allocation it covers due at once.
func EffectiveLease(classLease, poolCap *metav1.Duration) *time.Duration {
	var out *time.Duration
	for _, d := range []*metav1.Duration{classLease, poolCap} {
		if d == nil || d.Duration <= 0 {
			continue
		}
		if out == nil || d.Duration < *out {
			v := d.Duration
			out = &v
		}
	}
	return out
}

// ErrRetentionLeaseRequired is returned for a claim whose Retain override
// would retain its address with no lease (see RetainOverridesClass).
var ErrRetentionLeaseRequired = errors.New("ipam: retaining this allocation would have no lease")

// RetainOverridesClass reports whether a claim stating override asks to retain
// what its class would delete. Only such a claim needs a lease to retain: a
// class whose own default is Retain retains by its owner's decision, lease or
// none, and a claim agreeing with it changes nothing.
//
// A claim may retain past its class's default only for as long as a lease
// allows. Without one, a create-retain-delete loop holds addresses with no end
// and, until quota counts retained addresses (milo-os/milo#1028), outside any
// limit.
func RetainOverridesClass(class *ipamv1alpha1.IPClass, override ipamv1alpha1.ReclaimPolicy) bool {
	return override == ipamv1alpha1.ReclaimRetain &&
		EffectiveReclaimPolicy(class, "") != ipamv1alpha1.ReclaimRetain
}

// retentionClassProject is the project whose class an allocation row was
// handed out under, and whether that can be told from the row and its pool.
//
// The row's recorded class_project is the class definition's project, which
// is exactly what a reference resolved to when the claim was made. Rows written
// before migration 007 did not record it. For those the pool's project is the
// definition's only where the pool itself offers the class by name: an
// operator pool offering a class lives in the project defining it. A pool
// provisioned by a class chain may sit under another project's class of the
// same name, so for one of those the class is not known, and the caller applies
// only the pool's cap. That can hold an address longer than its class meant,
// never shorter. Neither path needs a tenant on the context: the sweeper has
// none.
func retentionClassProject(pool *ipamv1alpha1.IPPool, poolKey, className string, classProject *string) (string, bool) {
	if classProject != nil && *classProject != "" {
		return *classProject, true
	}
	if pool != nil && className != "" && slices.Contains(pool.Spec.ClassNames, className) {
		return tenant.ProjectFromKey(poolKey), true
	}
	return "", false
}

// classRetentionLease reads the retention lease of class name in project.
//
// A class that no longer exists sets no lease, and the pool's cap alone
// applies: deleting a class must not strand what it retained. Any other
// failure is returned, and the caller leaves the lease unevaluated.
//
// The read runs under a savepoint, so a failed statement does not abort the
// caller's transaction: retention must not fail a claim's delete because a
// class could not be read.
func classRetentionLease(ctx context.Context, tx pgx.Tx, project, name string) (*metav1.Duration, error) {
	if name == "" {
		return nil, nil
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("open savepoint for class %q: %w", name, err)
	}
	class, err := resolveClassIn(ctx, sp, project, name)
	if err != nil {
		_ = sp.Rollback(ctx)
		if errors.Is(err, ErrClassNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, fmt.Errorf("release savepoint for class %q: %w", name, err)
	}
	return class.Spec.RetentionLease, nil
}

// leaseFor resolves the lease of one retained row in pool, which the caller
// holds locked. ok is false when the class could not be read; the error says
// why, for the caller's log. A class the row does not identify contributes no
// lease (see retentionClassProject).
func leaseFor(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, poolKey, className string, classProject *string) (lease *time.Duration, ok bool, err error) {
	return leaseWith(ctx, tx, pool, poolKey, className, classProject, classRetentionLease)
}

// classLeaseReader reads a class's retention lease; classRetentionLease, or a
// pass's memo of it.
type classLeaseReader func(ctx context.Context, tx pgx.Tx, project, name string) (*metav1.Duration, error)

func leaseWith(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, poolKey, className string, classProject *string, read classLeaseReader) (*time.Duration, bool, error) {
	var classLease *metav1.Duration
	if project, known := retentionClassProject(pool, poolKey, className, classProject); known {
		var err error
		if classLease, err = read(ctx, tx, project, className); err != nil {
			return nil, false, err
		}
	}
	var poolCap *metav1.Duration
	if pool != nil {
		poolCap = pool.Spec.MaxRetentionLease
	}
	return EffectiveLease(classLease, poolCap), true, nil
}

// writeLease stores the deadline lease gives a retained row and returns the
// row's retention as stored.
//
// The deadline is retention plus the lease, rounded up to the whole second
// (ipam_ceil_second). The API reports times to the second, so status.expiresAt
// is exactly the instant the sweeper acts on, and the address is never due
// before the full lease has passed. It writes only a row that is still retained and
// not yet evaluated, so it can neither give a bound row a deadline nor move one
// already fixed; changed is false when it wrote nothing. The caller holds the
// row's pool locked.
func writeLease(ctx context.Context, tx pgx.Tx, allocationKey string, lease *time.Duration) (RetainedAllocation, bool, error) {
	defer metrics.ObserveQuery("write_retention_lease", time.Now())
	var micros *int64
	if lease != nil {
		v := lease.Microseconds()
		micros = &v
	}
	var (
		retainedAt time.Time
		expiresAt  pgtype.Timestamptz
	)
	err := tx.QueryRow(ctx,
		`UPDATE ipam_cidr_allocations
		    SET expires_at = COALESCE(ipam_ceil_second(retained_at + $2::bigint * interval '1 microsecond'), 'infinity')
		  WHERE allocation_key = $1 AND claim_key IS NULL AND purpose = 'Claim'
		    AND retained_at IS NOT NULL AND expires_at IS NULL
		 RETURNING retained_at, expires_at`, allocationKey, micros,
	).Scan(&retainedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetainedAllocation{}, false, nil
	}
	if err != nil {
		return RetainedAllocation{}, false, fmt.Errorf("write retention lease of %q: %w", allocationKey, err)
	}
	return retainedFrom(allocationKey, retainedAt, expiresAt), true, nil
}

// retainedFrom describes a retained row from its stored retention columns.
func retainedFrom(allocationKey string, retainedAt time.Time, expiresAt pgtype.Timestamptz) RetainedAllocation {
	r := RetainedAllocation{AllocationKey: allocationKey, RetainedAt: retainedAt}
	switch {
	case !expiresAt.Valid:
		r.Lease = LeaseUnevaluated
	case expiresAt.InfinityModifier != pgtype.Finite:
		r.Lease = LeaseNone
	default:
		r.Lease = LeaseFinite
		r.ExpiresAt = expiresAt.Time
	}
	return r
}

// RetainedAtTime is RetainedAt as the IPAllocation reports it.
func (r RetainedAllocation) RetainedAtTime() *metav1.Time {
	t := metav1.NewTime(r.RetainedAt)
	return &t
}

// ExpiresAtTime is the deadline as the IPAllocation reports it, nil when no
// finite lease is known.
func (r RetainedAllocation) ExpiresAtTime() *metav1.Time {
	if r.Lease != LeaseFinite {
		return nil
	}
	t := metav1.NewTime(r.ExpiresAt)
	return &t
}

// RetainedCondition is the Retained condition an IPAllocation in this state
// reports. The message says what will happen to the address without naming
// the sweeper's mode: the condition outlives a change of mode, and which mode
// a deployment runs is the operator's to say, in its configuration.
func (r RetainedAllocation) RetainedCondition() metav1.Condition {
	cond := metav1.Condition{
		Type:               ipamv1alpha1.AllocationConditionRetained,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(r.RetainedAt),
	}
	switch r.Lease {
	case LeaseFinite:
		cond.Reason = ipamv1alpha1.RetainedReasonLeaseActive
		cond.Message = fmt.Sprintf("retained when its claim was deleted; its retention lease runs out at %s, "+
			"when the address becomes due for release", r.ExpiresAt.UTC().Format(time.RFC3339))
	case LeaseNone:
		cond.Reason = ipamv1alpha1.RetainedReasonNoLease
		cond.Message = "retained when its claim was deleted; neither its class nor its pool sets a retention lease, " +
			"so it is held until the IPAllocation is deleted or a claim of the same name takes it back"
	default:
		cond.Reason = ipamv1alpha1.RetainedReasonLeasePending
		cond.Message = "retained when its claim was deleted; its retention lease could not be evaluated then, " +
			"and the retention sweeper evaluates it on its next pass"
	}
	return cond
}

// writeRetentionStatus publishes r on the IPAllocation at r.AllocationKey, in
// the transaction that changed the row. An allocation row with no object over
// it still holds its address; there is just nothing to publish.
func writeRetentionStatus(ctx context.Context, tx pgx.Tx, r RetainedAllocation) error {
	var data []byte
	err := tx.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, r.AllocationKey).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read IPAllocation %q: %w", r.AllocationKey, err)
	}
	var alloc ipamv1alpha1.IPAllocation
	if err := json.Unmarshal(data, &alloc); err != nil {
		return fmt.Errorf("decode IPAllocation %q: %w", r.AllocationKey, err)
	}
	alloc.Status.RetainedAt = r.RetainedAtTime()
	alloc.Status.ExpiresAt = r.ExpiresAtTime()
	meta.SetStatusCondition(&alloc.Status.Conditions, r.RetainedCondition())
	updated, err := json.Marshal(&alloc)
	if err != nil {
		return fmt.Errorf("encode IPAllocation %q: %w", r.AllocationKey, err)
	}
	if _, err := updateObject(ctx, tx, r.AllocationKey, updated); err != nil {
		return fmt.Errorf("publish retention of IPAllocation %q: %w", r.AllocationKey, err)
	}
	return nil
}

// inSavepoint runs fn under a savepoint of tx and rolls it back if fn fails,
// so a failure undoes only fn's writes and leaves tx usable.
func inSavepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("open savepoint: %w", err)
	}
	if err := fn(sp); err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

// evaluateRetained fixes the lease of a row retention has just unbound. A class
// that cannot be read leaves the lease unevaluated for the sweeper, and is
// logged rather than returned: the claim is being deleted either way.
func evaluateRetained(ctx context.Context, tx pgx.Tx, pool *ipamv1alpha1.IPPool, row retainedRow) (RetainedAllocation, error) {
	lease, ok, err := leaseFor(ctx, tx, pool, row.poolKey, row.className, row.classProject)
	if !ok {
		klog.ErrorS(err, "Could not evaluate the retention lease; the sweeper will retry",
			"allocation", row.allocationKey, "class", row.className)
		return RetainedAllocation{AllocationKey: row.allocationKey, RetainedAt: row.retainedAt, Lease: LeaseUnevaluated}, nil
	}
	r, changed, err := writeLease(ctx, tx, row.allocationKey, lease)
	if err != nil {
		return RetainedAllocation{}, err
	}
	if !changed {
		return RetainedAllocation{AllocationKey: row.allocationKey, RetainedAt: row.retainedAt, Lease: LeaseUnevaluated}, nil
	}
	return r, nil
}

// retainedRow is a retained claim allocation row as the lease paths read it.
type retainedRow struct {
	allocationKey string
	poolKey       string
	className     string
	classProject  *string
	retainedAt    time.Time
}
