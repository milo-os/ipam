package main

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestPrefixClaimHappyPath(t *testing.T) {
	pool := newPool("staging-backbone", "10.7.0.0/16", ipamv1alpha1.IPv4, 65536, 20316)
	cs := newFakeClientset(pool)
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		claim.Name = "staging-env-7"
		claim.Status.Phase = ipamv1alpha1.ClaimBound
		claim.Status.AllocatedCIDR = "10.7.4.0/24"
		claim.Status.BoundAllocationRef = &ipamv1alpha1.LocalRef{Name: "alloc-9f2a1c"}
		return true, claim, nil
	})

	ta := newTestApp(cs, nil)
	err := runPrefixClaim(ta.app, &claimOptions{pool: "staging-backbone", length: 24})
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	out := ta.out.String()
	for _, want := range []string{"Claimed", "10.7.4.0/24", "staging-backbone", "staging-env-7", "alloc-9f2a1c"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPrefixClaimQuietPrintsOnlyCIDR(t *testing.T) {
	pool := newPool("p", "10.7.0.0/16", ipamv1alpha1.IPv4, 65536, 0)
	cs := newFakeClientset(pool)
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		claim.Name = "x"
		claim.Status.AllocatedCIDR = "10.7.4.0/24"
		return true, claim, nil
	})
	ta := newTestApp(cs, &globalOptions{output: outputTable, color: "never", quiet: true})
	if err := runPrefixClaim(ta.app, &claimOptions{pool: "p", length: 24}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ta.out.String()); got != "10.7.4.0/24" {
		t.Fatalf("quiet output = %q, want just the CIDR", got)
	}
}

func TestPrefixClaimExhaustionExitCode(t *testing.T) {
	pool := newPool("env-pool", "10.7.0.0/22", ipamv1alpha1.IPv4, 1024, 1004)
	cs := newFakeClientset(pool)
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, statusErr(507, "InsufficientStorage", "pool full")
	})
	ta := newTestApp(cs, nil)
	err := runPrefixClaim(ta.app, &claimOptions{pool: "env-pool", length: 22})
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	ce := toCLIError(err)
	if ce.code != exitExhausted {
		t.Fatalf("exit code = %d, want %d (IPAM_POOL_EXHAUSTED)", ce.code, exitExhausted)
	}
	if !strings.Contains(ce.msg, "env-pool") {
		t.Errorf("message missing pool name: %q", ce.msg)
	}
}

