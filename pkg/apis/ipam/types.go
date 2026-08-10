package ipam

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// IPFamily identifies an IP address family.
type IPFamily string

const (
	IPv4 IPFamily = "IPv4"
	IPv6 IPFamily = "IPv6"
)

// Strategy selects how a free sub-block is chosen.
// PoolSelectionStrategy chooses WHICH POOL serves a claim (set on IPClass).
// AllocationStrategy chooses which free block is taken from inside it (set on
// IPPool). See the v1alpha1 types for why these are separate.
type PoolSelectionStrategy string

const (
	PoolFirstFit      PoolSelectionStrategy = "FirstFit"
	PoolLeastUtilized PoolSelectionStrategy = "LeastUtilized"
)

type AllocationStrategy string

const (
	FirstFit      AllocationStrategy = "FirstFit"
	BestFit       AllocationStrategy = "BestFit"
	LeastUtilized AllocationStrategy = "LeastUtilized"
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

// ClassPhase is the high-level lifecycle phase of an IPClass.
type ClassPhase string

const (
	ClassPending ClassPhase = "Pending"
	ClassReady   ClassPhase = "Ready"
	ClassError   ClassPhase = "Error"
)

// IsDefaultClassAnnotation marks a class as the default for its address
// family. A claim naming no class gets the default for the family it
// requested. At most one class per family may carry this.
const IsDefaultClassAnnotation = "ipam.miloapis.com/is-default-class"

// LocalRef references another IPAM object in the same namespace by name.
type LocalRef struct {
	Name string
}

// ObjectRef is an opaque cross-API reference. APIGroup and Kind are strings
// to avoid forcing consumers to import the referenced type's package.
type ObjectRef struct {
	APIGroup  string
	Kind      string
	Namespace string
	Name      string
}

// ScopeRef identifies one participant in a claim's scope — the network a
// claim is made for, the location it lives in, or anything else a class names
// as a role. The allocator never interprets these; it compares them.
//
// Refs are opaque by construction: the service imports no consumer types and
// resolves nothing. A name is enough to distinguish spaces, because every
// claim is already scoped to a project and a name is unique within one.
type ScopeRef struct {
	APIGroup string
	Kind     string
	Name     string
}

// ClassSourceRef names a class in another project.
type ClassSourceRef struct {
	Project string
	Name    string
}

// PrefixLengthRange bounds the sizes a claim of a class may request. A
// fixed-size class sets Min and Max equal.
type PrefixLengthRange struct {
	Min int32
	Max int32
}

// InternalRouting describes what routing does with an address inside its
// location.
type InternalRouting string

const (
	InternalRoutingNone InternalRouting = "None"
	InternalRoutingHost InternalRouting = "Host"
)

// ExternalRouting describes what routing does with an address beyond its
// location.
type ExternalRouting string

const (
	ExternalRoutingNone      ExternalRouting = "None"
	ExternalRoutingAggregate ExternalRouting = "Aggregate"
)

// RoutingSpec states advertisement inside a location and beyond it separately,
// because the two are frequently opposite: a per-instance address is a
// distinct route within its location and must never appear outside it — only
// the covering block leaves.
type RoutingSpec struct {
	Internal InternalRouting
	External ExternalRouting
}

// ReservationSpec withholds positions at the edges of a pool, counted in units
// of UnitPrefixLength. Each reserved position becomes a real allocation held
// by the pool, so reserved space has an owner, appears in utilization, and can
// be programmed. It is inventory, not an invisible hole.
type ReservationSpec struct {
	Leading  int32
	Trailing int32
	// UnitPrefixLength is the block size a reserved position occupies. It must
	// be set whenever Leading or Trailing is non-zero; the pool cannot infer
	// it, since pools serve classes of differing allocation sizes.
	UnitPrefixLength int32
}

// Pool visibility constants for IPPoolSpec.Visibility and
// IPClassSpec.Visibility.
const (
	VisibilityPlatform string = "platform"
	VisibilityConsumer string = "consumer"
	VisibilityShared   string = "shared"
)

// AllocationSpec configures sub-allocation behaviour for a pool.
type AllocationSpec struct {
	MinPrefixLength int
	MaxPrefixLength int
	Strategy        AllocationStrategy
}

// PoolCapacity reports a pool's address counts exactly, as decimal strings.
// See the v1alpha1 type for why they are not int64.
type PoolCapacity struct {
	Total     string
	Allocated string
	Available string
}

// ----------------------------------------------------------------------------
// IPClass — operator-defined policy naming a kind of address space.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient
// +genclient:nonNamespaced

// IPClass is the policy object a consumer names to get an address. It carries
// only what the allocator needs to hand one out: which space it comes from,
// and what it must not collide with.
type IPClass struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPClassSpec
	Status IPClassStatus
}

