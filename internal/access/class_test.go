package access

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

type recordingChecker struct {
	allow bool
	err   error
	asked []string
}

func (c *recordingChecker) CanUseClass(_ context.Context, className string) (bool, error) {
	c.asked = append(c.asked, className)
	return c.allow, c.err
}

func classWith(visibility string, poolPer ...string) *ipamv1alpha1.IPClass {
	return &ipamv1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public-unicast-ipv4"},
		Spec: ipamv1alpha1.IPClassSpec{
			IPFamily:   ipamv1alpha1.IPv4,
			Visibility: visibility,
			PoolPer:    poolPer,
		},
	}
}

func TestAuthorizeClassConsumption(t *testing.T) {
	tests := []struct {
		name       string
		class      *ipamv1alpha1.IPClass
		isPlatform bool
		checker    ClassAccessChecker
		wantDenied bool
		wantReason string
		wantAsked  bool
	}{
		{
			name:      "a shared class with a grant",
			class:     classWith(ipamv1alpha1.VisibilityShared),
			checker:   &recordingChecker{allow: true},
			wantAsked: true,
		},
		{
			name:      "a consumer class with a grant",
			class:     classWith(ipamv1alpha1.VisibilityConsumer),
			checker:   &recordingChecker{allow: true},
			wantAsked: true,
		},
		{
			name:       "a consumer class without a grant",
			class:      classWith(ipamv1alpha1.VisibilityConsumer),
			checker:    &recordingChecker{allow: false},
			wantDenied: true,
			wantReason: "sar_denied",
			wantAsked:  true,
		},
		{
			// Fail closed. An apiserver started without an authorizer has no
			// boundary, and treating that as "allow" is the exact failure the
			// design says must not happen.
			name:       "no checker configured is a denial",
			class:      classWith(ipamv1alpha1.VisibilityShared),
			checker:    nil,
			wantDenied: true,
			wantReason: "no_checker",
		},
		{
			// The default has to be closed, or every class written before this
			// check existed would be silently published to every tenant.
			name:       "an unmarked class is platform-only",
			class:      classWith(""),
			checker:    &recordingChecker{allow: true},
			wantDenied: true,
			wantReason: "visibility",
		},
		{
			// Checked before the SAR, so a mistaken grant cannot open an
			// operator-internal class.
			name:       "a platform class is closed to tenants even with a grant",
			class:      classWith(ipamv1alpha1.VisibilityPlatform),
			checker:    &recordingChecker{allow: true},
			wantDenied: true,
			wantReason: "visibility",
		},
		{
			// A container class provisions pools for the classes below it and is
			// nobody's to claim. This is a correctness rule before it is an
			// authorization one, so it holds for the platform too.
			name:       "a container class is refused to tenants",
			class:      classWith(ipamv1alpha1.VisibilityShared, "network"),
			checker:    &recordingChecker{allow: true},
			wantDenied: true,
			wantReason: "container_class",
		},
		{
			name:       "a container class is refused to the platform as well",
			class:      classWith(ipamv1alpha1.VisibilityShared, "network"),
			isPlatform: true,
			checker:    &recordingChecker{allow: true},
			wantDenied: true,
			wantReason: "container_class",
		},
		{
			// The platform authored the catalog and carries no tenant identity.
			// Without this the service could not bootstrap.
			name:       "the platform reaches a platform class with no checker",
			class:      classWith(ipamv1alpha1.VisibilityPlatform),
			isPlatform: true,
			checker:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := AuthorizeClassConsumption(context.Background(), tt.class, tt.isPlatform, tt.checker)

			if !tt.wantDenied {
				if err != nil {
					t.Fatalf("expected the claim to be allowed, got %v", err)
				}
			} else {
				if !errors.Is(err, ErrClassDenied) {
					t.Fatalf("expected ErrClassDenied, got %v", err)
				}
				if !strings.Contains(err.Error(), tt.wantReason) {
					t.Errorf("error %q does not carry reason %q", err, tt.wantReason)
				}
			}

			if rc, ok := tt.checker.(*recordingChecker); ok {
				asked := len(rc.asked) > 0
				if asked != tt.wantAsked {
					t.Errorf("SAR consulted = %v, want %v", asked, tt.wantAsked)
				}
			}
		})
	}
}

// A broken authorizer is a system failure, not a denial. Collapsing the two
// would let an outage read as "this tenant has no access", which an operator
// would chase in the wrong place.
func TestAuthorizeClassConsumptionSurfacesCheckerErrors(t *testing.T) {
	boom := errors.New("authorizer unavailable")
	err := AuthorizeClassConsumption(context.Background(),
		classWith(ipamv1alpha1.VisibilityShared), false, &recordingChecker{err: boom})

	if errors.Is(err, ErrClassDenied) {
		t.Fatal("an authorizer failure must not be reported as a denial")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected the underlying error to survive, got %v", err)
	}
}