func TestPrefixClaimDryRunShowsExactCIDR(t *testing.T) {
	pool := newPool("prod-backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 1024, 256)
	cs := newFakeClientset(pool)
	// The server (honoring DryRun) computes the real next block and persists
	// nothing; model that by returning a claim with the computed CIDR and
	// handling the action so the tracker stays empty.
	var sawDryRun bool
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(k8stesting.CreateActionImpl); ok {
			for _, v := range ca.CreateOptions.DryRun {
				if v == metav1.DryRunAll {
					sawDryRun = true
				}
			}
		}
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		claim.Status.Phase = ipamv1alpha1.ClaimBound
		claim.Status.AllocatedCIDR = "10.4.0.0/14"
		return true, claim, nil
	})
	ta := newTestApp(cs, nil)
	err := runPrefixClaim(ta.app, &claimOptions{pool: "prod-backbone", length: 14, dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sawDryRun {
		t.Error("dry-run claim was not sent with CreateOptions.DryRun=[All]")
	}
	// Nothing persisted (reactor handled the action; tracker stays empty).
	if list, _ := cs.IpamV1alpha1().IPClaims("default").List(context.TODO(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("dry-run persisted %d claims, want 0", len(list.Items))
	}
	errOut := ta.err.String()
	if !strings.Contains(errOut, "Dry run") {
		t.Errorf("missing dry-run banner: %q", errOut)
	}
	if !strings.Contains(errOut, "10.4.0.0/14") {
		t.Errorf("dry-run should show the exact would-be CIDR: %q", errOut)
	}
	if !strings.Contains(errOut, "utilization") {
		t.Errorf("missing projected utilization: %q", errOut)
	}
}

func TestPrefixClaimIdempotentByName(t *testing.T) {
	pool := newPool("prod-backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 1024, 256)
	existing := &ipamv1alpha1.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "app-net-3", Namespace: "default"},
		Spec: ipamv1alpha1.IPClaimSpec{
			IPFamily: ipamv1alpha1.IPv4, PrefixLength: 24,
			PoolRef: &ipamv1alpha1.NamespacedRef{Name: "prod-backbone"},
		},
		Status: ipamv1alpha1.IPClaimStatus{Phase: ipamv1alpha1.ClaimBound, AllocatedCIDR: "10.4.16.0/24"},
	}
	cs := newFakeClientset(pool, existing)
	createCalls := 0
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createCalls++
		return false, nil, nil
	})
	ta := newTestApp(cs, nil)
	err := runPrefixClaim(ta.app, &claimOptions{pool: "prod-backbone", length: 24, name: "app-net-3"})
	if err != nil {
		t.Fatal(err)
	}
	if createCalls != 0 {
		t.Fatalf("idempotent claim created %d new objects, want 0", createCalls)
	}
	out := ta.out.String()
	if !strings.Contains(out, "Reused existing") || !strings.Contains(out, "10.4.16.0/24") {
		t.Errorf("idempotent output unexpected:\n%s", out)
	}
}

func TestPrefixClaimRequiresPoolOrSelector(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	err := runPrefixClaim(ta.app, &claimOptions{length: 24})
	if err == nil {
		t.Fatal("expected usage error")
	}
	if toCLIError(err).code != exitUsage {
		t.Fatalf("code = %d, want usage(%d)", toCLIError(err).code, exitUsage)
	}
}

func TestPrefixClaimChildPoolUnsupported(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	err := runPrefixClaim(ta.app, &claimOptions{pool: "p", length: 24, childPool: "kid"})
	if err == nil || toCLIError(err).code != exitUsage {
		t.Fatalf("expected usage error for --child-pool, got %v", err)
	}
}

func TestBuildClaimSynthesizesNameWhenUnnamed(t *testing.T) {
	// The apiserver has no server-side generateName, so an unnamed claim must
	// carry an explicit metadata.name and must NOT set generateName.
	c := buildClaim(&claimOptions{pool: "p", length: 24}, "default", ipamv1alpha1.IPv4)
	if c.Name == "" {
		t.Fatal("expected a synthesized metadata.name for an unnamed claim")
	}
	if c.GenerateName != "" {
		t.Fatalf("generateName must be empty (unsupported by apiserver), got %q", c.GenerateName)
	}
	if !strings.HasPrefix(c.Name, "prefix-") {
		t.Errorf("name %q should start with prefix-", c.Name)
	}
	// A second build yields a different name (claims are not idempotent without --name).
	c2 := buildClaim(&claimOptions{pool: "p", length: 24}, "default", ipamv1alpha1.IPv4)
	if c.Name == c2.Name {
		t.Errorf("expected distinct names across unnamed claims, both = %q", c.Name)
	}
}

func TestBuildClaimRespectsExplicitName(t *testing.T) {
	c := buildClaim(&claimOptions{pool: "p", length: 24, name: "app-net-3"}, "default", ipamv1alpha1.IPv4)
	if c.Name != "app-net-3" || c.GenerateName != "" {
		t.Fatalf("explicit name not honored: name=%q generateName=%q", c.Name, c.GenerateName)
	}
}

func TestGenerateResourceNameDNS1123(t *testing.T) {
	name := generateResourceName("prefix")
	if !strings.HasPrefix(name, "prefix-") {
		t.Fatalf("missing prefix: %q", name)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			t.Fatalf("name %q contains non-DNS-1123 rune %q", name, r)
		}
	}
}

