package access

// Namespace liveness (#86, closes #72).
//
// # What is being enforced, and what deliberately is not
//
// Decided 2026-08-09: ownership of a namespace is NOT IPAM's problem. RBAC
// already decides whether a caller may write into one, and IPAM does not need
// its own model of who owns what.
//
// What IPAM must not do is bind an address into a namespace nothing will ever
// collect. Namespace deletion IS the collector: when a namespace goes, the
// claims in it go, and the allocator releases their addresses. A claim admitted
// into a namespace that is already Terminating is an allocation with no
// collector — it survives, holds its address, and nothing names it afterwards.
//
// So the property is LIVENESS, not existence and not ownership. The check is
// correspondingly narrow: refuse when the namespace is Terminating.
//
// A lookup that returns NotFound answers #72 through the same path, without a
// separate policy: a namespace that does not exist has no collector either.
//
// # Why this is not an admission plugin
//
// It would be the natural home and it is the wrong one. IPAM installs admission
// plugins ONLY under --enable-quota, and the dev overlay sets
// ENABLE_QUOTA=false — so an admission-based check would be inert on every dev
// cluster, in every e2e suite and in every load run. That is #85 exactly, and
// it was measured after the fix had supposedly landed. This lives in the claim
// registry, which always runs.
//
// # Where the namespace is looked up
//
// NOT in the root cluster. A project-scoped claim's namespace lives in that
// project's control plane, so validating against the root would reject
// legitimate claims — which is why simply enabling upstream NamespaceLifecycle
// does not work here. The routing is the one milo's quota plugin already uses:
// a base rest.Config whose Host is rewritten to
// /apis/resourcemanager.miloapis.com/v1alpha1/projects/<id>/control-plane.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// NamespaceState is what the claim path needs to know about a namespace.
//
// Unknown is a distinct value rather than an error folded into Live, because
// "the namespace is fine" and "we could not tell" must not collapse. They lead
// to the same admission decision today (both proceed) and to different
// operator-facing messages, and if the decision ever diverges the distinction
// has to already exist.
type NamespaceState int

const (
	// NamespaceUnknown means the lookup did not produce an answer. The caller
	// proceeds — see NamespaceChecker's doc on failing open.
	NamespaceUnknown NamespaceState = iota
	// NamespaceLive means the namespace exists and is not terminating.
	NamespaceLive
	// NamespaceTerminating means deletion has begun. Nothing new may be bound
	// into it.
	NamespaceTerminating
	// NamespaceMissing means the namespace does not exist (#72).
	NamespaceMissing
)

func (s NamespaceState) String() string {
	switch s {
	case NamespaceLive:
		return "Live"
	case NamespaceTerminating:
		return "Terminating"
	case NamespaceMissing:
		return "Missing"
	default:
		return "Unknown"
	}
}

// NamespaceChecker answers whether a namespace can still collect what is bound
// into it.
//
// # Failing open is a deliberate decision, not an oversight
//
// A lookup that ERRORS returns NamespaceUnknown and the claim proceeds. Only a
// definitive Terminating or Missing refuses.
//
// The asymmetry is not close. Admitting a claim into a doomed namespace costs
// one orphaned allocation: recoverable, rare, and visible. Failing closed puts
// another service's availability in the hot path of every allocation, so a
// partial outage of the control plane becomes a total outage of addressing.
// IPAM exists to hand out addresses; it must not stop doing that because a
// namespace lookup timed out.
type NamespaceChecker interface {
	// State reports the namespace's liveness within a project. An error is
	// returned alongside NamespaceUnknown so the caller can log the cause; the
	// caller must not treat it as a refusal.
	State(ctx context.Context, project, namespace string) (NamespaceState, error)
}

// projectNamespaceChecker looks a namespace up in its project's control plane.
type projectNamespaceChecker struct {
	base *rest.Config

	mu      sync.Mutex
	clients map[string]kubernetes.Interface
	live    map[string]time.Time // (project, namespace) -> when Live expires
	ttl     time.Duration
	now     func() time.Time
}

var _ NamespaceChecker = (*projectNamespaceChecker)(nil)

// liveTTL is how long a Live answer is reused.
//
// # Only the positive answer is cached, and that direction is the safe one
//
// Caching Live briefly risks admitting a claim into a namespace that began
// terminating within the window — bounded by the TTL, and the same one orphaned
// allocation the fail-open decision already accepts.
//
// Caching a REFUSAL would be the unsafe direction: a namespace that was
// Terminating or Missing and is now fine would keep being refused for the
// window, so a real claim fails for a state that no longer holds. Refusals are
// therefore never cached and are always a fresh lookup.
//
// The cache exists because this is the claim hot path. #98 measured a latency
// gate breached by ~20 extra database round trips per LIST; a control-plane
// HTTP call per claim create is a larger cost against a tighter budget.
const liveTTL = 10 * time.Second

