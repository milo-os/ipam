package main

import (
	"crypto/rand"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// generateResourceName mints a DNS-1123-safe, collision-resistant name of the
// form "<prefix>-<10 base36 chars>" for resources the user did not name
// explicitly. Used because the IPAM apiserver does not implement server-side
// metadata.generateName.
func generateResourceName(prefix string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	const n = 10
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand.Read essentially never fails; on the impossible error, use a fixed
		// but still valid suffix rather than panicking.
		for i := range b {
			b[i] = 'x'
		}
		return prefix + "-" + string(b)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return prefix + "-" + string(b)
}

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

// parseLabelSelector converts a "k=v,k2=v2" string into a *LabelSelector,
// returning an empty (match-everything) selector on a parse error so that the
// server can perform its own validation rather than the plugin guessing.
func parseLabelSelector(s string) *metav1.LabelSelector {
	sel, err := metav1.ParseToLabelSelector(s)
	if err != nil || sel == nil {
		return &metav1.LabelSelector{}
	}
	return sel
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
