package ippool

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func offeredClass(name string, mutate func(*ipamv1alpha1.IPClassSpec)) *ipamv1alpha1.IPClass {
	spec := ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4}
	if mutate != nil {
		mutate(&spec)
	}
	return &ipamv1alpha1.IPClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

// The rule this test guards is the quiet one: non-overlap is enforced only
// within an address space, so two classes disagreeing about what an address
// space *is* would each hand out addresses the other already holds, with
// nothing logged.
func TestUniqueWithinMustAgreeAcrossOfferedClasses(t *testing.T) {
	path := field.NewPath("spec", "classNames")

	perNetwork := offeredClass("tenant-endpoint-ipv4", func(s *ipamv1alpha1.IPClassSpec) {
		s.UniqueWithin = []string{"network"}
	})
	platformWide := offeredClass("public-unicast-ipv4", func(s *ipamv1alpha1.IPClassSpec) {
		s.UniqueWithin = nil
	})

	errs := validateUniqueWithinAgreement([]*ipamv1alpha1.IPClass{perNetwork, platformWide}, path)
	if len(errs) == 0 {
		t.Fatal("expected disagreeing uniqueWithin to be rejected")
	}
	if !strings.Contains(errs[0].Error(), "already holds") {
		t.Errorf("the error should explain the consequence, got %v", errs[0])
	}

	// Agreement is enough; the classes need not be otherwise alike.
	agreeing := []*ipamv1alpha1.IPClass{
		perNetwork,
		offeredClass("other-ipv4", func(s *ipamv1alpha1.IPClassSpec) { s.UniqueWithin = []string{"network"} }),
	}
	if errs := validateUniqueWithinAgreement(agreeing, path); len(errs) != 0 {
		t.Errorf("classes agreeing on uniqueWithin must be accepted, got %v", errs)
	}

	// The roles are a set, so ordering is not disagreement.
	reordered := []*ipamv1alpha1.IPClass{
		offeredClass("a", func(s *ipamv1alpha1.IPClassSpec) { s.UniqueWithin = []string{"network", "location"} }),
		offeredClass("b", func(s *ipamv1alpha1.IPClassSpec) { s.UniqueWithin = []string{"location", "network"} }),
	}
	if errs := validateUniqueWithinAgreement(reordered, path); len(errs) != 0 {
		t.Errorf("role order must not read as disagreement, got %v", errs)
	}

	// A single class has nothing to disagree with.
	if errs := validateUniqueWithinAgreement([]*ipamv1alpha1.IPClass{perNetwork}, path); len(errs) != 0 {
		t.Errorf("a lone class must be accepted, got %v", errs)
	}
}

func TestValidateOfferedClass(t *testing.T) {
	path := field.NewPath("spec", "classNames").Index(0)

	t.Run("family must match the pool", func(t *testing.T) {
		v6 := offeredClass("tenant-endpoint-ipv6", func(s *ipamv1alpha1.IPClassSpec) {
			s.IPFamily = ipamv1alpha1.IPv6
		})
		errs := validateOfferedClass(&ipam.IPPool{}, v6, "IPv4", path)
		if len(errs) == 0 || !strings.Contains(errs[0].Error(), "IPv6") {
			t.Fatalf("expected a family mismatch, got %v", errs)
		}
	})

	// Only the top of a chain draws from the pools that offer it. Offering a
	// pool to a class that carves from a parent reads as capacity that is not
	// there.
	t.Run("a class with a parent cannot be offered a pool directly", func(t *testing.T) {
		child := offeredClass("tenant-subnet-ipv6", func(s *ipamv1alpha1.IPClassSpec) {
			s.ParentClassName = "tenant-network-ipv6"
		})
		errs := validateOfferedClass(&ipam.IPPool{}, child, "IPv4", path)
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "carves from") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a parent-class error, got %v", errs)
		}
	})

	// The design's own worked examples must validate. Each of these was rejected
	// by an earlier version of this check, which required the class to name
	// every role the pool declared in spec.scope — conflating an eligibility
	// filter with the uniqueness key.
	t.Run("the design's located pools validate against their leaf classes", func(t *testing.T) {
		located := &ipam.IPPool{Spec: ipam.IPPoolSpec{
			Scope: map[string]ipam.ScopeRef{
				"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
			},
		}}
		perSite := &ipam.IPPool{Spec: ipam.IPPoolSpec{
			Scope: map[string]ipam.ScopeRef{
				"site": {APIGroup: "networking.datumapis.com", Kind: "Site", Name: "pop-ord1"},
			},
		}}

		cases := []struct {
			name  string
			pool  *ipam.IPPool
			class *ipamv1alpha1.IPClass
		}{
			{
				// uniqueWithin: [] — routable, so unique everywhere — over the
				// location's public pool.
				name:  "public-unicast-ipv4 over a per-location public pool",
				pool:  located,
				class: offeredClass("public-unicast-ipv4", nil),
			},
			{
				// uniqueWithin: [network] over a /20 every network in the
				// location shares. Adding "location" here to satisfy a check
				// would assert two networks may hold one address if they differ
				// in location, which is precisely the widening to avoid.
				name: "tenant-endpoint-ipv4 over a per-location shared range",
				pool: located,
				class: offeredClass("tenant-endpoint-ipv4", func(s *ipamv1alpha1.IPClassSpec) {
					s.UniqueWithin = []string{"network"}
				}),
			},
			{
				// The fabric plan: every per-PoP pool carries scope.site and no
				// fabric class names it.
				name:  "a fabric class over a per-site pool",
				pool:  perSite,
				class: offeredClass("fabric-loopback-ipv4", nil),
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if errs := validateOfferedClass(tc.pool, tc.class, "IPv4", path); len(errs) != 0 {
					t.Fatalf("the design's own example must be accepted, got %v", errs)
				}
			})
		}
	})
}

