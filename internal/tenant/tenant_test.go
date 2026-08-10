package tenant

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
)

// ctxFor builds a request context carrying the parent extras Milo's front gate
// forwards, optionally under a configured platform project.
func ctxFor(platformProject, parentName string) context.Context {
	ctx := context.Background()
	if platformProject != "" {
		ctx = WithPlatformProject(ctx, platformProject)
	}
	if parentName == "" {
		// A caller with no parent extras at all — the shape an unscoped
		// kubeconfig produces.
		return request.WithUser(ctx, &user.DefaultInfo{Name: "someone"})
	}
	return request.WithUser(ctx, &user.DefaultInfo{
		Name: "someone",
		Extra: map[string][]string{
			ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			ExtraParentType:     {"Project"},
			ExtraParentName:     {parentName},
		},
	})
}

// TestIsPlatformIsTheConfiguredProjectNotTheAbsenceOfATenant is the
// authorization-boundary test for this change, and it is the one worth reading
// before the others.
//
// IsPlatform() used to mean "carries no tenant extras". Two things consume it
// and they pull in opposite directions:
//
//   - access.AuthorizeClassConsumption bypasses the visibility gate and the SAR
//     for a platform caller.
//   - cmd/ipam/admission.go already DENIES claim CREATE for a caller with no
//     derivable consumer.
//
// So under the old definition the bypass was held by exactly the callers the
// admission plugin refuses to serve, while the platform's own tooling — which
// now authenticates as a project like everything else — would lose it. That is
// an inverted authorization boundary, not a naming problem, so the case that
// must never regress is the first one below: no extras is NOT platform.
func TestIsPlatformIsTheConfiguredProjectNotTheAbsenceOfATenant(t *testing.T) {
	const platform = "milo-platform"

	tests := []struct {
		name            string
		platformProject string
		parentName      string
		want            bool
	}{
		{
			name:            "no tenant extras is not platform",
			platformProject: platform,
			parentName:      "",
			want:            false,
		},
		{
			name:            "the configured platform project is platform",
			platformProject: platform,
			parentName:      platform,
			want:            true,
		},
		{
			name:            "another tenant is not platform",
			platformProject: platform,
			parentName:      "project-alpha",
			want:            false,
		},
		{
			// Unconfigured must not promote anyone. A caller whose project
			// happens to be named the same as an unset config is still a
			// tenant.
			name:            "nothing is platform when the flag is unset",
			platformProject: "",
			parentName:      platform,
			want:            false,
		},
		{
			name:            "no extras and no config is not platform either",
			platformProject: "",
			parentName:      "",
			want:            false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FromContext(ctxFor(tc.platformProject, tc.parentName)).IsPlatform()
			if got != tc.want {
				t.Fatalf("IsPlatform() = %v, want %v (platformProject=%q parent=%q)",
					got, tc.want, tc.platformProject, tc.parentName)
			}
		})
	}
}

// TestZeroIdentityIsNotPlatform pins the fail-closed property at the type
// level, independent of any context. Identity is constructed as a literal in
// several places for its key helpers, and none of those literals can be allowed
// to produce a value that answers "yes, I am the platform".
func TestZeroIdentityIsNotPlatform(t *testing.T) {
	if (Identity{}).IsPlatform() {
		t.Fatal("zero Identity reports IsPlatform() = true; it must fail closed")
	}
	if (Identity{Name: "milo-platform"}).IsPlatform() {
		t.Fatal("a literal Identity naming the platform project reports IsPlatform() = true; " +
			"the answer must come from the configured value on the context, not from the name alone")
	}
}

// TestPlatformIdentityRequiresConfiguration covers the "what happens when it is
// unset" half. A platform-owned lookup must fail with a message naming the
// flag, never fall back to the unprefixed keyspace — a silent fallback is how
// this ships looking fine and breaks in production.
func TestPlatformIdentityRequiresConfiguration(t *testing.T) {
	if _, err := PlatformIdentity(context.Background()); !errors.Is(err, ErrNoPlatformProject) {
		t.Fatalf("PlatformIdentity with no configuration: err = %v, want ErrNoPlatformProject", err)
	}

	id, err := PlatformIdentity(WithPlatformProject(context.Background(), "milo-platform"))
	if err != nil {
		t.Fatalf("PlatformIdentity: unexpected error: %v", err)
	}
	if !id.IsPlatform() {
		t.Fatal("PlatformIdentity returned an Identity that does not report IsPlatform()")
	}
	if got, want := id.KeyPrefix(), "project/milo-platform/"; got != want {
		t.Fatalf("PlatformIdentity().KeyPrefix() = %q, want %q", got, want)
	}
}

// TestPlatformOwnedKeysAreProjectPrefixed is the storage half of the model:
// there is no unprefixed keyspace any more, so a platform-owned object's key
// is a project key like every other.
func TestPlatformOwnedKeysAreProjectPrefixed(t *testing.T) {
	id, err := PlatformIdentity(WithPlatformProject(context.Background(), "milo-platform"))
	if err != nil {
		t.Fatalf("PlatformIdentity: %v", err)
	}
	got := id.ResourceKey("ipclasses", "public-unicast-ipv4")
	want := "project/milo-platform/ipam.miloapis.com/ipclasses/public-unicast-ipv4"
	if got != want {
		t.Fatalf("ResourceKey = %q, want %q", got, want)
	}
}

// TestPlatformProjectIsNotClientSupplied guards the obvious attack on a
// context-carried value: the platform project must come from the server's own
// configuration, and nothing a caller sends may set or change it. The extras
// are the only thing a caller controls here, so a caller claiming to be the
// platform project only succeeds when the server was configured to agree.
func TestPlatformProjectIsNotClientSupplied(t *testing.T) {
	// Configured for one project; a caller asserts a different one.
	ctx := ctxFor("milo-platform", "attacker-project")
	if FromContext(ctx).IsPlatform() {
		t.Fatal("a caller asserting a non-platform project was treated as platform")
	}

	// No configuration at all; a caller asserts the name the operator would
	// have used.
	if FromContext(ctxFor("", "milo-platform")).IsPlatform() {
		t.Fatal("a caller was treated as platform with no server-side configuration")
	}
}
