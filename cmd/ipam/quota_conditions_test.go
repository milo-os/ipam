package main

import (
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	"go.miloapis.com/milo/pkg/quota/engine"
)

const (
	ipClaimsMetric      = "ipam.miloapis.com/ipclaims"
	publicIPv4Metric    = "ipam.miloapis.com/public-ipv4claims"
	publicIPv4Component = "../../config/components/service-catalog-public-ipv4/kustomization.yaml"
)

// publicIPv4Condition reads the condition the public IPv4 component sets on the
// public IPv4 metric, so the test exercises the expression that ships.
func publicIPv4Condition(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(publicIPv4Component)
	if err != nil {
		t.Fatalf("read component: %v", err)
	}
	var component struct {
		Patches []struct {
			Patch string `json:"patch"`
		} `json:"patches"`
	}
	if err := yaml.Unmarshal(raw, &component); err != nil {
		t.Fatalf("parse component: %v", err)
	}
	for _, p := range component.Patches {
		var ops []struct {
			Path  string `json:"path"`
			Value any    `json:"value"`
		}
		if err := yaml.Unmarshal([]byte(p.Patch), &ops); err != nil {
			continue
		}
		for _, op := range ops {
			if !strings.HasSuffix(op.Path, "/metricCostConditions") {
				continue
			}
			conditions, _ := op.Value.(map[string]any)
			if c, ok := conditions[publicIPv4Metric].(string); ok && c != "" {
				return c
			}
		}
	}
	t.Fatalf("no metricCostConditions entry for %s in %s", publicIPv4Metric, publicIPv4Component)
	return ""
}

// TestPublicIPv4LimitChargesOnlyPublicIPv4Claims renders the IPClaim quota
// policy with the quota engine IPAM's admission plugin embeds. A Milo
// dependency that drops request conditions charges every claim against the
// public IPv4 limit, which once refused private and IPv6 claims in staging.
func TestPublicIPv4LimitChargesOnlyPublicIPv4Claims(t *testing.T) {
	policy := &quotav1alpha1.ClaimCreationPolicy{}
	policy.Name = "ipam-miloapis-com-ipclaim"
	policy.Spec.Target.ResourceClaimTemplate.Spec.Requests = []quotav1alpha1.ResourceRequestTemplate{
		{ResourceType: ipClaimsMetric, Amount: 1},
		{ResourceType: publicIPv4Metric, Amount: 1, Condition: publicIPv4Condition(t)},
	}

	celEngine, err := engine.NewCELEngine()
	if err != nil {
		t.Fatalf("NewCELEngine: %v", err)
	}
	templates := engine.NewTemplateEngine(celEngine, logr.Discard())

	cases := []struct {
		name string
		spec map[string]any
		want []string
	}{
		{"public IPv4", map[string]any{"ipFamily": "IPv4", "addressScope": "Public"}, []string{ipClaimsMetric, publicIPv4Metric}},
		{"private IPv4", map[string]any{"ipFamily": "IPv4", "addressScope": "Private"}, []string{ipClaimsMetric}},
		{"unmarked IPv4", map[string]any{"ipFamily": "IPv4"}, []string{ipClaimsMetric}},
		{"public IPv6", map[string]any{"ipFamily": "IPv6", "addressScope": "Public"}, []string{ipClaimsMetric}},
		{"unmarked IPv6", map[string]any{"ipFamily": "IPv6"}, []string{ipClaimsMetric}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trigger := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "ipam.miloapis.com/v1alpha1",
				"kind":       "IPClaim",
				"metadata":   map[string]any{"name": "claim", "namespace": "default"},
				"spec":       tc.spec,
			}}
			claim, err := templates.RenderClaim(policy, &engine.EvaluationContext{Object: trigger})
			if err != nil {
				t.Fatalf("RenderClaim: %v", err)
			}
			var got []string
			for _, r := range claim.Spec.Requests {
				got = append(got, r.ResourceType)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("charged %v, want %v", got, tc.want)
			}
		})
	}
}
