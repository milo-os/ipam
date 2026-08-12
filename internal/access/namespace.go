package access

// Namespace liveness.
//
// # What this enforces, and what it does not
//
// Ownership of a namespace is not IPAM's problem. RBAC already decides whether
// a caller may write into one.
//
// What IPAM must not do is bind an address into a namespace that nothing will
// ever collect. Namespace deletion is the collector: when a namespace goes, the
// claims in it go, and the allocator releases their addresses. A claim admitted
// into a namespace that is already terminating is an allocation with no
// collector. It survives, holds its address, and nothing names it afterwards. A
// namespace that never existed has no collector either, so both answers refuse
// through the same path.
//
// The property is liveness, not existence and not ownership.
//
// # Why this is not an admission plugin
//
// Admission would be the natural home and the wrong one. IPAM installs
// admission plugins only under --enable-quota, and the dev overlay and every
// e2e suite set ENABLE_QUOTA=false, so an admission-based check would be inert
// exactly where it is exercised. The claim registry calls this instead, and the
// registry always runs.
//
// # Where the namespace is looked up
//
// Not in the root cluster. A project-scoped claim's namespace lives in that
// project's control plane, so validating against the root would reject
// legitimate claims. That is also why upstream NamespaceLifecycle does not work
// here. The routing matches the one milo's quota plugin uses: a base
// rest.Config whose Host is rewritten to
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
// "the namespace is fine" and "the lookup gave no answer" must not collapse.
// Both admit the claim, but they produce different operator-facing messages.
type NamespaceState int

const (
	// NamespaceUnknown means the lookup did not produce an answer. The caller
	// proceeds — see NamespaceChecker on failing open.
	NamespaceUnknown NamespaceState = iota
	// NamespaceLive means the namespace exists and is not terminating.
	NamespaceLive
	// NamespaceTerminating means deletion has begun. Nothing new may be bound
	// into it.
	NamespaceTerminating
	// NamespaceMissing means the namespace does not exist.
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
// # Failing open is a deliberate choice
//
// A lookup that errors returns NamespaceUnknown and the claim proceeds. Only a
// definitive Terminating or Missing refuses.
//
// The asymmetry is wide. Admitting a claim into a doomed namespace costs one
// orphaned allocation: recoverable, rare, and visible. Failing closed puts
// another service's availability in the hot path of every allocation, so a
// partial control-plane outage becomes a total addressing outage. IPAM exists
// to hand out addresses, and must keep doing so when a namespace lookup times
// out.
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
	live    map[string]time.Time // "<project>/<namespace>" -> when Live expires
	serving map[string]time.Time // project -> when the control-plane probe expires
	ttl     time.Duration
	now     func() time.Time
}

var _ NamespaceChecker = (*projectNamespaceChecker)(nil)

// liveTTL is how long a Live answer is reused, and maxLiveEntries bounds how
// many are held.
//
// # Only the positive answer is cached, which is the safe direction
//
// Caching Live briefly risks admitting a claim into a namespace that started
// terminating inside the window. The TTL bounds that, and the cost is the same
// single orphaned allocation the fail-open choice already accepts.
//
// Caching a refusal is the unsafe direction: a namespace that was Terminating
// or Missing and is now fine would keep being refused for the window, failing a
// real claim for a state that no longer holds. Refusals always take a fresh
// lookup.
//
// This is a TTL cache rather than an informer. An informer would hold an open
// watch and a full namespace cache for every project IPAM has served, unbounded
// in the number of projects and paying for namespaces no claim ever names. The
// cache exists because this runs on the claim hot path, where a control-plane
// round trip per create is the largest addition to the latency budget.
const (
	liveTTL        = 10 * time.Second
	maxLiveEntries = 4096
)

// servingTTL is how long the answer to "does this project have a control plane
// at all?" is reused. That changes on the timescale of a cluster being built,
// not of a claim, so it is held far longer than a namespace's state. As with
// Live, only the positive answer is kept.
const servingTTL = 5 * time.Minute

