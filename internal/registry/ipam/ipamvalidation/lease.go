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

// RetentionLeaseChanged reports whether an update sets, clears or changes a
// lease. An update that leaves the lease alone is not validated against the
// bounds: an object stored before the bounds existed must stay editable, and
// a label change is not a decision about its lease.
func RetentionLeaseChanged(updated, old *metav1.Duration) bool {
	if updated == nil || old == nil {
		return updated != old
	}
	return updated.Duration != old.Duration
}
