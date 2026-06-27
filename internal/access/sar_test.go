package access

import "testing"

// TestResourceAndNameFromPoolKey pins the expected (resource, name) output for
// every key shape the IPAM apiserver actually produces. Pools are served as the
// "ippools" resource and stored under ".../ippools/<name>" keys, so the "use"
// SAR must target "ippools" — not the nonexistent "ipprefixes" the parser
// previously emitted. This test guards against that regression.
func TestResourceAndNameFromPoolKey(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		resource string
		expected string
	}{
		{
			name:     "platform-scoped IPPool",
			key:      "/ipam.miloapis.com/ippools/my-pool",
			resource: "ippools",
			expected: "my-pool",
		},
		{
			name:     "project-scoped IPPool",
			key:      "project/team-alpha/ipam.miloapis.com/ippools/edge-pool",
			resource: "ippools",
			expected: "edge-pool",
		},
		{
			name:     "unknown kind falls back to last segment as name and ippools as resource",
			key:      "/ipam.miloapis.com/somethingelse/foo",
			resource: "ippools",
			expected: "foo",
		},
		{
			name:     "bare name (defensive fallback)",
			key:      "lone-name",
			resource: "ippools",
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
