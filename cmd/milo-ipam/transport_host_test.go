package main

import "testing"

func TestEnsureScheme(t *testing.T) {
	cases := map[string]string{
		"api.datum.net":         "https://api.datum.net",
		"https://api.datum.net": "https://api.datum.net",
		"http://localhost:8443": "http://localhost:8443",
		"":                      "",
	}
	for in, want := range cases {
		if got := ensureScheme(in); got != want {
			t.Errorf("ensureScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestControlPlaneHost(t *testing.T) {
	const base = "api.staging.env.datum.net"
	cases := []struct {
		name string
		env  datumEnv
		want string
	}{
		{
			name: "project scopes to the project control plane",
			env:  datumEnv{apiHost: base, project: "datum-cloud"},
			want: "https://api.staging.env.datum.net/apis/resourcemanager.miloapis.com/v1alpha1/projects/datum-cloud/control-plane",
		},
		{
			name: "org (no project) scopes to the org control plane",
			env:  datumEnv{apiHost: base, org: "datum-technology"},
			want: "https://api.staging.env.datum.net/apis/resourcemanager.miloapis.com/v1alpha1/organizations/datum-technology/control-plane",
		},
		{
			name: "project wins over org",
			env:  datumEnv{apiHost: base, org: "datum-technology", project: "datum-cloud"},
			want: "https://api.staging.env.datum.net/apis/resourcemanager.miloapis.com/v1alpha1/projects/datum-cloud/control-plane",
		},
		{
			name: "neither set uses the platform root",
			env:  datumEnv{apiHost: base},
			want: "https://api.staging.env.datum.net",
		},
		{
			name: "existing scheme and trailing slash are normalized",
			env:  datumEnv{apiHost: "https://api.datum.net/", project: "p"},
			want: "https://api.datum.net/apis/resourcemanager.miloapis.com/v1alpha1/projects/p/control-plane",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := controlPlaneHost(tc.env); got != tc.want {
				t.Errorf("controlPlaneHost() = %q, want %q", got, tc.want)
			}
		})
	}
}
