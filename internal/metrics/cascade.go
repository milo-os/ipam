package metrics

import (
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

var (
	// CascadeOutcomes counts pool-provisioning attempts by what happened.
	//
	// The label that matters is "lost". A claim into a scope nothing has used
	// yet races every other claim into that scope, and exactly one wins; the
	// rest read the winner's pool. A healthy service shows a burst of losses
	// alongside each provision and nothing in between. A sustained loss rate
	// with no provisions means requests are contending on an identity row whose
	// winner keeps aborting, which is the failure mode this metric exists to
	// make visible — from the outside it looks only like slightly slow claims.
	//
	// class: the provisioning class's name — deliberately not the scope, which
	// is unbounded. outcome: "provisioned" | "lost".
	CascadeOutcomes = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "cascade_outcomes_total",
			Help:           "Pool provisioning attempts by outcome",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"class", "outcome"},
	)

	// Reclaims counts allocations disposed of at claim release, by the policy
	// that decided their fate.
	//
	// Retain is the one to watch. A retained address is capacity nobody else can
	// use, and on a finite public range that is the cost that matters — so the
	// ratio of Retain to Delete is the leading indicator of a range filling with
	// addresses no live workload holds, well before utilization shows it.
	Reclaims = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "reclaims_total",
			Help:           "Allocations disposed of at release, by reclaim policy",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"policy"},
	)
)

func init() {
	legacyregistry.MustRegister(CascadeOutcomes, Reclaims)
}

// RecordCascadeOutcome records the result of one pool-provisioning attempt.
// outcome is "provisioned" or "lost".
func RecordCascadeOutcome(className, outcome string) {
	CascadeOutcomes.WithLabelValues(className, outcome).Inc()
}

// RecordReclaim records one allocation's disposition at release. policy is
// "Delete" or "Retain".
func RecordReclaim(policy string) {
	Reclaims.WithLabelValues(policy).Inc()
}

