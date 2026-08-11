package main

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// orDash renders an empty string as an em dash so table cells never collapse.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// refOrNil returns a *LocalRef for a non-empty name, else nil.
func refOrNil(name string) *ipamv1alpha1.LocalRef {
	if name == "" {
		return nil
	}
	return &ipamv1alpha1.LocalRef{Name: name}
}

// parseFamily normalizes a user-supplied family string into the API enum.
func parseFamily(s string) (ipamv1alpha1.IPFamily, error) {
	switch strings.ToLower(s) {
	case "ipv4", "v4", "4":
		return ipamv1alpha1.IPv4, nil
	case "ipv6", "v6", "6":
		return ipamv1alpha1.IPv6, nil
	default:
		return "", usageErrorf("invalid --family %q: must be ipv4 or ipv6", s)
	}
}

// parseStrategy validates an allocation strategy string against the API enum.
func parseStrategy(s string) (ipamv1alpha1.AllocationStrategy, error) {
	switch strings.ToLower(s) {
	case "firstfit", "first-fit", "first":
		return ipamv1alpha1.FirstFit, nil
	case "bestfit", "best-fit", "best":
		return ipamv1alpha1.BestFit, nil
	case "leastutilized", "least-utilized", "least":
		return ipamv1alpha1.LeastUtilized, nil
	default:
		return "", usageErrorf("invalid --strategy %q: must be FirstFit, BestFit, or LeastUtilized", s)
	}
}

// renderMachine handles the machine output formats (json, yaml, name). It
// returns done=true when it produced output for the requested format, so the
// caller can skip its human rendering. obj must carry its GVK for json/yaml;
// nameFn supplies the "kind/name" line for -o name.
func (a *app) renderMachine(obj runtime.Object, nameFn func() string) (done bool, err error) {
	switch a.opts.output {
	case outputJSON:
		return true, encodeJSON(a.io.Out, obj)
	case outputYAML:
		return true, encodeYAML(a.io.Out, obj)
	case outputName:
		_, _ = fmt.Fprintln(a.io.Out, nameFn())
		return true, nil
	}
	return false, nil
}
