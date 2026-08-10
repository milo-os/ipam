package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	milorequest "go.miloapis.com/milo/pkg/request"

	"go.miloapis.com/ipam/internal/tenant"
)

// userWithParent builds a context carrying the forwarded iam.miloapis.com
// parent-* extras the way milo's front gate delivers them, so
// tenant.FromContext resolves the expected scope.
func userWithParent(kind, name string) context.Context {
	info := &user.DefaultInfo{
		Name: "tester",
		Extra: map[string][]string{
			tenant.ExtraParentType: {kind},
			tenant.ExtraParentName: {name},
		},
	}
	return genericapirequest.WithUser(context.Background(), info)
}

func TestConsumerContextFilter(t *testing.T) {
	cases := []struct {
		name        string
		ctx         context.Context
		wantProject string
		wantOrg     string
	}{
		{
			name:        "project-scoped sets project on context",
			ctx:         userWithParent("Project", "proj-123"),
			wantProject: "proj-123",
		},
		{
			name:    "org-scoped sets organization on context",
			ctx:     userWithParent("Organization", "org-abc"),
			wantOrg: "org-abc",
		},
		{
			name: "platform-scoped (no parent) sets neither",
			ctx:  genericapirequest.WithUser(context.Background(), &user.DefaultInfo{Name: "platform"}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotProject, gotOrg string
			var hadProject, hadOrg bool
			inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotProject, hadProject = milorequest.ProjectID(r.Context())
				gotOrg, hadOrg = milorequest.OrganizationID(r.Context())
			})

			h := consumerContextFilter(inner)
			req := httptest.NewRequest(http.MethodPost, "/apis/ipam.miloapis.com/v1alpha1/ipclaims", nil).
				WithContext(tc.ctx)
			h.ServeHTTP(httptest.NewRecorder(), req)

			if tc.wantProject != "" {
				if !hadProject || gotProject != tc.wantProject {
					t.Errorf("project: got (%q, %v), want %q", gotProject, hadProject, tc.wantProject)
				}
			} else if hadProject && gotProject != "" {
				t.Errorf("project: expected none, got %q", gotProject)
			}

			if tc.wantOrg != "" {
				if !hadOrg || gotOrg != tc.wantOrg {
					t.Errorf("org: got (%q, %v), want %q", gotOrg, hadOrg, tc.wantOrg)
				}
			} else if hadOrg && gotOrg != "" {
				t.Errorf("org: expected none, got %q", gotOrg)
			}
		})
	}
}

func ipclaimAttrs(op admission.Operation) admission.Attributes {
	gvr := schema.GroupVersionResource{Group: "ipam.miloapis.com", Version: "v1alpha1", Resource: "ipclaims"}
	gvk := schema.GroupVersionKind{Group: "ipam.miloapis.com", Version: "v1alpha1", Kind: "IPClaim"}
	return admission.NewAttributesRecord(nil, nil, gvk, "default", "my-claim", gvr, "", op, nil, false, &user.DefaultInfo{Name: "tester"})
}

func poolAttrs(op admission.Operation) admission.Attributes {
	gvr := schema.GroupVersionResource{Group: "ipam.miloapis.com", Version: "v1alpha1", Resource: "ippools"}
	gvk := schema.GroupVersionKind{Group: "ipam.miloapis.com", Version: "v1alpha1", Kind: "IPPool"}
	return admission.NewAttributesRecord(nil, nil, gvk, "", "my-pool", gvr, "", op, nil, false, &user.DefaultInfo{Name: "tester"})
}

func TestPlatformConsumerGuard(t *testing.T) {
	guard := newPlatformConsumerGuard()

	cases := []struct {
		name       string
		ctx        context.Context
		attrs      admission.Attributes
		wantDenied bool
	}{
		{
			name:       "platform-scoped ipclaim create is denied",
			ctx:        context.Background(),
			attrs:      ipclaimAttrs(admission.Create),
			wantDenied: true,
		},
		{
			name:       "project-scoped ipclaim create is allowed",
			ctx:        milorequest.WithProject(context.Background(), "proj-1"),
			attrs:      ipclaimAttrs(admission.Create),
			wantDenied: false,
		},
		{
			name:       "org-scoped ipclaim create is allowed",
			ctx:        milorequest.WithOrganization(context.Background(), "org-1"),
			attrs:      ipclaimAttrs(admission.Create),
			wantDenied: false,
		},
		{
			name:       "platform-scoped pool create is allowed (not quota-protected)",
			ctx:        context.Background(),
			attrs:      poolAttrs(admission.Create),
			wantDenied: false,
		},
		{
			name:       "non-create operation is allowed",
			ctx:        context.Background(),
			attrs:      ipclaimAttrs(admission.Update),
			wantDenied: false,
		},
		{
			name:       "empty project value is treated as no consumer (denied)",
			ctx:        milorequest.WithProject(context.Background(), ""),
			attrs:      ipclaimAttrs(admission.Create),
			wantDenied: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guard.Validate(tc.ctx, tc.attrs, nil)
			if tc.wantDenied {
				if err == nil {
					t.Fatalf("expected denial, got nil")
				}
				if !apierrors.IsForbidden(err) {
					t.Errorf("expected Forbidden (403), got %v", err)
				}
			} else if err != nil {
				t.Errorf("expected allow, got %v", err)
			}
		})
	}
}

func TestHasDerivableConsumer(t *testing.T) {
	if hasDerivableConsumer(context.Background()) {
		t.Error("bare context should have no consumer")
	}
	if !hasDerivableConsumer(milorequest.WithProject(context.Background(), "p")) {
		t.Error("project context should have a consumer")
	}
	if !hasDerivableConsumer(milorequest.WithOrganization(context.Background(), "o")) {
		t.Error("org context should have a consumer")
	}
	if hasDerivableConsumer(milorequest.WithProject(context.Background(), "")) {
		t.Error("empty project should not count as a consumer")
	}
}
