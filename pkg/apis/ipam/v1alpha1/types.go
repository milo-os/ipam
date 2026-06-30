package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// IPFamily identifies an IP address family.
// +kubebuilder:validation:Enum=IPv4;IPv6
type IPFamily string

const (
	IPv4 IPFamily = "IPv4"
	IPv6 IPFamily = "IPv6"
)

// Strategy selects how a free sub-block is chosen.
// +kubebuilder:validation:Enum=FirstFit;BestFit;LeastUtilized
type Strategy string

const (
	FirstFit      Strategy = "FirstFit"
	BestFit       Strategy = "BestFit"
	LeastUtilized Strategy = "LeastUtilized"
)

// ReclaimPolicy controls disposition of the underlying allocation when a
// claim is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type ReclaimPolicy string

const (
	ReclaimDelete ReclaimPolicy = "Delete"
	ReclaimRetain ReclaimPolicy = "Retain"
)

// ClaimPhase is the high-level lifecycle phase of a claim.
// +kubebuilder:validation:Enum=Pending;Bound;Releasing;Error
type ClaimPhase string

const (
	ClaimPending   ClaimPhase = "Pending"
	ClaimBound     ClaimPhase = "Bound"
	ClaimReleasing ClaimPhase = "Releasing"
	ClaimError     ClaimPhase = "Error"
)

// AllocationPhase is the high-level lifecycle phase of an IPAllocation.
// +kubebuilder:validation:Enum=Pending;Ready;Exhausted;Error
type AllocationPhase string

const (
	AllocationPending   AllocationPhase = "Pending"
	AllocationReady     AllocationPhase = "Ready"
	AllocationExhausted AllocationPhase = "Exhausted"
	AllocationError     AllocationPhase = "Error"
)

// PoolPhase is the high-level lifecycle phase of an IPPool.
// +kubebuilder:validation:Enum=Pending;Ready;Exhausted;Error
type PoolPhase string

const (
	PoolPending   PoolPhase = "Pending"
	PoolReady     PoolPhase = "Ready"
	PoolExhausted PoolPhase = "Exhausted"
	PoolError     PoolPhase = "Error"
)

// LocalRef references another IPAM object in the same namespace by name.
type LocalRef struct {
	Name string `json:"name"`
}

// NamespacedRef references a named resource with an optional cross-project
// pointer. ProjectRef nil means the reference resolves in the caller's own
// project (or the platform scope for non-tenant requests).
type NamespacedRef struct {
	Name string `json:"name"`
	// +optional
	ProjectRef *LocalRef `json:"projectRef,omitempty"`
}

// PoolSelector picks a parent IPPool by labels, optionally scoped to a
// specific project for cross-project shared pools.
type PoolSelector struct {
	// +optional
	*metav1.LabelSelector `json:",inline"`
	// +optional
	ProjectRef *LocalRef `json:"projectRef,omitempty"`
}

// Pool visibility constants for IPPool.spec.visibility.
const (
	VisibilityPlatform string = "platform"
	VisibilityConsumer string = "consumer"
	VisibilityShared   string = "shared"
)

