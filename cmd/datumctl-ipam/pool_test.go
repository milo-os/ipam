package main

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestPoolListTable(t *testing.T) {
	cs := newFakeClientset(
		newPool("prod-backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 73),
		newPool("edge-v6", "2001:db8::/32", ipamv1alpha1.IPv6, 100, 4),
	)
	ta := newTestApp(cs, nil)
	cmd := newPoolListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{"NAME", "UTILIZATION", "LARGEST FREE", "prod-backbone", "73%", "edge-v6", "4%"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// TestPoolListPrefersServerStatus covers the IPv6 / child-pool case the int64
// capacity counts can't: a child pool declares no spec.ipFamily, and its IPv6
// address space is too large for the saturated capacity counts to yield a
// correct utilization. The server now reports family, utilization, and the
// largest free block in status; the table must read those rather than the
// misleading capacity-derived values.
func TestPoolListPrefersServerStatus(t *testing.T) {
	child := &ipamv1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "v6-child"},
		// No spec.ipFamily — inherited from the parent, surfaced in status.
		Spec: ipamv1alpha1.IPPoolSpec{
			ParentPoolRef: &ipamv1alpha1.LocalRef{Name: "v6-root"},
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: "2001:db8:e2e0::/48",
			IPFamily:      ipamv1alpha1.IPv6,
			// Saturated counts that would compute 0% via the old client-side
			// path; the status fields below are authoritative.
			Capacity:           ipamv1alpha1.PoolCapacity{Total: 1<<63 - 1, Allocated: 0, Available: 1<<63 - 1},
			UtilizationPercent: 6,
			LargestFreePrefix:  45,
		},
	}
	cs := newFakeClientset(child)
	ta := newTestApp(cs, nil)
	cmd := newPoolListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{"IPv6", "6%", "/45"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing server-reported %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0%") {
		t.Errorf("table used saturated capacity (0%%) instead of status utilization:\n%s", out)
	}
}

func TestPoolListName(t *testing.T) {
	cs := newFakeClientset(newPool("p1", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 0))
	ta := newTestApp(cs, &globalOptions{output: outputName, color: "never"})
	cmd := newPoolListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ta.out.String()); got != "ippool/p1" {
		t.Fatalf("name output = %q, want ippool/p1", got)
	}
}

func TestPoolTreeHierarchy(t *testing.T) {
	root := newPool("prod-backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 73)
	child := newPool("us-west", "10.1.0.0/16", ipamv1alpha1.IPv4, 100, 61)
	child.Spec.ParentPoolRef = &ipamv1alpha1.LocalRef{Name: "prod-backbone"}
	cs := newFakeClientset(root, child)
	ta := newTestApp(cs, nil)
	cmd := newPoolTreeCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	if !strings.Contains(out, "prod-backbone") || !strings.Contains(out, "us-west") {
		t.Fatalf("tree missing nodes:\n%s", out)
	}
	if !strings.Contains(out, "└─") && !strings.Contains(out, "├─") {
		t.Errorf("tree missing connectors:\n%s", out)
	}
	if !strings.Contains(out, "child pool") {
		t.Errorf("tree missing child annotation:\n%s", out)
	}
}

func TestPoolReleaseDryRunListsBlastRadius(t *testing.T) {
	root := newPool("backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 10)
	child := newPool("region", "10.1.0.0/16", ipamv1alpha1.IPv4, 100, 5)
	child.Spec.ParentPoolRef = &ipamv1alpha1.LocalRef{Name: "backbone"}
	claim := &ipamv1alpha1.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "leaf", Namespace: "default"},
		Spec:       ipamv1alpha1.IPClaimSpec{IPFamily: ipamv1alpha1.IPv4, PrefixLength: 24, PoolRef: &ipamv1alpha1.NamespacedRef{Name: "backbone"}},
	}
	cs := newFakeClientset(root, child, claim)
	ta := newTestApp(cs, nil)
	cmd := newPoolReleaseCommand(ta.app)
	_ = cmd.Flags().Set("dry-run", "true")
	if err := cmd.RunE(cmd, []string{"backbone"}); err != nil {
		t.Fatal(err)
	}
	out := ta.err.String()
	if !strings.Contains(out, "Dry run") || !strings.Contains(out, "region") || !strings.Contains(out, "leaf") {
		t.Errorf("dry-run blast radius incomplete:\n%s", out)
	}
}

func TestPoolReleaseRefusesNonInteractiveWithoutYes(t *testing.T) {
	cs := newFakeClientset(newPool("backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 0))
	// Default test app stdin is a strings.Reader (not a TTY) -> non-interactive.
	ta := newTestApp(cs, nil)
	cmd := newPoolReleaseCommand(ta.app)
	err := cmd.RunE(cmd, []string{"backbone"})
	if err == nil {
		t.Fatal("expected refusal without --yes in non-interactive mode")
	}
	if toCLIError(err).code != exitAborted {
		t.Fatalf("code = %d, want aborted(%d)", toCLIError(err).code, exitAborted)
	}
}

func TestPoolCreateDryRun(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	cmd := newPoolCreateCommand(ta.app)
	_ = cmd.Flags().Set("cidr", "10.0.0.0/8")
	_ = cmd.Flags().Set("dry-run", "true")
	if err := cmd.RunE(cmd, []string{"newpool"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.err.String(), "Dry run") {
		t.Errorf("missing dry-run banner:\n%s", ta.err.String())
	}
	// Nothing should have been created.
	if list, _ := cs.IpamV1alpha1().IPPools().List(context.TODO(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("dry-run created %d pools, want 0", len(list.Items))
	}
}
