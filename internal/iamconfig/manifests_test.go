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
		Plural       string   `json:"plural"`
		Permissions  []string `json:"permissions"`
		Subresources []struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		} `json:"subresources"`
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
		for _, sub := range pr.Spec.Subresources {
			for _, verb := range sub.Permissions {
				declared["ipam.miloapis.com/"+plural+"/"+sub.Name+"."+verb] = true
			}
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

// Status is written by the apiserver alone. A role that could update or patch
// a subresource would let a caller overwrite the allocation the server reported.
func TestRolesGrantNoSubresourceWrites(t *testing.T) {
	for name, r := range loadRoles(t) {
		for _, perm := range r.Spec.IncludedPermissions {
			resource, verb, _ := strings.Cut(strings.TrimPrefix(perm, "ipam.miloapis.com/"), ".")
			if strings.HasPrefix(perm, "ipam.miloapis.com/") && strings.Contains(resource, "/") && verb != "get" {
				t.Errorf("role %q grants %q, a write to a server-owned subresource", name, perm)
			}
		}
	}
}

// A consumer releases the addresses its own claims retained: releasing is
// deleting the IPAllocation, and the allocator refuses a bound one, so delete
// is safe to grant where a claim can be filed. Nothing may write one, and an
// admin keeps the release through the consumer role it inherits.
func TestConsumersCanReleaseAllocationsButNotWriteThem(t *testing.T) {
	roles := loadRoles(t)
	for _, name := range []string{"ipam.miloapis.com-consumer", "ipam.miloapis.com-admin"} {
		perms := permissionsOf(roles, name, map[string]bool{})
		if !perms["ipam.miloapis.com/ipallocations.delete"] {
			t.Errorf("role %q cannot release an IPAllocation", name)
		}
		for _, verb := range []string{"create", "update", "patch"} {
			if perm := "ipam.miloapis.com/ipallocations." + verb; perms[perm] {
				t.Errorf("role %q grants %q; allocations are written only by their claims", name, perm)
			}
		}
	}
	viewer := permissionsOf(roles, "ipam.miloapis.com-viewer", map[string]bool{})
	if viewer["ipam.miloapis.com/ipallocations.delete"] {
		t.Error("the viewer role can release an IPAllocation")
	}
}

// The e2e cluster authorizes with RBAC rather than Milo IAM, so the consumer
// identity's access is restated as a ClusterRole. It must grant exactly what
// the consumer role does, or a consumer suite proves something about a role
// nobody ships.
func TestTheE2EConsumerRoleMirrorsTheConsumerRole(t *testing.T) {
	const path = "../../test/e2e/lib/tenant-rbac.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	type clusterRole struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Rules []struct {
			APIGroups []string `json:"apiGroups"`
			Resources []string `json:"resources"`
			Verbs     []string `json:"verbs"`
		} `json:"rules"`
	}
	got := map[string]bool{}
	found := false
	for _, doc := range strings.Split(string(data), "\n---\n") {
		var cr clusterRole
		if err := yaml.Unmarshal([]byte(doc), &cr); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if cr.Kind != "ClusterRole" || cr.Metadata.Name != "e2e-tenant-ipam-consumer" {
			continue
		}
		found = true
		for _, rule := range cr.Rules {
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						got[group+"/"+resource+"."+verb] = true
					}
				}
			}
		}
	}
	if !found {
		t.Fatalf("%s has no ClusterRole e2e-tenant-ipam-consumer", path)
	}
	want := permissionsOf(loadRoles(t), "ipam.miloapis.com-consumer", map[string]bool{})
	for perm := range want {
		if !got[perm] {
			t.Errorf("the e2e consumer ClusterRole lacks %q, which the consumer role grants", perm)
		}
	}
	for perm := range got {
		if !want[perm] {
			t.Errorf("the e2e consumer ClusterRole grants %q, which the consumer role does not", perm)
		}
	}
}