// The write-time half of the consent rule.
//
// `spec.classNames` is a pool volunteering itself to a class, written by the
// pool's owner. Discovery searches every project now that the platform is one,
// so without a matching statement from the class any tenant who can create an
// IPPool could list a popular class name on it and start receiving other
// tenants' claims.
//
// This check is not the boundary — allocator.DiscoverPool is, at read time,
// because consent is revocable and a write-time decision is a snapshot. What
// this buys is an error naming the field at the moment the pool is written,
// instead of a pool that is accepted and then serves nobody.
func TestValidateClassConsent(t *testing.T) {
	const platformProject = "milo-platform"
	path := field.NewPath("spec", "classNames").Index(0)

	// The identity a request carries. platformProject is stamped on by
	// tenant.FromContext from the server's configuration, so a context is the
	// only way to build one that can answer IsPlatform.
	identity := func(project string) tenant.Identity {
		ctx := tenant.WithPlatformProject(context.Background(), platformProject)
		if project == "" {
			return tenant.FromContext(request.WithUser(ctx, &user.DefaultInfo{Name: "someone"}))
		}
		return tenant.FromContext(request.WithUser(ctx, &user.DefaultInfo{
			Name: "someone",
			Extra: map[string][]string{
				tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
				tenant.ExtraParentType:     {"Project"},
				tenant.ExtraParentName:     {project},
			},
		}))
	}

	tests := []struct {
		name            string
		project         string
		backingProjects []string
		wantErr         bool
	}{
		{
			// The additive property. Every class in an existing catalog has an
			// empty list and is backed by platform-authored pools; requiring the
			// platform to list itself would break all of them at once.
			name:    "the platform project need not be listed",
			project: platformProject,
			wantErr: false,
		},
		{
			name:            "a listed project may back the class",
			project:         "project-alpha",
			backingProjects: []string{"project-alpha"},
			wantErr:         false,
		},
		{
			name:            "an unlisted project may not",
			project:         "attacker-project",
			backingProjects: []string{"project-alpha"},
			wantErr:         true,
		},
		{
			name:    "a class naming nobody is backed by the platform alone",
			project: "project-alpha",
			wantErr: true,
		},
		{
			// A caller with no tenant is not the platform. Under the old
			// definition of IsPlatform it was, and it would have cleared this
			// check by virtue of carrying no identity at all.
			name:    "a caller with no tenant is not the platform",
			project: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class := offeredClass("public-unicast-ipv4", func(s *ipamv1alpha1.IPClassSpec) {
				s.BackingProjects = tt.backingProjects
			})
			errs := validateClassConsent(identity(tt.project), class, path)
			if tt.wantErr {
				if len(errs) == 0 {
					t.Fatalf("project %q must not be allowed to back the class", tt.project)
				}
				if !strings.Contains(errs[0].Error(), "backingProjects") {
					t.Errorf("the error should name the field to edit, got %v", errs[0])
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("project %q must be allowed to back the class, got %v", tt.project, errs)
			}
		})
	}
}
