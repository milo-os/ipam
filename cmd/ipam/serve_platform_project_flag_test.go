package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tenant"
)

// The platform project is configuration, not an inference from an empty tenant.
// It is required rather than defaulted because every value it could default to
// is wrong somewhere: "" reinstates the unprefixed keyspace this change exists
// to remove, and any concrete name would silently make some tenant's project
// the platform on a cluster that happened to use that name.
func TestPlatformProjectValidation(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{
			name:  "a plain project id is accepted",
			value: "milo-platform",
		},
		{
			name:    "unset is rejected at startup",
			value:   "",
			wantErr: "--platform-project is required",
		},
		{
			// The value becomes a storage key prefix — "project/<name>/" — so a
			// slash would let it name a keyspace other than its own.
			name:    "a value containing a slash is rejected",
			value:   "milo/platform",
			wantErr: "must be a valid project name",
		},
		{
			name:    "a value that is not a DNS name is rejected",
			value:   "Milo Platform",
			wantErr: "must be a valid project name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &IPAMServerOptions{
				PostgresDSN:     "host=localhost",
				PlatformProject: tt.value,
			}
			err := o.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %q to be accepted, got %v", tt.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %q to be rejected", tt.value)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// The flag has no default. A default here would be a fallback wearing a
// different hat.
func TestPlatformProjectHasNoDefault(t *testing.T) {
	if got := NewIPAMServerOptions().PlatformProject; got != "" {
		t.Errorf("default --platform-project = %q, want no default", got)
	}
}

// The filter is what carries the configured value from a flag to every
// tenant.FromContext call in the request path. Without it the value exists in
// the process and nowhere the code that needs it can see it, and every caller —
// including the platform's own tooling — reads as a tenant.
//
// This is the test that would have caught wiring the flag but forgetting the
// filter, which is a failure that produces no error anywhere: lookups just miss.
func TestPlatformProjectFilterPutsTheValueOnTheRequestContext(t *testing.T) {
	const platform = "milo-platform"

	var got tenant.Identity
	inner := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		got = tenant.FromContext(req.Context())
	})

	handler := platformProjectFilter(platform, inner)

	req := httptest.NewRequest(http.MethodGet, "/apis/ipam.miloapis.com/v1alpha1/ipclasses", nil)
	req = req.WithContext(request.WithUser(req.Context(), &user.DefaultInfo{
		Name: "platform-operator",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {platform},
		},
	}))

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !got.IsPlatform() {
		t.Fatal("a request from the configured platform project did not read as platform through the filter")
	}
	if got.KeyPrefix() != "project/"+platform+"/" {
		t.Fatalf("KeyPrefix() = %q, want the platform project's prefix", got.KeyPrefix())
	}
}

// The same filter must not promote a tenant, and must not promote a caller with
// no extras at all. Both are the inverted-boundary case from
// internal/tenant's TestIsPlatformIsTheConfiguredProjectNotTheAbsenceOfATenant,
// asserted here through the real HTTP path rather than a synthesised context.
func TestPlatformProjectFilterDoesNotPromoteOtherCallers(t *testing.T) {
	const platform = "milo-platform"

	for _, tc := range []struct {
		name  string
		extra map[string][]string
	}{
		{name: "a different project", extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {"project-alpha"},
		}},
		{name: "no extras at all", extra: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got tenant.Identity
			handler := platformProjectFilter(platform, http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
				got = tenant.FromContext(req.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/apis/ipam.miloapis.com/v1alpha1/ipclasses", nil)
			req = req.WithContext(request.WithUser(req.Context(),
				&user.DefaultInfo{Name: "someone", Extra: tc.extra}))

			handler.ServeHTTP(httptest.NewRecorder(), req)

			if got.IsPlatform() {
				t.Fatalf("%s was treated as the platform", tc.name)
			}
		})
	}
}
