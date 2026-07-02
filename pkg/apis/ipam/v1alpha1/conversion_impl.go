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

func toIpamPoolSelector(in *PoolSelector) *ipam.PoolSelector {
	if in == nil {
		return nil
	}
	return &ipam.PoolSelector{
		LabelSelector: in.LabelSelector.DeepCopy(),
		ProjectRef:    toIpamLocalRef(in.ProjectRef),
	}
}
func toV1PoolSelector(in *ipam.PoolSelector) *PoolSelector {
	if in == nil {
		return nil
	}
	return &PoolSelector{
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

func toStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func toStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ----------------------------------------------------------------------------
// IPPool
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPPool_To_ipam(in *IPPool, out *ipam.IPPool) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPPoolSpec{
		CIDR:          in.Spec.CIDR,
		IPFamily:      ipam.IPFamily(in.Spec.IPFamily),
		ParentPoolRef: toIpamLocalRef(in.Spec.ParentPoolRef),
		PrefixLength:  in.Spec.PrefixLength,
		Allocation:    toIpamAllocation(in.Spec.Allocation),
		Visibility:    in.Spec.Visibility,
		ClassNames:    toStringSlice(in.Spec.ClassNames),
	}
	out.Status = ipam.IPPoolStatus{
		Phase:              ipam.PoolPhase(in.Status.Phase),
		AllocatedCIDR:      in.Status.AllocatedCIDR,
		IPFamily:           ipam.IPFamily(in.Status.IPFamily),
		Capacity:           ipam.PoolCapacity(in.Status.Capacity),
		LargestFreePrefix:  in.Status.LargestFreePrefix,
		UtilizationPercent: in.Status.UtilizationPercent,
		Conditions:         toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPPool_To_v1alpha1(in *ipam.IPPool, out *IPPool) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPPoolSpec{
		CIDR:          in.Spec.CIDR,
		IPFamily:      IPFamily(in.Spec.IPFamily),
		ParentPoolRef: toV1LocalRef(in.Spec.ParentPoolRef),
		PrefixLength:  in.Spec.PrefixLength,
		Allocation:    toV1Allocation(in.Spec.Allocation),
		Visibility:    in.Spec.Visibility,
		ClassNames:    toStringSlice(in.Spec.ClassNames),
	}
	out.Status = IPPoolStatus{
		Phase:              PoolPhase(in.Status.Phase),
		AllocatedCIDR:      in.Status.AllocatedCIDR,
		IPFamily:           IPFamily(in.Status.IPFamily),
		Capacity:           PoolCapacity(in.Status.Capacity),
		LargestFreePrefix:  in.Status.LargestFreePrefix,
		UtilizationPercent: in.Status.UtilizationPercent,
		Conditions:         toIpamConditions(in.Status.Conditions),
	}
	return nil
}

func convert_v1alpha1_IPPoolList_To_ipam(in *IPPoolList, out *ipam.IPPoolList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPPool, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPPool_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPPoolList_To_v1alpha1(in *ipam.IPPoolList, out *IPPoolList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPPool, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPPool_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// IPAllocation
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPAllocation_To_ipam(in *IPAllocation, out *ipam.IPAllocation) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPAllocationSpec{
		IPFamily:  ipam.IPFamily(in.Spec.IPFamily),
		PoolRef:   ipam.LocalRef{Name: in.Spec.PoolRef.Name},
		ClassName: in.Spec.ClassName,
	}
	out.Status = ipam.IPAllocationStatus{
		Phase:         ipam.AllocationPhase(in.Status.Phase),
		AllocatedCIDR: in.Status.AllocatedCIDR,
		Conditions:    toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPAllocation_To_v1alpha1(in *ipam.IPAllocation, out *IPAllocation) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPAllocationSpec{
		IPFamily:  IPFamily(in.Spec.IPFamily),
		PoolRef:   LocalRef{Name: in.Spec.PoolRef.Name},
		ClassName: in.Spec.ClassName,
	}
	out.Status = IPAllocationStatus{
		Phase:         AllocationPhase(in.Status.Phase),
		AllocatedCIDR: in.Status.AllocatedCIDR,
		Conditions:    toIpamConditions(in.Status.Conditions),
	}
	return nil
}

func convert_v1alpha1_IPAllocationList_To_ipam(in *IPAllocationList, out *ipam.IPAllocationList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPAllocation, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPAllocation_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPAllocationList_To_v1alpha1(in *ipam.IPAllocationList, out *IPAllocationList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPAllocation, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPAllocation_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// IPClaim
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPClaim_To_ipam(in *IPClaim, out *ipam.IPClaim) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPClaimSpec{
		IPFamily:      ipam.IPFamily(in.Spec.IPFamily),
		PrefixLength:  in.Spec.PrefixLength,
		ClassName:     in.Spec.ClassName,
		PoolSelector:  toIpamPoolSelector(in.Spec.PoolSelector),
		PoolRef:       toIpamNamespacedRef(in.Spec.PoolRef),
		ReclaimPolicy: ipam.ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:      toIpamObjectRef(in.Spec.OwnerRef),
	}
	out.Status = ipam.IPClaimStatus{
		Phase:              ipam.ClaimPhase(in.Status.Phase),
		AllocatedCIDR:      in.Status.AllocatedCIDR,
		BoundAllocationRef: toIpamLocalRef(in.Status.BoundAllocationRef),
		Conditions:         toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPClaim_To_v1alpha1(in *ipam.IPClaim, out *IPClaim) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPClaimSpec{
		IPFamily:      IPFamily(in.Spec.IPFamily),
		PrefixLength:  in.Spec.PrefixLength,
		ClassName:     in.Spec.ClassName,
		PoolSelector:  toV1PoolSelector(in.Spec.PoolSelector),
		PoolRef:       toV1NamespacedRef(in.Spec.PoolRef),
		ReclaimPolicy: ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:      toV1ObjectRef(in.Spec.OwnerRef),
	}
	out.Status = IPClaimStatus{
		Phase:              ClaimPhase(in.Status.Phase),
		AllocatedCIDR:      in.Status.AllocatedCIDR,
		BoundAllocationRef: toV1LocalRef(in.Status.BoundAllocationRef),
		Conditions:         toIpamConditions(in.Status.Conditions),
	}
	return nil
}

func convert_v1alpha1_IPClaimList_To_ipam(in *IPClaimList, out *ipam.IPClaimList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPClaim, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPClaim_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPClaimList_To_v1alpha1(in *ipam.IPClaimList, out *IPClaimList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPClaim, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPClaim_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// IPClass
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPClass_To_ipam(in *IPClass, out *ipam.IPClass) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPClassSpec{
		Provisioner: in.Spec.Provisioner,
		Parameters:  toStringMap(in.Spec.Parameters),
		IPFamily:    ipam.IPFamily(in.Spec.IPFamily),
		Strategy:    ipam.Strategy(in.Spec.Strategy),
		AllowedPrefixLengths: ipam.PrefixLengthRange{
			Min: in.Spec.AllowedPrefixLengths.Min,
			Max: in.Spec.AllowedPrefixLengths.Max,
		},
		DefaultPrefixLength: in.Spec.DefaultPrefixLength,
		ReclaimPolicy:       ipam.ReclaimPolicy(in.Spec.ReclaimPolicy),
		Visibility:          in.Spec.Visibility,
	}
	return nil
}
func convert_ipam_IPClass_To_v1alpha1(in *ipam.IPClass, out *IPClass) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPClassSpec{
		Provisioner: in.Spec.Provisioner,
		Parameters:  toStringMap(in.Spec.Parameters),
		IPFamily:    IPFamily(in.Spec.IPFamily),
		Strategy:    Strategy(in.Spec.Strategy),
		AllowedPrefixLengths: PrefixLengthRange{
			Min: in.Spec.AllowedPrefixLengths.Min,
			Max: in.Spec.AllowedPrefixLengths.Max,
		},
		DefaultPrefixLength: in.Spec.DefaultPrefixLength,
		ReclaimPolicy:       ReclaimPolicy(in.Spec.ReclaimPolicy),
		Visibility:          in.Spec.Visibility,
	}
	return nil
}

func convert_v1alpha1_IPClassList_To_ipam(in *IPClassList, out *ipam.IPClassList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ipam.IPClass, len(in.Items))
		for i := range in.Items {
			if err := convert_v1alpha1_IPClass_To_ipam(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
func convert_ipam_IPClassList_To_v1alpha1(in *ipam.IPClassList, out *IPClassList) error {
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]IPClass, len(in.Items))
		for i := range in.Items {
			if err := convert_ipam_IPClass_To_v1alpha1(&in.Items[i], &out.Items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
