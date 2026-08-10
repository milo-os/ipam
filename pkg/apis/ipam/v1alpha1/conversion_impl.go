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

func toIpamObjectRef(in *ObjectRef) *ipam.ObjectRef {
	if in == nil {
		return nil
	}
	return &ipam.ObjectRef{
		APIGroup:  in.APIGroup,
		Kind:      in.Kind,
		Namespace: in.Namespace,
		Name:      in.Name,
		UID:       in.UID,
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
		UID:       in.UID,
	}
}

// toIpamScope and toV1Scope copy a claim's, pool's, or allocation's scope map.
// The map is copied rather than aliased so a conversion never lets a caller
// mutate the source object's scope — the field is immutable by contract and
// aliasing would make that contract enforceable only by convention.
func toIpamScope(in map[string]ScopeRef) map[string]ipam.ScopeRef {
	if in == nil {
		return nil
	}
	out := make(map[string]ipam.ScopeRef, len(in))
	for role, ref := range in {
		out[role] = ipam.ScopeRef{
			APIGroup: ref.APIGroup,
			Kind:     ref.Kind,
			Name:     ref.Name,
			UID:      ref.UID,
		}
	}
	return out
}
func toV1Scope(in map[string]ipam.ScopeRef) map[string]ScopeRef {
	if in == nil {
		return nil
	}
	out := make(map[string]ScopeRef, len(in))
	for role, ref := range in {
		out[role] = ScopeRef{
			APIGroup: ref.APIGroup,
			Kind:     ref.Kind,
			Name:     ref.Name,
			UID:      ref.UID,
		}
	}
	return out
}