type IPClassSpec struct {
	// Source makes this class a reference to a class in another project rather
	// than a definition of its own. Every other field must be empty when it is
	// set, and no pool may name such a class. See the v1alpha1 type.
	Source *ClassSourceRef

	// IPFamily is the single address family this class hands out. Required and
	// immutable. Dual-stack is two classes, never one.
	IPFamily IPFamily

	// ParentClassName is the class whose allocations this one carves from.
	// Empty means allocations come from the pools offering this class directly.
	// Immutable: changing it strands every existing allocation outside its
	// declared ancestry.
	ParentClassName string

	// PoolPer names the scope roles that determine how many pools this class
	// provisions: one pool per distinct combination of these references. It
	// appears only on classes named as a parent, because only a parent
	// provisions pools. It is a constraint backing a unique index, not a
	// lookup.
	PoolPer []string

	// UniqueWithin states what defines one independent address space: two
	// allocations may hold the same address if, and only if, they differ in one
	// of these references. Empty — the default and the strictest — means one
	// space platform-wide. Immutable.
	UniqueWithin []string

	// AllowedPrefixLengths bounds the sizes a claim of this class may request.
	AllowedPrefixLengths *PrefixLengthRange

	// DefaultPrefixLength is the size used when a claim asks for none.
	DefaultPrefixLength int32

	// Reservations are applied to every pool this class provisions for its
	// children. A cascade-provisioned pool has no author to state reservations
	// on it, so a per-subnet gateway reservation can only be said here.
	// Operator-authored pools state their own on the pool, and a pool that
	// carries both keeps its own.
	//
	// Meaningful only alongside PoolPer.
	Reservations *ReservationSpec

	// Routing describes what the network does with addresses of this class.
	Routing RoutingSpec

	// Strategy selects WHICH POOL serves a claim when several back this class.
	// Not IPPool.spec.allocation.strategy, which selects the block inside it.
	Strategy PoolSelectionStrategy

	// ReclaimPolicy is the default disposition of an allocation when its claim
	// is deleted. A claim may override it.
	ReclaimPolicy ReclaimPolicy

	// RetentionLease bounds how long an allocation of this class may stay
	// retained after its claim is deleted. Unset means no expiry, which is the
	// default deliberately: a lease that defaulted to on would reclaim
	// addresses in existing deployments nobody asked to have reclaimed.
	//
	// A claim cannot override this — retention the holder can extend
	// indefinitely is not a lease. The effective lease is the shorter of this
	// and the pool's MaxRetentionLease.
	RetentionLease *metav1.Duration

	// Visibility controls who may name this class on a claim.
	Visibility string

	// Provisioner names the component responsible for realising allocations of
	// this class. Empty means the IPAM service itself.
	Provisioner string

	// Parameters carries provisioner-specific configuration, opaque to IPAM.
	Parameters map[string]string
}

type IPClassStatus struct {
	Phase ClassPhase
	// OfferingPools is the number of pools backing this class: those offering
	// the root of its chain, which for a parented class is not this class. Zero
	// means every claim naming this class fails. Computed on read and absent
	// from WATCH events — see the v1alpha1 type for the full statement, which is
	// the one consumers read.
	OfferingPools int32
	// ProvisionedPools is the number of pools this class has provisioned for
	// its children. Meaningful only when PoolPer is set.
	ProvisionedPools int32
	// RequiredScopeRoles is the resolved set of scope roles a claim of this
	// class must supply: this class's UniqueWithin unioned with every PoolPer
	// along its parent chain. Resolved server-side so a client need not walk
	// ParentClassName upward and union by hand.
	RequiredScopeRoles []string
	Conditions         []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPClassList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPClass
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

	// ClassNames are the classes this pool offers itself to. A claim naming one
	// of these classes may draw from this pool, subject to family and scope
	// agreement.
	ClassNames []string

	// ClassRef names the class that provisioned this pool, set by the allocator
	// on pools it creates during a cascade and empty on operator-authored
	// pools.
	ClassRef *LocalRef

	// Scope is the combination of references this pool exists for, keyed by
	// role. On a cascade-provisioned pool it is the projection of the
	// triggering claim's scope onto the provisioning class's PoolPer. On an
	// operator-authored pool it declares the pool's own constraints.
	Scope map[string]ScopeRef

	// Reservations withholds positions at the edges of this pool.
	Reservations *ReservationSpec

	// MaxRetentionLease caps how long any allocation drawn from this pool may
	// stay retained, whatever the class asks for. The cap lives here because
	// the pool is the thing that runs out. Unset means the class's lease
	// applies unchanged.
	MaxRetentionLease *metav1.Duration

	Allocation AllocationSpec
	Visibility string
}

