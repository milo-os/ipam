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
// pool the same tenant already owns.
//
// Two root pools over one range hand the same address to unrelated claims, and
// nothing downstream catches it: an exclusion constraint keyed on pool_key
// enforces uniqueness within a pool, so rows in different pools never conflict,
// however much their ranges overlap.
//
// The check is narrow by design:
//
//   - Root pools only. A child carves its range from its parent, so it always
//     nests, and checking it would reject every child.
//   - One tenant only. Private space is tenant-scoped, so two tenants both
//     holding 10.0.0.0/8 are separate address spaces.
//   - Create only. Nothing rewrites stored pools, so a tenant that already
//     holds an overlapping pair keeps it.
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
// It returns 409 rather than 422 because the request is well formed and the
// state of the world is what refuses it. That also makes the refusal
// actionable: deleting or re-ranging the named pool lets the same request
// succeed. The message names the other pool and both ranges, because a bare
// "Conflict" sends the operator to the database to find out what they hit.
func overlapConflict(name, cidr string, other rootPool) error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: v1alpha1.GroupName, Resource: "ippools"},
		name,
		fmt.Errorf("spec.cidr %s overlaps root IPPool %q (%s) in this project; "+
			"two root pools over one range hand the same address to unrelated claims, "+
			"because IPAM enforces address uniqueness within a pool, not across pools. "+
			"Narrow one of the ranges, or carve this pool from %q by setting spec.parentPoolRef",
			cidr, other.name, other.cidr.String(), other.name),
	)
}

// rootPool is one existing root pool's identity and range.
type rootPool struct {
	name string
	cidr net.IPNet
}

// tenantRootPoolCIDRs returns the range of every root pool this tenant owns,
// except the name being created.
//
// Excluding that name keeps a repeated create reporting AlreadyExists rather
// than an overlap with itself, which is the better message for the more common
// mistake.
//
// The query tests parentPoolRef IS NULL, which matches an absent key as well as
// an explicit null; a root pool omits the field, since it is omitempty on the
// wire. Overlap comparison stays in Go: no index serves an inet predicate here,
// so the scan is sequential either way, and a single implementation of "do
// these ranges overlap" cannot disagree with itself.
func (r *AllocatingIPPoolREST) tenantRootPoolCIDRs(ctx context.Context, id tenant.Identity, excludeName string) ([]rootPool, error) {
	// Build the prefix from ResourceKey rather than formatting it here, so it
	// stays in step with the keys the allocator locks and the registry writes.
	// The prefix already selects this tenant's pools, so the query does not
	// filter on the kind column.
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
			// A stored CIDR that does not parse cannot be compared against, and
			// cannot be allocating anything either. Skipping it keeps one
			// malformed row from refusing every later create.
			continue
		}
		out = append(out, rootPool{name: name, cidr: *ipnet})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate root pools: %w", err)
	}
	return out, nil
}
