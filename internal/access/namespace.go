package access

// Namespace liveness. Deleting a namespace is what releases the claims in it,
// so an address bound into a namespace that is terminating or gone is never
// collected. This refuses those two states. Ownership is RBAC's job, not this
// package's.
//
// Not an admission plugin: IPAM installs those only under --enable-quota, and
// dev and e2e run with it off. The claim registry calls this instead.
//
// The lookup goes to the project's control plane, not the root cluster, because
// that is where a project-scoped claim's namespace lives. Same routing milo's
// quota plugin uses.

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
// Unknown stays distinct from Live: both admit the claim, but only one of them
// is an answer.
type NamespaceState int

const (
	// NamespaceUnknown means the lookup gave no answer; the caller proceeds.
	NamespaceUnknown NamespaceState = iota
	// NamespaceLive means the namespace exists and is not terminating.
	NamespaceLive
	// NamespaceTerminating means deletion has begun.
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
// It fails open: only a definitive Terminating or Missing refuses, and a failed
// lookup admits. Admitting into a doomed namespace costs one orphaned
// allocation; failing closed would turn a control-plane outage into an
// addressing outage.
type NamespaceChecker interface {
	// State reports the namespace's liveness within a project. An error comes
	// back with NamespaceUnknown so the caller can log it, never as a refusal.
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

// liveTTL is how long a Live answer is reused; maxLiveEntries bounds the cache.
//
// Only Live is cached. Caching a refusal would keep failing real claims for a
// state that no longer holds, so refusals always take a fresh lookup. A TTL
// cache rather than an informer, which would hold a watch and a full namespace
// cache per project IPAM has ever served.
const (
	liveTTL        = 10 * time.Second
	maxLiveEntries = 4096
)

// servingTTL is how long "this project has a control plane" is reused. It
// changes when clusters are built, not when claims are made, so it is held far
// longer than a namespace's state.
const servingTTL = 5 * time.Minute

// NewNamespaceChecker builds a checker over a base config, or returns nil when
// there is none. A nil checker disables the check — the opposite of
// ClassAccessChecker, which is an authorization boundary and fails closed.
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
// Duplicated from milo's quota plugin, which builds it internally, because this
// check must work without --enable-quota.
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
		// Nothing to route to, or nothing to look up. Not an error: the tenancy
		// layer already refuses a caller with no project, and a cluster-scoped
		// object has no namespace.
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
		// A missing namespace and a missing control plane both 404, and only the
		// first is an answer. Where projects have no control planes, as in kind
		// and e2e, every claim would otherwise read as bound for a missing
		// namespace. So a 404 refuses only once the probe confirms the control
		// plane is serving.
		if serving, probeErr := c.controlPlaneServes(ctx, project, cl); !serving {
			return NamespaceUnknown, fmt.Errorf("project %q has no reachable control plane: %w", project, probeErr)
		}
		return NamespaceMissing, nil
	case err != nil:
		return NamespaceUnknown, err
	}

	if ns.Status.Phase == corev1.NamespaceTerminating || ns.DeletionTimestamp != nil {
		// Both signals: DeletionTimestamp is set when deletion is requested,
		// Phase only once the namespace controller observes it. Phase alone
		// leaves a window where the namespace still reports Active.
		return NamespaceTerminating, nil
	}

	c.cacheLive(key)
	return NamespaceLive, nil
}

// controlPlaneServes reports whether the project's control-plane path answers at
// all, which is what makes a 404 beneath it meaningful.
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
		// Every entry still live, so nothing evicts on merit. Dropping the map
		// costs a round trip per key and keeps the bound.
		if len(c.live) >= maxLiveEntries {
			c.live = map[string]time.Time{}
		}
	}
	c.live[key] = now.Add(c.ttl)
}

// RefuseNamespace builds the error for a namespace that cannot accept a claim,
// or nil for a state that admits it. The wording mirrors stock Kubernetes, which
// is the sentence operators already recognise.
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

// LogUndetermined records a lookup that produced no answer. A deployment where
// this fires constantly is one where the check is doing nothing.
func LogUndetermined(project, namespace string, err error) {
	klog.V(2).InfoS("Could not determine namespace state; admitting the claim",
		"project", project, "namespace", namespace, "err", err)
}
