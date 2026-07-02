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

func newClass(name string, family ipamv1alpha1.IPFamily, min, max int, reclaim ipamv1alpha1.ReclaimPolicy, isDefault bool) *ipamv1alpha1.IPClass {
	c := &ipamv1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPClassSpec{
			Provisioner:          ipamv1alpha1.NativeProvisioner,
			IPFamily:             family,
			Strategy:             ipamv1alpha1.LeastUtilized,
			AllowedPrefixLengths: ipamv1alpha1.PrefixLengthRange{Min: min, Max: max},
			DefaultPrefixLength:  max,
			ReclaimPolicy:        reclaim,
			Visibility:           "shared",
		},
	}
	if isDefault {
		c.Annotations = map[string]string{ipamv1alpha1.IsDefaultClassAnnotation: "true"}
	}
	return c
}

func TestClassListTable(t *testing.T) {
	cs := newFakeClientset(
		newClass("internal-ipv4", ipamv1alpha1.IPv4, 24, 28, ipamv1alpha1.ReclaimDelete, true),
		newClass("public-egress", ipamv1alpha1.IPv4, 24, 28, ipamv1alpha1.ReclaimRetain, false),
	)
	ta := newTestApp(cs, nil)
	if err := newClassListCommand(ta.app).RunE(newClassListCommand(ta.app), nil); err != nil {
		t.Fatalf("class list failed: %v", err)
	}
	out := ta.out.String()
	for _, want := range []string{"internal-ipv4", "public-egress", "/24 – /28", "Delete", "Retain", "*"} {
		if !strings.Contains(out, want) {
			t.Errorf("class list output missing %q:\n%s", want, out)
		}
	}
}

func TestClassShowDetail(t *testing.T) {
	cs := newFakeClientset(newClass("public-egress", ipamv1alpha1.IPv4, 24, 28, ipamv1alpha1.ReclaimRetain, false))
	ta := newTestApp(cs, nil)
	cmd := newClassShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"public-egress"}); err != nil {
		t.Fatalf("class show failed: %v", err)
	}
	out := ta.out.String()
	for _, want := range []string{"public-egress", "IPv4", ipamv1alpha1.NativeProvisioner, "LeastUtilized", "Retain"} {
		if !strings.Contains(out, want) {
			t.Errorf("class show output missing %q:\n%s", want, out)
		}
	}
}

func TestClassShowNotFound(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	cmd := newClassShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"ghost"})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if toCLIError(err).code != exitNotFound {
		t.Fatalf("code = %d, want notFound(%d)", toCLIError(err).code, exitNotFound)
	}
}

func TestPrefixClaimByClass(t *testing.T) {
	cs := newFakeClientset(
		newClass("public-egress", ipamv1alpha1.IPv4, 24, 28, ipamv1alpha1.ReclaimRetain, false),
	)
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		// The class-based claim must carry spec.className and no poolRef.
		if claim.Spec.ClassName != "public-egress" {
			t.Errorf("claim className = %q, want public-egress", claim.Spec.ClassName)
		}
		if claim.Spec.PoolRef != nil {
			t.Errorf("class claim must not set poolRef, got %v", claim.Spec.PoolRef)
		}
		claim.Name = "egress-1"
		claim.Status.Phase = ipamv1alpha1.ClaimBound
		claim.Status.AllocatedCIDR = "203.0.113.0/26"
		claim.Status.BoundAllocationRef = &ipamv1alpha1.LocalRef{Name: "alloc-abc"}
		return true, claim, nil
	})
	// The success line resolves the chosen pool from the bound allocation.
	cs.PrependReactor("get", "ipallocations", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &ipamv1alpha1.IPAllocation{
			ObjectMeta: metav1.ObjectMeta{Name: "alloc-abc", Namespace: "default"},
			Spec:       ipamv1alpha1.IPAllocationSpec{PoolRef: ipamv1alpha1.LocalRef{Name: "prod-egress-us-east"}, ClassName: "public-egress"},
		}, nil
	})

	ta := newTestApp(cs, nil)
	if err := runPrefixClaim(ta.app, &claimOptions{class: "public-egress", length: 26}); err != nil {
		t.Fatalf("class claim failed: %v", err)
	}
	out := ta.out.String()
	for _, want := range []string{"Claimed", "203.0.113.0/26", `class "public-egress"`, "prod-egress-us-east"} {
		if !strings.Contains(out, want) {
			t.Errorf("class claim output missing %q:\n%s", want, out)
		}
	}
}

func TestPrefixClaimClassMutuallyExclusive(t *testing.T) {
	cs := newFakeClientset()
	ta := newTestApp(cs, nil)
	err := runPrefixClaim(ta.app, &claimOptions{class: "egress", pool: "p", length: 26})
	if err == nil {
		t.Fatal("expected usage error for --class with --pool")
	}
	if toCLIError(err).code != exitUsage {
		t.Fatalf("code = %d, want usage(%d)", toCLIError(err).code, exitUsage)
	}
}

func TestPrefixClaimByClassOmitsLength(t *testing.T) {
	// With --class and no --length, the CLI must submit anyway (the server
	// applies the class default); it must not fail on the "needs a size" guard.
	cs := newFakeClientset(newClass("public-egress", ipamv1alpha1.IPv4, 24, 28, ipamv1alpha1.ReclaimRetain, false))
	cs.PrependReactor("create", "ipclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		claim := action.(k8stesting.CreateAction).GetObject().(*ipamv1alpha1.IPClaim)
		claim.Name = "egress-2"
		claim.Status.Phase = ipamv1alpha1.ClaimBound
		claim.Status.AllocatedCIDR = "203.0.113.64/26"
		return true, claim, nil
	})
	ta := newTestApp(cs, nil)
	if err := runPrefixClaim(ta.app, &claimOptions{class: "public-egress"}); err != nil {
		t.Fatalf("class claim without --length should succeed: %v", err)
	}
	_ = context.Background()
}
