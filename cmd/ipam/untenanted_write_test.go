package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapiserver "k8s.io/apiserver/pkg/server"

	"go.miloapis.com/ipam/internal/tenant"
)

// Every object this service stores belongs to a project. A caller with no
// project has nowhere legitimate to write, and before this gate such a write
// succeeded — landing at an unprefixed key in a keyspace no read path consults,
// and answering 201 Created for an object nothing would ever find.
//
// These tests assert the PROPERTY (a write with no project is refused; a read
// is not) rather than the mechanism, so they survive the gate moving between
// the handler chain and admission.

// withUser returns a request whose context carries the given parent extras,
// mimicking what Milo's front gate forwards after authentication.
func withUser(req *http.Request, project string) *http.Request {
	extra := map[string][]string{}
	if project != "" {
		extra[tenant.ExtraParentAPIGroup] = []string{tenant.ParentAPIGroupProject}
		extra[tenant.ExtraParentType] = []string{tenant.ParentTypeProject}
		extra[tenant.ExtraParentName] = []string{project}
	}
	ctx := genericapirequest.WithUser(req.Context(), &user.DefaultInfo{
		Name:  "tester",
		Extra: extra,
	})
	return req.WithContext(ctx)
}

// serve runs a request through the filter, reporting whether the inner handler
// was reached. "Reached" is the real question: a gate that returns 403 but
// still dispatches would pass a status-code-only assertion.
func serve(t *testing.T, req *http.Request) (status int, reachedInner bool) {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedInner = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	untenantedWriteFilter(inner).ServeHTTP(rec, req)
	return rec.Code, reachedInner
}

func TestUntenantedWriteIsRefused(t *testing.T) {
	// A NAMED object. The collection form of DELETE is deliberately exempt —
	// see TestUntenantedCollectionDeleteIsAllowed — so using a collection path
	// here would assert the opposite of the intended behaviour for one method
	// and pass for the wrong reason for the other three.
	const path = "/apis/ipam.miloapis.com/v1alpha1/namespaces/default/ipclaims/claim-a"

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		t.Run(method, func(t *testing.T) {
			req := withUser(httptest.NewRequest(method, path, nil), "")
			status, reached := serve(t, req)

			if status != http.StatusForbidden {
				t.Errorf("status = %d, want %d", status, http.StatusForbidden)
			}
			if reached {
				t.Error("request reached the API handler; the write was not actually stopped")
			}
		})
	}
}

// The refusal must be a well-formed Status object, not a bare string. A caller
// that cannot parse the response cannot distinguish this from a proxy error,
// and "403 from somewhere" is the shape of failure that gets misdiagnosed.
func TestUntenantedWriteReturnsParseableStatus(t *testing.T) {
	req := withUser(httptest.NewRequest(
		http.MethodPost, "/apis/ipam.miloapis.com/v1alpha1/ippools", nil), "")

	rec := httptest.NewRecorder()
	untenantedWriteFilter(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, req)

	var status metav1.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("response body is not a metav1.Status: %v (body: %q)", err, rec.Body.String())
	}
	if status.Reason != metav1.StatusReasonForbidden {
		t.Errorf("reason = %q, want %q", status.Reason, metav1.StatusReasonForbidden)
	}
	// The usual cause is a kubeconfig talking to IPAM directly instead of
	// through Milo's front gate, so the message must name the missing extra.
	if !strings.Contains(status.Message, tenant.ExtraParentName) {
		t.Errorf("message does not name the missing extra %q: %q",
			tenant.ExtraParentName, status.Message)
	}
}

// A project-scoped write must pass through untouched. Without this the gate
// could "pass" by refusing everything.
func TestTenantedWriteIsAllowed(t *testing.T) {
	req := withUser(httptest.NewRequest(
		http.MethodPost, "/apis/ipam.miloapis.com/v1alpha1/ippools", nil), "project-alpha")

	status, reached := serve(t, req)
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if !reached {
		t.Error("a project-scoped write was blocked")
	}
}

