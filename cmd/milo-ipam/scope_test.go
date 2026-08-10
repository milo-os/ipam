package main

import (
	"testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestParseScopeEntry(t *testing.T) {
	cases := []struct {
		name string
		arg  string
		want ipamv1alpha1.ScopeRef
		role string
	}{
		{
			name: "known role resolves group and kind",
			arg:  "network=default",
			role: "network",
			want: ipamv1alpha1.ScopeRef{APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
		},
		{
			name: "known role, other kind",
			arg:  "location=us-central-1",
			role: "location",
			want: ipamv1alpha1.ScopeRef{APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
		},
		{
			name: "qualified form for an unknown role",
			arg:  "site=Site.infra.example.com/dc-1",
			role: "site",
			want: ipamv1alpha1.ScopeRef{APIGroup: "infra.example.com", Kind: "Site", Name: "dc-1"},
		},
		{
			name: "qualified form overrides the known-role table",
			arg:  "network=VPC.other.example.com/prod",
			role: "network",
			want: ipamv1alpha1.ScopeRef{APIGroup: "other.example.com", Kind: "VPC", Name: "prod"},
		},
		{
			name: "uid suffix pins the reference",
			arg:  "network=default#4f2a-1c",
			role: "network",
			want: ipamv1alpha1.ScopeRef{APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default", UID: "4f2a-1c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, ref, err := parseScopeEntry(tc.arg)
			if err != nil {
				t.Fatalf("parseScopeEntry(%q) failed: %v", tc.arg, err)
			}
			if role != tc.role {
				t.Errorf("role = %q, want %q", role, tc.role)
			}
			if ref != tc.want {
				t.Errorf("ref = %+v, want %+v", ref, tc.want)
			}
		})
	}
}

// An unknown role given a bare name cannot be resolved into a well-formed
// ScopeRef. Guessing a group would produce a reference that compares unequal to
// the one the platform writes, which is a silently separate address space.
func TestParseScopeRejectsUnqualifiedUnknownRole(t *testing.T) {
	_, _, err := parseScopeEntry("rack=r14")
	if err == nil {
		t.Fatal("expected an error for a bare name under an unknown role")
	}
	if toCLIError(err).code != exitUsage {
		t.Fatalf("code = %d, want usage(%d)", toCLIError(err).code, exitUsage)
	}
}

func TestParseScopeRejectsMalformed(t *testing.T) {
	for _, arg := range []string{"network", "=default", "network=", "site=Site/dc-1", "site=Site.infra.example.com/"} {
		if _, _, err := parseScopeEntry(arg); err == nil {
			t.Errorf("parseScopeEntry(%q) should have failed", arg)
		}
	}
}

// A claim's scope is immutable and decides which address space it lands in, so
// a duplicated role is an error rather than a silent last-wins.
func TestBuildScopeRejectsDuplicateRole(t *testing.T) {
	_, err := buildScope([]string{"network=a", "network=b"})
	if err == nil {
		t.Fatal("expected an error for a repeated role")
	}
	if toCLIError(err).code != exitUsage {
		t.Fatalf("code = %d, want usage(%d)", toCLIError(err).code, exitUsage)
	}
}

func TestBuildScopeEmptyIsNil(t *testing.T) {
	scope, err := buildScope(nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope != nil {
		t.Fatalf("scope = %v, want nil so the field is omitted entirely", scope)
	}
}

// A scope printed by `show` must parse back into the same reference, so an
// operator can reproduce a claim from what they read.
func TestFormatScopeRefRoundTrips(t *testing.T) {
	want := ipamv1alpha1.ScopeRef{APIGroup: "infra.example.com", Kind: "Site", Name: "dc-1", UID: "abc"}
	_, got, err := parseScopeEntry("site=" + formatScopeRef(want))
	if err != nil {
		t.Fatalf("re-parsing %q failed: %v", formatScopeRef(want), err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestParseObjectRef(t *testing.T) {
	ref, err := parseObjectRef("Instance.compute.datumapis.com/hello-0")
	if err != nil {
		t.Fatal(err)
	}
	if ref.APIGroup != "compute.datumapis.com" || ref.Kind != "Instance" || ref.Name != "hello-0" || ref.Namespace != "" {
		t.Fatalf("ref = %+v", ref)
	}
	nsRef, err := parseObjectRef("Instance.compute.datumapis.com/app-team/hello-0")
	if err != nil {
		t.Fatal(err)
	}
	if nsRef.Namespace != "app-team" || nsRef.Name != "hello-0" {
		t.Fatalf("namespaced ref = %+v", nsRef)
	}
	if _, err := parseObjectRef("hello-0"); err == nil {
		t.Error("an unqualified owner should be rejected")
	}
}

func TestFormatScopeIsStable(t *testing.T) {
	scope := map[string]ipamv1alpha1.ScopeRef{
		"network":  {Name: "default"},
		"location": {Name: "us-central-1"},
	}
	if got := formatScope(scope); got != "location=us-central-1 network=default" {
		t.Fatalf("formatScope = %q, want role-sorted output", got)
	}
	if got := formatScope(nil); got != "—" {
		t.Fatalf("formatScope(nil) = %q, want an em dash", got)
	}
}