// NewNamespaceChecker builds a checker over a base config, or returns nil when
// there is none.
//
// A nil checker DISABLES the check rather than denying every claim, which is
// the opposite of ClassAccessChecker's nil behaviour and is deliberate: the
// class check is an authorization boundary and must fail closed, while this is
// a liveness hint and must fail open. Wiring neither leaves IPAM behaving as it
// did before #86.
func NewNamespaceChecker(base *rest.Config) NamespaceChecker {
	if base == nil {
		return nil
	}
	return &projectNamespaceChecker{
		base:    rest.CopyConfig(base),
		clients: map[string]kubernetes.Interface{},
		live:    map[string]time.Time{},
		ttl:     liveTTL,
		now:     time.Now,
	}
}

// projectControlPlaneHost rewrites a base host to a project's control plane.
//
// The same path milo's quota plugin targets. Kept here rather than imported
// because the quota plugin builds it internally and IPAM must not depend on
// --enable-quota for this check to work (#85).
func projectControlPlaneHost(base, project string) string {
	return fmt.Sprintf("%s/apis/resourcemanager.miloapis.com/v1alpha1/projects/%s/control-plane",
		strings.TrimSuffix(base, "/"), project)
}

func (c *projectNamespaceChecker) clientFor(project string) (kubernetes.Interface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[project]; ok {
		return cl, nil
	}
	cfg := rest.CopyConfig(c.base)
	cfg.Host = projectControlPlaneHost(c.base.Host, project)
	cl, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build client for project %q: %w", project, err)
	}
	c.clients[project] = cl
	return cl, nil
}

func (c *projectNamespaceChecker) State(ctx context.Context, project, namespace string) (NamespaceState, error) {
	if project == "" || namespace == "" {
		// Nothing to route to, or nothing to look up. Not an error: a caller
		// with no project is a permanent, legitimate category (the control
		// plane's own controllers), and a cluster-scoped claim has no namespace.
		return NamespaceUnknown, nil
	}

	key := project + "/" + namespace
	c.mu.Lock()
	if until, ok := c.live[key]; ok && c.now().Before(until) {
		c.mu.Unlock()
		return NamespaceLive, nil
	}
	c.mu.Unlock()

	cl, err := c.clientFor(project)
	if err != nil {
		return NamespaceUnknown, err
	}

	ns, err := cl.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return NamespaceMissing, nil
	case err != nil:
		return NamespaceUnknown, err
	}

	if ns.Status.Phase == corev1.NamespaceTerminating || ns.DeletionTimestamp != nil {
		// Both signals, not just the phase. DeletionTimestamp is set the moment
		// deletion is requested; Phase follows when the namespace controller
		// observes it. Reading only Phase leaves a window in which deletion has
		// begun and the namespace still reports Active.
		return NamespaceTerminating, nil
	}

	c.mu.Lock()
	c.live[key] = c.now().Add(c.ttl)
	c.mu.Unlock()
	return NamespaceLive, nil
}

// RefuseNamespace builds the error for a namespace that cannot accept a claim.
//
// The message deliberately mirrors the one stock Kubernetes returns for the
// same condition, because it is the sentence operators already recognise and
// search for.
func RefuseNamespace(state NamespaceState, namespace string, gr schema.GroupResource) error {
	switch state {
	case NamespaceTerminating:
		return apierrors.NewForbidden(
			gr, "",
			fmt.Errorf("unable to create new content in namespace %s because it is being terminated: "+
				"an address bound into a terminating namespace would outlive everything that "+
				"could release it", namespace))
	case NamespaceMissing:
		return apierrors.NewForbidden(
			gr, "",
			fmt.Errorf("namespace %s does not exist: an address bound into it would have nothing "+
				"to collect it", namespace))
	default:
		return nil
	}
}

// LogUndetermined records a lookup that produced no answer.
//
// Separate from the refusal path on purpose. "The namespace is terminating" and
// "we could not determine the namespace state" are different answers, and a
// deployment where the second is happening constantly is one where this check
// is silently doing nothing — which is the failure mode that hides.
func LogUndetermined(project, namespace string, err error) {
	klog.V(2).InfoS("Could not determine namespace state; admitting the claim",
		"project", project, "namespace", namespace, "err", err)
}
