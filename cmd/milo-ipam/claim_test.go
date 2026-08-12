package main

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestBuildClaim(t *testing.T) {
	cases := []struct {
		name    string
		opts    claimOptions
		check   func(*testing.T, *ipamv1alpha1.IPClaim)
		wantErr string
	}{
		{
			name:    "neither class nor family",
			opts:    claimOptions{},
			wantErr: "a claim needs a class",
		},
		{
			name: "class and scope",
			opts: claimOptions{class: "tenant-endpoint-ipv4", scope: []string{"network=default"}},
			check: func(t *testing.T, c *ipamv1alpha1.IPClaim) {
				if c.Spec.ClassName != "tenant-endpoint-ipv4" {
					t.Errorf("class = %q", c.Spec.ClassName)
				}
				if c.Spec.Scope["network"].Name != "default" {
					t.Errorf("scope = %v", c.Spec.Scope)
				}
			},
		},
		{
			name: "family alone selects the default class",
			opts: claimOptions{family: "ipv6"},
			check: func(t *testing.T, c *ipamv1alpha1.IPClaim) {
				if c.Spec.IPFamily != ipamv1alpha1.IPv6 {
					t.Errorf("family = %q", c.Spec.IPFamily)
				}
				if c.Spec.ClassName != "" {
					t.Errorf("class = %q, want empty so the server picks the default", c.Spec.ClassName)
				}
			},
		},
		{
			name: "prefix length is a pointer so unset differs from zero",
			opts: claimOptions{class: "c", prefixLength: 26},
			check: func(t *testing.T, c *ipamv1alpha1.IPClaim) {
				if c.Spec.PrefixLength == nil || *c.Spec.PrefixLength != 26 {
					t.Errorf("prefixLength = %v, want 26", c.Spec.PrefixLength)
				}
			},
		},
		{
			name: "omitted prefix length stays nil",
			opts: claimOptions{class: "c"},
			check: func(t *testing.T, c *ipamv1alpha1.IPClaim) {
				if c.Spec.PrefixLength != nil {
					t.Errorf("prefixLength = %v, want nil (use the class default)", *c.Spec.PrefixLength)
				}
			},
		},
		{
			name: "reclaim policy is normalized to the API enum",
			opts: claimOptions{class: "c", reclaimPolicy: "retain"},
			check: func(t *testing.T, c *ipamv1alpha1.IPClaim) {
				if c.Spec.ReclaimPolicy != ipamv1alpha1.ReclaimRetain {
					t.Errorf("reclaimPolicy = %q, want Retain", c.Spec.ReclaimPolicy)
				}
			},
		},
		{
			name:    "an unknown reclaim policy is rejected here, not by the server",
			opts:    claimOptions{class: "c", reclaimPolicy: "keep"},
			wantErr: "must be Delete or Retain",
		},
		{
			name:    "invalid address",
			opts:    claimOptions{class: "c", address: "not-an-ip"},
			wantErr: "expected an IP address or CIDR",
		},
		{
			name: "a name is synthesized when none is given",
			opts: claimOptions{class: "c"},
			check: func(t *testing.T, c *ipamv1alpha1.IPClaim) {
				if !strings.HasPrefix(c.Name, "claim-") || len(c.Name) != len("claim-")+10 {
					t.Errorf("generated name = %q", c.Name)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claim, err := buildClaim(&tc.opts, "default")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q", tc.wantErr)
				}
				if toCLIError(err).code != exitUsage {
					t.Errorf("code = %d, want usage(%d)", toCLIError(err).code, exitUsage)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if claim.Kind != "IPClaim" || claim.APIVersion != apiVersion {
				t.Errorf("GVK = %s/%s", claim.APIVersion, claim.Kind)
			}
			tc.check(t, claim)
		})
	}
}

// A claim naming a class whose required roles it does not supply is caught
// before the round trip, and the message names the missing roles.
func TestClaimCreatePreflightsScope(t *testing.T) {
	class := newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1)
	class.Status.RequiredScopeRoles = []string{"location", "network"}
	cs := newFakeClientset(class)
	ta := newTestApp(cs, nil)

	err := runClaimCreate(ta.app, &claimOptions{
		class: "tenant-endpoint-ipv4",
		scope: []string{"network=default"},
	})
	if err == nil {
		t.Fatal("expected a refusal for the missing role")
	}
	ce := toCLIError(err)
	if ce.code != exitUsage {
		t.Fatalf("code = %d, want usage(%d)", ce.code, exitUsage)
	}
	if !strings.Contains(ce.msg, `"location"`) {
		t.Errorf("message does not name the missing role: %q", ce.msg)
	}
	if !strings.Contains(ce.fix, "--scope location=") {
		t.Errorf("fix does not offer the flag: %q", ce.fix)
	}
}

