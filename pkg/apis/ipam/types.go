package ipam

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// IPFamily identifies an IP address family.
type IPFamily string

const (
	IPv4 IPFamily = "IPv4"
	IPv6 IPFamily = "IPv6"
)

// Strategy selects how a free sub-block is chosen.
type Strategy string

const (
	FirstFit      Strategy = "FirstFit"
	BestFit       Strategy = "BestFit"
	LeastUtilized Strategy = "LeastUtilized"
)

// ReclaimPolicy controls disposition of the underlying allocation when a
// claim is deleted.
type ReclaimPolicy string

const (
	ReclaimDelete ReclaimPolicy = "Delete"
	ReclaimRetain ReclaimPolicy = "Retain"
)

// ClaimPhase is the high-level lifecycle phase of a claim.
type ClaimPhase string

const (
	ClaimPending   ClaimPhase = "Pending"
	ClaimBound     ClaimPhase = "Bound"
	ClaimReleasing ClaimPhase = "Releasing"
	ClaimError     ClaimPhase = "Error"
)

// PrefixPhase is the high-level lifecycle phase of an IP prefix.
type PrefixPhase string

const (
	PrefixPending   PrefixPhase = "Pending"
	PrefixReady     PrefixPhase = "Ready"
	PrefixExhausted PrefixPhase = "Exhausted"
	PrefixError     PrefixPhase = "Error"
)

// LocalRef references another IPAM object in the same namespace by name.
type LocalRef struct {
	Name string
}

// NamespacedRef references a named resource with an optional cross-project
// pointer. ProjectRef nil means the reference resolves in the caller's own
// project (or the platform scope for non-tenant requests).
type NamespacedRef struct {
	Name       string
	ProjectRef *LocalRef
}

// PrefixSelector picks a parent IPPrefix by labels, optionally scoped to a
// specific project for cross-project shared pools.
type PrefixSelector struct {
	*metav1.LabelSelector
	ProjectRef *LocalRef
}

// ObjectRef is an opaque cross-API reference. APIGroup and Kind are strings
// to avoid forcing consumers to import the referenced type's package.
type ObjectRef struct {
	APIGroup  string
	Kind      string
	Namespace string
	Name      string
}

// AllocationSpec configures sub-allocation behaviour for a prefix.
type AllocationSpec struct {
	MinPrefixLength int
	MaxPrefixLength int
	Strategy        Strategy
}

// PrefixCapacity reports utilization for an IPPrefix.
type PrefixCapacity struct {
	Total     int64
	Allocated int64
	Available int64
}

// ASNCapacity reports utilization for an ASNPool.
type ASNCapacity struct {
	Total     int64
	Allocated int64
	Available int64
}

// ASNRange is an inclusive [Start, End] ASN range.
type ASNRange struct {
	Start int64
	End   int64
}

// IPPrefixTemplate is the metadata + spec used to materialise an IPPrefix
// child created atomically with an IPPrefixClaim.
type IPPrefixTemplate struct {
	Metadata metav1.ObjectMeta
	Spec     IPPrefixSpec
}

// ----------------------------------------------------------------------------
// IPPrefixClass — cluster-scoped class of prefix pools.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient
// +genclient:nonNamespaced

// IPPrefixClass declares operational properties shared by a class of
// IPPrefix pools.
type IPPrefixClass struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec IPPrefixClassSpec
}

type IPPrefixClassSpec struct {
	Visibility        string
	DefaultAllocation AllocationSpec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPPrefixClassList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPPrefixClass
}

// ----------------------------------------------------------------------------
// IPPrefix — the prefix pool itself.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

type IPPrefix struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPPrefixSpec
	Status IPPrefixStatus
}

type IPPrefixSpec struct {
	CIDR       string
	IPFamily   IPFamily
	ClassRef   LocalRef
	Allocation AllocationSpec
	ParentRef  *ObjectRef
}

type IPPrefixStatus struct {
	Phase      PrefixPhase
	CIDR       string
	Capacity   PrefixCapacity
	Conditions []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPPrefixList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPPrefix
}

// ----------------------------------------------------------------------------
// IPPrefixClaim
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

type IPPrefixClaim struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPPrefixClaimSpec
	Status IPPrefixClaimStatus
}

type IPPrefixClaimSpec struct {
	IPFamily            IPFamily
	PrefixLength        int
	PrefixSelector      *PrefixSelector
	PrefixRef           *NamespacedRef
	ChildPrefixTemplate *IPPrefixTemplate
	ReclaimPolicy       ReclaimPolicy
	OwnerRef            *ObjectRef
}

type IPPrefixClaimStatus struct {
	Phase          ClaimPhase
	AllocatedCIDR  string
	BoundPrefixRef *LocalRef
	Conditions     []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPPrefixClaimList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPPrefixClaim
}

// ----------------------------------------------------------------------------
// IPAddress
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

type IPAddress struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPAddressSpec
	Status IPAddressStatus
}

type IPAddressSpec struct {
	Address   string
	IPFamily  IPFamily
	PrefixRef LocalRef
	ClaimRef  *LocalRef
}

type IPAddressStatus struct {
	Conditions []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPAddressList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPAddress
}

// ----------------------------------------------------------------------------
// IPAddressClaim
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

type IPAddressClaim struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPAddressClaimSpec
	Status IPAddressClaimStatus
}

type IPAddressClaimSpec struct {
	IPFamily       IPFamily
	PrefixSelector *PrefixSelector
	PrefixRef      *NamespacedRef
	ReclaimPolicy  ReclaimPolicy
	OwnerRef       *ObjectRef
}

type IPAddressClaimStatus struct {
	Phase           ClaimPhase
	AllocatedIP     string
	BoundAddressRef *LocalRef
	Conditions      []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPAddressClaimList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPAddressClaim
}

// ----------------------------------------------------------------------------
// ASNPoolClass
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient
// +genclient:nonNamespaced

type ASNPoolClass struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec ASNPoolClassSpec
}

type ASNPoolClassSpec struct {
	Visibility string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type ASNPoolClassList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []ASNPoolClass
}

// ----------------------------------------------------------------------------
// ASNPool
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient
// +genclient:nonNamespaced

type ASNPool struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   ASNPoolSpec
	Status ASNPoolStatus
}

type ASNPoolSpec struct {
	Ranges   []ASNRange
	ClassRef LocalRef
}

type ASNPoolStatus struct {
	Capacity   ASNCapacity
	Conditions []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type ASNPoolList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []ASNPool
}

// ----------------------------------------------------------------------------
// ASNClaim
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

type ASNClaim struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   ASNClaimSpec
	Status ASNClaimStatus
}

type ASNClaimSpec struct {
	ClassRef *LocalRef
	PoolRef  *LocalRef
	OwnerRef *ObjectRef
}

type ASNClaimStatus struct {
	Phase        ClaimPhase
	ASN          int64
	BoundPoolRef *LocalRef
	Conditions   []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type ASNClaimList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []ASNClaim
}
