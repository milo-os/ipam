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

func TestClaimCreateHappyPath(t *testing.T) {
	cs := newFakeClientset(
		newPool("us-central-1-tenant", "10.4.0.0/14", ipamv1alpha1.IPv4, 262144, 81000),
		newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 3),
	)
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		claim.Status.Phase = ipamv1alpha1.ClaimBound
		claim.Status.Address = "10.4.16.9"
		claim.Status.AllocatedCIDR = "10.4.16.9/32"
		// The pool is a result, not an input: the server resolves it from the
		// class and the claim's scope and reports it in status.
		claim.Status.PoolRef = &ipamv1alpha1.LocalRef{Name: "us-central-1-tenant"}
		claim.Status.BoundAllocationRef = &ipamv1alpha1.LocalRef{Name: "alloc-9f2a1c"}
		return true, claim, nil
	})

	ta := newTestApp(cs, nil)
	err := runClaimCreate(ta.app, &claimOptions{
		class: "tenant-endpoint-ipv4",
		scope: []string{"network=default", "location=us-central-1"},
	})
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	out := ta.out.String()
	for _, want := range []string{
		"Claimed", "10.4.16.9", "tenant-endpoint-ipv4",
		"location=us-central-1 network=default", "us-central-1-tenant", "alloc-9f2a1c",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// The claim path inverts under the class model: the scope the caller typed must
// reach the server as a well-formed role→ref map, and no pool may be named.
func TestBuildClaimSubmitsScopeAndNoPool(t *testing.T) {
	claim, err := buildClaim(&claimOptions{
		class: "tenant-endpoint-ipv4",
		scope: []string{"network=default", "location=us-central-1"},
		owner: "Instance.compute.datumapis.com/hello-0",
	}, "app-team")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Spec.ClassName != "tenant-endpoint-ipv4" {
		t.Errorf("className = %q", claim.Spec.ClassName)
	}
	if len(claim.Spec.Scope) != 2 {
		t.Fatalf("scope = %v, want two roles", claim.Spec.Scope)
	}
	net := claim.Spec.Scope["network"]
	if net.Name != "default" || net.Kind != "Network" || net.APIGroup != "networking.datumapis.com" {
		t.Errorf("network ref = %+v", net)
	}
	if net.UID != "" {
		t.Errorf("the CLI must not invent a UID: %+v", net)
	}
	if claim.Spec.OwnerRef == nil || claim.Spec.OwnerRef.Name != "hello-0" {
		t.Errorf("ownerRef = %+v", claim.Spec.OwnerRef)
	}
	if claim.Spec.PrefixLength != nil {
		t.Errorf("prefixLength should be nil so the class default applies, got %d", *claim.Spec.PrefixLength)
	}
}

// A claim naming neither a class nor a family has nothing to resolve, and the
// error has to point at the command that lists what can be claimed.
func TestClaimCreateRequiresClassOrFamily(t *testing.T) {
	_, err := buildClaim(&claimOptions{scope: []string{"network=default"}}, "default")
	if err == nil {
		t.Fatal("expected a usage error")
	}
	ce := toCLIError(err)
	if ce.code != exitUsage {
		t.Fatalf("code = %d, want usage(%d)", ce.code, exitUsage)
	}
	if !strings.Contains(ce.msg, "class list") {
		t.Errorf("error should point at `class list`: %q", ce.msg)
	}
}

func TestClaimCreateFamilyOnlySelectsDefaultClass(t *testing.T) {
	claim, err := buildClaim(&claimOptions{family: "ipv6"}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Spec.ClassName != "" || claim.Spec.IPFamily != ipamv1alpha1.IPv6 {
		t.Fatalf("spec = %+v, want an empty class and IPv6", claim.Spec)
	}
}

func TestClaimCreatePrefixLengthIsAPointer(t *testing.T) {
	claim, err := buildClaim(&claimOptions{class: "c", prefixLength: 26}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Spec.PrefixLength == nil || *claim.Spec.PrefixLength != 26 {
		t.Fatalf("prefixLength = %v, want 26", claim.Spec.PrefixLength)
	}
}

func TestClaimCreateQuietPrintsOnlyAddress(t *testing.T) {
	cs := newFakeClientset(newClass("c", ipamv1alpha1.IPv4, 1))
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		claim.Status.Address = "10.7.4.9"
		claim.Status.AllocatedCIDR = "10.7.4.9/32"
		return true, claim, nil
	})
	ta := newTestApp(cs, &globalOptions{output: outputTable, color: "never", quiet: true})
	if err := runClaimCreate(ta.app, &claimOptions{class: "c"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ta.out.String()); got != "10.7.4.9" {
		t.Fatalf("quiet output = %q, want just the address", got)
	}
}

// Exhaustion is the signature failure, and under the class model the caller
// never named a pool — so the message must name the class and the pools behind
// it rather than echoing a pool the user did not choose.
func TestClaimCreateExhaustionNamesClassAndPools(t *testing.T) {
	pool := newPool("us-central-1-tenant", "10.4.0.0/14", ipamv1alpha1.IPv4, 1024, 1024)
	pool.Spec.ClassNames = []string{"tenant-endpoint-ipv4"}
	cs := newFakeClientset(pool, newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1))
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, statusErr(507, "InsufficientStorage", "pool full")
	})
	ta := newTestApp(cs, nil)
	err := runClaimCreate(ta.app, &claimOptions{
		class: "tenant-endpoint-ipv4",
		scope: []string{"location=us-central-1"},
	})
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	ce := toCLIError(err)
	if ce.code != exitExhausted {
		t.Fatalf("exit code = %d, want %d (IPAM_POOL_EXHAUSTED)", ce.code, exitExhausted)
	}
	for _, want := range []string{"tenant-endpoint-ipv4", "location=us-central-1", "us-central-1-tenant"} {
		if !strings.Contains(ce.msg, want) {
			t.Errorf("message missing %q: %q", want, ce.msg)
		}
	}
}

