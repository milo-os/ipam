package main

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

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
// The server no longer reports an exact largest free prefix — status.
// largestFreePrefix was removed because computing it meant reading every
// allocation in the pool on every write. What this still pins is the half that
// survives: status.utilizationPercent is authoritative over the client-side
// figure derived from the integer capacity counts, which saturate for wide IPv6
// prefixes and would read 0% for a pool that is 6% full.
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
			// A /20 of IPv6, exact. These counts used to saturate at MaxInt64
			// and the client-side ratio computed 0%; status.utilizationPercent
			// is still the authoritative figure.
			Capacity:           ipamv1alpha1.PoolCapacity{Total: "324518553658426726783156020576256", Allocated: "0", Available: "324518553658426726783156020576256"},
			UtilizationPercent: 6,
		},
	}
	cs := newFakeClientset(child)
	ta := newTestApp(cs, nil)
	cmd := newPoolListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{"IPv6", "6%"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing server-reported %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0%") {
		t.Errorf("table used saturated capacity (0%%) instead of status utilization:\n%s", out)
	}
	// The largest-free column is now the capacity-derived estimate, which
	// cannot be computed from a saturated IPv6 count and shows as unknown.
	// Asserted so the degradation is pinned rather than discovered: if this
	// ever prints a prefix again, something is reporting a number it cannot
	// know.
	if strings.Contains(out, "/45") {
		t.Errorf("table printed an exact largest-free prefix the server no longer reports:\n%s", out)
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

// TestPoolTreePrefersServerStatus mirrors the list/show coverage: the tree must
// render family and utilization from the server-reported status (set on child
// pools and accurate for IPv6), not from spec.ipFamily and the int64 capacity
// counts that are blank on children and overflow for IPv6.
func TestPoolTreePrefersServerStatus(t *testing.T) {
	root := &ipamv1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "v6-root"},
		Spec:       ipamv1alpha1.IPPoolSpec{CIDR: "2001:db8::/32", IPFamily: ipamv1alpha1.IPv6},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "2001:db8::/32",
			IPFamily: ipamv1alpha1.IPv6, UtilizationPercent: 18},
	}
	child := &ipamv1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "v6-region"},
		// No spec.ipFamily — inherited; family comes from status.
		Spec: ipamv1alpha1.IPPoolSpec{ParentPoolRef: &ipamv1alpha1.LocalRef{Name: "v6-root"}},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: "2001:db8::/36",
			IPFamily: ipamv1alpha1.IPv6, UtilizationPercent: 12, // A saturated capacity that the old client-side path would misread.
			Capacity: ipamv1alpha1.PoolCapacity{Total: "324518553658426726783156020576256", Allocated: "0", Available: "324518553658426726783156020576256"},
		},
	}
	cs := newFakeClientset(root, child)
	ta := newTestApp(cs, nil)
	cmd := newPoolTreeCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	// The child line must carry IPv6 (from status) and 12% (from status), and
	// must not show a blank family or the saturated 0%/100%.
	if !strings.Contains(out, "v6-region") {
		t.Fatalf("tree missing child node:\n%s", out)
	}
	for _, want := range []string{"IPv6", "12% used", "18% used"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree missing server-reported %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "—     ") || strings.Contains(out, "100% used") {
		t.Errorf("tree used spec family / saturated capacity instead of status:\n%s", out)
	}
}

func TestPoolReleaseDryRunListsBlastRadius(t *testing.T) {
	root := newPool("backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 10)
	child := newPool("region", "10.1.0.0/16", ipamv1alpha1.IPv4, 100, 5)
	child.Spec.ParentPoolRef = &ipamv1alpha1.LocalRef{Name: "backbone"}
	// A retained allocation with no claim behind it: it still holds an address
	// out of the pool, so it must appear in the blast radius even though no
	// claim points at it.
	alloc := &ipamv1alpha1.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{Name: "leaf", Namespace: "default"},
		Spec: ipamv1alpha1.IPAllocationSpec{
			IPFamily:      ipamv1alpha1.IPv4,
			PoolRef:       ipamv1alpha1.LocalRef{Name: "backbone"},
			ClassName:     "tenant-endpoint-ipv4",
			Purpose:       ipamv1alpha1.PurposeClaim,
			ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
		},
		Status: ipamv1alpha1.IPAllocationStatus{AllocatedCIDR: "10.0.4.0/24"},
	}
	cs := newFakeClientset(root, child, alloc)
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
	if !strings.Contains(out, "retained") {
		t.Errorf("blast radius should say the allocation has no claim behind it:\n%s", out)
	}
}

