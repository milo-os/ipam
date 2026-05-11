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
		internal, external interface{}
		toInternal         conversion.ConversionFunc
		toExternal         conversion.ConversionFunc
	}{
		{
			(*ipam.IPPrefixClass)(nil), (*IPPrefixClass)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPPrefixClass_To_ipam(a.(*IPPrefixClass), b.(*ipam.IPPrefixClass))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPPrefixClass_To_v1alpha1(a.(*ipam.IPPrefixClass), b.(*IPPrefixClass))
			},
		},
		{
			(*ipam.IPPrefixClassList)(nil), (*IPPrefixClassList)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPPrefixClassList_To_ipam(a.(*IPPrefixClassList), b.(*ipam.IPPrefixClassList))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPPrefixClassList_To_v1alpha1(a.(*ipam.IPPrefixClassList), b.(*IPPrefixClassList))
			},
		},
		{
			(*ipam.IPPrefix)(nil), (*IPPrefix)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPPrefix_To_ipam(a.(*IPPrefix), b.(*ipam.IPPrefix))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPPrefix_To_v1alpha1(a.(*ipam.IPPrefix), b.(*IPPrefix))
			},
		},
		{
			(*ipam.IPPrefixList)(nil), (*IPPrefixList)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPPrefixList_To_ipam(a.(*IPPrefixList), b.(*ipam.IPPrefixList))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPPrefixList_To_v1alpha1(a.(*ipam.IPPrefixList), b.(*IPPrefixList))
			},
		},
		{
			(*ipam.IPPrefixClaim)(nil), (*IPPrefixClaim)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPPrefixClaim_To_ipam(a.(*IPPrefixClaim), b.(*ipam.IPPrefixClaim))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPPrefixClaim_To_v1alpha1(a.(*ipam.IPPrefixClaim), b.(*IPPrefixClaim))
			},
		},
		{
			(*ipam.IPPrefixClaimList)(nil), (*IPPrefixClaimList)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPPrefixClaimList_To_ipam(a.(*IPPrefixClaimList), b.(*ipam.IPPrefixClaimList))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPPrefixClaimList_To_v1alpha1(a.(*ipam.IPPrefixClaimList), b.(*IPPrefixClaimList))
			},
		},
		{
			(*ipam.IPAddress)(nil), (*IPAddress)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPAddress_To_ipam(a.(*IPAddress), b.(*ipam.IPAddress))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPAddress_To_v1alpha1(a.(*ipam.IPAddress), b.(*IPAddress))
			},
		},
		{
			(*ipam.IPAddressList)(nil), (*IPAddressList)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPAddressList_To_ipam(a.(*IPAddressList), b.(*ipam.IPAddressList))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPAddressList_To_v1alpha1(a.(*ipam.IPAddressList), b.(*IPAddressList))
			},
		},
		{
			(*ipam.IPAddressClaim)(nil), (*IPAddressClaim)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPAddressClaim_To_ipam(a.(*IPAddressClaim), b.(*ipam.IPAddressClaim))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPAddressClaim_To_v1alpha1(a.(*ipam.IPAddressClaim), b.(*IPAddressClaim))
			},
		},
		{
			(*ipam.IPAddressClaimList)(nil), (*IPAddressClaimList)(nil),
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_v1alpha1_IPAddressClaimList_To_ipam(a.(*IPAddressClaimList), b.(*ipam.IPAddressClaimList))
			},
			func(a, b interface{}, sc conversion.Scope) error {
				return convert_ipam_IPAddressClaimList_To_v1alpha1(a.(*ipam.IPAddressClaimList), b.(*IPAddressClaimList))
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
