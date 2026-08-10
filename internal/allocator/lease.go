package allocator

// Retention leases.
//
// A retained address is capacity nobody else can use. That is the price of an
// address surviving a redeploy, and on a finite public range it is the cost that
// matters — so retention carries a lease rather than lasting forever.
//
// This file resolves how long a given allocation may stay retained. Sweeping the
// expired ones is a separate concern; see the sweeper, which consumes this.

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// EffectiveLease resolves how long an allocation of a class, drawn from a pool,
// may stay retained after its claim is deleted.
//
// Two sources, and the rule between them is `min`:
//
//   - The class states the lease, because retention is already a class-level
//     policy and the class is what a consumer names.
//   - The pool caps it, because **the pool is the thing that runs out**. A class
//     is a policy that would like to hold addresses; a pool is the finite range
//     they come out of, and a scarce range must be able to refuse a generous
//     class rather than negotiate with it.
//
// A nil result means no expiry, and that is what *both unset* produces. The
// default is deliberately off: a lease that defaulted to on would begin
// reclaiming addresses in existing deployments that nobody asked to have
// reclaimed, which is the largest possible instance of the failure this service
// has repeatedly produced in smaller ones.
//
// Note what is absent: a claim cannot extend its own lease. A claim may override
// reclaimPolicy, because whether an address is held at all is the holder's
// business; how long it may be held against everyone else's capacity is not.
// Retention a holder can extend indefinitely is not a lease.
func EffectiveLease(class *ipamv1alpha1.IPClass, pool *ipamv1alpha1.IPPool) *time.Duration {
	var classLease, poolCap *time.Duration
	if class != nil && class.Spec.RetentionLease != nil {
		d := class.Spec.RetentionLease.Duration
		classLease = &d
	}
	if pool != nil && pool.Spec.MaxRetentionLease != nil {
		d := pool.Spec.MaxRetentionLease.Duration
		poolCap = &d
	}

	switch {
	case classLease == nil && poolCap == nil:
		// Nobody asked for expiry. Retained means retained.
		return nil
	case classLease == nil:
		// The pool caps retention even for a class that states none. This is the
		// case that makes the cap useful: an operator hardening a scarce range
		// does not have to find and edit every class that draws from it.
		return poolCap
	case poolCap == nil:
		return classLease
	case *poolCap < *classLease:
		return poolCap
	default:
		return classLease
	}
}

// LeaseExpiry returns the instant a retained allocation becomes eligible for
// release, or false when it never does.
//
// retainedAt is when the allocation lost its claim — not when it was allocated.
// The distinction is the whole point of the column: an address allocated a year
// ago and retained yesterday has a full lease ahead of it, and measuring from
// allocation would expire it on the first sweep. That is the failure this
// signature exists to make impossible to write by accident, which is why it
// takes the instant rather than the allocation.
func LeaseExpiry(retainedAt time.Time, lease *time.Duration) (time.Time, bool) {
	if lease == nil || retainedAt.IsZero() {
		return time.Time{}, false
	}
	return retainedAt.Add(*lease), true
}

// ValidateLease checks a retention lease field wherever one appears.
//
// A zero duration is rejected rather than treated as "expire immediately". An
// operator who writes `retentionLease: 0s` has almost certainly written a
// placeholder, and reading it as "release the moment the claim goes" turns
// Retain into Delete silently — the two policies would become
// indistinguishable in effect while remaining distinct in the API. Unset is how
// you say "no expiry"; there is no way to say "expire instantly" because
// reclaimPolicy Delete already says it.
func ValidateLease(lease *metav1.Duration, fieldName string) error {
	if lease == nil {
		return nil
	}
	if lease.Duration <= 0 {
		return fmt.Errorf(
			"%s must be a positive duration; leave it unset for no expiry, and use reclaimPolicy Delete to release immediately",
			fieldName)
	}
	return nil
}
