package ipclaim

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func testClass() *ipamv1alpha1.IPClass {
	return &ipamv1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public-egress"},
		Spec: ipamv1alpha1.IPClassSpec{
			Provisioner:          ipamv1alpha1.NativeProvisioner,
			IPFamily:             ipamv1alpha1.IPv4,
			Strategy:             ipamv1alpha1.LeastUtilized,
			AllowedPrefixLengths: ipamv1alpha1.PrefixLengthRange{Min: 24, Max: 28},
			DefaultPrefixLength:  26,
			ReclaimPolicy:        ipamv1alpha1.ReclaimRetain,
		},
	}
}

func TestApplyClassPolicy(t *testing.T) {
	t.Run("fills empty fields from the class", func(t *testing.T) {
		claim := &ipam.IPClaim{}
		applyClassPolicy(claim, testClass())
		if claim.Spec.IPFamily != ipam.IPv4 {
			t.Errorf("ipFamily = %q, want IPv4", claim.Spec.IPFamily)
		}
		if claim.Spec.PrefixLength != 26 {
			t.Errorf("prefixLength = %d, want 26 (class default)", claim.Spec.PrefixLength)
		}
		if claim.Spec.ReclaimPolicy != ipam.ReclaimRetain {
			t.Errorf("reclaimPolicy = %q, want Retain", claim.Spec.ReclaimPolicy)
		}
	})

	t.Run("explicit claim values win over class policy", func(t *testing.T) {
		claim := &ipam.IPClaim{Spec: ipam.IPClaimSpec{
			PrefixLength:  28,
			ReclaimPolicy: ipam.ReclaimDelete,
		}}
		applyClassPolicy(claim, testClass())
		if claim.Spec.PrefixLength != 28 {
			t.Errorf("prefixLength = %d, want 28 (explicit)", claim.Spec.PrefixLength)
		}
		if claim.Spec.ReclaimPolicy != ipam.ReclaimDelete {
			t.Errorf("reclaimPolicy = %q, want Delete (explicit)", claim.Spec.ReclaimPolicy)
		}
	})
}

func TestPrefixWithinClass(t *testing.T) {
	c := testClass()
	tests := []struct {
		name    string
		prefix  int
		wantErr bool
	}{
		{name: "within bounds", prefix: 26},
		{name: "at min", prefix: 24},
		{name: "at max", prefix: 28},
		{name: "below min", prefix: 22, wantErr: true},
		{name: "above max", prefix: 30, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := prefixWithinClass(tt.prefix, c)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for prefix %d", tt.prefix)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for prefix %d: %v", tt.prefix, err)
			}
		})
	}
}

func TestPrefixWithinClass_UnboundedClass(t *testing.T) {
	// A class with no allowedPrefixLengths accepts any positive prefix.
	c := &ipamv1alpha1.IPClass{Spec: ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4}}
	if err := prefixWithinClass(30, c); err != nil {
		t.Fatalf("unbounded class should accept prefix 30: %v", err)
	}
}

func TestClassResolutionError(t *testing.T) {
	t.Run("named class not found is a bad request", func(t *testing.T) {
		claim := &ipam.IPClaim{Spec: ipam.IPClaimSpec{ClassName: "ghost"}}
		reason, err := classResolutionError(allocator.ErrClassNotFound, claim)
		if reason != "class_not_found" {
			t.Errorf("reason = %q, want class_not_found", reason)
		}
		if err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("no default class is a bad request", func(t *testing.T) {
		claim := &ipam.IPClaim{}
		reason, err := classResolutionError(allocator.ErrClassNotFound, claim)
		if reason != "class_not_found" {
			t.Errorf("reason = %q, want class_not_found", reason)
		}
		if err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("other errors are internal", func(t *testing.T) {
		claim := &ipam.IPClaim{}
		reason, _ := classResolutionError(errors.New("boom"), claim)
		if reason != "internal" {
			t.Errorf("reason = %q, want internal", reason)
		}
	})
}

func TestResolveClass_DirectPoolResolvesNoClass(t *testing.T) {
	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	r := &AllocatingREST{strategy: NewStrategy(scheme)}
	claim := &ipam.IPClaim{Spec: ipam.IPClaimSpec{PoolRef: &ipam.NamespacedRef{Name: "us-east"}}}
	// A direct poolRef claim must not touch the database; db is nil here, so a
	// class lookup would panic. Getting (nil, nil) proves the class path is
	// skipped entirely.
	class, err := r.resolveClass(context.Background(), tenant.Identity{Name: "proj"}, claim)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if class != nil {
		t.Fatalf("expected no class for a direct poolRef claim, got %v", class)
	}
}

func TestValidateIPClaim_SelectorExclusivity(t *testing.T) {
	base := func() *ipam.IPClaim {
		return &ipam.IPClaim{Spec: ipam.IPClaimSpec{IPFamily: ipam.IPv4, PrefixLength: 26}}
	}
	tests := []struct {
		name    string
		mutate  func(*ipam.IPClaim)
		wantErr bool
	}{
		{name: "className only", mutate: func(c *ipam.IPClaim) { c.Spec.ClassName = "egress" }},
		{name: "poolRef only", mutate: func(c *ipam.IPClaim) { c.Spec.PoolRef = &ipam.NamespacedRef{Name: "p"} }},
		{
			name:    "none set",
			mutate:  func(*ipam.IPClaim) {},
			wantErr: true,
		},
		{
			name: "className and poolRef",
			mutate: func(c *ipam.IPClaim) {
				c.Spec.ClassName = "egress"
				c.Spec.PoolRef = &ipam.NamespacedRef{Name: "p"}
			},
			wantErr: true,
		},
		{
			name: "poolRef and poolSelector",
			mutate: func(c *ipam.IPClaim) {
				c.Spec.PoolRef = &ipam.NamespacedRef{Name: "p"}
				c.Spec.PoolSelector = &ipam.PoolSelector{}
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			errs := validateIPClaim(c)
			if tt.wantErr && len(errs) == 0 {
				t.Fatalf("expected validation error, got none")
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("unexpected validation errors: %v", errs)
			}
		})
	}
}

func TestValidateUpdate_ClassNameImmutable(t *testing.T) {
	old := &ipam.IPClaim{Spec: ipam.IPClaimSpec{IPFamily: ipam.IPv4, PrefixLength: 26, ClassName: "egress"}}
	updated := old.DeepCopy()
	updated.Spec.ClassName = "internal"
	errs := NewStrategy(nil).ValidateUpdate(context.Background(), updated, old)
	if len(errs) == 0 {
		t.Fatalf("expected className immutability error")
	}
}