// NewNamespaceChecker builds a checker over a base config, or returns nil when
// there is none.
//
// A nil checker disables the check rather than denying every claim. That is the
// opposite of ClassAccessChecker's nil behaviour, and intentional: the class
// check is an authorization boundary and must fail closed, while this is a
// liveness check and must fail open.
func NewNamespaceChecker(base *rest.Config) NamespaceChecker {
	if base == nil {
		return nil
	}
	return &projectNamespaceChecker{
		base:    rest.CopyConfig(base),
		clients: map[string]kubernetes.Interface{},
		live:    map[string]time.Time{},
		serving: map[string]time.Time{},
		ttl:     liveTTL,
		now:     time.Now,
	}
}

// projectControlPlaneHost rewrites a base host to a project's control plane.
//
// This is the path milo's quota plugin targets. It lives here rather than being
// imported, because the quota plugin builds it internally and this check must
// work without --enable-quota.
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
		// carrying no project has already been refused by the tenancy layer,
		// and a cluster-scoped object has no namespace.
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
		// "This namespace is not there" and "nothing is there" both arrive as a
		// 404, and only the first is an answer. Where projects have no control
		// planes, such as a kind cluster or an e2e run, the whole control-plane
		// path is absent, and every claim would otherwise read as bound for a
		// missing namespace. A 404 therefore refuses only once the probe
		// confirms the control plane is serving. Otherwise it is a failed lookup
		// like any other.
		if serving, probeErr := c.controlPlaneServes(ctx, project, cl); !serving {
			return NamespaceUnknown, fmt.Errorf("project %q has no reachable control plane: %w", project, probeErr)
		}
		return NamespaceMissing, nil
	case err != nil:
		return NamespaceUnknown, err
	}

	if ns.Status.Phase == corev1.NamespaceTerminating || ns.DeletionTimestamp != nil {
		// Read both signals, not just the phase. DeletionTimestamp is set the
		// moment deletion is requested, and Phase follows when the namespace
		// controller observes it. Reading only Phase leaves a window in which
		// deletion has begun and the namespace still reports Active.
		return NamespaceTerminating, nil
	}

	c.cacheLive(key)
	return NamespaceLive, nil
}

// controlPlaneServes reports whether the project's control-plane path answers
// at all, so a 404 for a namespace under it can be read as an answer.
func (c *projectNamespaceChecker) controlPlaneServes(ctx context.Context, project string, cl kubernetes.Interface) (bool, error) {
	c.mu.Lock()
	until, ok := c.serving[project]
	c.mu.Unlock()
	if ok && c.now().Before(until) {
		return true, nil
	}

	if err := cl.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Error(); err != nil {
		return false, err
	}

	c.mu.Lock()
	c.serving[project] = c.now().Add(servingTTL)
	c.mu.Unlock()
	return true, nil
}

func (c *projectNamespaceChecker) cacheLive(key string) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.live) >= maxLiveEntries {
		for k, until := range c.live {
			if !now.Before(until) {
				delete(c.live, k)
			}
		}
		// Still full: every entry is live, so nothing can be evicted on merit.
		// Dropping the map costs a round trip per key and keeps the bound.
		if len(c.live) >= maxLiveEntries {
			c.live = map[string]time.Time{}
		}
	}
	c.live[key] = now.Add(c.ttl)
}

// RefuseNamespace builds the error for a namespace that cannot accept a claim,
// or nil for a state that admits it.
//
// The wording mirrors what stock Kubernetes returns for the same condition,
// because that is the sentence operators already recognise.
func RefuseNamespace(state NamespaceState, namespace string, gr schema.GroupResource) error {
	switch state {
	case NamespaceTerminating:
		return apierrors.NewForbidden(gr, "",
			fmt.Errorf("unable to create new content in namespace %s because it is being terminated: "+
				"an address bound into a terminating namespace would outlive everything that "+
				"could release it", namespace))
	case NamespaceMissing:
		return apierrors.NewForbidden(gr, "",
			fmt.Errorf("namespace %s does not exist: an address bound into it would have nothing "+
				"to collect it", namespace))
	default:
		return nil
	}
}

// LogUndetermined records a lookup that produced no answer.
//
// This stays separate from the refusal path on purpose. "The namespace is
// terminating" and "the namespace state is undetermined" are different answers.
// A deployment where the second happens constantly is one where this check does
// nothing, which is the failure mode that otherwise hides.
func LogUndetermined(project, namespace string, err error) {
	klog.V(2).InfoS("Could not determine namespace state; admitting the claim",
		"project", project, "namespace", namespace, "err", err)
}
