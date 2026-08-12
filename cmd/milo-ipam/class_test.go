package main

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestClassListTable(t *testing.T) {
	v4 := newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 2)
	v4.Spec.DefaultPrefixLength = 32
	v4.Annotations = map[string]string{ipamv1alpha1.IsDefaultClassAnnotation: "true"}
	v6 := newClass("tenant-subnet-ipv6", ipamv1alpha1.IPv6, 1)
	v6.Spec.ParentClassName = "site-aggregate-ipv6"
	cs := newFakeClientset(v4, v6)
	ta := newTestApp(cs, nil)

	cmd := newClassListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{"NAME", "POOLS", "tenant-endpoint-ipv4", "(default)", "/32", "site-aggregate-ipv6"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// A class no pool offers cannot satisfy any claim. The count is easy to skim
// past, so the condition is also stated in words.
func TestClassListCallsOutAClassWithNoPools(t *testing.T) {
	cs := newFakeClientset(newClass("orphan", ipamv1alpha1.IPv4, 0))
	ta := newTestApp(cs, nil)
	cmd := newClassListCommand(ta.app)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "0 (none)") {
		t.Errorf("table did not mark the empty count:\n%s", ta.out.String())
	}
	if !strings.Contains(ta.err.String(), `No pool offers "orphan"`) {
		t.Errorf("warning missing:\n%s", ta.err.String())
	}
}

func TestClassListFiltersByFamily(t *testing.T) {
	cs := newFakeClientset(
		newClass("v4", ipamv1alpha1.IPv4, 1),
		newClass("v6", ipamv1alpha1.IPv6, 1),
	)
	ta := newTestApp(cs, &globalOptions{output: outputName, color: "never"})
	cmd := newClassListCommand(ta.app)
	if err := cmd.Flags().Set("family", "ipv6"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ta.out.String()); got != "ipclass/v6" {
		t.Fatalf("output = %q, want only the IPv6 class", got)
	}
}

// The resolved requirement is what a claim must supply, so `show` leads with it
// and names the pools that back the class.
func TestClassShowDetail(t *testing.T) {
	class := newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1)
	class.Spec.UniqueWithin = []string{"network"}
	class.Spec.AllowedPrefixLengths = &ipamv1alpha1.PrefixLengthRange{Min: 28, Max: 32}
	class.Spec.DefaultPrefixLength = 32
	class.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimRetain
	class.Spec.RetentionLease = &metav1.Duration{Duration: 72 * 3600e9}
	class.Status.RequiredScopeRoles = []string{"location", "network"}

	pool := newPool("us-west-v4", "10.1.0.0/16", ipamv1alpha1.IPv4, 100, 40)
	pool.Spec.ClassNames = []string{"tenant-endpoint-ipv4"}
	cs := newFakeClientset(class, pool)
	ta := newTestApp(cs, nil)

	cmd := newClassShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"tenant-endpoint-ipv4"}); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	for _, want := range []string{
		"Claims must scope by", "location, network",
		"Unique within", "Retain", "72h0m0s",
		"Pools offering this class", "us-west-v4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q:\n%s", want, out)
		}
	}
}

// A referencing class holds no policy of its own, and a local pool listing says
// nothing about whether it works. The "no pool offers this" warning would be
// wrong here, so it must not fire.
func TestClassShowReference(t *testing.T) {
	class := newClass("shared-anycast", ipamv1alpha1.IPv4, 0)
	class.Spec.Source = &ipamv1alpha1.ClassSourceRef{Project: "platform", Name: "anycast-ipv4"}
	cs := newFakeClientset(class)
	ta := newTestApp(cs, nil)

	cmd := newClassShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"shared-anycast"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "class anycast-ipv4 in project platform") {
		t.Errorf("detail does not name the referenced class:\n%s", ta.out.String())
	}
	if strings.Contains(ta.err.String(), "No pool offers") {
		t.Errorf("a reference must not be reported as unbacked:\n%s", ta.err.String())
	}
}

// A mistyped class name is the likeliest cause of a 404, so the miss lists what
// does exist.
func TestClassShowNotFoundListsWhatExists(t *testing.T) {
	cs := newFakeClientset(newClass("tenant-endpoint-ipv4", ipamv1alpha1.IPv4, 1))
	ta := newTestApp(cs, nil)
	cmd := newClassShowCommand(ta.app)
	err := cmd.RunE(cmd, []string{"tenant-endpoint-ipv"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	ce := toCLIError(err)
	if ce.code != exitNotFound {
		t.Fatalf("code = %d, want not found(%d)", ce.code, exitNotFound)
	}
	if !strings.Contains(ce.fix, "tenant-endpoint-ipv4") {
		t.Errorf("fix does not list the classes that exist: %q", ce.fix)
	}
}

func TestClassUnitCell(t *testing.T) {
	cases := []struct {
		name string
		spec ipamv1alpha1.IPClassSpec
		want string
	}{
		{name: "nothing stated", want: "—"},
		{name: "fixed size", spec: ipamv1alpha1.IPClassSpec{DefaultPrefixLength: 32}, want: "/32"},
		{
			name: "a range is shown alongside the default",
			spec: ipamv1alpha1.IPClassSpec{
				DefaultPrefixLength:  26,
				AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 24, Max: 28},
			},
			want: "/26 (/24–/28)",
		},
		{
			name: "a fixed-size class states min == max and adds nothing",
			spec: ipamv1alpha1.IPClassSpec{
				DefaultPrefixLength:  32,
				AllowedPrefixLengths: &ipamv1alpha1.PrefixLengthRange{Min: 32, Max: 32},
			},
			want: "/32",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classUnitCell(&ipamv1alpha1.IPClass{Spec: tc.spec}); got != tc.want {
				t.Fatalf("classUnitCell = %q, want %q", got, tc.want)
			}
		})
	}
}