func TestPrefixClaimDefaultPathCreatesNamedClaim(t *testing.T) {
	// End-to-end: no --name. The plugin must set a name client-side so the create
	// succeeds (mirrors the live-cluster Story 1 path that previously failed with
	// "metadata.name was not generated").
	pool := newPool("us-west", "10.0.0.0/16", ipamv1alpha1.IPv4, 65536, 0)
	cs := newFakeClientset(pool)
	var sawName string
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		if claim.Name == "" {
			return true, nil, statusErr(500, "InternalError", "Internal error occurred: metadata.name was not generated")
		}
		sawName = claim.Name
		claim.Status.Phase = ipamv1alpha1.ClaimBound
		claim.Status.AllocatedCIDR = "10.0.0.0/24"
		claim.Status.BoundAllocationRef = &ipamv1alpha1.LocalRef{Name: "alloc-1"}
		return true, claim, nil
	})
	ta := newTestApp(cs, nil)
	if err := runPrefixClaim(ta.app, &claimOptions{pool: "us-west", length: 24}); err != nil {
		t.Fatalf("default claim failed: %v", err)
	}
	if sawName == "" {
		t.Fatal("plugin submitted a claim without metadata.name")
	}
	if !strings.Contains(ta.out.String(), "10.0.0.0/24") {
		t.Errorf("missing allocated CIDR in output:\n%s", ta.out.String())
	}
}

func TestPrefixListJSON(t *testing.T) {
	claim := &ipamv1alpha1.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
		Spec:       ipamv1alpha1.IPClaimSpec{IPFamily: ipamv1alpha1.IPv4, PrefixLength: 24},
		Status:     ipamv1alpha1.IPClaimStatus{Phase: ipamv1alpha1.ClaimBound, AllocatedCIDR: "10.0.0.0/24"},
	}
	cs := newFakeClientset(claim)
	ta := newTestApp(cs, &globalOptions{output: outputJSON, color: "never"})
	cmd := newPrefixListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "10.0.0.0/24") {
		t.Errorf("json output missing CIDR:\n%s", ta.out.String())
	}
}

// TestPrefixClaimInfersIPv6FromChildPoolStatus is a regression test: a child
// pool carries no spec.ipFamily (its family is derived from the carved CIDR and
// lives in status.ipFamily). Claiming from it without --family must infer IPv6
// from the pool's effective family, not default to IPv4 and reject a /64.
func TestPrefixClaimInfersIPv6FromChildPoolStatus(t *testing.T) {
	child := &ipamv1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "region-v6"},
		Spec: ipamv1alpha1.IPPoolSpec{
			ParentPoolRef: &ipamv1alpha1.LocalRef{Name: "root-v6"},
			PrefixLength:  56,
			// spec.IPFamily intentionally empty — the bug was reading this.
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: "fe00::/56",
			IPFamily:      ipamv1alpha1.IPv6,
		},
	}
	cs := newFakeClientset(child)
	var submittedFamily ipamv1alpha1.IPFamily
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		submittedFamily = claim.Spec.IPFamily
		claim.Name = "region-v6-a"
		claim.Status.Phase = ipamv1alpha1.ClaimBound
		claim.Status.AllocatedCIDR = "fe00::/64"
		return true, claim, nil
	})

	ta := newTestApp(cs, nil)
	if err := runPrefixClaim(ta.app, &claimOptions{pool: "region-v6", length: 64}); err != nil {
		t.Fatalf("claim from IPv6 child pool failed (family should be inferred from status): %v", err)
	}
	if submittedFamily != ipamv1alpha1.IPv6 {
		t.Errorf("expected claim family IPv6 inferred from pool status, got %q", submittedFamily)
	}
	if out := ta.out.String(); !strings.Contains(out, "fe00::/64") {
		t.Errorf("expected allocated CIDR in output:\n%s", out)
	}
}