func TestClaimCreateDryRunShowsResolvedPool(t *testing.T) {
	cs := newFakeClientset(
		newPool("us-central-1-tenant", "10.4.0.0/14", ipamv1alpha1.IPv4, 1024, 256),
		newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1),
	)
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
		claim.Status.Address = "10.4.0.7"
		claim.Status.PoolRef = &ipamv1alpha1.LocalRef{Name: "us-central-1-tenant"}
		return true, claim, nil
	})
	ta := newTestApp(cs, nil)
	err := runClaimCreate(ta.app, &claimOptions{class: "tenant-endpoint-ipv4", dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sawDryRun {
		t.Error("dry-run claim was not sent with CreateOptions.DryRun=[All]")
	}
	if list, _ := cs.IpamV1alpha1().IPClaims("default").List(context.TODO(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("dry-run persisted %d claims, want 0", len(list.Items))
	}
	errOut := ta.err.String()
	for _, want := range []string{"Dry run", "10.4.0.7", "us-central-1-tenant"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, errOut)
		}
	}
}

func TestClaimCreateIdempotentByName(t *testing.T) {
	existing := &ipamv1alpha1.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "web-vip", Namespace: "default"},
		Spec:       ipamv1alpha1.IPClaimSpec{ClassName: "public-unicast-ipv4"},
		Status: ipamv1alpha1.IPClaimStatus{
			Phase: ipamv1alpha1.ClaimBound, Address: "198.51.100.11",
			PoolRef: &ipamv1alpha1.LocalRef{Name: "public-v4"},
		},
	}
	cs := newFakeClientset(existing)
	createCalls := 0
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createCalls++
		return false, nil, nil
	})
	ta := newTestApp(cs, nil)
	err := runClaimCreate(ta.app, &claimOptions{class: "public-unicast-ipv4", name: "web-vip"})
	if err != nil {
		t.Fatal(err)
	}
	if createCalls != 0 {
		t.Fatalf("idempotent claim created %d new objects, want 0", createCalls)
	}
	out := ta.out.String()
	if !strings.Contains(out, "Reused existing") || !strings.Contains(out, "198.51.100.11") {
		t.Errorf("idempotent output unexpected:\n%s", out)
	}
}