// The platform is an ordinary project and must not be a special case here.
func TestPlatformProjectWriteIsAllowed(t *testing.T) {
	req := withUser(httptest.NewRequest(
		http.MethodPost, "/apis/ipam.miloapis.com/v1alpha1/ipclasses", nil), "milo-platform")
	req = req.WithContext(tenant.WithPlatformProject(req.Context(), "milo-platform"))

	status, reached := serve(t, req)
	if status != http.StatusOK || !reached {
		t.Errorf("platform write blocked: status = %d, reached = %v", status, reached)
	}
}

// Reads are deliberately NOT gated. An untenanted read already returns nothing,
// which is the tenancy model working rather than failing — and it is the
// documented answer to "why does my kubectl show zero pools". Gating it would
// turn a correct empty list into an error and invalidate that explanation.
func TestUntenantedReadIsNotGated(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req := withUser(httptest.NewRequest(
				method, "/apis/ipam.miloapis.com/v1alpha1/ippools", nil), "")

			status, reached := serve(t, req)
			if status != http.StatusOK || !reached {
				t.Errorf("read was gated: status = %d, reached = %v", status, reached)
			}
		})
	}
}

// Discovery, health and metrics live outside the IPAM API prefix and have no
// project. Gating them would break readiness probes and monitoring scrapes,
// which is a self-inflicted outage rather than a tenancy fix.
func TestNonAPIPathsAreNotGated(t *testing.T) {
	for _, path := range []string{
		"/healthz", "/readyz", "/livez", "/metrics",
		"/apis", "/apis/ipam.miloapis.com", "/openapi/v2",
	} {
		t.Run(path, func(t *testing.T) {
			// POST specifically: the gate keys on method, so a read-only
			// assertion here would not prove the path exemption.
			req := withUser(httptest.NewRequest(http.MethodPost, path, nil), "")

			status, reached := serve(t, req)
			if status != http.StatusOK || !reached {
				t.Errorf("non-API path was gated: status = %d, reached = %v", status, reached)
			}
		})
	}
}

// A request with no authenticated user at all must be refused on a write, the
// same as one whose user carries no parent extras. These arrive by different
// routes and tenant.FromContext collapses them to the same empty Name.
func TestWriteWithNoUserInfoIsRefused(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/apis/ipam.miloapis.com/v1alpha1/ippools", nil)

	status, reached := serve(t, req)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
	if reached {
		t.Error("request with no user info reached the API handler")
	}
}

func TestTenancyFiltersAreInstalledWithoutQuota(t *testing.T) {
	for _, enableQuota := range []bool{false, true} {
		name := "quota-off"
		if enableQuota {
			name = "quota-on"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &genericapiserver.RecommendedConfig{}
			// Seed an identity chain so the installers compose against it
			// rather than DefaultBuildHandlerChain, which needs a fully
			// populated Config. Everything above this line is ours.
			cfg.BuildHandlerChainFunc = func(h http.Handler, _ *genericapiserver.Config) http.Handler { return h }
			installRequestFilters(cfg, "milo-platform", enableQuota)

			reached := false
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})

			// Build the chain THE INSTALLER PRODUCED. Calling
			// untenantedWriteFilter directly here would test the filter, which
			// was never broken, and would pass with the gate wired back inside
			// the quota branch — the exact defect this test exists to catch.
			handler := cfg.BuildHandlerChainFunc(inner, &genericapiserver.Config{})

			req := withUser(httptest.NewRequest(
				http.MethodPost, "/apis/ipam.miloapis.com/v1alpha1/ippools", nil), "")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("untenanted write with enableQuota=%v: status = %d, want %d",
					enableQuota, rec.Code, http.StatusForbidden)
			}
			if reached {
				t.Errorf("untenanted write with enableQuota=%v reached the handler", enableQuota)
			}
		})
	}
}