// ObjectRef is an opaque cross-API reference.
type ObjectRef struct {
	APIGroup  string `json:"apiGroup"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// AllocationSpec configures sub-allocation behaviour for a pool.
type AllocationSpec struct {
	MinPrefixLength int      `json:"minPrefixLength,omitempty"`
	MaxPrefixLength int      `json:"maxPrefixLength,omitempty"`
	Strategy        Strategy `json:"strategy,omitempty"`
}

// PoolCapacity reports the address-count view of an IPPool. The counts are
// exact for IPv4. For address spaces larger than an int64 (e.g. wide IPv6
// prefixes) Total saturates to the maximum int64 and Allocated/Available are
// clamped to non-negative values rather than overflowing; consumers needing an
// accurate IPv6 view should read UtilizationPercent and LargestFreePrefix.
type PoolCapacity struct {
	Total     int64 `json:"total"`
	Allocated int64 `json:"allocated"`
	Available int64 `json:"available"`
}

// ----------------------------------------------------------------------------
// IPPool — cluster-scoped allocatable address space.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ippool
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.allocatedCIDR`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.status.ipFamily`
// +kubebuilder:printcolumn:name="Largest Free",type=integer,JSONPath=`.status.largestFreePrefix`
// +kubebuilder:printcolumn:name="Util%",type=integer,JSONPath=`.status.utilizationPercent`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +genclient:nonNamespaced

// IPPool is an allocatable address space. Root pools declare a CIDR
// directly; child pools carve a sub-prefix from a parent pool.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPPoolSpec   `json:"spec,omitempty"`
	Status IPPoolStatus `json:"status,omitempty"`
}

type IPPoolSpec struct {
	// +optional
	CIDR string `json:"cidr,omitempty"`
	// +optional
	IPFamily IPFamily `json:"ipFamily,omitempty"`
	// +optional
	ParentPoolRef *LocalRef `json:"parentPoolRef,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	PrefixLength int `json:"prefixLength,omitempty"`
	// +optional
	Allocation AllocationSpec `json:"allocation,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=platform;consumer;shared
	Visibility string `json:"visibility,omitempty"`
}

type IPPoolStatus struct {
	// +optional
	Phase PoolPhase `json:"phase,omitempty"`
	// +optional
	AllocatedCIDR string `json:"allocatedCIDR,omitempty"`
	// ipFamily is the effective address family of this pool: taken from
	// spec.ipFamily on root pools and derived from the carved
	// status.allocatedCIDR on child pools.
	// +optional
	IPFamily IPFamily `json:"ipFamily,omitempty"`
	// +optional
	Capacity PoolCapacity `json:"capacity,omitempty"`
	// largestFreePrefix is the prefix length of the largest free aligned
	// block currently available (e.g. 45 for a free /45). Zero when the pool
	// is exhausted or its capacity is not yet computed. This is the
	// family-agnostic signal for remaining headroom; the integer capacity
	// fields saturate for very large address spaces.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	LargestFreePrefix int32 `json:"largestFreePrefix"`
	// utilizationPercent is the allocated share of the pool's address space,
	// 0–100, computed with arbitrary-precision arithmetic so it is accurate
	// for both IPv4 and IPv6.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	UtilizationPercent int32 `json:"utilizationPercent"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPPool `json:"items"`
}

// ----------------------------------------------------------------------------
// IPAllocation — namespace-scoped, system-created allocation record.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ipalloc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.allocatedCIDR`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient

// IPAllocation records a CIDR carved out of an IPPool by an IPClaim.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPAllocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPAllocationSpec   `json:"spec,omitempty"`
	Status IPAllocationStatus `json:"status,omitempty"`
}

type IPAllocationSpec struct {
	IPFamily IPFamily `json:"ipFamily"`
	PoolRef  LocalRef `json:"poolRef"`
}

type IPAllocationStatus struct {
	// +optional
	Phase AllocationPhase `json:"phase,omitempty"`
	// +optional
	AllocatedCIDR string `json:"allocatedCIDR,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPAllocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAllocation `json:"items"`
}

// ----------------------------------------------------------------------------
// IPClaim
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ipclaim
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.allocatedCIDR`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.ipFamily`
// +kubebuilder:printcolumn:name="Length",type=integer,JSONPath=`.spec.prefixLength`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPClaimSpec   `json:"spec,omitempty"`
	Status IPClaimStatus `json:"status,omitempty"`
}

type IPClaimSpec struct {
	IPFamily IPFamily `json:"ipFamily"`
	// PrefixLength is the requested sub-prefix size in bits. Must be a
	// valid mask length for the chosen ipFamily (0-32 for IPv4, 0-128
	// for IPv6).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	PrefixLength int `json:"prefixLength"`
	// +optional
	PoolSelector *PoolSelector `json:"poolSelector,omitempty"`
	// +optional
	PoolRef *NamespacedRef `json:"poolRef,omitempty"`
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
	// +optional
	OwnerRef *ObjectRef `json:"ownerRef,omitempty"`
}

type IPClaimStatus struct {
	// +optional
	Phase ClaimPhase `json:"phase,omitempty"`
	// +optional
	AllocatedCIDR string `json:"allocatedCIDR,omitempty"`
	// +optional
	BoundAllocationRef *LocalRef `json:"boundAllocationRef,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPClaim `json:"items"`
}
