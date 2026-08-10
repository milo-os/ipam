package main

import (
	"strings"
	"testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestClassListTable(t *testing.T) {
	cs := newFakeClientset(
		newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 12),
		newClass("tenant-endpoint-ipv6", ipamv1alpha1.IPv6, 12),
	)
	ta := newTestApp(cs, nil)
	cmd := newClassListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{"NAME", "FAMILY", "POOLS", "tenant-endpoint-ipv4", "IPv6", "12"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// A class no pool offers itself to cannot satisfy any claim. Learning that from
// `list` is the whole point of surfacing the count, so the zero has to be loud
// in the cell and stated in words.
func TestClassListCallsOutClassesWithNoPools(t *testing.T) {
	cs := newFakeClientset(
		newClass("healthy", ipamv1alpha1.IPv4, 3),
		newClass("orphaned", ipamv1alpha1.IPv4, 0),
	)
	ta := newTestApp(cs, nil)
	cmd := newClassListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "0 (none)") {
		t.Errorf("zero-pool cell should read as more than a bare 0:\n%s", ta.out.String())
	}
	warn := ta.err.String()
	if !strings.Contains(warn, "orphaned") {
		t.Errorf("expected a warning naming the starved class:\n%s", warn)
	}
	if strings.Contains(warn, "healthy") {
		t.Errorf("warning should not name a backed class:\n%s", warn)
	}
}

func TestClassListFiltersByFamily(t *testing.T) {
	cs := newFakeClientset(
		newClass("v4", ipamv1alpha1.IPv4, 1),
		newClass("v6", ipamv1alpha1.IPv6, 1),
	)
	ta := newTestApp(cs, &globalOptions{output: outputName, color: "never"})
	cmd := newClassListCommand(ta.app)
	_ = cmd.Flags().Set("family", "ipv6")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ta.out.String()); got != "ipclass/v6" {
		t.Fatalf("filtered list = %q, want just ipclass/v6", got)
	}
}

func TestClassShowNamesBackingPools(t *testing.T) {
	pool := newPool("us-central-1-tenant", "10.4.0.0/14", ipamv1alpha1.IPv4, 100, 61)
	pool.Spec.ClassNames = []string{"tenant-endpoint-ipv4"}
	cs := newFakeClientset(newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1), pool)
	ta := newTestApp(cs, nil)
	cmd := newClassShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"tenant-endpoint-ipv4"}); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{"Unique within", "network", "Pools offering this class", "us-central-1-tenant", "61%"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q:\n%s", want, out)
		}
	}
}

func TestClassShowWithNoPoolsExplainsTheConsequence(t *testing.T) {
	cs := newFakeClientset(newClass("orphaned", ipamv1alpha1.IPv4, 0))
	ta := newTestApp(cs, nil)
	cmd := newClassShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"orphaned"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.err.String(), "every claim naming it fails") {
		t.Errorf("expected the consequence spelled out:\n%s", ta.err.String())
	}
}

// A mistyped class name is the likeliest 404 on this surface, so the error lists
// the classes that do exist.
func TestClassShowNotFoundListsAlternatives(t *testing.T) {
	cs := newFakeClientset(newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1))
	ta := newTestApp(cs, nil)
	cmd := newClassShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"tenant-endpoint-ip4"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	ce := toCLIError(err)
	if ce.code != exitNotFound {
		t.Fatalf("code = %d, want notFound(%d)", ce.code, exitNotFound)
	}
	if !strings.Contains(ce.fix, "tenant-endpoint-ipv4") {
		t.Errorf("fix should list the classes that exist: %q", ce.fix)
	}
}