func TestBuildClaimSynthesizesNameWhenUnnamed(t *testing.T) {
	// The apiserver has no server-side generateName, so an unnamed claim must
	// carry an explicit metadata.name and must NOT set generateName.
	c, err := buildClaim(&claimOptions{class: "c"}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name == "" || c.GenerateName != "" {
		t.Fatalf("name=%q generateName=%q", c.Name, c.GenerateName)
	}
	if !strings.HasPrefix(c.Name, "claim-") {
		t.Errorf("name %q should start with claim-", c.Name)
	}
	c2, _ := buildClaim(&claimOptions{class: "c"}, "default")
	if c.Name == c2.Name {
		t.Errorf("expected distinct names across unnamed claims, both = %q", c.Name)
	}
}

func TestBuildClaimRespectsExplicitName(t *testing.T) {
	c, err := buildClaim(&claimOptions{class: "c", name: "web-vip"}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "web-vip" || c.GenerateName != "" {
		t.Fatalf("explicit name not honored: name=%q generateName=%q", c.Name, c.GenerateName)
	}
}

func TestGenerateResourceNameDNS1123(t *testing.T) {
	name := generateResourceName("claim")
	if !strings.HasPrefix(name, "claim-") {
		t.Fatalf("missing prefix: %q", name)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			t.Fatalf("name %q contains non-DNS-1123 rune %q", name, r)
		}
	}
}

func TestClaimListFiltersByClassAndScope(t *testing.T) {
	cs := newFakeClientset(
		newClaim("a", "tenant-endpoint-ipv4", "10.4.0.1", map[string]string{"network": "default"}),
		newClaim("b", "tenant-endpoint-ipv4", "10.4.0.2", map[string]string{"network": "other"}),
		newClaim("c", "public-unicast-ipv4", "198.51.100.1", nil),
	)
	ta := newTestApp(cs, &globalOptions{output: outputName, color: "never"})
	cmd := newClaimListCommand(ta.app)
	_ = cmd.Flags().Set("class", "tenant-endpoint-ipv4")
	_ = cmd.Flags().Set("scope", "network=default")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ta.out.String()); got != "ipclaim/a" {
		t.Fatalf("filtered list = %q, want just ipclaim/a", got)
	}
}

func TestClaimListJSON(t *testing.T) {
	cs := newFakeClientset(newClaim("c1", "tenant-endpoint-ipv4", "10.0.0.7", nil))
	ta := newTestApp(cs, &globalOptions{output: outputJSON, color: "never"})
	cmd := newClaimListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "10.0.0.7") {
		t.Errorf("json output missing address:\n%s", ta.out.String())
	}
}

func TestClaimShowResolvesByAddress(t *testing.T) {
	cs := newFakeClientset(newClaim("web-vip", "public-unicast-ipv4", "198.51.100.11", nil))
	claim, err := resolveClaim(cs, "default", "198.51.100.11")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Name != "web-vip" {
		t.Fatalf("resolved %q, want web-vip", claim.Name)
	}
}

// --name makes a *retry* idempotent. It used to make any reuse of the name
// idempotent: a second create naming a different class or scope was answered
// with the first claim's address, and the success output rendered the existing
// claim's class and scope, so it read as confirmation of what was asked for.
func TestClaimCreateRefusesAReusedNameForADifferentRequest(t *testing.T) {
	existing := newClaim("web-vip", "public-unicast-ipv4", "198.51.100.11",
		map[string]string{"network": "alpha"})
	cs := newFakeClientset(existing)
	ta := newTestApp(cs, nil)
	cmd := newClaimCreateCommand(ta.app)
	_ = cmd.Flags().Set("name", "web-vip")
	_ = cmd.Flags().Set("class", "public-unicast-ipv4")
	_ = cmd.Flags().Set("scope", "network=beta")

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatalf("reusing a name for a different scope succeeded; output:\n%s", ta.out.String())
	}
	ce, ok := err.(*cliError)
	if !ok {
		t.Fatalf("error is %T, want *cliError", err)
	}
	if ce.code != exitConflict {
		t.Errorf("exit code = %d, want %d (the flags are well-formed; the name is taken)", ce.code, exitConflict)
	}
	// Naming the field that differs is the point: "already exists" alone leaves
	// the reader unable to tell a stale name from a typo'd scope.
	if !strings.Contains(ce.fix, "network=beta") || !strings.Contains(ce.fix, "network=alpha") {
		t.Errorf("fix does not name the scope on both sides:\n%s", ce.fix)
	}
	if strings.Contains(ta.out.String(), "198.51.100.11") {
		t.Errorf("the existing address was rendered as an answer:\n%s", ta.out.String())
	}
}

