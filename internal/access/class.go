package access

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tracing"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ErrClassDenied is returned when a caller may not consume the class it named.
//
// It is a sentinel rather than a message because the reason must not always
// reach the caller: "that class exists but you may not use it" and "no such
// class" are the same information leak, and the class catalog is
// platform-internal. Callers map it to 403, having already decided whether the
// class name in the request is something the caller told us or something we
// would be telling them.
var ErrClassDenied = errors.New("ipam: class not available to this caller")

// ClassAccessChecker answers whether the caller may consume a class.
//
// It is deliberately shaped like PoolAccessChecker, because it replaces it. A
// claim used to name a pool, so the pool was the thing to authorize. Under the
// class model a claim names a class and the allocator picks the pool, which
// makes the class name the only authorization boundary that exists.
type ClassAccessChecker interface {
	// CanUseClass runs a "use" authorization check against the named IPClass.
	CanUseClass(ctx context.Context, className string) (bool, error)
}

type sarClassChecker struct {
	authz authorizer.Authorizer
}

// NewClassAccessChecker wraps an authorizer so the registry layer can ask "may
// the caller use this class?" without extracting UserInfo itself.
func NewClassAccessChecker(authz authorizer.Authorizer) ClassAccessChecker {
	return &sarClassChecker{authz: authz}
}

// CanUseClass runs a "use" verb check against ipclasses/<name>.
//
// IPClass is cluster-scoped, so there is no namespace on the attributes — a
// grant is written against the class by name, which is what makes the catalog
// governable: an operator can hand `public-unicast-ipv4` to one tenant without
// handing over `tenant-endpoint-ipv4`.
//
// Returns false with no error when the context carries no user. That is a
// denial rather than a system failure, and treating it as an error would turn a
// fail-closed decision into a 500.
func (c *sarClassChecker) CanUseClass(ctx context.Context, className string) (bool, error) {
	user, ok := request.UserFrom(ctx)
	if !ok {
		return false, nil
	}
	decision, _, err := c.authz.Authorize(ctx, authorizer.AttributesRecord{
		User:            user,
		Verb:            "use",
		APIGroup:        "ipam.miloapis.com",
		Resource:        "ipclasses",
		Name:            className,
		ResourceRequest: true,
	})
	return decision == authorizer.DecisionAllow, err
}

// AuthorizeClassConsumption decides whether a caller may claim an address of a
// class. It is the boundary the design calls for: "once consumers name classes
// instead of pools, the class name is the only authorization boundary left, and
// the check must fail closed."
//
// Three gates, in increasing cost:
//
//  1. **A container class is nobody's to claim.** A class with PoolPer exists to
//     provision pools for the classes below it; no claim ever names one. This is
//     a correctness rule before it is an authorization one — a claim bound
//     directly to a container class would take a whole subnet out of the space
//     the endpoints below it draw from — so it is refused for platform callers
//     too, and refused before anything else is considered.
//
//  2. **Visibility.** A `platform` class is operator-internal and is never
//     nameable by a tenant, whatever grants exist. This is the coarse gate an
//     operator sets once on the class, and it is checked before the SAR so a
//     misconfigured grant cannot open an internal class.
//
//  3. **The SAR.** For everything a tenant may reach in principle, the caller
//     must hold `use` on the class by name.
//
// Platform-scoped callers clear gates 2 and 3: they are the platform, they
// authored the catalog, and the identity extras that would carry a tenant are
// absent precisely because there is no tenant. Gate 1 still applies to them.
//
// isPlatform comes from the tenant identity rather than being re-derived here,
// so there is one answer to "is this the platform" in the service rather than
// two that can disagree.
func AuthorizeClassConsumption(ctx context.Context, class *ipamv1alpha1.IPClass, isPlatform bool, checker ClassAccessChecker) error {
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanAuthorizeClass)
	defer span.End()
	span.SetAttributes(attribute.String(tracing.AttrClassName, class.Name))

	deny := func(reason string) error {
		span.SetAttributes(
			attribute.String(tracing.AttrDecision, "denied"),
			attribute.String(tracing.AttrReason, reason),
		)
		return fmt.Errorf("%w: %s", ErrClassDenied, reason)
	}
	allow := func() error {
		span.SetAttributes(attribute.String(tracing.AttrDecision, "allowed"))
		return nil
	}

	// Gate 1 — container classes are not claimable by anyone.
	if len(class.Spec.PoolPer) > 0 {
		return deny("container_class")
	}

	if isPlatform {
		return allow()
	}

	// Gate 2 — visibility. An empty visibility is treated as platform-only,
	// which is the fail-closed default: a class an operator never marked for
	// consumers is not one, and the alternative would silently publish every
	// class written before this check existed.
	switch class.Spec.Visibility {
	case ipamv1alpha1.VisibilityConsumer, ipamv1alpha1.VisibilityShared:
	default:
		return deny("visibility")
	}

	// Gate 3 — the SAR. A nil checker is a denial, not a bypass. The apiserver
	// runs without an authorizer only in configurations where nothing should be
	// authorizing anything, and treating that as "allow" is exactly the failure
	// mode "fail closed" is written to prevent.
	if checker == nil {
		return deny("no_checker")
	}
	allowed, err := checker.CanUseClass(ctx, class.Name)
	if err != nil {
		return fmt.Errorf("authorize class %q: %w", class.Name, err)
	}
	if !allowed {
		return deny("sar_denied")
	}
	return allow()
}
