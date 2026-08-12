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
// Two root pools over one range hand the same address to unrelated claims, and
// nothing downstream notices: address uniqueness is enforced per pool, by an
// exclusion constraint keyed on pool_key, so rows in different pools never
// conflict however much their ranges do.
//
// The check is deliberately narrow:
//
//   - Root pools only. A child's range is carved from its parent and is nested
//     by construction, so checking it would reject every child.
//   - One tenant only. Private space is tenant-scoped and overlap-safe across
//     tenants; two tenants holding 10.0.0.0/8 are separate address spaces.
//   - Create time only. It reads no stored row and rewrites nothing, so a
//     tenant that already holds an overlapping pair keeps it.
func (r *AllocatingIPPoolREST) validateNoRootOverlap(ctx context.Context, pool *ipam.IPPool, id tenant.Identity) error {
	if pool.Spec.ParentPoolRef != nil || pool.Spec.CIDR == "" {
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
		if allocation.CIDRsOverlap(*candidate, other.cidr) {
			return overlapConflict(pool.Name, pool.Spec.CIDR, other)
		}
	}
	return nil
}

// overlapConflict builds the refusal for a root pool that collides with
// another.
//
// 409 rather than 422: the request is well formed and the refusal is about the
// state of the world, which is also what makes it actionable — deleting or
// re-ranging the named pool makes the same request succeed. It names the other
// pool and both ranges, because "Conflict" with no subject sends the operator
// to the database to find out what they collided with.
func overlapConflict(name, cidr string, other rootPool) error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ippools"},
		name,
		fmt.Errorf("spec.cidr %s overlaps root IPPool %q (%s) in this project; "+
			"two root pools over one range hand the same address to unrelated claims, "+
			"because address uniqueness is enforced within a pool and not across pools. "+
			"Narrow one of the ranges, or carve this pool from %q by setting spec.parentPoolRef",
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
// an overlap with itself rather than AlreadyExists, which is the worse message
// for the far more common mistake.
//
// The parentPoolRef test is IS NULL, which covers a key that is absent as well
// as one explicitly null — a root pool omits the field entirely, since it is
// omitempty on the wire. The overlap comparison itself stays in Go: no index
// could serve an inet predicate here, so it is a sequential scan either way,
// and one implementation of "do these ranges overlap" cannot disagree with
// itself.
func (r *AllocatingIPPoolREST) tenantRootPoolCIDRs(ctx context.Context, id tenant.Identity, excludeName string) ([]rootPool, error) {
	// Derived from ResourceKey rather than formatted here, so it stays in step
	// with the keys the allocator locks and the registry writes. The prefix
	// selects this tenant's pools on its own, so the query does not filter on
	// the kind column.
	prefix := id.ResourceKey("ippools", "")

	rows, err := r.db.Query(ctx,
		`SELECT name, ipam_data_to_jsonb(data)->'spec'->>'cidr'
		   FROM ipam_objects
		  WHERE key LIKE $1 || '%'
		    AND name <> $2
		    AND ipam_data_to_jsonb(data)->'spec'->'parentPoolRef' IS NULL
		    AND ipam_data_to_jsonb(data)->'spec'->>'cidr' IS NOT NULL`,
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
			// A stored pool whose CIDR does not parse cannot be compared
			// against, and it cannot be allocating anything either. Skipping it
			// keeps one malformed row from refusing every subsequent create.
			continue
		}
		out = append(out, rootPool{name: name, cidr: *ipnet})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate root pools: %w", err)
	}
	return out, nil
}
