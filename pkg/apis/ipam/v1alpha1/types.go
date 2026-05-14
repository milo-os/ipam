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

// PrefixPhase is the high-level lifecycle phase of an IP prefix.
// +kubebuilder:validation:Enum=Pending;Ready;Exhausted;Error
type PrefixPhase string

const (
	PrefixPending   PrefixPhase = "Pending"
	PrefixReady     PrefixPhase = "Ready"
	PrefixExhausted PrefixPhase = "Exhausted"
	PrefixError     PrefixPhase = "Error"
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

// PrefixSelector picks a parent IPPrefix by labels, optionally scoped to a
// specific project for cross-project shared pools.
type PrefixSelector struct {
	// +optional
	*metav1.LabelSelector `json:",inline"`
	// +optional
	ProjectRef *LocalRef `json:"projectRef,omitempty"`
}

// Pool visibility constants for IPPrefixClass.spec.visibility.
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

// AllocationSpec configures sub-allocation behaviour for a prefix.
type AllocationSpec struct {
	MinPrefixLength int      `json:"minPrefixLength,omitempty"`
	MaxPrefixLength int      `json:"maxPrefixLength,omitempty"`
	Strategy        Strategy `json:"strategy,omitempty"`
}

// PrefixCapacity reports utilization for an IPPrefix.
type PrefixCapacity struct {
	Total     int64 `json:"total"`
	Allocated int64 `json:"allocated"`
	Available int64 `json:"available"`
}

// IPPrefixTemplate is the metadata + spec used to materialise an IPPrefix
// child created atomically with an IPPrefixClaim.
type IPPrefixTemplate struct {
	Metadata metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec     IPPrefixSpec      `json:"spec"`
}

// ----------------------------------------------------------------------------
// IPPrefixClass — cluster-scoped class of prefix pools.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ippc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Visibility",type=string,JSONPath=`.spec.visibility`
// +kubebuilder:printcolumn:name="ReqVerify",type=boolean,JSONPath=`.spec.requiresVerification`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +genclient:nonNamespaced

// IPPrefixClass declares operational properties shared by a class of
// IPPrefix pools.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPrefixClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IPPrefixClassSpec `json:"spec,omitempty"`
}

type IPPrefixClassSpec struct {
	// Visibility controls cross-project access semantics for IPPrefix
	// pools that reference this class. "platform" pools are platform-only
	// (callers see them only when running with platform scope);
	// "consumer" pools are visible to a single project; "shared" pools
	// are eligible for cross-project allocation via prefixSelector.projectRef
	// gated by a SubjectAccessReview.
	// +optional
	// +kubebuilder:validation:Enum=platform;consumer;shared
	Visibility string `json:"visibility,omitempty"`
	// +optional
	DefaultAllocation AllocationSpec `json:"defaultAllocation,omitempty"`
}

// +kubebuilder:object:root=true

// IPPrefixClassList is a list of IPPrefixClass.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPrefixClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPPrefixClass `json:"items"`
}

// ----------------------------------------------------------------------------
// IPPrefix
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ipp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.spec.cidr`
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.ipFamily`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.classRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +genclient:nonNamespaced

// IPPrefix is a CIDR pool from which sub-prefixes or addresses can be
// allocated.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPrefix struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPPrefixSpec   `json:"spec,omitempty"`
	Status IPPrefixStatus `json:"status,omitempty"`
}

type IPPrefixSpec struct {
	// CIDR is the parent prefix in canonical form, e.g. "10.0.0.0/8"
	// (IPv4) or "2001:db8::/32" (IPv6). Validation parses with
	// net.ParseCIDR and rejects malformed values.
	CIDR     string   `json:"cidr"`
	IPFamily IPFamily `json:"ipFamily"`
	ClassRef LocalRef `json:"classRef"`
	// +optional
	Allocation AllocationSpec `json:"allocation,omitempty"`
	// +optional
	ParentRef *ObjectRef `json:"parentRef,omitempty"`
}

type IPPrefixStatus struct {
	// +optional
	Phase PrefixPhase `json:"phase,omitempty"`
	// +optional
	CIDR string `json:"cidr,omitempty"`
	// +optional
	Capacity PrefixCapacity `json:"capacity,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPrefixList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPPrefix `json:"items"`
}

// ----------------------------------------------------------------------------
// IPPrefixClaim
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ippc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.allocatedCIDR`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.status.boundPrefixRef.name`
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.ipFamily`
// +kubebuilder:printcolumn:name="Length",type=integer,JSONPath=`.spec.prefixLength`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPrefixClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPPrefixClaimSpec   `json:"spec,omitempty"`
	Status IPPrefixClaimStatus `json:"status,omitempty"`
}

type IPPrefixClaimSpec struct {
	IPFamily IPFamily `json:"ipFamily"`
	// PrefixLength is the requested sub-prefix size in bits. Must be a
	// valid mask length for the chosen ipFamily (0-32 for IPv4, 0-128
	// for IPv6).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	PrefixLength int `json:"prefixLength"`
	// +optional
	PrefixSelector *PrefixSelector `json:"prefixSelector,omitempty"`
	// +optional
	PrefixRef *NamespacedRef `json:"prefixRef,omitempty"`
	// +optional
	ChildPrefixTemplate *IPPrefixTemplate `json:"childPrefixTemplate,omitempty"`
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
	// +optional
	OwnerRef *ObjectRef `json:"ownerRef,omitempty"`
}

type IPPrefixClaimStatus struct {
	// +optional
	Phase ClaimPhase `json:"phase,omitempty"`
	// +optional
	AllocatedCIDR string `json:"allocatedCIDR,omitempty"`
	// +optional
	BoundPrefixRef *LocalRef `json:"boundPrefixRef,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPrefixClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPPrefixClaim `json:"items"`
}

// ----------------------------------------------------------------------------
// IPAddress
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ipa
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.ipFamily`
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.spec.prefixRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPAddress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPAddressSpec   `json:"spec,omitempty"`
	Status IPAddressStatus `json:"status,omitempty"`
}

type IPAddressSpec struct {
	Address   string   `json:"address"`
	IPFamily  IPFamily `json:"ipFamily"`
	PrefixRef LocalRef `json:"prefixRef"`
	// +optional
	ClaimRef *LocalRef `json:"claimRef,omitempty"`
}

type IPAddressStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPAddressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAddress `json:"items"`
}

// ----------------------------------------------------------------------------
// IPAddressClaim
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ipac
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.allocatedIP`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.prefixRef.name`
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.ipFamily`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPAddressClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPAddressClaimSpec   `json:"spec,omitempty"`
	Status IPAddressClaimStatus `json:"status,omitempty"`
}

type IPAddressClaimSpec struct {
	IPFamily IPFamily `json:"ipFamily"`
	// +optional
	PrefixSelector *PrefixSelector `json:"prefixSelector,omitempty"`
	// +optional
	PrefixRef *NamespacedRef `json:"prefixRef,omitempty"`
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
	// +optional
	OwnerRef *ObjectRef `json:"ownerRef,omitempty"`
}

type IPAddressClaimStatus struct {
	// +optional
	Phase ClaimPhase `json:"phase,omitempty"`
	// +optional
	AllocatedIP string `json:"allocatedIP,omitempty"`
	// +optional
	BoundAddressRef *LocalRef `json:"boundAddressRef,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPAddressClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAddressClaim `json:"items"`
}

