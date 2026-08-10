package main

import (
	"strings"
	"testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// The reverse lookup is the operator's first question during an incident. It is
// answerable only because IPAllocationSpec now carries the class, the owner, and
// the scope alongside the pool.
func TestAddressShowAnswersWhoHoldsIt(t *testing.T) {
	alloc := newAllocation("alloc-1", "public-unicast-ipv4", "public-v4", "198.51.100.11/32", "198.51.100.11")
	alloc.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimRetain
	alloc.Spec.ClaimRef = &ipamv1alpha1.LocalRef{Name: "hello-sandbox-vip"}
	alloc.Spec.OwnerRef = &ipamv1alpha1.ObjectRef{
		APIGroup: "compute.datumapis.com", Kind: "Instance", Name: "hello-sandbox-us-central-1-0",
	}
	alloc.Spec.Scope = map[string]ipamv1alpha1.ScopeRef{
		"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
	}
	cs := newFakeClientset(alloc)
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"198.51.100.11"}); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{
		"public-unicast-ipv4",
		"Instance hello-sandbox-us-central-1-0",
		"via claim hello-sandbox-vip",
		"us-central-1",
		"Retain",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("answer missing %q:\n%s", want, out)
		}
	}
}

// Asking about a host inside a claimed block should still name the holder, and
// the tightest enclosing allocation is the right answer when blocks nest.
func TestAddressShowFallsBackToTheEnclosingBlock(t *testing.T) {
	wide := newAllocation("wide", "tenant-subnet-ipv4", "backbone", "10.4.0.0/14", "")
	narrow := newAllocation("narrow", "tenant-endpoint-ipv4", "us-central-1-tenant", "10.4.16.0/24", "")
	cs := newFakeClientset(wide, narrow)
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"10.4.16.9"}); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	if !strings.Contains(out, "falls inside 10.4.16.0/24") {
		t.Errorf("expected the most specific enclosing block:\n%s", out)
	}
	if !strings.Contains(out, "tenant-endpoint-ipv4") {
		t.Errorf("expected the narrow allocation's class:\n%s", out)
	}
}

func TestAddressShowUnallocated(t *testing.T) {
	cs := newFakeClientset(newAllocation("a", "c", "p", "10.4.16.0/24", ""))
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"192.0.2.9"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	if toCLIError(err).code != exitNotFound {
		t.Fatalf("code = %d, want notFound(%d)", toCLIError(err).code, exitNotFound)
	}
}

func TestAddressShowRejectsNonAddress(t *testing.T) {
	ta := newTestApp(newFakeClientset(), nil)
	cmd := newAddressShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"web-vip"})
	if err == nil || toCLIError(err).code != exitUsage {
		t.Fatalf("expected a usage error for a name, got %v", err)
	}
}

// --unclaimed is the Retain leak check: addresses still held with nothing
// pointing at them.
func TestAllocationListUnclaimed(t *testing.T) {
	bound := newAllocation("bound", "c", "p", "10.0.0.0/32", "10.0.0.0")
	bound.Spec.ClaimRef = &ipamv1alpha1.LocalRef{Name: "claim-1"}
	retained := newAllocation("retained", "c", "p", "10.0.0.1/32", "10.0.0.1")
	cs := newFakeClientset(bound, retained)
	ta := newTestApp(cs, &globalOptions{output: outputName, color: "never"})
	cmd := newAllocationListCommand(ta.app)
	_ = cmd.Flags().Set("unclaimed", "true")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ta.out.String()); got != "ipallocation/retained" {
		t.Fatalf("unclaimed list = %q, want just ipallocation/retained", got)
	}
}

// An allocation with a live claim must not be deleted out from under it, or the
// claim and its address disagree.
func TestAllocationReleaseRefusesWhileClaimed(t *testing.T) {
	alloc := newAllocation("a", "c", "p", "10.0.0.0/32", "10.0.0.0")
	alloc.Spec.ClaimRef = &ipamv1alpha1.LocalRef{Name: "web-vip"}
	cs := newFakeClientset(alloc)
	ta := newTestApp(cs, nil)
	cmd := newAllocationReleaseCommand(ta.app)
	err := cmd.RunE(cmd, []string{"a"})
	if err == nil {
		t.Fatal("expected a conflict")
	}
	ce := toCLIError(err)
	if ce.code != exitConflict {
		t.Fatalf("code = %d, want conflict(%d)", ce.code, exitConflict)
	}
	if !strings.Contains(ce.fix, "claim release web-vip") {
		t.Errorf("fix should point at the claim: %q", ce.fix)
	}
}

