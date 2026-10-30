package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	"go.miloapis.com/milo/pkg/quota/engine"
)

// Exercise the engine embedded in this binary, with the condition shipped in
// the service catalog component. Decoding the policy from JSON also catches a
// dependency downgrade that silently discards the request condition field.
func TestPublicIPv4QuotaCondition(t *testing.T) {
	data, err := os.ReadFile("../../config/components/service-catalog-public-ipv4/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var component struct {
		Patches []struct {
			Patch string `json:"patch"`
		} `json:"patches"`
	}
	if err := yaml.Unmarshal(data, &component); err != nil {
		t.Fatal(err)
	}
	var condition string
	for _, patch := range component.Patches {
		var operations []struct {
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		}
		if err := yaml.Unmarshal([]byte(patch.Patch), &operations); err != nil {
			t.Fatal(err)
		}
		for _, op := range operations {
			if op.Path == "/spec/quota/metricRules/0/metricCostConditions" {
				var conditions map[string]string
				if err := json.Unmarshal(op.Value, &conditions); err != nil {
					t.Fatal(err)
				}
				condition = conditions["ipam.miloapis.com/public-ipv4claims"]
			}
		}
	}
	if condition == "" {
		t.Fatal("public IPv4 quota condition is missing")
	}
	cel, err := engine.NewCELEngine()
	if err != nil {
		t.Fatal(err)
	}
	renderer := engine.NewTemplateEngine(cel, logr.Discard())
	cases := []struct {
		name       string
		spec       map[string]any
		wantPublic bool
	}{
		{"PublicIPv4", map[string]any{"ipFamily": "IPv4", "addressScope": "Public"}, true},
		{"PublicIPv6", map[string]any{"ipFamily": "IPv6", "addressScope": "Public"}, false},
		{"PrivateIPv4", map[string]any{"ipFamily": "IPv4", "addressScope": "Private"}, false},
		{"PrivateIPv6", map[string]any{"ipFamily": "IPv6", "addressScope": "Private"}, false},
		{"MissingFamily", map[string]any{"addressScope": "Public"}, false},
		{"MissingScope", map[string]any{"ipFamily": "IPv4"}, false},
		{"MissingBoth", map[string]any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"spec": map[string]any{
					"target": map[string]any{
						"resourceClaimTemplate": map[string]any{
							"metadata": map[string]any{"name": "test-claim"},
							"spec": map[string]any{
								"consumerRef": map[string]any{
									"apiGroup": "resourcemanager.miloapis.com",
									"kind":     "Project", "name": "test-project",
								},
								"requests": []map[string]any{
									{"resourceType": "ipam.miloapis.com/ipclaims", "amount": 1},
									{"resourceType": "ipam.miloapis.com/public-ipv4claims", "amount": 1, "condition": condition},
								},
							},
						},
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var policy quotav1alpha1.ClaimCreationPolicy
			if err := json.Unmarshal(raw, &policy); err != nil {
				t.Fatal(err)
			}
			claim, err := renderer.RenderClaim(&policy, &engine.EvaluationContext{
				Object: &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "ipam.miloapis.com/v1alpha1", "kind": "IPClaim",
					"metadata": map[string]any{"name": "test-claim", "namespace": "default"},
					"spec":     tc.spec,
				}},
				Namespace: "default",
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.wantPublic {
				want++
			}
			if len(claim.Spec.Requests) != want {
				t.Fatalf("requests = %+v, want %d (public IPv4 charged: %v)", claim.Spec.Requests, want, tc.wantPublic)
			}
			if claim.Spec.Requests[0].ResourceType != "ipam.miloapis.com/ipclaims" || claim.Spec.Requests[0].Amount != 1 {
				t.Fatalf("base claim quota changed: %+v", claim.Spec.Requests)
			}
			if tc.wantPublic && (claim.Spec.Requests[1].ResourceType != "ipam.miloapis.com/public-ipv4claims" || claim.Spec.Requests[1].Amount != 1) {
				t.Fatalf("public IPv4 charge = %+v", claim.Spec.Requests)
			}
		})
	}
}
