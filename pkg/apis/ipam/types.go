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

// AllocationPhase is the high-level lifecycle phase of an IPAllocation.
type AllocationPhase string

const (
	AllocationPending   AllocationPhase = "Pending"
	AllocationReady     AllocationPhase = "Ready"
	AllocationExhausted AllocationPhase = "Exhausted"
	AllocationError     AllocationPhase = "Error"
)

// PoolPhase is the high-level lifecycle phase of an IPPool.
type PoolPhase string

const (
	PoolPending   PoolPhase = "Pending"
	PoolReady     PoolPhase = "Ready"
	PoolExhausted PoolPhase = "Exhausted"
	PoolError     PoolPhase = "Error"
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

// PoolSelector picks a parent IPPool by labels, optionally scoped to a
// specific project for cross-project shared pools.
type PoolSelector struct {
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

// AllocationSpec configures sub-allocation behaviour for a pool.
type AllocationSpec struct {
	MinPrefixLength int
	MaxPrefixLength int
	Strategy        Strategy
}

// PoolCapacity reports utilization for an IPPool.
type PoolCapacity struct {
	Total     int64
	Allocated int64
	Available int64
}

// ----------------------------------------------------------------------------
// IPPool — cluster-scoped allocatable address space.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient
// +genclient:nonNamespaced

type IPPool struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPPoolSpec
	Status IPPoolStatus
}

type IPPoolSpec struct {
	CIDR          string
	IPFamily      IPFamily
	ParentPoolRef *LocalRef
	PrefixLength  int
	Allocation    AllocationSpec
	Visibility    string
}

type IPPoolStatus struct {
	Phase      PoolPhase
	CIDR       string
	Capacity   PoolCapacity
	Conditions []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPPoolList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPPool
}

// ----------------------------------------------------------------------------
// IPAllocation — namespaced, system-created allocation record.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

type IPAllocation struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPAllocationSpec
	Status IPAllocationStatus
}

type IPAllocationSpec struct {
	CIDR     string
	IPFamily IPFamily
	PoolRef  LocalRef
}

type IPAllocationStatus struct {
	Phase      AllocationPhase
	CIDR       string
	Capacity   PoolCapacity
	Conditions []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPAllocationList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPAllocation
}

// ----------------------------------------------------------------------------
// IPClaim
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

type IPClaim struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPClaimSpec
	Status IPClaimStatus
}

type IPClaimSpec struct {
	IPFamily      IPFamily
	PrefixLength  int
	PoolSelector  *PoolSelector
	PoolRef       *NamespacedRef
	ReclaimPolicy ReclaimPolicy
	OwnerRef      *ObjectRef
}

type IPClaimStatus struct {
	Phase              ClaimPhase
	AllocatedCIDR      string
	BoundAllocationRef *LocalRef
	Conditions         []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPClaimList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPClaim
}