// The other half, and the one that breaks if the comparison is too eager: a
// genuine retry must still be idempotent. The server defaults fields the request
// left empty, so an omitted flag is a question not asked, never a mismatch.
func TestClaimCreateReusedNameStaysIdempotentForTheSameRequest(t *testing.T) {
	existing := newClaim("web-vip", "public-unicast-ipv4", "198.51.100.11",
		map[string]string{"network": "alpha"})
	// Fields the caller never supplied but the server resolved.
	existing.Spec.PrefixLength = int32Ptr(32)
	existing.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimDelete

	for _, tc := range []struct {
		name  string
		flags map[string]string
	}{
		{"same class and scope", map[string]string{"class": "public-unicast-ipv4", "scope": "network=alpha"}},
		{"scope omitted", map[string]string{"class": "public-unicast-ipv4"}},
		{"class omitted, family only", map[string]string{"family": "ipv4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := newFakeClientset(existing.DeepCopy())
			ta := newTestApp(cs, nil)
			cmd := newClaimCreateCommand(ta.app)
			_ = cmd.Flags().Set("name", "web-vip")
			for f, v := range tc.flags {
				_ = cmd.Flags().Set(f, v)
			}
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatalf("a retry was refused: %v", err)
			}
			if !strings.Contains(ta.out.String(), "Reused existing claim") {
				t.Errorf("retry did not report reuse:\n%s", ta.out.String())
			}
		})
	}
}

// The address-form claim lookup used to offer `address show` as the way to find
// an address "allocated in another project". It is not: `address show` reads the
// same tenant keyspace this search just read, so following it reached a second
// not-found that pointed back here. The remedy must be scoped to the case it
// actually solves — an address held with no claim — and must not promise a
// cross-project search that does not exist.
func TestClaimAddressLookupOffersNoCrossProjectRemedy(t *testing.T) {
	cs := newFakeClientset(newClaim("web-vip", "public-unicast-ipv4", "198.51.100.11", nil))
	_, err := resolveClaim(cs, "default", "203.0.113.9")
	if err == nil {
		t.Fatal("expected a not-found for an address no claim holds")
	}
	ce, ok := err.(*cliError)
	if !ok {
		t.Fatalf("error is %T, want *cliError", err)
	}
	if ce.code != exitNotFound {
		t.Errorf("exit code = %d, want %d", ce.code, exitNotFound)
	}
	// The one case the reverse lookup does answer.
	if !strings.Contains(ce.fix, "datumctl ipam address show 203.0.113.9") {
		t.Errorf("fix does not name the reverse lookup:\n%s", ce.fix)
	}
	if !strings.Contains(ce.fix, "Retain") {
		t.Errorf("fix does not scope the reverse lookup to the held-without-a-claim case:\n%s", ce.fix)
	}
	// The remedies that cannot work. "--project" is the specific fabrication:
	// nothing this CLI owns re-targets the tenant on a direct transport, and no
	// cross-project search exists on either.
	for _, banned := range []string{"--project", "widen the search", "allocation list"} {
		if strings.Contains(ce.fix, banned) {
			t.Errorf("fix names %q, which cannot answer a cross-project lookup:\n%s", banned, ce.fix)
		}
	}
}

// A claim under Retain does not free its address on release, and the CLI must
// say so before the user confirms rather than after.
func TestClaimReleaseDryRunNamesRetention(t *testing.T) {
	claim := newClaim("retained", "public-unicast-ipv4", "198.51.100.11", nil)
	claim.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimRetain
	claim.Status.BoundAllocationRef = &ipamv1alpha1.LocalRef{Name: "alloc-1"}
	cs := newFakeClientset(claim)
	ta := newTestApp(cs, nil)
	cmd := newClaimReleaseCommand(ta.app)
	_ = cmd.Flags().Set("dry-run", "true")
	if err := cmd.RunE(cmd, []string{"retained"}); err != nil {
		t.Fatal(err)
	}
	out := ta.err.String()
	if !strings.Contains(out, "retains (does not free)") || !strings.Contains(out, "alloc-1") {
		t.Errorf("release preview should say the address survives:\n%s", out)
	}
}

