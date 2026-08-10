package v1alpha1

import (
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// RegisterConversions wires conversion functions for round-tripping between
// v1alpha1 and internal IPAM types. The internal and external structs are
// declared with identical field shapes; sub-types differ only by tag and
// named type, so conversion is a series of mechanical field copies.
func RegisterConversions(s *runtime.Scheme) error {
	pairs := []struct {
		internal, external any
		toInternal         conversion.ConversionFunc
		toExternal         conversion.ConversionFunc
	}{
		{
			(*ipam.IPClass)(nil), (*IPClass)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_IPClass_To_ipam(a.(*IPClass), b.(*ipam.IPClass))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_ipam_IPClass_To_v1alpha1(a.(*ipam.IPClass), b.(*IPClass))
			},
		},
		{
			(*ipam.IPClassList)(nil), (*IPClassList)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_IPClassList_To_ipam(a.(*IPClassList), b.(*ipam.IPClassList))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_ipam_IPClassList_To_v1alpha1(a.(*ipam.IPClassList), b.(*IPClassList))
			},
		},
		{
			(*ipam.IPPool)(nil), (*IPPool)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_IPPool_To_ipam(a.(*IPPool), b.(*ipam.IPPool))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_ipam_IPPool_To_v1alpha1(a.(*ipam.IPPool), b.(*IPPool))
			},
		},
		{
			(*ipam.IPPoolList)(nil), (*IPPoolList)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_IPPoolList_To_ipam(a.(*IPPoolList), b.(*ipam.IPPoolList))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_ipam_IPPoolList_To_v1alpha1(a.(*ipam.IPPoolList), b.(*IPPoolList))
			},
		},
		{
			(*ipam.IPAllocation)(nil), (*IPAllocation)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_IPAllocation_To_ipam(a.(*IPAllocation), b.(*ipam.IPAllocation))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_ipam_IPAllocation_To_v1alpha1(a.(*ipam.IPAllocation), b.(*IPAllocation))
			},
		},
		{
			(*ipam.IPAllocationList)(nil), (*IPAllocationList)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_IPAllocationList_To_ipam(a.(*IPAllocationList), b.(*ipam.IPAllocationList))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_ipam_IPAllocationList_To_v1alpha1(a.(*ipam.IPAllocationList), b.(*IPAllocationList))
			},
		},
		{
			(*ipam.IPClaim)(nil), (*IPClaim)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_IPClaim_To_ipam(a.(*IPClaim), b.(*ipam.IPClaim))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_ipam_IPClaim_To_v1alpha1(a.(*ipam.IPClaim), b.(*IPClaim))
			},
		},
		{
			(*ipam.IPClaimList)(nil), (*IPClaimList)(nil),
			func(a, b any, sc conversion.Scope) error {
				return convert_v1alpha1_IPClaimList_To_ipam(a.(*IPClaimList), b.(*ipam.IPClaimList))
			},
			func(a, b any, sc conversion.Scope) error {
				return convert_ipam_IPClaimList_To_v1alpha1(a.(*ipam.IPClaimList), b.(*IPClaimList))
			},
		},
	}
	for _, p := range pairs {
		if err := s.AddGeneratedConversionFunc(p.external, p.internal, p.toInternal); err != nil {
			return err
		}
		if err := s.AddGeneratedConversionFunc(p.internal, p.external, p.toExternal); err != nil {
			return err
		}
	}
	return nil
}