// A class that requires no scope must not be turned into a refusal by an empty
// RequiredScopeRoles.
func TestClaimCreateAllowsScopelessClass(t *testing.T) {
	cs := newFakeClientset(newClass("flat", ipamv1alpha1.IPv4, 1))
	ta := newTestApp(cs, nil)
	if err := runClaimCreate(ta.app, &claimOptions{class: "flat", name: "web-vip"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "web-vip") {
		t.Errorf("success output missing the claim:\n%s", ta.out.String())
	}
}

// A retry with the same --name returns the existing allocation instead of
// consuming a second address.
func TestClaimCreateIsIdempotentOnName(t *testing.T) {
	cs := newFakeClientset(
		newClass("public-unicast-ipv4", ipamv1alpha1.IPv4, 1),
		newClaim("web-vip", "public-unicast-ipv4", "198.51.100.11/32", "public-v4"),
	)
	ta := newTestApp(cs, nil)
	if err := runClaimCreate(ta.app, &claimOptions{class: "public-unicast-ipv4", name: "web-vip"}); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	if !strings.Contains(out, "Reused existing claim") {
		t.Errorf("retry did not report reuse:\n%s", out)
	}
	if !strings.Contains(out, "198.51.100.11/32") {
		t.Errorf("retry did not return the held address:\n%s", out)
	}
}

// Reusing a name for a different request is refused: returning the old address
// would be answering a question that was not asked.
func TestClaimCreateRefusesReusedNameForADifferentRequest(t *testing.T) {
	existing := newClaim("web-vip", "public-unicast-ipv4", "198.51.100.11/32", "public-v4")
	existing.Spec.Scope = map[string]ipamv1alpha1.ScopeRef{
		"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "prod"},
	}
	cs := newFakeClientset(newClass("public-unicast-ipv4", ipamv1alpha1.IPv4, 1), existing)
	ta := newTestApp(cs, nil)

	err := runClaimCreate(ta.app, &claimOptions{
		class: "public-unicast-ipv4",
		name:  "web-vip",
		scope: []string{"network=staging"},
	})
	if err == nil {
		t.Fatal("expected a refusal for the reused name")
	}
	ce := toCLIError(err)
	if ce.code != exitConflict {
		t.Fatalf("code = %d, want conflict(%d)", ce.code, exitConflict)
	}
	if !strings.Contains(ce.fix, "network=staging") || !strings.Contains(ce.fix, "network=prod") {
		t.Errorf("fix does not show both sides of the difference: %q", ce.fix)
	}
}

// An existing claim silent on a field is not evidence of a difference: it is
// derivable, and treating it as one would break the ordinary retry.
func TestClaimRequestDiffIgnoresSilence(t *testing.T) {
	requested := &ipamv1alpha1.IPClaim{Spec: ipamv1alpha1.IPClaimSpec{
		ClassName: "tenant-endpoint-ipv4",
		IPFamily:  ipamv1alpha1.IPv4,
	}}
	existing := &ipamv1alpha1.IPClaim{Spec: ipamv1alpha1.IPClaimSpec{
		// A claim that named a class carries no family in its spec.
		ClassName: "tenant-endpoint-ipv4",
	}}
	if diffs := claimRequestDiff(requested, existing); len(diffs) != 0 {
		t.Fatalf("diffs = %v, want none", diffs)
	}
}

// Dry-run must reach the server with DryRunAll, since the exact address and the
// resolved pool can only come from the allocation transaction.
func TestClaimCreateDryRunIsServerSide(t *testing.T) {
	cs := newFakeClientset(newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1))
	var sawDryRun bool
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		sawDryRun = len(create.(k8stesting.CreateActionImpl).CreateOptions.DryRun) > 0
		claim := create.GetObject().(*ipamv1alpha1.IPClaim).DeepCopy()
		claim.Status = ipamv1alpha1.IPClaimStatus{
			Phase:         ipamv1alpha1.ClaimBound,
			AllocatedCIDR: "10.1.2.0/26",
			PoolRef:       &ipamv1alpha1.LocalRef{Name: "us-west-v4"},
		}
		return true, claim, nil
	})
	ta := newTestApp(cs, nil)

	err := runClaimCreate(ta.app, &claimOptions{class: "tenant-endpoint-ipv4", dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sawDryRun {
		t.Error("create was not sent with DryRunAll")
	}
	out := ta.err.String()
	for _, want := range []string{"Dry run", "10.1.2.0/26", "us-west-v4"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q:\n%s", want, out)
		}
	}
	if ta.out.String() != "" {
		t.Errorf("dry-run wrote to the data stream: %q", ta.out.String())
	}
}

