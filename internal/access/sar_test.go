package access

import "testing"

// TestResourceAndNameFromPoolKey pins the expected (resource, name) output for
// every key shape the IPAM apiserver actually produces. The previous parser
// expected `project/<id>/<kind-singular>/<name>` — a shape that never appears
// in storage — so every SAR ran against `ipprefixes` regardless of the actual
// pool kind. This test guards against that regression.
func TestResourceAndNameFromPoolKey(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		resource string
		expected string
	}{
		{
			name:     "platform-scoped IPPrefix",
			key:      "/ipam.miloapis.com/ipprefixes/my-pool",
			resource: "ipprefixes",
			expected: "my-pool",
		},
		{
			name:     "project-scoped IPPrefix",
			key:      "project/team-alpha/ipam.miloapis.com/ipprefixes/edge-prefix",
			resource: "ipprefixes",
			expected: "edge-prefix",
		},
		{
			name:     "unknown kind falls back to last segment as name and ipprefixes as resource",
			key:      "/ipam.miloapis.com/somethingelse/foo",
			resource: "ipprefixes",
			expected: "foo",
		},
		{
			name:     "bare name (defensive fallback)",
			key:      "lone-name",
			resource: "ipprefixes",
			expected: "lone-name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRes, gotName := resourceAndNameFromPoolKey(tc.key)
			if gotRes != tc.resource {
				t.Errorf("resource: got %q, want %q", gotRes, tc.resource)
			}
			if gotName != tc.expected {
				t.Errorf("name: got %q, want %q", gotName, tc.expected)
			}
		})
	}
}
