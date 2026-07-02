package ipclass

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func validClass() *ipam.IPClass {
	return &ipam.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public-egress"},
		Spec: ipam.IPClassSpec{
			Provisioner:          ipam.NativeProvisioner,
			IPFamily:             ipam.IPv4,
			Strategy:             ipam.LeastUtilized,
			AllowedPrefixLengths: ipam.PrefixLengthRange{Min: 24, Max: 28},
			DefaultPrefixLength:  26,
			ReclaimPolicy:        ipam.ReclaimRetain,
			Visibility:           "shared",
		},
	}
}

func TestPrepareForCreateDefaultsProvisioner(t *testing.T) {
	c := validClass()
	c.Spec.Provisioner = ""
	NewStrategy(nil).PrepareForCreate(context.Background(), c)
	if c.Spec.Provisioner != ipam.NativeProvisioner {
		t.Fatalf("provisioner: got %q, want %q", c.Spec.Provisioner, ipam.NativeProvisioner)
	}
}

func TestValidateIPClass(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ipam.IPClass)
		wantErr bool
	}{
		{name: "valid", mutate: func(*ipam.IPClass) {}},
		{
			name:    "unknown provisioner rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.Provisioner = "ipam.miloapis.com/aws-byoip" },
			wantErr: true,
		},
		{
			name:    "missing ipFamily rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.IPFamily = "" },
			wantErr: true,
		},
		{
			name:    "bogus ipFamily rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.IPFamily = "IPv5" },
			wantErr: true,
		},
		{
			name:    "unknown strategy rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.Strategy = "Random" },
			wantErr: true,
		},
		{
			name:   "empty strategy allowed",
			mutate: func(c *ipam.IPClass) { c.Spec.Strategy = "" },
		},
		{
			name:    "unknown reclaimPolicy rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.ReclaimPolicy = "Recycle" },
			wantErr: true,
		},
		{
			name:    "unknown visibility rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.Visibility = "public" },
			wantErr: true,
		},
		{
			name:    "min greater than max rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.AllowedPrefixLengths = ipam.PrefixLengthRange{Min: 28, Max: 24} },
			wantErr: true,
		},
		{
			name: "IPv4 max above 32 rejected",
			mutate: func(c *ipam.IPClass) {
				c.Spec.AllowedPrefixLengths = ipam.PrefixLengthRange{Min: 24, Max: 40}
				c.Spec.DefaultPrefixLength = 26
			},
			wantErr: true,
		},
		{
			name: "IPv6 max up to 128 allowed",
			mutate: func(c *ipam.IPClass) {
				c.Spec.IPFamily = ipam.IPv6
				c.Spec.AllowedPrefixLengths = ipam.PrefixLengthRange{Min: 48, Max: 64}
				c.Spec.DefaultPrefixLength = 56
			},
		},
		{
			name:    "default below allowed min rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.DefaultPrefixLength = 20 },
			wantErr: true,
		},
		{
			name:    "default above allowed max rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.DefaultPrefixLength = 30 },
			wantErr: true,
		},
		{
			name:   "zero default allowed (no default)",
			mutate: func(c *ipam.IPClass) { c.Spec.DefaultPrefixLength = 0 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validClass()
			tt.mutate(c)
			errs := validateIPClass(c)
			if tt.wantErr && len(errs) == 0 {
				t.Fatalf("expected validation error, got none")
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("unexpected validation errors: %v", errs)
			}
		})
	}
}

func TestValidateUpdateImmutability(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ipam.IPClass)
		wantErr bool
	}{
		{name: "no change ok", mutate: func(*ipam.IPClass) {}},
		{
			name:    "ipFamily change rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.IPFamily = ipam.IPv6 },
			wantErr: true,
		},
		{
			name:    "provisioner change rejected",
			mutate:  func(c *ipam.IPClass) { c.Spec.Provisioner = "ipam.miloapis.com/aws-byoip" },
			wantErr: true,
		},
		{
			name:   "policy fields mutable",
			mutate: func(c *ipam.IPClass) { c.Spec.ReclaimPolicy = ipam.ReclaimDelete; c.Spec.Strategy = ipam.FirstFit },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := validClass()
			updated := validClass()
			tt.mutate(updated)
			errs := NewStrategy(nil).ValidateUpdate(context.Background(), updated, old)
			if tt.wantErr && len(errs) == 0 {
				t.Fatalf("expected update error, got none")
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("unexpected update errors: %v", errs)
			}
		})
	}
}

func TestOtherDefaultClass(t *testing.T) {
	mkDefault := func(name string) ipam.IPClass {
		return ipam.IPClass{ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{ipam.IsDefaultClassAnnotation: "true"},
		}}
	}
	mkPlain := func(name string) ipam.IPClass {
		return ipam.IPClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}

	tests := []struct {
		name    string
		classes []ipam.IPClass
		self    string
		want    string
	}{
		{
			name:    "no other default",
			classes: []ipam.IPClass{mkPlain("a"), mkDefault("self")},
			self:    "self",
			want:    "",
		},
		{
			name:    "another class is default",
			classes: []ipam.IPClass{mkDefault("other"), mkPlain("self")},
			self:    "self",
			want:    "other",
		},
		{
			name:    "self already the only default (update no-op)",
			classes: []ipam.IPClass{mkDefault("self")},
			self:    "self",
			want:    "",
		},
		{
			name:    "creating first default",
			classes: []ipam.IPClass{mkPlain("a"), mkPlain("b")},
			self:    "self",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := otherDefaultClass(tt.classes, tt.self); got != tt.want {
				t.Fatalf("otherDefaultClass = %q, want %q", got, tt.want)
			}
		})
	}
}
