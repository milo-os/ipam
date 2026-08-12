package access

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

var ipclaims = schema.GroupResource{Group: "ipam.miloapis.com", Resource: "ipclaims"}

// nsServer answers namespace GETs from handler and records the paths it was
// asked for.
type nsServer struct {
	*httptest.Server

	mu    sync.Mutex
	paths []string
}

func (s *nsServer) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func newNSServer(t *testing.T, handler http.HandlerFunc) *nsServer {
	t.Helper()
	s := &nsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func writeNamespace(w http.ResponseWriter, ns *corev1.Namespace) {
	ns.APIVersion, ns.Kind = "v1", "Namespace"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ns)
}

func checkerFor(s *nsServer) *projectNamespaceChecker {
	return NewNamespaceChecker(&rest.Config{Host: s.URL}).(*projectNamespaceChecker)
}

func TestStateReportsALiveNamespace(t *testing.T) {
	s := newNSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeNamespace(w, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
			Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
		})
	})

	got, err := checkerFor(s).State(context.Background(), "tenant-a", "team-a")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != NamespaceLive {
		t.Errorf("state = %v, want Live", got)
	}
}

// The namespace lives in the project's control plane, not in the root cluster,
// so the lookup must be routed there.
func TestStateLooksTheNamespaceUpInItsProjectControlPlane(t *testing.T) {
	s := newNSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeNamespace(w, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})
	})

	if _, err := checkerFor(s).State(context.Background(), "tenant-a", "team-a"); err != nil {
		t.Fatalf("State: %v", err)
	}

	want := "/apis/resourcemanager.miloapis.com/v1alpha1/projects/tenant-a/control-plane/api/v1/namespaces/team-a"
	if got := s.requested(); len(got) != 1 || got[0] != want {
		t.Errorf("requested %v, want [%s]", got, want)
	}
}

// A control plane that is serving, answering that the namespace is not there.
func TestStateReportsAMissingNamespace(t *testing.T) {
	s := newNSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/version") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"major":"1","minor":"32"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	got, err := checkerFor(s).State(context.Background(), "tenant-a", "gone")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != NamespaceMissing {
		t.Errorf("state = %v, want Missing", got)
	}
}

// Where projects have no control planes — a kind cluster, every e2e run — the
// whole control-plane path 404s. Reading that as "the namespace is gone" would
// refuse every claim in those environments.
func TestStateDoesNotReadAnAbsentControlPlaneAsAMissingNamespace(t *testing.T) {
	s := newNSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	got, err := checkerFor(s).State(context.Background(), "tenant-a", "team-a")
	if got != NamespaceUnknown {
		t.Errorf("state = %v, want Unknown", got)
	}
	if err == nil {
		t.Error("State reported no reason for an undetermined answer")
	}
	if RefuseNamespace(got, "team-a", ipclaims) != nil {
		t.Error("an absent control plane produced a refusal")
	}
}

// Deletion is visible on the timestamp before the namespace controller moves
// the phase, and a claim admitted in that window is one nothing collects.
func TestStateReportsTerminatingFromEitherSignal(t *testing.T) {
	deleting := metav1.NewTime(time.Now())
	for _, tc := range []struct {
		name string
		ns   *corev1.Namespace
	}{
		{"phase", &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
			Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
		}},
		{"deletionTimestamp only", &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", DeletionTimestamp: &deleting},
			Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newNSServer(t, func(w http.ResponseWriter, _ *http.Request) {
				writeNamespace(w, tc.ns.DeepCopy())
			})

			got, err := checkerFor(s).State(context.Background(), "tenant-a", "team-a")
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != NamespaceTerminating {
				t.Errorf("state = %v, want Terminating", got)
			}
		})
	}
}

// A lookup that failed must be distinguishable from one that answered, or
// addressing would stop whenever the control plane is unwell.
func TestStateReportsUnknownWithAnErrorWhenTheLookupFails(t *testing.T) {
	s := newNSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	got, err := checkerFor(s).State(context.Background(), "tenant-a", "team-a")
	if err == nil {
		t.Fatal("State returned no error for a failed lookup")
	}
	if got != NamespaceUnknown {
		t.Errorf("state = %v, want Unknown", got)
	}
	if RefuseNamespace(got, "team-a", ipclaims) != nil {
		t.Error("an undetermined state produced a refusal")
	}
}

func TestStateCachesOnlyTheLiveAnswer(t *testing.T) {
	var mu sync.Mutex
	phase := corev1.NamespaceActive
	s := newNSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeNamespace(w, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
			Status:     corev1.NamespaceStatus{Phase: phase},
		})
	})
	c := checkerFor(s)
	ctx := context.Background()

	for range 3 {
		if _, err := c.State(ctx, "tenant-a", "team-a"); err != nil {
			t.Fatalf("State: %v", err)
		}
	}
	if got := len(s.requested()); got != 1 {
		t.Errorf("%d lookups for a repeated live namespace, want 1", got)
	}

	// Past the TTL the answer is taken again, and a refusal is never held: a
	// namespace that is no longer terminating must not stay refused.
	c.now = func() time.Time { return time.Now().Add(2 * liveTTL) }
	mu.Lock()
	phase = corev1.NamespaceTerminating
	mu.Unlock()
	for range 3 {
		got, err := c.State(ctx, "tenant-a", "team-a")
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if got != NamespaceTerminating {
			t.Fatalf("state = %v, want Terminating", got)
		}
	}
	if got := len(s.requested()); got != 4 {
		t.Errorf("%d lookups total, want 4 — a refusal was cached", got)
	}
}

// A caller with no project has already been refused by the tenancy layer, and a
// cluster-scoped object has no namespace. Neither is a lookup.
func TestStateAnswersUnknownWithNothingToLookUp(t *testing.T) {
	s := newNSServer(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a lookup was issued with no project or no namespace")
	})
	c := checkerFor(s)

	for _, tc := range [][2]string{{"", "team-a"}, {"tenant-a", ""}} {
		got, err := c.State(context.Background(), tc[0], tc[1])
		if err != nil || got != NamespaceUnknown {
			t.Errorf("State(%q, %q) = %v, %v; want Unknown, nil", tc[0], tc[1], got, err)
		}
	}
}

func TestNewNamespaceCheckerWithoutAConfigDisablesTheCheck(t *testing.T) {
	if c := NewNamespaceChecker(nil); c != nil {
		t.Errorf("NewNamespaceChecker(nil) = %v, want nil so the check disables rather than denies", c)
	}
}