// 507 is the signature IPAM failure and the caller named no pool, so the message
// has to work backwards from the class to the pools behind it.
func TestClaimCreateExhaustionNamesTheBackingPools(t *testing.T) {
	pool := newPool("us-west-v4", "10.1.0.0/16", ipamv1alpha1.IPv4, 100, 100)
	pool.Spec.ClassNames = []string{"tenant-endpoint-ipv4"}
	cs := newFakeClientset(newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1), pool)
	cs.PrependReactor("create", "ipclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, statusErr(507, "InsufficientStorage", "pool exhausted")
	})
	ta := newTestApp(cs, nil)

	err := runClaimCreate(ta.app, &claimOptions{class: "tenant-endpoint-ipv4", scope: []string{"network=default"}})
	if err == nil {
		t.Fatal("expected an exhaustion error")
	}
	ce := toCLIError(err)
	if ce.code != exitExhausted {
		t.Fatalf("code = %d, want exhausted(%d)", ce.code, exitExhausted)
	}
	for _, want := range []string{"tenant-endpoint-ipv4", "network=default", "us-west-v4"} {
		if !strings.Contains(ce.msg, want) {
			t.Errorf("message missing %q: %q", want, ce.msg)
		}
	}
}

// -o quiet is the scripting contract: the address, and nothing else.
func TestClaimCreateQuietPrintsOnlyTheAddress(t *testing.T) {
	cs := newFakeClientset(newClass("c", ipamv1alpha1.IPv4, 1))
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim).DeepCopy()
		claim.Status.AllocatedCIDR = "10.0.0.8/32"
		claim.Status.Address = "10.0.0.8"
		return true, claim, nil
	})
	ta := newTestApp(cs, &globalOptions{output: outputTable, color: "never", quiet: true})
	if err := runClaimCreate(ta.app, &claimOptions{class: "c"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ta.out.String()); got != "10.0.0.8" {
		t.Fatalf("quiet output = %q, want the bare address", got)
	}
}

func TestClaimListFilters(t *testing.T) {
	withScope := newClaim("a", "tenant-endpoint-ipv4", "10.1.0.0/26", "us-west-v4")
	withScope.Spec.Scope = map[string]ipamv1alpha1.ScopeRef{
		"network": {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
	}
	other := newClaim("b", "public-unicast-ipv4", "198.51.100.5/32", "public-v4")
	cs := newFakeClientset(withScope, other)

	cases := []struct {
		name  string
		flags map[string]string
		scope []string
		want  []string
		gone  []string
	}{
		{name: "unfiltered", want: []string{"a", "b"}},
		{name: "by class", flags: map[string]string{"class": "public-unicast-ipv4"}, want: []string{"b"}, gone: []string{"a"}},
		{name: "by pool", flags: map[string]string{"pool": "us-west-v4"}, want: []string{"a"}, gone: []string{"b"}},
		{name: "by scope", scope: []string{"network=default"}, want: []string{"a"}, gone: []string{"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ta := newTestApp(cs, &globalOptions{output: outputName, color: "never"})
			cmd := newClaimListCommand(ta.app)
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
				if !strings.Contains(out, "ipclaim/"+name) {
					t.Errorf("missing %q:\n%s", name, out)
				}
			}
			for _, name := range tc.gone {
				if strings.Contains(out, "ipclaim/"+name) {
					t.Errorf("filter did not exclude %q:\n%s", name, out)
				}
			}
		})
	}
}