var (
	// LeaseSweepCandidates counts retained allocations a sweep pass examined.
	//
	// This is the metric that makes the others readable. A pass that released
	// nothing because nothing was due, and a pass that released nothing because
	// its query silently matched no rows, are both "zero released" — and they
	// mean opposite things. Alert on candidates being flat at zero while
	// retained allocations exist, not on expirations being zero.
	LeaseSweepCandidates = metrics.NewCounter(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "lease_sweep_candidates_total",
			Help:           "Retained allocations examined by the lease sweeper",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// LeaseSweepNoLease counts examined allocations that had no effective lease
	// and are therefore held indefinitely.
	//
	// On a scarce range this is the number that explains why capacity is not
	// coming back: addresses are retained, the sweeper is healthy, and no policy
	// says they may be released. It also surfaces the pre-lease rows migration
	// 004 deliberately left exempt.
	LeaseSweepNoLease = metrics.NewCounter(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "lease_sweep_no_lease_total",
			Help:           "Retained allocations examined that carry no effective lease",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// LeaseSweepErrors counts pools a sweep pass failed on. Individual failures
	// are skipped rather than aborting the pass, so without this a partially
	// broken sweep looks like a healthy one.
	LeaseSweepErrors = metrics.NewCounter(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "lease_sweep_errors_total",
			Help:           "Pools the lease sweeper failed to process",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// LeaseExpirations counts allocations moved through the expiry phases.
	// outcome: "marked" | "released".
	LeaseExpirations = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "ipam",
			Name:           "lease_expirations_total",
			Help:           "Retained allocations marked expiring or released by the lease sweeper",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"class", "outcome"},
	)
)

func init() {
	legacyregistry.MustRegister(
		LeaseSweepCandidates, LeaseSweepNoLease, LeaseSweepErrors, LeaseExpirations)
}

// RecordLeaseSweep records the totals from one sweep pass.
func RecordLeaseSweep(candidates, marked, released, noLease int) {
	LeaseSweepCandidates.Add(float64(candidates))
	LeaseSweepNoLease.Add(float64(noLease))
}

// RecordLeaseExpiration records one allocation's transition. outcome is
// "marked" or "released".
func RecordLeaseExpiration(className, outcome string) {
	LeaseExpirations.WithLabelValues(className, outcome).Inc()
}

// RecordLeaseSweepError records a pool the sweeper could not process.
func RecordLeaseSweepError() { LeaseSweepErrors.Inc() }

// ReclaimedRetained counts retained allocations recovered by a replacement
// claim — an address that survived a redeploy and came back to its holder.
//
// It is the counterpart to reclaims_total's absence being meaningful: if
// retention is configured and this stays at zero while allocations are being
// retained, replacements are not finding their addresses and the feature is
// costing capacity without delivering the property it exists for.
var ReclaimedRetained = metrics.NewCounterVec(
	&metrics.CounterOpts{
		Namespace:      "ipam",
		Name:           "reclaimed_retained_total",
		Help:           "Retained allocations recovered by a replacement claim",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"class"},
)

func init() { legacyregistry.MustRegister(ReclaimedRetained) }

// RecordReclaimRetained records one recovered address.
func RecordReclaimRetained(className string) {
	ReclaimedRetained.WithLabelValues(className).Inc()
}

// ProvisionedPoolCapacitySkipped counts capacity updates deliberately not
// exported as per-pool gauges because the pool was cascade-provisioned.
//
// It exists so the exclusion is visible rather than silent. Without it, an
// operator who cannot find a tenant pool in `ipam_pool_utilization_ratio` has no
// way to tell "the exclusion is working as designed" from "the metric is
// broken", and the first thing anyone does with a missing series is go looking
// for a bug. A steadily climbing counter here says the omission is deliberate
// and how much of it there is.
//
// Deliberately unlabelled: labelling it by pool would reintroduce exactly the
// unbounded cardinality it exists to record avoiding.
var ProvisionedPoolCapacitySkipped = metrics.NewCounter(
	&metrics.CounterOpts{
		Namespace:      "ipam",
		Name:           "provisioned_pool_capacity_skipped_total",
		Help:           "Capacity updates for cascade-provisioned pools not exported as per-pool gauges (bounded-cardinality policy)",
		StabilityLevel: metrics.ALPHA,
	},
)

func init() { legacyregistry.MustRegister(ProvisionedPoolCapacitySkipped) }

// RecordProvisionedPoolCapacitySkipped records one such omission.
func RecordProvisionedPoolCapacitySkipped() { ProvisionedPoolCapacitySkipped.Inc() }

// ChangelogCompaction counts compaction passes by outcome.
//
// outcome: "compacted" | "skipped_locked" | "error".
//
// It exists because compaction is a background task with no caller to fail.
// Before this, a pass that deadlocked rolled back, logged, and left the
// changelog uncompacted — and the only visible consequence was the changelog
// growing, which looks like traffic rather than a fault. Two replicas were
// deadlocking against each other on every pass and nothing reported it.
//
// "skipped_locked" is the healthy steady state on a multi-replica deployment,
// not a problem: exactly one replica compacts per pass and the others record a
// skip. An alert should fire on "error", and on "compacted" going to zero
// across ALL replicas — that is compaction having stopped, which is what the
// watch horizon depends on.
var ChangelogCompaction = metrics.NewCounterVec(
	&metrics.CounterOpts{
		Namespace:      "ipam",
		Name:           "changelog_compaction_total",
		Help:           "Changelog compaction passes by outcome",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"outcome"},
)

// ChangelogRowsCompacted counts rows removed by compaction.
var ChangelogRowsCompacted = metrics.NewCounter(
	&metrics.CounterOpts{
		Namespace:      "ipam",
		Name:           "changelog_rows_compacted_total",
		Help:           "Changelog rows removed by compaction",
		StabilityLevel: metrics.ALPHA,
	},
)

func init() {
	legacyregistry.MustRegister(ChangelogCompaction, ChangelogRowsCompacted)
}

// RecordChangelogCompaction records one compaction pass.
func RecordChangelogCompaction(outcome string, rows int64) {
	ChangelogCompaction.WithLabelValues(outcome).Inc()
	if rows > 0 {
		ChangelogRowsCompacted.Add(float64(rows))
	}
}
