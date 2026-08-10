package ippool

import (
	"context"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// validateClassOffers checks a pool's spec.classNames against the classes it
// names, in a read-only transaction.
//
// It lives here rather than in the strategy because every rule needs the class
// catalog, and a strategy has no store to read. It runs before the creation
// transaction so a rejected pool leaves nothing behind.
func (r *AllocatingIPPoolREST) validateClassOffers(ctx context.Context, pool *ipam.IPPool, ipFamily string) error {
	if len(pool.Spec.ClassNames) == 0 {
		return nil
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin class offer validation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	classes := make([]*ipamv1alpha1.IPClass, 0, len(pool.Spec.ClassNames))
	var errs field.ErrorList
	path := field.NewPath("spec", "classNames")

	for i, name := range pool.Spec.ClassNames {
		class, lerr := allocator.LoadClass(ctx, tx, name)
		if lerr != nil {
			if errors.Is(lerr, allocator.ErrClassNotFound) {
				errs = append(errs, field.Invalid(path.Index(i), name,
					fmt.Sprintf("IPClass %q does not exist", name)))
				continue
			}
			return fmt.Errorf("load class %q: %w", name, lerr)
		}
		classes = append(classes, class)
		errs = append(errs, validateOfferedClass(pool, class, ipFamily, path.Index(i))...)
		errs = append(errs, validateClassConsent(tenant.FromContext(ctx), class, path.Index(i))...)
	}

	errs = append(errs, validateUniqueWithinAgreement(classes, path)...)

	if len(errs) > 0 {
		return apierrors.NewInvalid(ipam.Kind("IPPool"), pool.Name, errs)
	}
	return nil
}

// validateOfferedClass checks one class a pool offers itself to.
func validateOfferedClass(pool *ipam.IPPool, class *ipamv1alpha1.IPClass, ipFamily string, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	if ipFamily != "" && string(class.Spec.IPFamily) != ipFamily {
		errs = append(errs, field.Invalid(path, class.Name,
			fmt.Sprintf("IPClass %q hands out %s but this pool holds %s",
				class.Name, class.Spec.IPFamily, ipFamily)))
	}

	// Only the top of a chain draws from the pools that offer it. A class with a
	// parent gets its space from that parent's provisioned pool, so offering a
	// pool to it does nothing — and reads as capacity that is not there.
	if class.Spec.ParentClassName != "" {
		errs = append(errs, field.Invalid(path, class.Name,
			fmt.Sprintf("IPClass %q carves from %q, so it draws from the pool that class provisions rather than from one offering it directly",
				class.Name, class.Spec.ParentClassName)))
	}

	// Deliberately NOT checked: whether the class names the roles the pool
	// declares in spec.scope.
	//
	// A pool's scope does two unrelated jobs, and an earlier version of this
	// function assumed only one of them. It is an *eligibility filter* — "this
	// pool serves us-central-1, and a claim from elsewhere must not reach it" —
	// and, on a cascade-provisioned pool, the *identity* the pool was built for.
	// Only the second has anything to do with the class's role lists.
	//
	// Eligibility is matched against the claim's own scope, not against any
	// projection onto the class (see allocator.poolServesScope). The network
	// layer supplies `location` on every claim it makes, so a leaf class that
	// names no location at all still reaches a located pool. That is the whole
	// point of the field: a claim made in one location reaches that location's
	// space without anyone naming it.
	//
	// Requiring the class to name the role rejected the design's own examples —
	// public-unicast-ipv4 over per-location public pools, tenant-endpoint-ipv4
	// over per-location /20s, and every fabric class over its per-site pools.
	// The tempting fix, adding `location` to uniqueWithin, is worse than the
	// check: it would assert that two allocations may hold the same address if
	// they differ in location, widening exactly the uniqueness key the design
	// warns against widening.
	return errs
}

// validateClassConsent rejects a pool offering itself to a class that has not
// consented to be backed by the pool's project.
//
// `spec.classNames` is the pool volunteering, written by the pool's owner.
// Discovery searches every project now that the platform is one, so a volunteer
// alone would mean any tenant who can create an IPPool could list a popular
// class name on it and start receiving other tenants' claims — learning that
// each claim happened, choosing the address it received, and holding the range
// it came from. The class is the thing being consumed, so the class consents:
// IPClass.spec.backingProjects.
//
// This is the friendly half of a rule enforced twice. The authoritative check is
// in allocator.DiscoverPool, at read time, because consent is revocable and a
// write-time snapshot cannot express that. What this adds is an error at the
// moment an operator writes the pool, naming the field to edit, instead of a
// pool that is accepted and then silently serves nobody.
func validateClassConsent(id tenant.Identity, class *ipamv1alpha1.IPClass, path *field.Path) field.ErrorList {
	// The platform project may back any class and need not be listed. Every
	// class written before backingProjects existed has an empty list and is
	// backed by platform-authored pools, so this is what makes the field
	// additive rather than a flag day.
	if id.IsPlatform() {
		return nil
	}
	if slices.Contains(class.Spec.BackingProjects, id.Name) {
		return nil
	}
	// The message names the remedy and does not distinguish "this class does not
	// list you" from "no such class" any further than the caller already knows —
	// they named the class, so its existence is not news to them.
	return field.ErrorList{field.Invalid(path, class.Name,
		fmt.Sprintf("IPClass %q does not list this project in spec.backingProjects, so a pool here may not back it; "+
			"an operator must add the project to the class before this pool can serve its claims", class.Name))}
}

// validateUniqueWithinAgreement rejects a pool offered to two classes whose
// uniqueWithin differs.
//
// Non-overlap is enforced per address space: the exclusion constraint compares
// allocations sharing a (pool_key, scope_digest), and the allocator's search
// reads only the allocations in the claim's own space. Both are correct given
// that every claim drawing from a pool computes its digest the same way.
//
// Two classes with different uniqueWithin break that premise. A pool serving
// both a `uniqueWithin: []` class and a `uniqueWithin: [network]` class holds
// the first class's allocations under the empty digest and the second's under a
// per-network one, so neither the constraint nor the search ever compares them
// — and the second class hands out addresses the first is already using. It
// fails as two holders of one address with nothing logged, which is the worst
// shape a bug in this service can take.
//
// The schema cannot express this rule, so it is enforced here.
func validateUniqueWithinAgreement(classes []*ipamv1alpha1.IPClass, path *field.Path) field.ErrorList {
	if len(classes) < 2 {
		return nil
	}
	first := classes[0]
	firstKey := uniqueWithinKey(first)
	for _, class := range classes[1:] {
		if uniqueWithinKey(class) != firstKey {
			return field.ErrorList{field.Invalid(path, classNames(classes),
				fmt.Sprintf("IPClass %q is unique within %v but %q is unique within %v; a pool offered to both would let one class hand out addresses the other already holds, because non-overlap is only enforced within one address space",
					first.Name, first.Spec.UniqueWithin, class.Name, class.Spec.UniqueWithin))}
		}
	}
	return nil
}

// uniqueWithinKey reduces a class's UniqueWithin to a comparable value, so that
// ordering differences do not read as disagreement — the roles are a set, and
// [network, location] is the same address space as [location, network].
//
// It goes through RoleSetKey rather than through the scope canonicalisation.
// The question here is about a set of role NAMES: there is no tenant and there
// are no references, and answering it with a scope digest meant handing
// Canonical an empty tenant and a map of empty refs to mean "not applicable" —
// which is the same call shape as a platform caller's real scope.
func uniqueWithinKey(class *ipamv1alpha1.IPClass) string {
	return scope.RoleSetKey(class.Spec.UniqueWithin)
}

func classNames(classes []*ipamv1alpha1.IPClass) []string {
	names := make([]string, len(classes))
	for i, c := range classes {
		names[i] = c.Name
	}
	return names
}
