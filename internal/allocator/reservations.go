package allocator

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/klog/v2"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// reservationFrom converts a pool's declared reservation into the form the
// allocation library expands. A nil spec withholds nothing.
func reservationFrom(spec *ipamv1alpha1.ReservationSpec) allocation.Reservation {
	if spec == nil {
		return allocation.Reservation{}
	}
	return allocation.Reservation{
		UnitPrefixLength: int(spec.UnitPrefixLength),
		Leading:          int(spec.Leading),
		Trailing:         int(spec.Trailing),
	}
}

// materialiseReservations writes a pool's reserved edge positions as allocation
// rows of purpose 'Reservation'.
//
// The rows are the mechanism, not a record of one. Every reader that already
// separates a claim from space the pool holds sees a reservation the moment it
// is a row: the search filter (`purpose <> 'Claim'`, which blocks every address
// space), the overlap constraint, the running consumption total, the specific
// address check, and the trigger that lowers a search floor on release. No
// search path has to learn about reservations separately, and the default
// FirstFit search — which reads the pool a page at a time and never holds the
// whole set — could not consult a spec field even if it wanted to.
//
// Call it with the pool row locked. It is idempotent: a pool already holding
// reservation rows is left alone, so it is safe on every allocation and it
// brings pools provisioned before this existed into line on their next one.
func materialiseReservations(ctx context.Context, tx pgx.Tx, poolKey string, pool *ipamv1alpha1.IPPool, parents []net.IPNet) error {
	reservation := reservationFrom(pool.Spec.Reservations)
	if reservation.IsZero() {
		return nil
	}

	held, err := holdsReservations(ctx, tx, poolKey)
	if err != nil || held {
		return err
	}

	blocks, err := reservation.BlocksIn(parents)
	if err != nil {
		// A reservation the pool cannot express is an operator error, and it
		// must not read as a full or an unreserved pool. Failing every claim
		// against this pool is the loud outcome; handing out the gateway is
		// the quiet one.
		return fmt.Errorf("expand reservations for pool %q: %w", poolKey, err)
	}

	family := effectivePoolFamily(pool)
	className := ""
	if pool.Spec.ClassRef != nil {
		className = pool.Spec.ClassRef.Name
	}

	var consumed *big.Int
	for _, block := range blocks {
		// Read before the insert, so the overlap query does not return the row
		// this iteration is about to write.
		next, cerr := consumptionAfterAllocate(ctx, tx, poolKey, parents, block)
		if cerr != nil {
			return cerr
		}
		inserted, ierr := insertReservation(ctx, tx, poolKey, block, family, className)
		if ierr != nil {
			return ierr
		}
		if !inserted {
			// Something already holds this position. An address in service
			// cannot be recalled, so reserve what is still reservable rather
			// than refusing every future allocation from the pool.
			klog.InfoS("Reserved position is already allocated; leaving it in place",
				"pool", poolKey, "block", block.String())
			continue
		}
		consumed = next
		if werr := writeConsumed(ctx, tx, poolKey, consumed); werr != nil {
			return werr
		}
	}

	if consumed == nil {
		return nil
	}
	if err := persistPoolCapacity(ctx, tx, pool, poolKey, parents, consumed); err != nil {
		return fmt.Errorf("update pool capacity after reserving: %w", err)
	}
	publishPrefixUtilization(poolKey, family, parents, consumed)
	return nil
}

// holdsReservations reports whether the pool already carries its reserved
// positions as rows.
func holdsReservations(ctx context.Context, tx pgx.Tx, poolKey string) (bool, error) {
	defer metrics.ObserveQuery("holds_reservations", time.Now())
	var held bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM ipam_cidr_allocations
		      WHERE pool_key = $1 AND purpose = 'Reservation'
		 )`, poolKey).Scan(&held); err != nil {
		return false, fmt.Errorf("read reservations for %q: %w", poolKey, err)
	}
	return held, nil
}

// insertReservation records one reserved position, reporting whether the row
// was written. It is not written when something already overlaps the position.
//
// scope_digest is the empty address space: a reservation belongs to no tenant's
// space, so it is compared against every `uniqueWithin: []` claim in the pool
// rather than against one holder's.
func insertReservation(ctx context.Context, tx pgx.Tx, poolKey string, block net.IPNet, family, className string) (bool, error) {
	defer metrics.ObserveQuery("insert_reservation", time.Now())
	tag, err := tx.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family,
		     purpose, class_name, scope_digest, reclaim_policy, owner_project)
		 SELECT $1, $2::cidr, NULL, $3, $4, 'Reservation', $5, $6, 'Delete', $7
		  WHERE NOT EXISTS (
		     SELECT 1 FROM ipam_cidr_allocations
		      WHERE pool_key = $1 AND allocated_cidr && $2::cidr
		 )`,
		poolKey, block.String(), reservationAllocationKey(poolKey, block), family,
		className, scope.EmptyAddressSpaceDigest(), tenant.ProjectFromKey(poolKey),
	)
	if err != nil {
		return false, fmt.Errorf("reserve %s in %q: %w", block.String(), poolKey, err)
	}
	return tag.RowsAffected() == 1, nil
}

// reservationAllocationKey is the row's identity. Derived from the pool and the
// block so re-running produces the same key for the same position.
func reservationAllocationKey(poolKey string, block net.IPNet) string {
	return poolKey + "/reservations/" + block.String()
}
