package main

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestAllocationListFilters(t *testing.T) {
	claimed := newAllocation("a", "tenant-endpoint-ipv4", "10.1.0.0/26", "us-west-v4", "claim-a")
	claimed.Spec.Scope = map[string]ipamv1alpha1.ScopeRef{
		"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
	}
	retained := newAllocation("b", "tenant-endpoint-ipv4", "10.1.0.64/26", "us-west-v4", "")
	reserved := newAllocation("c", "", "10.1.255.0/24", "us-west-v4", "")
	reserved.Spec.Purpose = ipamv1alpha1.PurposeReservation
	cs := newFakeClientset(claimed, retained, reserved)

	cases := []struct {
		name  string
		flags map[string]string
		scope []string
		want  []string
		gone  []string
	}{
		{name: "unfiltered", want: []string{"a", "b", "c"}},
		{name: "unclaimed is the leak check", flags: map[string]string{"unclaimed": "true"}, want: []string{"b", "c"}, gone: []string{"a"}},
		{name: "by purpose", flags: map[string]string{"purpose": "reservation"}, want: []string{"c"}, gone: []string{"a", "b"}},
		{name: "by class", flags: map[string]string{"class": "tenant-endpoint-ipv4"}, want: []string{"a", "b"}, gone: []string{"c"}},
		{name: "by scope", scope: []string{"network=default"}, want: []string{"a"}, gone: []string{"b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ta := newTestApp(cs, &globalOptions{output: outputName, color: "never"})
			cmd := newAllocationListCommand(ta.app)
			for k, v := range tc.flags {
				if err := cmd.Flags().Set(k, v); err != nil {
					t.Fatal(err)
				}
			}
			for _, s := range tc.scope {
				if err := cmd.Flags().Set("scope", s); err != nil {
					t.Fatal(err)
				}
			}
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatal(err)
			}
			out := ta.out.String()
			for _, name := range tc.want {
				if !strings.Contains(out, "ipallocation/"+name) {
					t.Errorf("missing %q:\n%s", name, out)
				}
			}
			for _, name := range tc.gone {
				if strings.Contains(out, "ipallocation/"+name) {
					t.Errorf("filter did not exclude %q:\n%s", name, out)
				}
			}
		})
	}
}

func TestAllocationListRejectsAnUnknownPurpose(t *testing.T) {
	ta := newTestApp(newFakeClientset(), nil)
	cmd := newAllocationListCommand(ta.app)
	if err := cmd.Flags().Set("purpose", "borrowed"); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, nil)
	if err == nil || toCLIError(err).code != exitUsage {
		t.Fatalf("error = %v, want usage(%d)", err, exitUsage)
	}
}

// The claim column distinguishes reserved space from a retained address: the
// two look identical on the wire and mean different things to an operator.
func TestAllocationClaimName(t *testing.T) {
	cases := []struct {
		name    string
		purpose ipamv1alpha1.AllocationPurpose
		claim   string
		want    string
	}{
		{name: "bound", purpose: ipamv1alpha1.PurposeClaim, claim: "web-vip", want: "web-vip"},
		{name: "retained", purpose: ipamv1alpha1.PurposeClaim, want: "— (retained)"},
		{name: "reserved", purpose: ipamv1alpha1.PurposeReservation, want: "— (reserved)"},
		{name: "pool carve", purpose: ipamv1alpha1.PurposePoolCarve, want: "— (child pool)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			al := newAllocation("a", "c", "10.0.0.0/24", "p", tc.claim)
			al.Spec.Purpose = tc.purpose
			if got := allocationClaimName(al); got != tc.want {
				t.Fatalf("allocationClaimName = %q, want %q", got, tc.want)
			}
		})
	}
}