func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copyParameters(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyDuration(in *metav1.Duration) *metav1.Duration {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func copyInt32Ptr(in *int32) *int32 {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func toIpamPrefixLengthRange(in *PrefixLengthRange) *ipam.PrefixLengthRange {
	if in == nil {
		return nil
	}
	return &ipam.PrefixLengthRange{Min: in.Min, Max: in.Max}
}
func toV1PrefixLengthRange(in *ipam.PrefixLengthRange) *PrefixLengthRange {
	if in == nil {
		return nil
	}
	return &PrefixLengthRange{Min: in.Min, Max: in.Max}
}

func toIpamReservations(in *ReservationSpec) *ipam.ReservationSpec {
	if in == nil {
		return nil
	}
	return &ipam.ReservationSpec{
		Leading:          in.Leading,
		Trailing:         in.Trailing,
		UnitPrefixLength: in.UnitPrefixLength,
	}
}
func toV1Reservations(in *ipam.ReservationSpec) *ReservationSpec {
	if in == nil {
		return nil
	}
	return &ReservationSpec{
		Leading:          in.Leading,
		Trailing:         in.Trailing,
		UnitPrefixLength: in.UnitPrefixLength,
	}
}

func toIpamRouting(in RoutingSpec) ipam.RoutingSpec {
	return ipam.RoutingSpec{
		Internal: ipam.InternalRouting(in.Internal),
		External: ipam.ExternalRouting(in.External),
	}
}
func toV1Routing(in ipam.RoutingSpec) RoutingSpec {
	return RoutingSpec{
		Internal: InternalRouting(in.Internal),
		External: ExternalRouting(in.External),
	}
}

func toIpamAllocation(in AllocationSpec) ipam.AllocationSpec {
	return ipam.AllocationSpec{
		MinPrefixLength: in.MinPrefixLength,
		MaxPrefixLength: in.MaxPrefixLength,
		Strategy:        ipam.AllocationStrategy(in.Strategy),
	}
}
func toV1Allocation(in ipam.AllocationSpec) AllocationSpec {
	return AllocationSpec{
		MinPrefixLength: in.MinPrefixLength,
		MaxPrefixLength: in.MaxPrefixLength,
		Strategy:        AllocationStrategy(in.Strategy),
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

// ----------------------------------------------------------------------------
// IPClass
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPClass_To_ipam(in *IPClass, out *ipam.IPClass) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPClassSpec{
		IPFamily:             ipam.IPFamily(in.Spec.IPFamily),
		ParentClassName:      in.Spec.ParentClassName,
		PoolPer:              copyStrings(in.Spec.PoolPer),
		UniqueWithin:         copyStrings(in.Spec.UniqueWithin),
		AllowedPrefixLengths: toIpamPrefixLengthRange(in.Spec.AllowedPrefixLengths),
		DefaultPrefixLength:  in.Spec.DefaultPrefixLength,
		Reservations:         toIpamReservations(in.Spec.Reservations),
		Routing:              toIpamRouting(in.Spec.Routing),
		Strategy:             ipam.PoolSelectionStrategy(in.Spec.Strategy),
		ReclaimPolicy:        ipam.ReclaimPolicy(in.Spec.ReclaimPolicy),
		RetentionLease:       copyDuration(in.Spec.RetentionLease),
		Visibility:           in.Spec.Visibility,
		BackingProjects:      copyStrings(in.Spec.BackingProjects),
		Provisioner:          in.Spec.Provisioner,
		Parameters:           copyParameters(in.Spec.Parameters),
	}
	out.Status = ipam.IPClassStatus{
		Phase:              ipam.ClassPhase(in.Status.Phase),
		OfferingPools:      in.Status.OfferingPools,
		ProvisionedPools:   in.Status.ProvisionedPools,
		RequiredScopeRoles: copyStrings(in.Status.RequiredScopeRoles),
		Conditions:         toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPClass_To_v1alpha1(in *ipam.IPClass, out *IPClass) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPClassSpec{
		IPFamily:             IPFamily(in.Spec.IPFamily),
		ParentClassName:      in.Spec.ParentClassName,
		PoolPer:              copyStrings(in.Spec.PoolPer),
		UniqueWithin:         copyStrings(in.Spec.UniqueWithin),
		AllowedPrefixLengths: toV1PrefixLengthRange(in.Spec.AllowedPrefixLengths),
		DefaultPrefixLength:  in.Spec.DefaultPrefixLength,
		Reservations:         toV1Reservations(in.Spec.Reservations),
		Routing:              toV1Routing(in.Spec.Routing),
		Strategy:             PoolSelectionStrategy(in.Spec.Strategy),
		ReclaimPolicy:        ReclaimPolicy(in.Spec.ReclaimPolicy),
		RetentionLease:       copyDuration(in.Spec.RetentionLease),
		Visibility:           in.Spec.Visibility,
		BackingProjects:      copyStrings(in.Spec.BackingProjects),
		Provisioner:          in.Spec.Provisioner,
		Parameters:           copyParameters(in.Spec.Parameters),
	}
	out.Status = IPClassStatus{
		Phase:              ClassPhase(in.Status.Phase),
		OfferingPools:      in.Status.OfferingPools,
		ProvisionedPools:   in.Status.ProvisionedPools,
		RequiredScopeRoles: copyStrings(in.Status.RequiredScopeRoles),
		Conditions:         toIpamConditions(in.Status.Conditions),
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

// ----------------------------------------------------------------------------
// IPPool
// ----------------------------------------------------------------------------

func convert_v1alpha1_IPPool_To_ipam(in *IPPool, out *ipam.IPPool) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = ipam.IPPoolSpec{
		CIDR:              in.Spec.CIDR,
		IPFamily:          ipam.IPFamily(in.Spec.IPFamily),
		ParentPoolRef:     toIpamLocalRef(in.Spec.ParentPoolRef),
		PrefixLength:      in.Spec.PrefixLength,
		ClassNames:        copyStrings(in.Spec.ClassNames),
		ClassRef:          toIpamLocalRef(in.Spec.ClassRef),
		Scope:             toIpamScope(in.Spec.Scope),
		Reservations:      toIpamReservations(in.Spec.Reservations),
		MaxRetentionLease: copyDuration(in.Spec.MaxRetentionLease),
		Allocation:        toIpamAllocation(in.Spec.Allocation),
		Visibility:        in.Spec.Visibility,
	}
	out.Status = ipam.IPPoolStatus{
		Phase:              ipam.PoolPhase(in.Status.Phase),
		AllocatedCIDR:      in.Status.AllocatedCIDR,
		IPFamily:           ipam.IPFamily(in.Status.IPFamily),
		ScopeDigest:        in.Status.ScopeDigest,
		Capacity:           ipam.PoolCapacity(in.Status.Capacity),
		UtilizationPercent: in.Status.UtilizationPercent,
		Conditions:         toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPPool_To_v1alpha1(in *ipam.IPPool, out *IPPool) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPPoolSpec{
		CIDR:              in.Spec.CIDR,
		IPFamily:          IPFamily(in.Spec.IPFamily),
		ParentPoolRef:     toV1LocalRef(in.Spec.ParentPoolRef),
		PrefixLength:      in.Spec.PrefixLength,
		ClassNames:        copyStrings(in.Spec.ClassNames),
		ClassRef:          toV1LocalRef(in.Spec.ClassRef),
		Scope:             toV1Scope(in.Spec.Scope),
		Reservations:      toV1Reservations(in.Spec.Reservations),
		MaxRetentionLease: copyDuration(in.Spec.MaxRetentionLease),
		Allocation:        toV1Allocation(in.Spec.Allocation),
		Visibility:        in.Spec.Visibility,
	}
	out.Status = IPPoolStatus{
		Phase:              PoolPhase(in.Status.Phase),
		AllocatedCIDR:      in.Status.AllocatedCIDR,
		IPFamily:           IPFamily(in.Status.IPFamily),
		ScopeDigest:        in.Status.ScopeDigest,
		Capacity:           PoolCapacity(in.Status.Capacity),
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
		IPFamily:      ipam.IPFamily(in.Spec.IPFamily),
		PoolRef:       ipam.LocalRef{Name: in.Spec.PoolRef.Name},
		ClassName:     in.Spec.ClassName,
		Purpose:       ipam.AllocationPurpose(in.Spec.Purpose),
		ClaimRef:      toIpamLocalRef(in.Spec.ClaimRef),
		Scope:         toIpamScope(in.Spec.Scope),
		ReclaimPolicy: ipam.ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:      toIpamObjectRef(in.Spec.OwnerRef),
	}
	out.Status = ipam.IPAllocationStatus{
		Phase:         ipam.AllocationPhase(in.Status.Phase),
		AllocatedCIDR: in.Status.AllocatedCIDR,
		Address:       in.Status.Address,
		ScopeDigest:   in.Status.ScopeDigest,
		Conditions:    toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPAllocation_To_v1alpha1(in *ipam.IPAllocation, out *IPAllocation) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPAllocationSpec{
		IPFamily:      IPFamily(in.Spec.IPFamily),
		PoolRef:       LocalRef{Name: in.Spec.PoolRef.Name},
		ClassName:     in.Spec.ClassName,
		Purpose:       AllocationPurpose(in.Spec.Purpose),
		ClaimRef:      toV1LocalRef(in.Spec.ClaimRef),
		Scope:         toV1Scope(in.Spec.Scope),
		ReclaimPolicy: ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:      toV1ObjectRef(in.Spec.OwnerRef),
	}
	out.Status = IPAllocationStatus{
		Phase:         AllocationPhase(in.Status.Phase),
		AllocatedCIDR: in.Status.AllocatedCIDR,
		Address:       in.Status.Address,
		ScopeDigest:   in.Status.ScopeDigest,
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
		ClassName:     in.Spec.ClassName,
		IPFamily:      ipam.IPFamily(in.Spec.IPFamily),
		Address:       in.Spec.Address,
		PrefixLength:  copyInt32Ptr(in.Spec.PrefixLength),
		Scope:         toIpamScope(in.Spec.Scope),
		ReclaimPolicy: ipam.ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:      toIpamObjectRef(in.Spec.OwnerRef),
	}
	out.Status = ipam.IPClaimStatus{
		Phase:              ipam.ClaimPhase(in.Status.Phase),
		AllocatedCIDR:      in.Status.AllocatedCIDR,
		Address:            in.Status.Address,
		PoolRef:            toIpamLocalRef(in.Status.PoolRef),
		BoundAllocationRef: toIpamLocalRef(in.Status.BoundAllocationRef),
		Conditions:         toIpamConditions(in.Status.Conditions),
	}
	return nil
}
func convert_ipam_IPClaim_To_v1alpha1(in *ipam.IPClaim, out *IPClaim) error {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = IPClaimSpec{
		ClassName:     in.Spec.ClassName,
		IPFamily:      IPFamily(in.Spec.IPFamily),
		Address:       in.Spec.Address,
		PrefixLength:  copyInt32Ptr(in.Spec.PrefixLength),
		Scope:         toV1Scope(in.Spec.Scope),
		ReclaimPolicy: ReclaimPolicy(in.Spec.ReclaimPolicy),
		OwnerRef:      toV1ObjectRef(in.Spec.OwnerRef),
	}
	out.Status = IPClaimStatus{
		Phase:              ClaimPhase(in.Status.Phase),
		AllocatedCIDR:      in.Status.AllocatedCIDR,
		Address:            in.Status.Address,
		PoolRef:            toV1LocalRef(in.Status.PoolRef),
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
