package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// ----------------------------------------------------------------------------
// Sub-type helpers
// ----------------------------------------------------------------------------

func toIpamLocalRef(in *LocalRef) *ipam.LocalRef {
	if in == nil {
		return nil
	}
	return &ipam.LocalRef{Name: in.Name}
}
func toV1LocalRef(in *ipam.LocalRef) *LocalRef {
	if in == nil {
		return nil
	}
	return &LocalRef{Name: in.Name}
}

func toIpamNamespacedRef(in *NamespacedRef) *ipam.NamespacedRef {
	if in == nil {
		return nil
	}
	return &ipam.NamespacedRef{
		Name:       in.Name,
		ProjectRef: toIpamLocalRef(in.ProjectRef),
	}
}
func toV1NamespacedRef(in *ipam.NamespacedRef) *NamespacedRef {
	if in == nil {
		return nil
	}
	return &NamespacedRef{
		Name:       in.Name,
		ProjectRef: toV1LocalRef(in.ProjectRef),
	}
}

func toIpamPrefixSelector(in *PrefixSelector) *ipam.PrefixSelector {
	if in == nil {
		return nil
	}
	return &ipam.PrefixSelector{
		LabelSelector: in.LabelSelector.DeepCopy(),
		ProjectRef:    toIpamLocalRef(in.ProjectRef),
	}
}
func toV1PrefixSelector(in *ipam.PrefixSelector) *PrefixSelector {
	if in == nil {
		return nil
	}
	return &PrefixSelector{
		LabelSelector: in.LabelSelector.DeepCopy(),
		ProjectRef:    toV1LocalRef(in.ProjectRef),
	}
}

func toIpamObjectRef(in *ObjectRef) *ipam.ObjectRef {
	if in == nil {
		return nil
	}
	return &ipam.ObjectRef{
		APIGroup:  in.APIGroup,
		Kind:      in.Kind,
		Namespace: in.Namespace,
		Name:      in.Name,
	}
}
func toV1ObjectRef(in *ipam.ObjectRef) *ObjectRef {
	if in == nil {
		return nil
	}
	return &ObjectRef{
		APIGroup:  in.APIGroup,
		Kind:      in.Kind,
		Namespace: in.Namespace,
		Name:      in.Name,
	}
}

func toIpamAllocation(in AllocationSpec) ipam.AllocationSpec {
	return ipam.AllocationSpec{
		MinPrefixLength: in.MinPrefixLength,
		MaxPrefixLength: in.MaxPrefixLength,
		Strategy:        ipam.Strategy(in.Strategy),
	}
}
func toV1Allocation(in ipam.AllocationSpec) AllocationSpec {
	return AllocationSpec{
		MinPrefixLength: in.MinPrefixLength,
		MaxPrefixLength: in.MaxPrefixLength,
		Strategy:        Strategy(in.Strategy),
	}
}

func toIpamConditions(in []metav1.Condition) []metav1.Condition {
	if in == nil {
		return nil
	}
	out := make([]metav1.Condition, len(in))
	copy(out, in)
	return out
}

func toIpamIPPrefixSpec(in *IPPrefixSpec) ipam.IPPrefixSpec {
	return ipam.IPPrefixSpec{
		CIDR:       in.CIDR,
		IPFamily:   ipam.IPFamily(in.IPFamily),
		ClassRef:   ipam.LocalRef{Name: in.ClassRef.Name},
		Allocation: toIpamAllocation(in.Allocation),
		ParentRef:  toIpamObjectRef(in.ParentRef),
	}
}
func toV1IPPrefixSpec(in *ipam.IPPrefixSpec) IPPrefixSpec {
	return IPPrefixSpec{
		CIDR:       in.CIDR,
		IPFamily:   IPFamily(in.IPFamily),
		ClassRef:   LocalRef{Name: in.ClassRef.Name},
		Allocation: toV1Allocation(in.Allocation),
		ParentRef:  toV1ObjectRef(in.ParentRef),
	}
}

func toIpamIPPrefixStatus(in *IPPrefixStatus) ipam.IPPrefixStatus {
	return ipam.IPPrefixStatus{
		Phase:      ipam.PrefixPhase(in.Phase),
		CIDR:       in.CIDR,
		Capacity:   ipam.PrefixCapacity(in.Capacity),
		Conditions: toIpamConditions(in.Conditions),
	}
}
func toV1IPPrefixStatus(in *ipam.IPPrefixStatus) IPPrefixStatus {
	return IPPrefixStatus{
		Phase:      PrefixPhase(in.Phase),
		CIDR:       in.CIDR,
		Capacity:   PrefixCapacity(in.Capacity),
		Conditions: toIpamConditions(in.Conditions),
	}
}

