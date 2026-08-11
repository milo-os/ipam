package iamconfig

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// Every resource the apiserver serves needs a ProtectedResource manifest and a
// role that grants it. Neither is reachable from the Go types, so a resource
// added without them serves requests that IAM denies — and the failure lands in
// a cluster, on every claim, rather than here.
//
// The manifests are read from disk rather than restated, so this fails when
// they drift rather than when someone forgets to update a copy of them.

const iamDir = "../../config/components/iam"

// plurals maps every kind the API group serves to its resource name. The test
// below requires an entry for each kind in the scheme, so a new kind fails here
// rather than shipping without an IAM manifest.
var plurals = map[string]string{
	"IPPool":       "ippools",
	"IPClass":      "ipclasses",
	"IPAllocation": "ipallocations",
	"IPClaim":      "ipclaims",
}

// servedKinds returns the kinds registered in the API group, excluding list
// types and the metav1 types the scheme adds for every group.
func servedKinds(t *testing.T) []string {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	var kinds []string
	for gvk := range scheme.AllKnownTypes() {
		if gvk.GroupVersion() != v1alpha1.SchemeGroupVersion {
			continue
		}
		if strings.HasSuffix(gvk.Kind, "List") || strings.HasSuffix(gvk.Kind, "Options") ||
			gvk.Kind == "WatchEvent" || strings.HasPrefix(gvk.Kind, "Delete") ||
			strings.HasPrefix(gvk.Kind, "Get") || strings.HasPrefix(gvk.Kind, "Create") ||
			strings.HasPrefix(gvk.Kind, "Update") || strings.HasPrefix(gvk.Kind, "Patch") ||
			gvk.Kind == "APIGroup" || gvk.Kind == "APIVersions" || gvk.Kind == "APIResourceList" ||
			gvk.Kind == "Status" {
			continue
		}
		kinds = append(kinds, gvk.Kind)
	}
	sort.Strings(kinds)
	return kinds
}

// servedResources is every plural the API group serves.
func servedResources(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, kind := range servedKinds(t) {
		plural, ok := plurals[kind]
		if !ok {
			t.Fatalf("kind %q is served but has no entry in plurals, so nothing checks its IAM manifests", kind)
		}
		out = append(out, plural)
	}
	return out
}

type protectedResource struct {
	Spec struct {
		Plural      string   `json:"plural"`
		Permissions []string `json:"permissions"`
	} `json:"spec"`
}

type role struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		InheritedRoles []struct {
			Name string `json:"name"`
		} `json:"inheritedRoles"`
		IncludedPermissions []string `json:"includedPermissions"`
	} `json:"spec"`
}

func loadProtectedResources(t *testing.T) map[string]protectedResource {
	t.Helper()
	out := map[string]protectedResource{}
	dir := filepath.Join(iamDir, "protected-resources")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "kustomization.yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var pr protectedResource
		if err := yaml.Unmarshal(data, &pr); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		if pr.Spec.Plural != "" {
			out[pr.Spec.Plural] = pr
		}
	}
	return out
}

func loadRoles(t *testing.T) map[string]role {
	t.Helper()
	out := map[string]role{}
	dir := filepath.Join(iamDir, "roles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "kustomization.yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var r role
		if err := yaml.Unmarshal(data, &r); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		if r.Metadata.Name != "" {
			out[r.Metadata.Name] = r
		}
	}
	return out
}

// permissionsOf resolves a role's own permissions plus everything it inherits.
func permissionsOf(roles map[string]role, name string, seen map[string]bool) map[string]bool {
	perms := map[string]bool{}
	if seen[name] {
		return perms
	}
	seen[name] = true
	r, ok := roles[name]
	if !ok {
		return perms
	}
	for _, p := range r.Spec.IncludedPermissions {
		perms[p] = true
	}
	for _, inherited := range r.Spec.InheritedRoles {
		for p := range permissionsOf(roles, inherited.Name, seen) {
			perms[p] = true
		}
	}
	return perms
}

func TestEveryServedResourceIsProtected(t *testing.T) {
	protected := loadProtectedResources(t)
	for _, plural := range servedResources(t) {
		if _, ok := protected[plural]; !ok {
			t.Errorf("the apiserver serves %q with no ProtectedResource manifest; "+
				"IAM would deny every request for it", plural)
		}
	}
}

// A claim resolves its class on every allocation, so a role that can file
// claims but cannot read classes files claims that always fail.
func TestConsumersCanReadEveryResourceAClaimTouches(t *testing.T) {
	roles := loadRoles(t)
	perms := permissionsOf(roles, "ipam.miloapis.com-consumer", map[string]bool{})

	for _, plural := range servedResources(t) {
		want := "ipam.miloapis.com/" + plural + ".get"
		if !perms[want] {
			t.Errorf("the consumer role lacks %q; a claim reads %s while allocating", want, plural)
		}
	}
	for _, verb := range []string{"create", "delete"} {
		want := "ipam.miloapis.com/ipclaims." + verb
		if !perms[want] {
			t.Errorf("the consumer role lacks %q", want)
		}
	}
}

// The verbs below are checked by SubjectAccessReview from the allocation and
// class-admission paths, not by the apiserver's own request handling, so
// nothing else ties them to a manifest. A verb the code checks but no
// ProtectedResource declares is denied for everyone, and the denial only
// surfaces as a cross-project reference or claim that can never be authorised.
func TestEveryVerbTheCodeChecksIsDeclared(t *testing.T) {
	protected := loadProtectedResources(t)
	for _, want := range []struct {
		plural, verb, site string
	}{
		{"ippools", "use", "internal/access/sar.go"},
		{"ipclasses", "use", "internal/access/class.go"},
	} {
		pr, ok := protected[want.plural]
		if !ok {
			t.Errorf("%s checks %q on %q, which has no ProtectedResource", want.site, want.verb, want.plural)
			continue
		}
		declared := false
		for _, v := range pr.Spec.Permissions {
			if v == want.verb {
				declared = true
			}
		}
		if !declared {
			t.Errorf("%s issues a %q SubjectAccessReview against %q, but the ProtectedResource "+
				"does not declare that permission, so IAM denies it for everyone",
				want.site, want.verb, want.plural)
		}
	}
}

// A permission nothing declares is a typo that grants nothing, and it reads as
// a grant until somebody tests the cluster.
func TestEveryGrantedPermissionIsDeclared(t *testing.T) {
	protected := loadProtectedResources(t)
	declared := map[string]bool{}
	for plural, pr := range protected {
		for _, verb := range pr.Spec.Permissions {
			declared["ipam.miloapis.com/"+plural+"."+verb] = true
		}
	}

	for name := range loadRoles(t) {
		for perm := range permissionsOf(loadRoles(t), name, map[string]bool{}) {
			if !strings.HasPrefix(perm, "ipam.miloapis.com/") {
				continue
			}
			if !declared[perm] {
				t.Errorf("role %q grants %q, which no ProtectedResource declares", name, perm)
			}
		}
	}
}
