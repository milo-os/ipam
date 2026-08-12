package main

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strings"

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

// orDashList renders a string list as a comma-joined cell, em-dash when empty.
func orDashList(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	return strings.Join(items, ",")
}

// scopeRolesCell renders a list of scope role names, substituting an explanatory
// phrase when the list is empty (an empty UniqueWithin is meaningful, not blank).
func scopeRolesCell(roles []string, whenEmpty string) string {
	if len(roles) == 0 {
		return whenEmpty
	}
	return strings.Join(roles, ", ")
}

// quoteAll wraps each element in double quotes, for listing names in prose.
func quoteAll(items []string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, `"`+s+`"`)
	}
	return out
}

// sortedKeys returns a map's keys in stable order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// int32Ptr returns a pointer to n. Optional numeric API fields are pointers so
// "not requested" is distinguishable from "requested zero" — for prefix length
// the difference is "use the class default" versus "give me a /0".
func int32Ptr(n int32) *int32 { return &n }

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