func toIpamPrefixTemplate(in *IPPrefixTemplate) *ipam.IPPrefixTemplate {
	if in == nil {
		return nil
	}
	return &ipam.IPPrefixTemplate{
		Metadata: *in.Metadata.DeepCopy(),
		Spec:     toIpamIPPrefixSpec(&in.Spec),
	}
}
func toV1PrefixTemplate(in *ipam.IPPrefixTemplate) *IPPrefixTemplate {
	if in == nil {
		return nil
	}
	return &IPPrefixTemplate{
		Metadata: *in.Metadata.DeepCopy(),
		Spec:     toV1IPPrefixSpec(&in.Spec),
	}
}

// ----------------------------------------------------------------------------
// IPPrefixClass
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPPrefixClass_To_ipam(in *IPPrefixClass, out *ipam.IPPrefixClass) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPPrefixClassSpec{
		Visibility:        in.Spec.Visibility,
		DefaultAllocation: toIpamAllocation(in.Spec.DefaultAllocation),
	}
	return nil
}
func convert_ipam_IPPrefixClass_To_v1alpha1(in *ipam.IPPrefixClass, out *IPPrefixClass) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPPrefixClassSpec{
		Visibility:        in.Spec.Visibility,
		DefaultAllocation: toV1Allocation(in.Spec.DefaultAllocation),
	}
	return nil
}

func convert_v1alpha1_IPPrefixClassList_To_ipam(in *IPPrefixClassList, out *ipam.IPPrefixClassList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPPrefixClass, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPPrefixClass_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPPrefixClassList_To_v1alpha1(in *ipam.IPPrefixClassList, out *IPPrefixClassList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPPrefixClass, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPPrefixClass_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// IPPrefix
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPPrefix_To_ipam(in *IPPrefix, out *ipam.IPPrefix) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = toIpamIPPrefixSpec(&in.Spec)
	out.Status = toIpamIPPrefixStatus(&in.Status)
	return nil
}
func convert_ipam_IPPrefix_To_v1alpha1(in *ipam.IPPrefix, out *IPPrefix) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = toV1IPPrefixSpec(&in.Spec)
	out.Status = toV1IPPrefixStatus(&in.Status)
	return nil
}