// The server rejects a claim missing a role its class requires rather than
// widening the comparison, so the CLI catches it first and says which flag is
// missing. IPClassStatus.RequiredScopeRoles is what makes this one GET instead
// of a walk up the parent chain.
func TestClaimCreatePreflightsRequiredScopeRoles(t *testing.T) {
	class := newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 3)
	class.Status.RequiredScopeRoles = []string{"network", "location"}
	cs := newFakeClientset(class)
	createCalls := 0
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createCalls++
		return false, nil, nil
	})
	ta := newTestApp(cs, nil)
	err := runClaimCreate(ta.app, &claimOptions{
		class: "tenant-endpoint-ipv4",
		scope: []string{"network=default"},
	})
	if err == nil {
		t.Fatal("expected a usage error for the missing location role")
	}
	ce := toCLIError(err)
	if ce.code != exitUsage {
		t.Fatalf("code = %d, want usage(%d)", ce.code, exitUsage)
	}
	if !strings.Contains(ce.msg, "location") {
		t.Errorf("message should name the missing role: %q", ce.msg)
	}
	if !strings.Contains(ce.fix, "--scope location=") {
		t.Errorf("fix should give the flag to add: %q", ce.fix)
	}
	if createCalls != 0 {
		t.Fatalf("preflight should have avoided the round trip, got %d creates", createCalls)
	}
}

func TestClaimCreatePreflightPassesWhenScopeComplete(t *testing.T) {
	class := newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 3)
	class.Status.RequiredScopeRoles = []string{"network"}
	cs := newFakeClientset(class)
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		claim.Status.Address = "10.4.0.1"
		return true, claim, nil
	})
	ta := newTestApp(cs, nil)
	err := runClaimCreate(ta.app, &claimOptions{
		class: "tenant-endpoint-ipv4",
		scope: []string{"network=default"},
	})
	if err != nil {
		t.Fatalf("complete scope should pass the preflight: %v", err)
	}
}

// A preflight that blocks a valid request is worse than no preflight, so a
// class the CLI cannot read leaves the decision to the server.
func TestClaimCreatePreflightDegradesOnClassReadFailure(t *testing.T) {
	cs := newFakeClientset()
	cs.PrependReactor("get", "ipclasses", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, statusErr(403, "Forbidden", "classes are not readable here")
	})
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		claim.Status.Address = "10.4.0.1"
		return true, claim, nil
	})
	ta := newTestApp(cs, nil)
	if err := runClaimCreate(ta.app, &claimOptions{class: "tenant-endpoint-ipv4"}); err != nil {
		t.Fatalf("an unreadable class must not block the claim: %v", err)
	}
}

// A mistyped class is caught before the create, with the classes that do exist.
func TestClaimCreatePreflightCatchesUnknownClass(t *testing.T) {
	cs := newFakeClientset(newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1))
	ta := newTestApp(cs, nil)
	err := runClaimCreate(ta.app, &claimOptions{class: "tenant-endpoint-ip4", scope: []string{"network=default"}})
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

// An owner UID takes no part in allocation identity, so unlike a scope UID the
// CLI records it whenever the caller supplies one.
func TestBuildClaimRecordsOwnerUID(t *testing.T) {
	c, err := buildClaim(&claimOptions{
		class: "c",
		owner: "Instance.compute.datumapis.com/hello-0#4f2a1c9e-bb01",
	}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.OwnerRef.UID != "4f2a1c9e-bb01" {
		t.Fatalf("ownerRef = %+v, want the UID recorded", c.Spec.OwnerRef)
	}
	if c.Spec.OwnerRef.Name != "hello-0" {
		t.Fatalf("the UID suffix leaked into the name: %+v", c.Spec.OwnerRef)
	}
}
