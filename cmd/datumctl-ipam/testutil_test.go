package main

import (
	"bytes"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	clientset "go.miloapis.com/ipam/pkg/client/clientset/versioned"
)

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
				Total:     total,
				Allocated: allocated,
				Available: total - allocated,
			},
		},
	}
}
