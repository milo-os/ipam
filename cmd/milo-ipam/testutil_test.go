package main

import (
	"bytes"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	clientset "go.miloapis.com/ipam/pkg/client/clientset/versioned"
	"go.miloapis.com/ipam/pkg/client/clientset/versioned/fake"
)

// newFakeClientset wraps the generated fake constructor. The generated client
// ships only NewSimpleClientset (NewClientset requires apply-config generation,
// which this client doesn't use), so the deprecation is suppressed once here
// rather than at every call site.
//
//nolint:staticcheck // NewClientset is not generated for this client
func newFakeClientset(objects ...runtime.Object) *fake.Clientset {
	return fake.NewSimpleClientset(objects...)
}

// testApp wires an app with buffer-backed streams and an injected clientset so
// command behavior can be asserted without a real cluster or TTY.
type testApp struct {
	app *app
	out *bytes.Buffer
	err *bytes.Buffer
	in  *strings.Reader
}

func newTestApp(cs clientset.Interface, opts *globalOptions) *testApp {
	if opts == nil {
		opts = &globalOptions{output: outputTable, color: "never"}
	}
	if opts.output == "" {
		opts.output = outputTable
	}
	if opts.color == "" {
		opts.color = "never"
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	in := strings.NewReader("")
	a := &app{
		io:   IOStreams{In: in, Out: out, ErrOut: errOut},
		opts: opts,
	}
	a.clientFactory = func() (clientset.Interface, string, error) {
		return cs, "default", nil
	}
	a.resolveColor()
	return &testApp{app: a, out: out, err: errOut, in: in}
}

// newClass builds a minimal IPClass for table/detail tests.
func newClass(name string, family ipamv1alpha1.IPFamily) *ipamv1alpha1.IPClass {
	return &ipamv1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ipamv1alpha1.IPClassSpec{IPFamily: family},
		Status:     ipamv1alpha1.IPClassStatus{Phase: ipamv1alpha1.ClassReady},
	}
}

// newClaim builds a bound IPClaim holding cidr.
func newClaim(name, class, cidr, pool string) *ipamv1alpha1.IPClaim {
	return &ipamv1alpha1.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       ipamv1alpha1.IPClaimSpec{ClassName: class},
		Status: ipamv1alpha1.IPClaimStatus{
			Phase:         ipamv1alpha1.ClaimBound,
			AllocatedCIDR: cidr,
			PoolRef:       &ipamv1alpha1.LocalRef{Name: pool},
		},
	}
}

// newAllocation builds a ready IPAllocation holding cidr. A nil claim name
// leaves ClaimRef unset, which is the retained/reserved shape.
func newAllocation(name, class, cidr, pool, claim string) *ipamv1alpha1.IPAllocation {
	al := &ipamv1alpha1.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ipamv1alpha1.IPAllocationSpec{
			IPFamily:  ipamv1alpha1.IPv4,
			PoolRef:   ipamv1alpha1.LocalRef{Name: pool},
			ClassName: class,
			Purpose:   ipamv1alpha1.PurposeClaim,
		},
		Status: ipamv1alpha1.IPAllocationStatus{
			Phase:         ipamv1alpha1.AllocationReady,
			AllocatedCIDR: cidr,
		},
	}
	if claim != "" {
		al.Spec.ClaimRef = &ipamv1alpha1.LocalRef{Name: claim}
	}
	return al
}

// newPool builds an IPPool with the given capacity for table/tree tests.
func newPool(name, cidr string, family ipamv1alpha1.IPFamily, total, allocated int64) *ipamv1alpha1.IPPool {
	return &ipamv1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ipamv1alpha1.IPPoolSpec{
			CIDR:     cidr,
			IPFamily: family,
		},
		Status: ipamv1alpha1.IPPoolStatus{
			Phase:         ipamv1alpha1.PoolReady,
			AllocatedCIDR: cidr,
			Capacity: ipamv1alpha1.PoolCapacity{
				Total:     strconv.FormatInt(total, 10),
				Allocated: strconv.FormatInt(allocated, 10),
				Available: strconv.FormatInt(total-allocated, 10),
			},
		},
	}
}
