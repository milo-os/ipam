package ippool

import (
	"context"
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// validateNoRootOverlap refuses a root pool whose range overlaps another root
// pool already owned by the same tenant.
//
// # What goes wrong without it
//
// Two root pools over one range hand the same address to unrelated claims, and
// nothing anywhere notices. Measured: `10.171.0.0/16` and `10.171.0.0/24` in one
// project, each offering a different class, gave out 10.171.0.0, .1 and .2
// twice over — deterministically, on the first claim from each pool, not as a
// race.
//
// The reason nothing catches it is worth stating precisely, because the
// database *does* carry a constraint for exactly this:
//
//	EXCLUDE USING gist (pool_key WITH =, scope_digest WITH =,
//	                    allocated_cidr inet_ops WITH &&)
//
// It leads with pool_key, so two rows in different pools can never conflict
// whatever their digest. And the colliding rows carry the *same* scope_digest —
// by the class model's own definition they are in one address space, so the
// core invariant is violated in the stored data. **The invariant is enforced per
// pool and stated per address space, and the defect lives entirely in the gap
// between those two sentences.**
//
// # Why the check is here rather than on the constraint
//
// Dropping pool_key from that EXCLUDE is the intuitive fix and it destroys the
// cascade. Measured against real data, it would reject 104 existing legitimate
// row pairs (87 PoolCarve/Claim, 9 PoolCarve/Reservation, 8 PoolCarve/PoolCarve)
// to catch 3 synthetic ones — because a child pool's allocations are *supposed*
// to sit inside the carve that created it. A fd00::/64 claim inside the
// fd00::/52 carve that made its pool is the cascade working, and it is
// indistinguishable from a collision unless the check knows about `purpose`.
//
// So this closes the operator-error path, which is the only way overlapping
// roots arise today: internal/allocator/cascade.go always sets ParentPoolRef, so
// the cascade only ever carves children and never mints a root. Real cross-pool
// enforcement is a separate, larger decision (#87 option B) and is not this.
//
// # Scope of the check, and what it deliberately does not do
//
//   - Root pools only. A child's range is carved out of its parent and is
//     therefore nested by construction; checking it would reject every child.
//   - One tenant only. Private space is tenant-scoped and overlap-safe across
//     tenants, and that case is already handled — tenancy has been part of the
//     digest since the address-space identity fix, so two tenants' allocations
//     differ by digest even at the same address.
//   - Existing pools are left alone. This is a create-time check and touches no
//     stored row, so a tenant that already has an overlapping pair keeps it. That
//     is deliberate: it stops the bleeding without a migration. It also means the
//     check does not make the property true of data written before it — see the
//     package docs and #87.
func (r *AllocatingIPPoolREST) validateNoRootOverlap(ctx context.Context, pool *ipam.IPPool, id tenant.Identity) error {
	if pool.Spec.ParentPoolRef != nil {
		return nil
	}
	if pool.Spec.CIDR == "" {
		return nil
	}
	_, candidate, err := net.ParseCIDR(pool.Spec.CIDR)
	if err != nil {
		return apierrors.NewBadRequest(fmt.Sprintf("parse spec.cidr %q: %v", pool.Spec.CIDR, err))
	}

	existing, err := r.tenantRootPoolCIDRs(ctx, id, pool.Name)
	if err != nil {
		return err
	}
	for _, other := range existing {
		if !allocation.CIDRsOverlap(*candidate, other.cidr) {
			continue
		}
		return overlapConflict(pool.Name, pool.Spec.CIDR, other)
	}
	return nil
}

// overlapConflict builds the refusal for a root pool that collides with another.
//
// 409 rather than 422: the request is well-formed and the refusal is about the
// state of the world, which is also what makes it actionable — deleting or
// re-ranging the named pool makes the same request succeed.
//
// It names the other pool and both ranges because "Conflict" with no subject
// sends the operator to the database to find out what they collided with.
func overlapConflict(name, cidr string, other rootPool) error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ippools"},
		name,
		fmt.Errorf("spec.cidr %s overlaps root IPPool %q (%s) in this project; "+
			"two root pools over one range hand the same address to unrelated "+
			"claims, and address uniqueness is enforced within a pool, not across "+
			"pools. Narrow one of the ranges, or carve this pool from %q by setting "+
			"spec.parentPoolRef instead",
			cidr, other.name, other.cidr.String(), other.name),
	)
}

// rootPool is one existing root pool's identity and range.
type rootPool struct {
	name string
	cidr net.IPNet
}

// tenantRootPoolCIDRs returns the ranges of every root pool this tenant already
// owns, excluding the name being created.
//
// Excluding the name matters: without it, re-creating an existing pool reports
// an overlap with itself rather than AlreadyExists, which is a worse message for
// the far more common mistake and would have masked the duplicate-create fix.
//
// The parentPoolRef test is `IS NULL`, which covers a key that is absent as well
// as one that is explicitly null — a root pool omits the field entirely, since
// it is omitempty on the wire.
//
// The overlap comparison is done in Go rather than with Postgres's `&&`
// operator, even though the column could be cast to inet. There is no index that
// could serve such a predicate, so it would be a sequential scan either way, and
// keeping the arithmetic in allocation.CIDRsOverlap means one implementation of
// "do these ranges overlap" rather than two that can disagree.
func (r *AllocatingIPPoolREST) tenantRootPoolCIDRs(ctx context.Context, id tenant.Identity, excludeName string) ([]rootPool, error) {
	// Every pool this tenant owns shares this key prefix. Deriving it from
	// ResourceKey rather than formatting it here keeps it in step with the keys
	// the allocator locks and the registry writes.
	prefix := id.ResourceKey("ippools", "")

	rows, err := r.db.Query(ctx,
		`SELECT name, (convert_from(data, 'UTF8')::jsonb)->'spec'->>'cidr'
		   FROM ipam_objects
		  WHERE kind = 'IPPool'
		    AND key LIKE $1 || '%'
		    AND name <> $2
		    AND (convert_from(data, 'UTF8')::jsonb)->'spec'->'parentPoolRef' IS NULL
		    AND (convert_from(data, 'UTF8')::jsonb)->'spec'->>'cidr' IS NOT NULL`,
		prefix, excludeName,
	)
	if err != nil {
		return nil, fmt.Errorf("list root pools for overlap check: %w", err)
	}
	defer rows.Close()

	var out []rootPool
	for rows.Next() {
		var name, cidrStr string
		if err := rows.Scan(&name, &cidrStr); err != nil {
			return nil, fmt.Errorf("scan root pool: %w", err)
		}
		_, ipnet, perr := net.ParseCIDR(cidrStr)
		if perr != nil {
			// A stored pool whose CIDR does not parse cannot be compared against.
			// Skipping it is right: refusing every subsequent pool create because
			// one existing row is malformed would turn a single bad object into a
			// service-wide outage, and the malformed pool cannot be allocating
			// anything either.
			continue
		}
		out = append(out, rootPool{name: name, cidr: *ipnet})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate root pools: %w", err)
	}
	return out, nil
}