// An IPPool is cluster-scoped and its allocations are not, so a pool hands
// addresses to any namespace that claims from it. The blast radius used to be
// computed from a single-namespace list, which reported "Blast radius: none" for
// a pool the server then refused to delete — a dry run saying safe about the one
// call an operator runs precisely because they are unsure.
func TestPoolReleaseBlastRadiusSpansNamespaces(t *testing.T) {
	pool := newPool("backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 10)
	elsewhere := newAllocation("leaf", "tenant-endpoint-ipv4", "backbone", "10.0.4.0/24", "10.0.4.0")
	elsewhere.Namespace = "someone-elses-project"

	cs := newFakeClientset(pool, elsewhere)
	ta := newTestApp(cs, nil) // the app's own namespace is "default"
	cmd := newPoolReleaseCommand(ta.app)
	_ = cmd.Flags().Set("dry-run", "true")
	if err := cmd.RunE(cmd, []string{"backbone"}); err != nil {
		t.Fatal(err)
	}
	out := ta.err.String()
	if strings.Contains(out, "none") {
		t.Errorf("dry run reported an empty blast radius for a pool held elsewhere:\n%s", out)
	}
	if !strings.Contains(out, "someone-elses-project/leaf") {
		t.Errorf("blast radius does not name the holding namespace:\n%s", out)
	}
}

// The cascade must address each object in its own namespace. Using the caller's
// namespace sends every delete to the wrong place, where the IsNotFound guard
// swallows the 404 — so the cascade reports success and the pool delete is then
// refused for allocations it claimed to have released.
func TestPoolReleaseCascadeDeletesInTheOwningNamespace(t *testing.T) {
	pool := newPool("backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 10)
	held := newAllocation("leaf", "tenant-endpoint-ipv4", "backbone", "10.0.4.0/24", "10.0.4.0")
	held.Namespace = "someone-elses-project"

	cs := newFakeClientset(pool, held)
	ta := newTestApp(cs, &globalOptions{output: outputTable, color: "never", assumeYes: true})
	cmd := newPoolReleaseCommand(ta.app)
	_ = cmd.Flags().Set("cascade", "true")
	if err := cmd.RunE(cmd, []string{"backbone"}); err != nil {
		t.Fatal(err)
	}
	_, err := cs.IpamV1alpha1().IPAllocations("someone-elses-project").
		Get(context.Background(), "leaf", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("allocation in the owning namespace survived the cascade (err = %v)", err)
	}
}

// --cascade promises to release everything under the pool. It used to stop at
// the claim, which is not the same thing: under reclaim policy Retain deleting
// the claim is exactly the operation that does NOT free the address. The
// allocation survived, the pool delete was refused, and the caller was left with
// their claim destroyed and the pool still standing — a partial, irreversible
// outcome caused by the flag meant to prevent one.
func TestPoolReleaseCascadeReleasesRetainedAllocations(t *testing.T) {
	pool := newPool("backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 10)
	claim := newClaim("kept", "tenant-endpoint-ipv4", "10.0.4.1", nil)
	held := newAllocation("leaf", "tenant-endpoint-ipv4", "backbone", "10.0.4.1/32", "10.0.4.1")
	held.Spec.ClaimRef = &ipamv1alpha1.LocalRef{Name: "kept"}
	held.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimRetain

	cs := newFakeClientset(pool, claim, held)
	ta := newTestApp(cs, &globalOptions{output: outputTable, color: "never", assumeYes: true})
	cmd := newPoolReleaseCommand(ta.app)
	_ = cmd.Flags().Set("cascade", "true")
	if err := cmd.RunE(cmd, []string{"backbone"}); err != nil {
		t.Fatal(err)
	}
	// The claim going away is not enough — Retain is defined by the allocation
	// outliving it, so the allocation is what has to be checked.
	if _, err := cs.IpamV1alpha1().IPAllocations("default").
		Get(context.Background(), "leaf", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("retained allocation survived the cascade (err = %v)", err)
	}
	if _, err := cs.IpamV1alpha1().IPClaims("default").
		Get(context.Background(), "kept", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("claim survived the cascade (err = %v)", err)
	}
}

// A caller who cannot list allocations cluster-wide gets a partial answer, and a
// partial answer must never render as an absence. This is the half that cannot
// be defended: the old code discarded the list error entirely, so a 403 and an
// empty cluster produced the same "Blast radius: none".
func TestPoolReleaseRefusesOnAPartialView(t *testing.T) {
	pool := newPool("backbone", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 10)
	cs := newFakeClientset(pool)
	// Deny only the cluster-wide list, exactly as RBAC does for a namespace-bound
	// identity; the namespaced list still succeeds and returns nothing.
	cs.PrependReactor("list", "ipallocations", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == metav1.NamespaceAll {
			return true, nil, apierrors.NewForbidden(
				ipamv1alpha1.SchemeGroupVersion.WithResource("ipallocations").GroupResource(), "", nil)
		}
		return false, nil, nil
	})

	t.Run("dry run never says none", func(t *testing.T) {
		ta := newTestApp(cs, nil)
		cmd := newPoolReleaseCommand(ta.app)
		_ = cmd.Flags().Set("dry-run", "true")
		if err := cmd.RunE(cmd, []string{"backbone"}); err != nil {
			t.Fatal(err)
		}
		out := ta.err.String()
		if strings.Contains(out, "none") {
			t.Errorf("a denied cluster-wide list rendered as an empty blast radius:\n%s", out)
		}
		if !strings.Contains(out, "UNKNOWN") {
			t.Errorf("dry run does not report the blast radius as unknown:\n%s", out)
		}
	})

	// Both the guarded release and --cascade must refuse. --cascade is the worse
	// of the two: it would delete every allocation it can see and then be refused
	// by the server for the ones it cannot, leaving a half-dismantled pool.
	for _, cascade := range []string{"false", "true"} {
		t.Run("release refuses, cascade="+cascade, func(t *testing.T) {
			ta := newTestApp(cs, &globalOptions{output: outputTable, color: "never", assumeYes: true})
			cmd := newPoolReleaseCommand(ta.app)
			_ = cmd.Flags().Set("cascade", cascade)
			err := cmd.RunE(cmd, []string{"backbone"})
			if err == nil {
				t.Fatal("released a pool whose blast radius could not be established")
			}
			ce, ok := err.(*cliError)
			if !ok {
				t.Fatalf("error is %T, want *cliError", err)
			}
			if ce.code != exitForbidden {
				t.Errorf("exit code = %d, want %d", ce.code, exitForbidden)
			}
		})
	}
}

// TestPoolCreateOffersClasses covers the field that decides whether a pool is
// reachable at all: capacity nobody offered to a class cannot satisfy a claim.
func TestPoolCreateOffersClasses(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	cmd := newPoolCreateCommand(ta.app)
	_ = cmd.Flags().Set("cidr", "10.4.0.0/14")
	_ = cmd.Flags().Set("class", "tenant-endpoint-ipv4")
	_ = cmd.Flags().Set("scope", "location=us-central-1")
	if err := cmd.RunE(cmd, []string{"us-central-1-tenant"}); err != nil {
		t.Fatal(err)
	}
	got, err := cs.IpamV1alpha1().IPPools().Get(context.TODO(), "us-central-1-tenant", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.ClassNames) != 1 || got.Spec.ClassNames[0] != "tenant-endpoint-ipv4" {
		t.Fatalf("classNames = %v, want [tenant-endpoint-ipv4]", got.Spec.ClassNames)
	}
	ref, ok := got.Spec.Scope["location"]
	if !ok {
		t.Fatalf("scope = %v, want a location role", got.Spec.Scope)
	}
	if ref.Name != "us-central-1" || ref.Kind != "Location" {
		t.Fatalf("location ref = %+v, want the well-known Location kind", ref)
	}
}

// TestPoolCreateWithoutClassWarns: a pool offering no class is legal but inert,
// and silently creating inert capacity is how a class ends up with zero pools.
func TestPoolCreateWithoutClassWarns(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	cmd := newPoolCreateCommand(ta.app)
	_ = cmd.Flags().Set("cidr", "10.0.0.0/8")
	if err := cmd.RunE(cmd, []string{"inert"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.err.String(), "no class") {
		t.Errorf("expected a warning about offering no class:\n%s", ta.err.String())
	}
}

func TestPoolCreateReservationNeedsUnit(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	cmd := newPoolCreateCommand(ta.app)
	_ = cmd.Flags().Set("cidr", "192.0.2.0/24")
	_ = cmd.Flags().Set("reserve-leading", "1")
	err := cmd.RunE(cmd, []string{"link-net"})
	if err == nil || toCLIError(err).code != exitUsage {
		t.Fatalf("expected a usage error without --reserve-unit, got %v", err)
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