// `claim show` takes the address as well as the name, because the address is
// what a user has in hand during an incident.
func TestClaimShowByAddress(t *testing.T) {
	claim := newClaim("web-vip", "public-unicast-ipv4", "198.51.100.11/32", "public-v4")
	claim.Status.Address = "198.51.100.11"
	cs := newFakeClientset(claim)
	ta := newTestApp(cs, nil)
	cmd := newClaimShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"198.51.100.11"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "web-vip") {
		t.Fatalf("show did not resolve the address to its claim:\n%s", ta.out.String())
	}
}

// An address with no claim behind it is a Retain release, which the reverse
// lookup finds. The miss says so rather than dead-ending.
func TestClaimShowByAddressPointsAtTheReverseLookup(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	cmd := newClaimShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"198.51.100.11"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	ce := toCLIError(err)
	if ce.code != exitNotFound {
		t.Fatalf("code = %d, want not found(%d)", ce.code, exitNotFound)
	}
	if !strings.Contains(ce.fix, "address show 198.51.100.11") {
		t.Errorf("fix does not offer the reverse lookup: %q", ce.fix)
	}
}

// The release prompt must state the real fate of the address. A claim silent on
// reclaim policy inherits it from its class, so the class has to be consulted.
func TestClaimReleaseDryRunResolvesReclaimPolicyFromTheClass(t *testing.T) {
	class := newClass("retained", ipamv1alpha1.IPv4, 1)
	class.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimRetain
	claim := newClaim("web-vip", "retained", "198.51.100.11/32", "public-v4")
	claim.Status.BoundAllocationRef = &ipamv1alpha1.LocalRef{Name: "alloc-1"}
	cs := newFakeClientset(class, claim)
	ta := newTestApp(cs, nil)

	cmd := newClaimReleaseCommand(ta.app)
	if err := cmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, []string{"web-vip"}); err != nil {
		t.Fatal(err)
	}
	out := ta.err.String()
	if !strings.Contains(out, "retains (does not free)") {
		t.Errorf("dry run did not report the inherited Retain policy:\n%s", out)
	}
	if !strings.Contains(out, "alloc-1") {
		t.Errorf("dry run did not name the surviving allocation:\n%s", out)
	}
}

func TestClaimReleaseDeletesAndReportsRetention(t *testing.T) {
	class := newClass("retained", ipamv1alpha1.IPv4, 1)
	class.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimRetain
	cs := newFakeClientset(class, newClaim("web-vip", "retained", "198.51.100.11/32", "public-v4"))
	// Non-interactive stdin auto-confirms a single-claim release.
	ta := newTestApp(cs, nil)
	cmd := newClaimReleaseCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"web-vip"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "allocation release") {
		t.Errorf("release did not say the address is still held:\n%s", ta.out.String())
	}
	if _, err := cs.IpamV1alpha1().IPClaims("default").Get(t.Context(), "web-vip", metav1.GetOptions{}); err == nil {
		t.Fatal("claim was not deleted")
	}
}

func TestClaimReleaseNotFound(t *testing.T) {
	ta := newTestApp(newFakeClientset(), nil)
	cmd := newClaimReleaseCommand(ta.app)
	err := cmd.RunE(cmd, []string{"nope"})
	if err == nil || toCLIError(err).code != exitNotFound {
		t.Fatalf("error = %v, want not found(%d)", err, exitNotFound)
	}
}