func TestAllocationListTableNamesRetainedAndReserved(t *testing.T) {
	retained := newAllocation("retained", "c", "p", "10.0.0.1/32", "10.0.0.1")
	reserved := newAllocation("reserved", "", "p", "10.0.0.2/32", "10.0.0.2")
	reserved.Spec.Purpose = ipamv1alpha1.PurposeReservation
	cs := newFakeClientset(retained, reserved)
	ta := newTestApp(cs, nil)
	cmd := newAllocationListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	if !strings.Contains(out, "(retained)") || !strings.Contains(out, "(reserved)") {
		t.Errorf("a claimless allocation should say why it has no claim:\n%s", out)
	}
}

// The reverse lookup is what an operator runs mid-incident, so a remedy that
// cannot work is worse than none — it has the same shape as a runbook step and
// costs a real attempt to disprove. On the kubeconfig transport there is no Milo
// front gate to stamp the tenant extras, so --project changes only what the
// output prints; naming it as the fix sends the reader down a dead end.
func TestAddressNotFoundNeverOffersProjectOnKubeconfigTransport(t *testing.T) {
	cs := newFakeClientset(newAllocation("a", "c", "p", "10.4.16.0/24", ""))
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"192.0.2.9"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	ce := toCLIError(err)
	if ce.code != exitNotFound {
		t.Fatalf("code = %d, want notFound(%d)", ce.code, exitNotFound)
	}
	if strings.Contains(ce.fix, "--project <id>") {
		t.Errorf("--project must not be offered as a remedy on this transport:\n%s", ce.fix)
	}
	if !strings.Contains(ce.fix, "will not widen it") {
		t.Errorf("fix should say plainly that --project does not help here:\n%s", ce.fix)
	}
	// The namespace bound is the one that actually bites first, and retrying
	// with -n is always a real remedy.
	if !strings.Contains(ce.fix, "-n <namespace>") {
		t.Errorf("fix should offer the namespace retry:\n%s", ce.fix)
	}
	// No cluster-wide reverse lookup exists; pointing at `allocation list`
	// implies one does.
	if strings.Contains(ce.fix, "widen the search") || strings.Contains(ce.fix, "allocation list") {
		t.Errorf("fix must not imply a cluster-wide search exists:\n%s", ce.fix)
	}
}

// On the Datum transport --project *is* a real remedy: it changes the
// control-plane URL path, and Milo's front gate turns that path into the
// caller's tenant identity. The message is transport-aware for that reason.
func TestAddressNotFoundOffersProjectOnDatumTransport(t *testing.T) {
	t.Setenv("DATUM_API_HOST", "api.datum.test")
	t.Setenv("DATUM_CREDENTIALS_HELPER", "/usr/bin/true")
	t.Setenv("DATUM_PROJECT", "acme-app-team")

	cs := newFakeClientset(newAllocation("a", "c", "p", "10.4.16.0/24", ""))
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"192.0.2.9"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	fix := toCLIError(err).fix
	if !strings.Contains(fix, "--project <id>") {
		t.Errorf("--project is a real remedy on the Datum transport and should be offered:\n%s", fix)
	}
	if !strings.Contains(fix, "-n <namespace>") {
		t.Errorf("the namespace retry applies on every transport:\n%s", fix)
	}
}

// The namespace the search actually covered belongs in the error: the first
// thing a reader checks is whether they looked in the right place.
func TestAddressNotFoundNamesTheNamespaceSearched(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"192.0.2.9"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	if !strings.Contains(toCLIError(err).msg, `"default"`) {
		t.Errorf("message should name the namespace searched: %q", toCLIError(err).msg)
	}
}