// Pins the ordering contract installRequestFilters documents: the platform
// filter is installed first so it runs first, before anything judges the
// request. A reordering that put the write gate first would still pass the
// tests above and would break the moment the gate needs the platform project.
func TestPlatformFilterIsInstalledBeforeWriteGate(t *testing.T) {
	cfg := &genericapiserver.RecommendedConfig{}
	// Seed an identity chain so the installers compose against it instead of
	// genericapiserver.DefaultBuildHandlerChain, which needs a fully populated
	// Config and would put apiserver machinery under test rather than our
	// ordering.
	cfg.BuildHandlerChainFunc = func(h http.Handler, _ *genericapiserver.Config) http.Handler { return h }
	installRequestFilters(cfg, "milo-platform", false)

	// installPlatformProjectFilter is what puts the value on the context; if it
	// ran after the gate, a gate that consulted it would see nothing.
	var seen string
	probe := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if p, ok := tenant.PlatformProjectFromContext(r.Context()); ok {
			seen = p
		}
	})
	chain := cfg.BuildHandlerChainFunc(probe, &genericapiserver.Config{})

	req := withUser(httptest.NewRequest(
		http.MethodGet, "/apis/ipam.miloapis.com/v1alpha1/ippools", nil), "milo-platform")
	chain.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "milo-platform" {
		t.Errorf("platform project not on context at dispatch: got %q", seen)
	}
}

// The namespace controller must DELETE-collection every namespaced type before
// it can drop a namespace's `kubernetes` finalizer, and it is a cluster-internal
// controller carrying no project. Refusing it broke namespace deletion for the
// entire cluster — including namespaces holding no IPAM objects at all, because
// the controller sweeps every type regardless of contents. Seven namespaces sat
// Terminating for the best part of an hour.
//
// The refusal prevented no write: with no project there is no keyspace, so the
// collection delete targets nothing. That is the same argument that exempts
// reads.
func TestUntenantedCollectionDeleteIsAllowed(t *testing.T) {
	for _, path := range []string{
		"/apis/ipam.miloapis.com/v1alpha1/namespaces/ns-1/ipclaims",
		"/apis/ipam.miloapis.com/v1alpha1/namespaces/ns-1/ipallocations",
		"/apis/ipam.miloapis.com/v1alpha1/ippools",
	} {
		t.Run(path, func(t *testing.T) {
			req := withUser(httptest.NewRequest(http.MethodDelete, path, nil), "")
			req = withRequestInfo(req, "deletecollection")

			status, reached := serve(t, req)
			if status != http.StatusOK || !reached {
				t.Errorf("collection delete was refused: status = %d, reached = %v", status, reached)
			}
		})
	}
}

// A NAMED delete with no project stays refused. That is what the gate is for:
// an untenanted caller reaching for a specific object at an unprefixed key.
// Without this, the collection exemption would be a hole rather than a carve-out.
func TestUntenantedNamedDeleteIsStillRefused(t *testing.T) {
	req := withUser(httptest.NewRequest(http.MethodDelete,
		"/apis/ipam.miloapis.com/v1alpha1/namespaces/ns-1/ipclaims/claim-a", nil), "")
	req = withRequestInfo(req, "delete")

	status, reached := serve(t, req)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
	if reached {
		t.Error("named delete reached the API handler")
	}
}

// The path fallback runs only if this filter is installed outside the
// apiserver's chain, where RequestInfo is absent. It must err toward NAMED, so
// an unparseable request stays refused rather than silently exempt.
func TestCollectionDeleteFallbackWithoutRequestInfo(t *testing.T) {
	cases := []struct {
		path       string
		collection bool
	}{
		{"/apis/ipam.miloapis.com/v1alpha1/ippools", true},
		{"/apis/ipam.miloapis.com/v1alpha1/namespaces/ns-1/ipclaims", true},
		{"/apis/ipam.miloapis.com/v1alpha1/namespaces/ns-1/ipclaims/", true},
		{"/apis/ipam.miloapis.com/v1alpha1/ippools/pool-a", false},
		{"/apis/ipam.miloapis.com/v1alpha1/namespaces/ns-1/ipclaims/claim-a", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, tc.path, nil)
			if got := isCollectionDelete(req); got != tc.collection {
				t.Errorf("isCollectionDelete = %v, want %v", got, tc.collection)
			}
		})
	}
}

// withRequestInfo attaches the parsed request info the apiserver chain would
// have set, so these tests exercise the same branch production takes rather
// than the fallback.
func withRequestInfo(req *http.Request, verb string) *http.Request {
	ctx := genericapirequest.WithRequestInfo(req.Context(), &genericapirequest.RequestInfo{
		IsResourceRequest: true,
		Verb:              verb,
		APIGroup:          "ipam.miloapis.com",
	})
	return req.WithContext(ctx)
}