type IPPoolStatus struct {
	Phase         PoolPhase
	AllocatedCIDR string
	// IPFamily is the effective address family of this pool: taken from
	// spec.ipFamily on root pools and derived from the carved
	// status.allocatedCIDR on child pools.
	IPFamily IPFamily
	// ScopeDigest is the canonical digest of Spec.Scope, computed by the
	// server. It is the value the uniqueness index is built on.
	ScopeDigest string
	Capacity    PoolCapacity
	// UtilizationPercent is the allocated share of the pool's address space,
	// 0–100, rounded to four decimal places. See the v1alpha1 type for why it
	// is a float.
	UtilizationPercent float64
	Conditions         []metav1.Condition
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

// AllocationPurpose distinguishes an address handed to a claim from one held
// by a pool to keep it out of circulation.
type AllocationPurpose string

const (
	// PurposeClaim is an allocation bound to an IPClaim.
	PurposeClaim AllocationPurpose = "Claim"
	// PurposeReservation is an allocation held by the pool itself to satisfy
	// Spec.Reservations. It has no claim and is never handed out.
	PurposeReservation AllocationPurpose = "Reservation"
	// PurposePoolCarve is the block a child pool occupies in its parent. It is
	// released when the child pool is deleted, which is what separates it from
	// a reservation: a reservation outlives everything except a change to the
	// pool's own spec.
	//
	// The allocator's search does not distinguish this from a reservation —
	// both are scope-independent. Only lifecycle does.
	PurposePoolCarve AllocationPurpose = "PoolCarve"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

// IPAllocation records an address or block handed out of an IPPool. An
// allocation may outlive the claim that created it: a claim released under
// ReclaimRetain leaves its allocation in place with ClaimRef cleared.
type IPAllocation struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPAllocationSpec
	Status IPAllocationStatus
}

type IPAllocationSpec struct {
	IPFamily IPFamily
	PoolRef  LocalRef

	// ClassName is the class this allocation was handed out under. Empty on
	// reservations, which belong to a pool rather than to a class.
	ClassName string

	// Purpose distinguishes a claim-bound allocation from a pool-held
	// reservation.
	Purpose AllocationPurpose

	// ClaimRef names the claim currently bound to this allocation. Nil on
	// reservations, and nil on a retained allocation whose claim has been
	// deleted.
	ClaimRef *LocalRef

	// Scope is the address space this allocation belongs to, keyed by role: the
	// claim's scope projected onto the class's UniqueWithin.
	Scope map[string]ScopeRef

	// ReclaimPolicy is the effective policy for this allocation, resolved from
	// the class and any claim override at bind time. It is recorded here
	// because the allocation outlives the claim that chose it.
	ReclaimPolicy ReclaimPolicy

	// OwnerRef is the opaque consumer object this allocation is held for. It
	// survives claim deletion under Retain, so a retained address still has an
	// attributable holder for quota and for cleanup.
	OwnerRef *ObjectRef
}

type IPAllocationStatus struct {
	Phase         AllocationPhase
	AllocatedCIDR string
	// Address is the single address form of AllocatedCIDR, set when the class
	// hands out host addresses rather than blocks.
	Address string
	// ScopeDigest is the canonical digest of Spec.Scope, computed by the
	// server — the value uniqueness is enforced on.
	ScopeDigest string
	Conditions  []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPAllocationList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPAllocation
}

// ----------------------------------------------------------------------------
// IPClaim — a long-lived request for an address of a named class.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

// IPClaim is a long-lived request for an address, bound to one allocation for
// as long as it exists. A claim names a class and carries the scope it was
// made for; it never names a pool, a CIDR, or a region.
type IPClaim struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   IPClaimSpec
	Status IPClaimStatus
}

type IPClaimSpec struct {
	// ClassName is the class of address being requested. Empty selects the
	// default class for IPFamily, which must then be set.
	ClassName string

	// IPFamily selects the default class when ClassName is empty. It is
	// ignored — and must agree, if set — when ClassName names a class.
	IPFamily IPFamily

	// Address names a specific address to bind. Omitted, the allocator chooses.
	Address string

	// PrefixLength is the requested block size in bits. Nil applies the class's
	// DefaultPrefixLength. Must fall within the class's AllowedPrefixLengths.
	PrefixLength *int32

	// Scope carries the references this claim is made for, keyed by role. The
	// allocator uses them for two things: resolving which pool to draw from,
	// and deciding which allocations this one must not collide with.
	//
	// A claim omitting a role its class names in UniqueWithin, or one its
	// parent chain needs to resolve a pool, is rejected rather than falling
	// back to a wider comparison. Immutable.
	Scope map[string]ScopeRef

	// ReclaimPolicy overrides the class default for this claim.
	ReclaimPolicy ReclaimPolicy

	// OwnerRef is the opaque consumer object this claim is made for.
	OwnerRef *ObjectRef
}

type IPClaimStatus struct {
	Phase         ClaimPhase
	AllocatedCIDR string
	// Address is the single address form of AllocatedCIDR, set when the class
	// hands out host addresses rather than blocks.
	Address string
	// PoolRef names the pool the allocation was drawn from. It is resolved by
	// the allocator, not requested.
	PoolRef            *LocalRef
	BoundAllocationRef *LocalRef
	Conditions         []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPClaimList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []IPClaim
}