func convert_v1alpha1_IPPrefixList_To_ipam(in *IPPrefixList, out *ipam.IPPrefixList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPPrefix, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPPrefix_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPPrefixList_To_v1alpha1(in *ipam.IPPrefixList, out *IPPrefixList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPPrefix, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPPrefix_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// IPPrefixClaim
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPPrefixClaim_To_ipam(in *IPPrefixClaim, out *ipam.IPPrefixClaim) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPPrefixClaimSpec{
		IPFamily:            ipam.IPFamily(in.Spec.IPFamily),
		PrefixLength:        in.Spec.PrefixLength,
		PrefixSelector:      toIpamPrefixSelector(in.Spec.PrefixSelector),
		PrefixRef:           toIpamNamespacedRef(in.Spec.PrefixRef),
		ChildPrefixTemplate: toIpamPrefixTemplate(in.Spec.ChildPrefixTemplate),
		ReclaimPolicy:       ipam.ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:            toIpamObjectRef(in.Spec.OwnerRef),
	}
	out.Status = ipam.IPPrefixClaimStatus{
		Phase:          ipam.ClaimPhase(in.Status.Phase),
		AllocatedCIDR:  in.Status.AllocatedCIDR,
		BoundPrefixRef: toIpamLocalRef(in.Status.BoundPrefixRef),
		Conditions:     toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPPrefixClaim_To_v1alpha1(in *ipam.IPPrefixClaim, out *IPPrefixClaim) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPPrefixClaimSpec{
		IPFamily:            IPFamily(in.Spec.IPFamily),
		PrefixLength:        in.Spec.PrefixLength,
		PrefixSelector:      toV1PrefixSelector(in.Spec.PrefixSelector),
		PrefixRef:           toV1NamespacedRef(in.Spec.PrefixRef),
		ChildPrefixTemplate: toV1PrefixTemplate(in.Spec.ChildPrefixTemplate),
		ReclaimPolicy:       ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:            toV1ObjectRef(in.Spec.OwnerRef),
	}
	out.Status = IPPrefixClaimStatus{
		Phase:          ClaimPhase(in.Status.Phase),
		AllocatedCIDR:  in.Status.AllocatedCIDR,
		BoundPrefixRef: toV1LocalRef(in.Status.BoundPrefixRef),
		Conditions:     toIpamConditions(in.Status.Conditions),
	}
	return nil
}

func convert_v1alpha1_IPPrefixClaimList_To_ipam(in *IPPrefixClaimList, out *ipam.IPPrefixClaimList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPPrefixClaim, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPPrefixClaim_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPPrefixClaimList_To_v1alpha1(in *ipam.IPPrefixClaimList, out *IPPrefixClaimList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPPrefixClaim, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPPrefixClaim_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// IPAddress
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPAddress_To_ipam(in *IPAddress, out *ipam.IPAddress) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPAddressSpec{
		Address:   in.Spec.Address,
		IPFamily:  ipam.IPFamily(in.Spec.IPFamily),
		PrefixRef: ipam.LocalRef{Name: in.Spec.PrefixRef.Name},
		ClaimRef:  toIpamLocalRef(in.Spec.ClaimRef),
	}
	out.Status = ipam.IPAddressStatus{Conditions: toIpamConditions(in.Status.Conditions)}
	return nil
}
func convert_ipam_IPAddress_To_v1alpha1(in *ipam.IPAddress, out *IPAddress) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPAddressSpec{
		Address:   in.Spec.Address,
		IPFamily:  IPFamily(in.Spec.IPFamily),
		PrefixRef: LocalRef{Name: in.Spec.PrefixRef.Name},
		ClaimRef:  toV1LocalRef(in.Spec.ClaimRef),
	}
	out.Status = IPAddressStatus{Conditions: toIpamConditions(in.Status.Conditions)}
	return nil
}

func convert_v1alpha1_IPAddressList_To_ipam(in *IPAddressList, out *ipam.IPAddressList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPAddress, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPAddress_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPAddressList_To_v1alpha1(in *ipam.IPAddressList, out *IPAddressList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPAddress, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPAddress_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// IPAddressClaim
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPAddressClaim_To_ipam(in *IPAddressClaim, out *ipam.IPAddressClaim) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPAddressClaimSpec{
		IPFamily:       ipam.IPFamily(in.Spec.IPFamily),
		PrefixSelector: toIpamPrefixSelector(in.Spec.PrefixSelector),
		PrefixRef:      toIpamNamespacedRef(in.Spec.PrefixRef),
		ReclaimPolicy:  ipam.ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:       toIpamObjectRef(in.Spec.OwnerRef),
	}
	out.Status = ipam.IPAddressClaimStatus{
		Phase:           ipam.ClaimPhase(in.Status.Phase),
		AllocatedIP:     in.Status.AllocatedIP,
		BoundAddressRef: toIpamLocalRef(in.Status.BoundAddressRef),
		Conditions:      toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPAddressClaim_To_v1alpha1(in *ipam.IPAddressClaim, out *IPAddressClaim) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPAddressClaimSpec{
		IPFamily:       IPFamily(in.Spec.IPFamily),
		PrefixSelector: toV1PrefixSelector(in.Spec.PrefixSelector),
		PrefixRef:      toV1NamespacedRef(in.Spec.PrefixRef),
		ReclaimPolicy:  ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:       toV1ObjectRef(in.Spec.OwnerRef),
	}
	out.Status = IPAddressClaimStatus{
		Phase:           ClaimPhase(in.Status.Phase),
		AllocatedIP:     in.Status.AllocatedIP,
		BoundAddressRef: toV1LocalRef(in.Status.BoundAddressRef),
		Conditions:      toIpamConditions(in.Status.Conditions),
	}
	return nil
}

func convert_v1alpha1_IPAddressClaimList_To_ipam(in *IPAddressClaimList, out *ipam.IPAddressClaimList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPAddressClaim, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPAddressClaim_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPAddressClaimList_To_v1alpha1(in *ipam.IPAddressClaimList, out *IPAddressClaimList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPAddressClaim, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPAddressClaim_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}


