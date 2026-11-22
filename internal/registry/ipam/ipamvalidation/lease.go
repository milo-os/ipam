package ipamvalidation

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// MaxRetentionLease is the longest retention lease a class or pool may state.
// A lease is a bound on holding an address nobody claims; one past a year is
// not a bound anyone means.
const MaxRetentionLease = 8760 * time.Hour

// RetentionLease checks a retention lease field wherever one appears.
//
// Zero is refused rather than read as "release immediately": an author who
// writes `0s` has almost certainly written a placeholder, and reading it that
// way would turn Retain into Delete while the two stayed distinct in the API.
// Unset is how a class or pool says "no expiry", and reclaimPolicy Delete is
// how it says "release at once".
func RetentionLease(lease *metav1.Duration, path *field.Path) field.ErrorList {
	if lease == nil {
		return nil
	}
	if lease.Duration <= 0 {
		return field.ErrorList{field.Invalid(path, lease.Duration.String(),
			"must be a positive duration; leave it unset for no expiry, and use reclaimPolicy Delete to release immediately")}
	}
	if lease.Duration > MaxRetentionLease {
		return field.ErrorList{field.Invalid(path, lease.Duration.String(),
			fmt.Sprintf("must be at most %s", MaxRetentionLease))}
	}
	return nil
}

// RetentionLeaseWarning is the warning for an object that sets a retention
// lease, or nil if it sets none.
//
// The lease is applied when a claim is deleted, and its deadline shown on the
// IPAllocation, but the sweeper runs in observe mode and releases nothing when
// it runs out yet. Saying so at the moment an author sets one is the
// difference between a lease they know is pending and one they believe is
// running.
func RetentionLeaseWarning(lease *metav1.Duration, path *field.Path) []string {
	if lease == nil {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s sets the deadline shown as a retained IPAllocation's status.expiresAt, but retention leases are not yet "+
			"enforced: a retained address is held until its IPAllocation is deleted, however long ago the lease ran out "+
			"(see https://github.com/milo-os/ipam/issues/107)",
		path)}
}

// RetentionLeaseChanged reports whether an update sets, clears or changes a
// lease. An update that leaves the lease alone is neither validated against
// the bounds nor warned about: an object stored before the bounds existed
// must stay editable, and a label change is not a decision about its lease.
func RetentionLeaseChanged(updated, old *metav1.Duration) bool {
	if updated == nil || old == nil {
		return updated != old
	}
	return updated.Duration != old.Duration
}