// Releasing an allocation that still has a claim would leave the claim and its
// address disagreeing, so it is refused and the claim command offered instead.
func TestAllocationReleaseRefusesABoundAllocation(t *testing.T) {
	cs := newFakeClientset(newAllocation("alloc-1", "c", "10.0.0.0/26", "us-west-v4", "web-vip"))
	ta := newTestApp(cs, nil)
	cmd := newAllocationReleaseCommand(ta.app)
	err := cmd.RunE(cmd, []string{"alloc-1"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	ce := toCLIError(err)
	if ce.code != exitConflict {
		t.Fatalf("code = %d, want conflict(%d)", ce.code, exitConflict)
	}
	if !strings.Contains(ce.fix, "claim release web-vip") {
		t.Errorf("fix does not offer the claim release: %q", ce.fix)
	}
}

// The deliberate hand-back that reclaim policy Retain defers to.
func TestAllocationReleaseFreesARetainedAddress(t *testing.T) {
	cs := newFakeClientset(newAllocation("alloc-1", "c", "10.0.0.0/26", "us-west-v4", ""))
	ta := newTestApp(cs, nil)
	cmd := newAllocationReleaseCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"alloc-1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "Released 10.0.0.0/26 back to pool") {
		t.Errorf("unexpected output:\n%s", ta.out.String())
	}
	if _, err := cs.IpamV1alpha1().IPAllocations("default").Get(t.Context(), "alloc-1", metav1.GetOptions{}); err == nil {
		t.Fatal("allocation was not deleted")
	}
}

func TestAllocationReleaseDryRun(t *testing.T) {
	cs := newFakeClientset(newAllocation("alloc-1", "c", "10.0.0.0/26", "us-west-v4", ""))
	ta := newTestApp(cs, nil)
	cmd := newAllocationReleaseCommand(ta.app)
	if err := cmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, []string{"alloc-1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.err.String(), "Dry run") {
		t.Errorf("missing dry-run banner:\n%s", ta.err.String())
	}
	if _, err := cs.IpamV1alpha1().IPAllocations("default").Get(t.Context(), "alloc-1", metav1.GetOptions{}); err != nil {
		t.Fatal("dry run deleted the allocation")
	}
}

// The reverse lookup answers the incident question: what is this address, and
// who holds it.
func TestAddressShowNamesTheHolder(t *testing.T) {
	class := newClass("public-unicast-ipv4", ipamv1alpha1.IPv4, 1)
	class.Spec.Routing = ipamv1alpha1.RoutingSpec{
		Internal: ipamv1alpha1.InternalRoutingHost,
		External: ipamv1alpha1.ExternalRoutingAggregate,
	}
	al := newAllocation("alloc-1", "public-unicast-ipv4", "198.51.100.11/32", "public-v4", "web-vip")
	al.Status.Address = "198.51.100.11"
	al.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimRetain
	al.Spec.OwnerRef = &ipamv1alpha1.ObjectRef{
		APIGroup: "compute.datumapis.com", Kind: "Instance", Name: "hello-0",
	}
	cs := newFakeClientset(class, al)
	ta := newTestApp(cs, nil)

	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"198.51.100.11"}); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{
		"198.51.100.11", "public-unicast-ipv4", "public-v4",
		"Instance hello-0", "via claim web-vip", "Retain", "internal=Host",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("answer missing %q:\n%s", want, out)
		}
	}
}

// A host inside a claimed block still has an answer: the tightest enclosing
// allocation is its holder.
func TestAddressShowFallsBackToTheEnclosingBlock(t *testing.T) {
	wide := newAllocation("wide", "c", "10.1.0.0/16", "p", "claim-wide")
	tight := newAllocation("tight", "c", "10.1.2.0/24", "p", "claim-tight")
	cs := newFakeClientset(wide, tight)
	ta := newTestApp(cs, nil)

	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"10.1.2.7"}); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	if !strings.Contains(out, "10.1.2.7 falls inside 10.1.2.0/24") {
		t.Errorf("answer did not pick the tightest block:\n%s", out)
	}
	if strings.Contains(out, "claim-wide") {
		t.Errorf("answer used the wider block:\n%s", out)
	}
}

// Two holders of one address is the condition this command is reached for, so
// it must be reported rather than silently resolved to whichever came first.
func TestAddressShowWarnsOnMultipleHolders(t *testing.T) {
	first := newAllocation("a", "c", "10.0.0.5/32", "pool-a", "claim-a")
	second := newAllocation("b", "c", "10.0.0.5/32", "pool-b", "claim-b")
	cs := newFakeClientset(first, second)
	ta := newTestApp(cs, nil)

	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"10.0.0.5/32"}); err != nil {
		t.Fatal(err)
	}
	warning := ta.err.String()
	if !strings.Contains(warning, "held by 2 allocations") {
		t.Fatalf("no warning was emitted:\n%s", warning)
	}
	for _, want := range []string{"pool-a", "pool-b"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning does not name %q:\n%s", want, warning)
		}
	}
}

// The machine form must carry the same fact: a caller handed one object cannot
// tell a doubly-held address from a healthy one.
func TestAddressShowJSONEmitsEveryHolder(t *testing.T) {
	cs := newFakeClientset(
		newAllocation("a", "c", "10.0.0.5/32", "pool-a", "claim-a"),
		newAllocation("b", "c", "10.0.0.5/32", "pool-b", "claim-b"),
	)
	ta := newTestApp(cs, &globalOptions{output: outputJSON, color: "never"})
	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"10.0.0.5/32"}); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	if !strings.Contains(out, "IPAllocationList") {
		t.Errorf("json did not emit a list of holders:\n%s", out)
	}
	for _, want := range []string{"pool-a", "pool-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("json missing holder %q:\n%s", want, out)
		}
	}
}

// The remedy is a runbook step, so it must only offer searches that can widen
// the result. On a kubeconfig transport --project is not one of them.
func TestAddressShowNotFoundFixMatchesTheTransport(t *testing.T) {
	ta := newTestApp(newFakeClientset(), nil)
	cmd := newAddressShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"10.0.0.5"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	ce := toCLIError(err)
	if ce.code != exitNotFound {
		t.Fatalf("code = %d, want not found(%d)", ce.code, exitNotFound)
	}
	if !strings.Contains(ce.fix, "-n <namespace>") {
		t.Errorf("fix does not offer the namespace retry: %q", ce.fix)
	}
	if !strings.Contains(ce.fix, "--project will not widen it") {
		t.Errorf("fix offers --project on a transport where it does nothing: %q", ce.fix)
	}
}

func TestAddressShowRejectsANonAddress(t *testing.T) {
	ta := newTestApp(newFakeClientset(), nil)
	cmd := newAddressShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"web-vip"})
	if err == nil || toCLIError(err).code != exitUsage {
		t.Fatalf("error = %v, want usage(%d)", err, exitUsage)
	}
}
