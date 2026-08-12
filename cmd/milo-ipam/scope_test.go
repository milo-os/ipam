package main

import (
	"strings"
	"testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestBuildScope(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    map[string]ipamv1alpha1.ScopeRef
		wantErr string
	}{
		{
			name: "known role resolves kind and group",
			args: []string{"network=default"},
			want: map[string]ipamv1alpha1.ScopeRef{
				"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
			},
		},
		{
			name: "qualified reference for an unknown role",
			args: []string{"site=Site.infra.example.com/dc-1"},
			want: map[string]ipamv1alpha1.ScopeRef{
				"site": {APIGroup: "infra.example.com", Kind: "Site", Name: "dc-1"},
			},
		},
		{
			name: "qualified form overrides the known-role table",
			args: []string{"network=Net.example.com/alt"},
			want: map[string]ipamv1alpha1.ScopeRef{
				"network": {APIGroup: "example.com", Kind: "Net", Name: "alt"},
			},
		},
		{
			name:    "an unknown role needs qualifying",
			args:    []string{"site=dc-1"},
			wantErr: "not one of the known roles",
		},
		{
			name:    "a repeated role is refused rather than last-wins",
			args:    []string{"network=a", "network=b"},
			wantErr: "given twice",
		},
		{
			name:    "missing separator",
			args:    []string{"network"},
			wantErr: "expected role=value",
		},
		{
			name:    "a qualifier without a group is not a reference",
			args:    []string{"site=Site/dc-1"},
			wantErr: "Kind.apiGroup/name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildScope(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q", tc.wantErr)
				}
				if toCLIError(err).code != exitUsage {
					t.Errorf("code = %d, want usage(%d)", toCLIError(err).code, exitUsage)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("scope = %v, want %v", got, tc.want)
			}
			for role, want := range tc.want {
				if got[role] != want {
					t.Errorf("scope[%q] = %+v, want %+v", role, got[role], want)
				}
			}
		})
	}
}

func TestParseObjectRef(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    *ipamv1alpha1.ObjectRef
		wantErr string
	}{
		{name: "empty yields no reference", in: ""},
		{
			name: "kind, group, name",
			in:   "Instance.compute.datumapis.com/hello-0",
			want: &ipamv1alpha1.ObjectRef{APIGroup: "compute.datumapis.com", Kind: "Instance", Name: "hello-0"},
		},
		{
			name: "namespaced",
			in:   "Instance.compute.datumapis.com/team-a/hello-0",
			want: &ipamv1alpha1.ObjectRef{APIGroup: "compute.datumapis.com", Kind: "Instance", Namespace: "team-a", Name: "hello-0"},
		},
		{name: "unqualified kind", in: "Instance/hello-0", wantErr: "must be qualified"},
		{name: "no name", in: "Instance.compute.datumapis.com", wantErr: "expected Kind.apiGroup/name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseObjectRef(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("ref = %+v, want nil", got)
			case tc.want != nil && got == nil:
				t.Fatalf("ref = nil, want %+v", tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("ref = %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

// formatScope is a table cell and an error-message fragment, so its ordering
// must not follow Go's randomized map iteration.
func TestFormatScopeIsStable(t *testing.T) {
	scope := map[string]ipamv1alpha1.ScopeRef{
		"network":  {Name: "default"},
		"location": {Name: "us-west"},
		"site":     {Name: "dc-1"},
	}
	const want = "location=us-west network=default site=dc-1"
	for i := 0; i < 20; i++ {
		if got := formatScope(scope); got != want {
			t.Fatalf("formatScope = %q, want %q", got, want)
		}
	}
	if got := formatScope(nil); got != "—" {
		t.Errorf("formatScope(nil) = %q, want an em dash", got)
	}
}

// A reference read out of `show` must paste back into --scope unchanged.
func TestFormatScopeRefRoundTrips(t *testing.T) {
	ref := ipamv1alpha1.ScopeRef{APIGroup: "infra.example.com", Kind: "Site", Name: "dc-1"}
	scope, err := buildScope([]string{"site=" + formatScopeRef(ref)})
	if err != nil {
		t.Fatal(err)
	}
	if scope["site"] != ref {
		t.Fatalf("round trip = %+v, want %+v", scope["site"], ref)
	}
}

func TestScopeContains(t *testing.T) {
	have := map[string]ipamv1alpha1.ScopeRef{
		"network":  {Name: "default"},
		"location": {Name: "us-west"},
	}
	cases := []struct {
		name string
		want map[string]ipamv1alpha1.ScopeRef
		ok   bool
	}{
		{"empty filter matches everything", nil, true},
		{"subset matches", map[string]ipamv1alpha1.ScopeRef{"network": {Name: "default"}}, true},
		{"wrong value does not", map[string]ipamv1alpha1.ScopeRef{"network": {Name: "other"}}, false},
		{"absent role does not", map[string]ipamv1alpha1.ScopeRef{"site": {Name: "dc-1"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopeContains(have, tc.want); got != tc.ok {
				t.Fatalf("scopeContains = %v, want %v", got, tc.ok)
			}
		})
	}
}
