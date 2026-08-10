package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// IPFamily identifies an IP address family.
// +kubebuilder:validation:Enum=IPv4;IPv6
type IPFamily string

const (
	IPv4 IPFamily = "IPv4"
	IPv6 IPFamily = "IPv6"
)

// PoolSelectionStrategy chooses WHICH POOL serves a claim, when several pools
// back one class. It is set on IPClass.
//
// Deliberately a different type from AllocationStrategy, which chooses which
// free block is taken from inside a pool. The two were one enum until
// 2026-08-09, and the conflation was actively harmful: removing
// status.largestFreePrefix made class-level BestFit unimplementable — it
// selected on contiguous headroom, which is the one question the remaining
// status does not answer — while pool-level BestFit is unaffected, because it
// reads the allocation set it is already holding. One shared value meant one
// edit silently changed both.
//
// +kubebuilder:validation:Enum=FirstFit;LeastUtilized
type PoolSelectionStrategy string

const (
	// PoolFirstFit takes the first pool by storage key. Deterministic across
	// callers, and it lets an operator steer allocation by naming pools in the
	// order they want them filled. The default.
	PoolFirstFit PoolSelectionStrategy = "FirstFit"
	// PoolLeastUtilized spreads claims across pools, which is what an operator
	// wants when several pools back one class and no single one should fill
	// first.
	PoolLeastUtilized PoolSelectionStrategy = "LeastUtilized"
)

// AllocationStrategy chooses WHICH FREE BLOCK is taken from inside a pool. It
// is set on IPPool.
//
// BestFit is available here and not in PoolSelectionStrategy: this operates on
// the allocation set the allocator already has in hand, so "the smallest region
// that fits" is answerable without an extra read. See PoolSelectionStrategy.
//
// +kubebuilder:validation:Enum=FirstFit;BestFit;LeastUtilized
type AllocationStrategy string

const (
	FirstFit      AllocationStrategy = "FirstFit"
	BestFit       AllocationStrategy = "BestFit"
	LeastUtilized AllocationStrategy = "LeastUtilized"
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

// ClassPhase is the high-level lifecycle phase of an IPClass.
// +kubebuilder:validation:Enum=Pending;Ready;Error
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
	Name string `json:"name"`
}

// ObjectRef is an opaque cross-API reference.
type ObjectRef struct {
	APIGroup  string `json:"apiGroup"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// UID pins this reference to one specific instance of the named object.
	// Unlike ScopeRef.UID it takes no part in allocation identity — it exists
	// so that "who holds this address" stays answerable after the holder has
	// been deleted and recreated under the same name, which is exactly the
	// case an operator hits during an incident.
	// +optional
	UID string `json:"uid,omitempty"`
}

// ScopeRef identifies one participant in a claim's scope — the network a
// claim is made for, the location it lives in, or anything else a class names
// as a role. The allocator never interprets these; it compares them.
//
// Refs are opaque by construction: the service imports no consumer types and
// resolves nothing. A name is enough to distinguish spaces, because every
// claim is already scoped to a project and a name is unique within one.
type ScopeRef struct {
	APIGroup string `json:"apiGroup"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	// UID pins this reference to one specific instance of the named object.
	// When set it participates in scope identity, so an object deleted and
	// recreated under the same name is a *different* address space and gets
	// fresh allocations rather than inheriting its predecessor's.
	//
	// Suppliers should set it. Omitting it makes identity name-based, which
	// lets a recreated object inherit — occasionally desirable, and the reason
	// this is a field rather than a rule.
	// +optional
	UID string `json:"uid,omitempty"`
}

// PrefixLengthRange bounds the sizes a claim of a class may request. A
// fixed-size class sets Min and Max equal.
type PrefixLengthRange struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	Min int32 `json:"min"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	Max int32 `json:"max"`
}

// InternalRouting describes what routing does with an address inside its
// location.
// +kubebuilder:validation:Enum=None;Host
type InternalRouting string

const (
	InternalRoutingNone InternalRouting = "None"
	InternalRoutingHost InternalRouting = "Host"
)

// ExternalRouting describes what routing does with an address beyond its
// location.
// +kubebuilder:validation:Enum=None;Aggregate
type ExternalRouting string

const (
	ExternalRoutingNone      ExternalRouting = "None"
	ExternalRoutingAggregate ExternalRouting = "Aggregate"
)

// RoutingSpec states advertisement inside a location and beyond it separately,
// because the two are frequently opposite: a per-instance address is a
// distinct route within its location and must never appear outside it — only
// the covering block leaves.
//
// An aggregate must be originated with a discard route. A class advertising an
// aggregate it cannot fully resolve blackholes the unallocated space inside it.
type RoutingSpec struct {
	// +optional
	Internal InternalRouting `json:"internal,omitempty"`
	// +optional
	External ExternalRouting `json:"external,omitempty"`
}

// ReservationSpec withholds positions at the edges of a pool, counted in units
// of UnitPrefixLength. Each reserved position becomes a real allocation held
// by the pool, so reserved space has an owner, appears in utilization, and can
// be programmed. It is inventory, not an invisible hole.
//
// Reservations live on the pool rather than on a class because the pool is the
// thing being carved: one reservation per pool, not one per address space
// carved from it. Classes with no parent — which have no provisioning class to
// host the field — are covered by the same mechanism as classes that do.
type ReservationSpec struct {
	// +optional
	// +kubebuilder:validation:Minimum=0
	Leading int32 `json:"leading,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	Trailing int32 `json:"trailing,omitempty"`
	// UnitPrefixLength is the block size a reserved position occupies. It must
	// be set whenever Leading or Trailing is non-zero; the pool cannot infer it,
	// since pools serve classes of differing allocation sizes.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	UnitPrefixLength int32 `json:"unitPrefixLength,omitempty"`
}

// AllocationSpec configures sub-allocation behaviour for a pool.
type AllocationSpec struct {
	MinPrefixLength int                `json:"minPrefixLength,omitempty"`
	MaxPrefixLength int                `json:"maxPrefixLength,omitempty"`
	Strategy        AllocationStrategy `json:"strategy,omitempty"`
}

// PoolCapacity reports the address-count view of an IPPool. The counts are
// exact for IPv4. For address spaces larger than an int64 (e.g. wide IPv6
// prefixes) Total saturates to the maximum int64 and Allocated/Available are
// clamped to non-negative values rather than overflowing; consumers needing an
// accurate IPv6 view should read UtilizationPercent and LargestFreePrefix.
// PoolCapacity reports a pool's address counts EXACTLY, as decimal strings.
//
// Strings because these are not int64 quantities. A /20 of IPv6 holds
// 2^108 addresses — thirty-three digits — and the previous int64 fields
// saturated at 9223372036854775807, which is not an address count but a
// ceiling. A pool reported `total: 9223372036854775807, allocated:
// 9007199254740991`, and neither figure was true: a client dividing them got a
// number that meant nothing, which is why utilizationPercent had to exist as
// the only trustworthy signal for IPv6.
//
// The cost is that a client wanting a percentage from these must do
// arbitrary-precision arithmetic. utilizationPercent is retained precisely so
// that most clients do not have to: it is the same measurement, already
// reduced. Use the counts when you need exactness, the percentage when you need
// a number to show someone.
//
// Decimal digits only — no sign, no exponent, no units. An empty string means
// the figure has not been computed, which is distinct from "0".
type PoolCapacity struct {
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+$`
	Total string `json:"total,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+$`
	Allocated string `json:"allocated,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+$`
	Available string `json:"available,omitempty"`
}

// Pool visibility constants for IPPool.spec.visibility and
// IPClass.spec.visibility.
const (
	VisibilityPlatform string = "platform"
	VisibilityConsumer string = "consumer"
	VisibilityShared   string = "shared"
)

// ----------------------------------------------------------------------------
// IPClass — operator-defined policy naming a kind of address space.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ipclass
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.ipFamily`
// +kubebuilder:printcolumn:name="Parent",type=string,JSONPath=`.spec.parentClassName`
// +kubebuilder:printcolumn:name="Default Length",type=integer,JSONPath=`.spec.defaultPrefixLength`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pools",type=integer,JSONPath=`.status.offeringPools`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +genclient:nonNamespaced

// IPClass is the policy object a consumer names to get an address. It carries
// only what the allocator needs to hand one out: which space it comes from,
// and what it must not collide with. Nothing on a class selects an allocation
// — a claim binds one when it is created.
//
// Consumers name a class and never a pool, a CIDR, a prefix length, or a
// region. Those are operator concerns and stay on this side of the boundary.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPClassSpec   `json:"spec,omitempty"`
	Status IPClassStatus `json:"status,omitempty"`
}

type IPClassSpec struct {
	// IPFamily is the single address family this class hands out. Required and
	// immutable. Dual-stack is two classes, never one.
	IPFamily IPFamily `json:"ipFamily"`

	// ParentClassName is the class whose allocations this one carves from.
	// Empty means allocations come from the pools that offer this class
	// directly via IPPool.spec.classNames.
	//
	// Immutable: changing it strands every existing allocation outside its
	// declared ancestry.
	// +optional
	ParentClassName string `json:"parentClassName,omitempty"`

	// PoolPer names the scope roles that determine how many pools this class
	// provisions: one pool per distinct combination of these references. It
	// appears only on classes named as a parent by some other class, because
	// only a parent provisions pools — a leaf class binds allocations directly.
	//
	// This is a constraint, not a lookup. It backs a unique index, so two
	// simultaneous claims observing no pool cannot both create one.
	// +optional
	// +listType=atomic
	PoolPer []string `json:"poolPer,omitempty"`

	// UniqueWithin states what defines one independent address space: two
	// allocations may hold the same address if, and only if, they differ in one
	// of these references. Empty — the default and the strictest — means one
	// space platform-wide.
	//
	// Setting it wider than the parent already requires is safe and wasteful.
	// Setting it narrower is how two holders end up with one address, which
	// shared per-location IPv4 tenant space wants and nothing else does.
	//
	// Immutable: this field states the guarantee, and the allocator's search
	// follows from it.
	// +optional
	// +listType=atomic
	UniqueWithin []string `json:"uniqueWithin,omitempty"`

	// AllowedPrefixLengths bounds the sizes a claim of this class may request.
	// +optional
	AllowedPrefixLengths *PrefixLengthRange `json:"allowedPrefixLengths,omitempty"`

	// DefaultPrefixLength is the size used when a claim asks for none.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	DefaultPrefixLength int32 `json:"defaultPrefixLength,omitempty"`

	// Reservations are applied to every pool this class provisions for its
	// children. It exists because a cascade-provisioned pool has no author to
	// state reservations on it: the subnet a claim conjures on first use is
	// built by the allocator, so "reserve the first /96 of each subnet for the
	// gateway" can only be said here, on the class that provisions subnets.
	//
	// Operator-authored pools state their own reservations on the pool
	// instead. A pool that carries both keeps its own — the pool is the more
	// specific statement, and an operator who wrote a reservation by hand
	// meant it.
	//
	// Meaningful only alongside PoolPer, since a class that provisions no
	// pools has nothing to apply this to.
	// +optional
	Reservations *ReservationSpec `json:"reservations,omitempty"`

	// Routing describes what the network does with addresses of this class.
	// +optional
	Routing RoutingSpec `json:"routing,omitempty"`

	// Strategy selects WHICH POOL serves a claim when several back this class.
	// Not to be confused with IPPool.spec.allocation.strategy, which selects
	// which free block is taken from inside the chosen pool.
	// +optional
	Strategy PoolSelectionStrategy `json:"strategy,omitempty"`

	// ReclaimPolicy is the default disposition of an allocation when its claim
	// is deleted. A claim may override it.
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// RetentionLease bounds how long an allocation of this class may stay
	// retained after its claim is deleted, before the service releases it.
	//
	// Unset means no expiry: a retained address is held until something
	// releases it explicitly. That is the default deliberately — shipping a
	// lease that defaults to on would reclaim addresses in existing
	// deployments that nobody asked to have reclaimed.
	//
	// A claim cannot override this. A claim already overrides ReclaimPolicy;
	// letting the holder also set its own expiry hands the scarce-space
	// decision back to the party that wants to consume it, and retention the
	// holder can extend indefinitely is not a lease.
	//
	// The effective lease is the shorter of this and the pool's
	// MaxRetentionLease.
	// +optional
	RetentionLease *metav1.Duration `json:"retentionLease,omitempty"`

	// Visibility controls who may name this class on a claim.
	// +optional
	// +kubebuilder:validation:Enum=platform;consumer;shared
	Visibility string `json:"visibility,omitempty"`

	// BackingProjects names the projects whose pools may back this class, in
	// addition to the platform project — which may always back it and need not
	// be listed.
	//
	// This is the class *consenting* to be backed, and it is the half that
	// IPPool.spec.classNames cannot supply. That field is a pool volunteering
	// itself, written by the pool's owner; without a matching statement from the
	// class side, any tenant able to create an IPPool in their own project could
	// list a popular class name on it and start receiving other tenants' claims.
	// They would learn that each claim happened, choose the address it received,
	// and hold the range it came from. Consent has to come from the class
	// because the class is the thing being consumed.
	//
	// Empty — the default — means the platform project alone, which is
	// fail-closed and is exactly the behaviour every existing class had when
	// discovery searched only platform-authored pools.
	//
	// Enforced in two places on purpose. IPPool writes are rejected when the
	// pool's project is not listed, so an operator gets an error naming the
	// field rather than a pool that silently serves nobody. Discovery applies
	// the same rule at read time, which is the authoritative one: consent is
	// revocable, and a write-time check alone would let every pool that passed
	// validation once keep serving forever after the project was removed here.
	// +optional
	// +listType=set
	BackingProjects []string `json:"backingProjects,omitempty"`

	// Provisioner names the component responsible for realising allocations of
	// this class, for classes whose addresses require action beyond
	// bookkeeping. Empty means the IPAM service itself.
	// +optional
	Provisioner string `json:"provisioner,omitempty"`

	// Parameters carries provisioner-specific configuration, opaque to IPAM.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

type IPClassStatus struct {
	// +optional
	Phase ClassPhase `json:"phase,omitempty"`
	// OfferingPools is the number of pools backing this class. Zero means every
	// claim naming this class fails, which is worth surfacing before a consumer
	// discovers it.
	//
	// It counts the pools offering the ROOT of this class's chain, which for a
	// class with a parentClassName is not this class. Only the root is backed by
	// operator-authored pools; every class below it is served out of the level
	// above, and no pool ever names it. Counting this class directly would
	// report zero for a chain whose claims all succeed.
	//
	// The server computes it on read, so it does NOT appear in WATCH events: no
	// event fires when a pool changes the count, because nothing writes to the
	// class. Read it with GET or LIST. A controller written to reconcile on
	// changes to this field would never wake up.
	// +optional
	OfferingPools int32 `json:"offeringPools"`
	// ProvisionedPools is the number of pools this class has provisioned for
	// its children. Meaningful only when PoolPer is set.
	// +optional
	ProvisionedPools int32 `json:"provisionedPools"`
	// RequiredScopeRoles is the resolved set of scope roles a claim of this
	// class must supply: this class's UniqueWithin unioned with every
	// PoolPer along its parent chain. The server resolves it because a client
	// otherwise has to walk ParentClassName upward and union by hand — several
	// GETs to validate one field — and because a claim missing a role is
	// rejected rather than widened, so knowing the set in advance is the
	// difference between a clear client-side error and a round trip.
	// +optional
	// +listType=atomic
	RequiredScopeRoles []string `json:"requiredScopeRoles,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPClass `json:"items"`
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

// IPPool is an allocatable address space. Root pools declare a CIDR directly;
// child pools carve a sub-prefix from a parent pool.
//
// A pool is durable infrastructure. Operators author root pools and offer them
// to classes; the allocator provisions child pools on demand, triggered by a
// claim but not owned by one — a pool outlives the claim that caused it.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPPoolSpec   `json:"spec,omitempty"`
	Status IPPoolStatus `json:"status,omitempty"`
}

type IPPoolSpec struct {
	// CIDR is the range a root pool hands out from. A child pool leaves it
	// empty: its range is carved from its parent and reported in
	// status.allocatedCIDR.
	//
	// Two ROOT pools of the same tenant may not overlap, and a create that would
	// overlap is refused with 409. The reason is not tidiness: address
	// uniqueness is enforced *within a pool*, so two roots over one range hand
	// the same address to unrelated claims and nothing downstream notices. See
	// internal/registry/ipam/ippool/rootoverlap.go.
	//
	// Overlap *inside* a pool is a different thing and is legitimate — that is
	// what IPClass.spec.uniqueWithin is for, and two networks sharing a
	// per-location range are expected to hold the same address.
	//
	// +optional
	CIDR string `json:"cidr,omitempty"`
	// +optional
	IPFamily IPFamily `json:"ipFamily,omitempty"`
	// ParentPoolRef names the pool this one is carved from. Empty on root
	// pools, which declare a CIDR instead.
	// +optional
	ParentPoolRef *LocalRef `json:"parentPoolRef,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	PrefixLength int `json:"prefixLength,omitempty"`

	// ClassNames are the classes this pool offers itself to. A claim naming one
	// of these classes may draw from this pool, subject to family and scope
	// agreement. Operator-authored pools set this; it is how capacity is
	// published to consumers without naming them.
	// +optional
	// +listType=set
	ClassNames []string `json:"classNames,omitempty"`

	// ClassRef names the class that provisioned this pool, set by the allocator
	// on pools it creates during a cascade and empty on operator-authored
	// pools. It records which class's configuration this pool was built from.
	// +optional
	ClassRef *LocalRef `json:"classRef,omitempty"`

	// Scope is the combination of references this pool exists for, keyed by
	// role. On a cascade-provisioned pool it is the projection of the
	// triggering claim's scope onto the provisioning class's PoolPer. On an
	// operator-authored pool it declares the pool's own constraints — a pool
	// with scope.location set serves only claims from that location.
	//
	// This map backs the unique index that makes concurrent provisioning safe.
	// +optional
	Scope map[string]ScopeRef `json:"scope,omitempty"`

	// Reservations withholds positions at the edges of this pool. See
	// ReservationSpec — reserved positions become real allocations held by the
	// pool, not invisible holes.
	// +optional
	Reservations *ReservationSpec `json:"reservations,omitempty"`

	// MaxRetentionLease caps how long any allocation drawn from this pool may
	// stay retained, whatever the class asks for. The effective lease is the
	// shorter of the two.
	//
	// The cap lives here because the pool is the thing that runs out. A class
	// declaring a long lease on scarce public space is asking the pool to bear
	// a cost the class does not pay, so the ceiling belongs on the object that
	// owns the scarcity rather than on the one that wants to consume it.
	//
	// Unset means the class's lease applies unchanged.
	// +optional
	MaxRetentionLease *metav1.Duration `json:"maxRetentionLease,omitempty"`

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
	// ScopeDigest is the canonical digest of spec.scope, computed by the
	// server. It is the value the uniqueness index is built on; surfacing it
	// makes "why did these two claims land in different pools" answerable
	// without reading the database.
	// +optional
	ScopeDigest string `json:"scopeDigest,omitempty"`
	// +optional
	Capacity PoolCapacity `json:"capacity,omitempty"`
	// utilizationPercent is the allocated share of the pool's address space,
	// 0–100, computed with arbitrary-precision arithmetic so it is accurate for
	// both IPv4 and IPv6, and rounded to four decimal places.
	//
	// A float because an integer percent is useless at the scale pools are
	// sized for: 256 addresses out of a /12 is 0.024%, which truncated to 0 and
	// read as "nothing allocated" on a pool holding sixteen claims. Four
	// decimals keeps a single address in a /12 visible.
	//
	// Kubernetes API conventions discourage floating point because it does not
	// round-trip reliably across every serializer. That applies to this field
	// and is accepted deliberately: IPAM is an aggregated apiserver serving
	// JSON, the alternative encodings (a scaled integer such as per-million, or
	// a resource.Quantity) are materially harder to read for the audience that
	// reads this field, and the exact address counts in capacity remain
	// available for anyone who needs a number that cannot drift.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	UtilizationPercent float64 `json:"utilizationPercent"`
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

// AllocationPurpose distinguishes an address handed to a claim from space held
// out of circulation, and — among the latter — why it is held.
//
// The allocator's search does not read this beyond `purpose != Claim`: every
// non-Claim value is scope-independent, because the space is genuinely gone
// from every address space carved from the pool rather than from only the one
// whose claim triggered it. The distinction between the non-Claim values
// exists for lifecycle, not for allocation — a pool's own reservations are not
// claims against it, and a delete guard that cannot tell them apart makes any
// pool with reservations undeletable.
// +kubebuilder:validation:Enum=Claim;Reservation;PoolCarve
type AllocationPurpose string

const (
	// PurposeClaim is an allocation bound to an IPClaim.
	PurposeClaim AllocationPurpose = "Claim"
	// PurposeReservation is an allocation held by the pool itself to satisfy
	// spec.reservations. It has no claim and is never handed out.
	PurposeReservation AllocationPurpose = "Reservation"
	// PurposePoolCarve is the block a child pool occupies in its parent. It is
	// released when the child pool is deleted, which is what separates it from
	// a reservation: a reservation outlives everything except a change to the
	// pool's own spec.
	PurposePoolCarve AllocationPurpose = "PoolCarve"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ipalloc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.allocatedCIDR`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.className`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.spec.claimRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient

// IPAllocation records an address or block handed out of an IPPool. It is
// created by the system, never by a consumer.
//
// An allocation may outlive the claim that created it: a claim released under
// ReclaimPolicy Retain leaves its allocation in place with ClaimRef cleared,
// still held and still counted against its holder, until something releases it
// explicitly.
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

	// ClassName is the class this allocation was handed out under. Empty on
	// reservations, which belong to a pool rather than to a class.
	// +optional
	ClassName string `json:"className,omitempty"`

	// Purpose distinguishes a claim-bound allocation from a pool-held
	// reservation.
	// +optional
	Purpose AllocationPurpose `json:"purpose,omitempty"`

	// ClaimRef names the claim currently bound to this allocation. Nil on
	// reservations, and nil on a retained allocation whose claim has been
	// deleted — which is what makes retention visible rather than inferred.
	// +optional
	ClaimRef *LocalRef `json:"claimRef,omitempty"`

	// Scope is the address space this allocation belongs to, keyed by role: the
	// claim's scope projected onto the class's UniqueWithin. Two allocations
	// may hold the same address only if their scopes differ.
	// +optional
	Scope map[string]ScopeRef `json:"scope,omitempty"`

	// ReclaimPolicy is the effective policy for this allocation, resolved from
	// the class and any claim override at bind time. It is recorded here
	// because the allocation outlives the claim that chose it.
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// OwnerRef is the opaque consumer object this allocation is held for. It
	// survives claim deletion under Retain, so a retained address still has an
	// attributable holder for quota and for cleanup.
	// +optional
	OwnerRef *ObjectRef `json:"ownerRef,omitempty"`
}

type IPAllocationStatus struct {
	// +optional
	Phase AllocationPhase `json:"phase,omitempty"`
	// +optional
	AllocatedCIDR string `json:"allocatedCIDR,omitempty"`
	// Address is the single address form of AllocatedCIDR, set when the class
	// hands out host addresses rather than blocks.
	// +optional
	Address string `json:"address,omitempty"`
	// ScopeDigest is the canonical digest of spec.scope, computed by the
	// server — the value uniqueness is enforced on.
	// +optional
	ScopeDigest string `json:"scopeDigest,omitempty"`
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
// IPClaim — a long-lived request for an address of a named class.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ipclaim
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.className`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.allocatedCIDR`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.status.poolRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient

// IPClaim is a long-lived request for an address, bound to one allocation for
// as long as it exists. A claim names a class and carries the scope it was
// made for; it never names a pool, a CIDR, or a region.
//
// Claims are created by the platform on a consumer's behalf, with a
// deterministic name derived from the thing being addressed — which is what
// makes the claim the durable identity a replacement instance finds again.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IPClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPClaimSpec   `json:"spec,omitempty"`
	Status IPClaimStatus `json:"status,omitempty"`
}

type IPClaimSpec struct {
	// ClassName is the class of address being requested. Empty selects the
	// default class for IPFamily, which must then be set.
	// +optional
	ClassName string `json:"className,omitempty"`

	// IPFamily selects the default class when ClassName is empty. It is
	// ignored — and must agree, if set — when ClassName names a class, since a
	// class already fixes its family.
	// +optional
	IPFamily IPFamily `json:"ipFamily,omitempty"`

	// Address names a specific address to bind. Omitted, the allocator chooses.
	// +optional
	Address string `json:"address,omitempty"`

	// PrefixLength is the requested block size in bits. Omitted, the class's
	// DefaultPrefixLength applies. Must fall within the class's
	// AllowedPrefixLengths.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	PrefixLength *int32 `json:"prefixLength,omitempty"`

	// Scope carries the references this claim is made for, keyed by role. The
	// allocator uses them for two things: resolving which pool to draw from,
	// and deciding which allocations this one must not collide with.
	//
	// A claim omitting a role its class names in UniqueWithin, or one its
	// parent chain needs to resolve a pool, is rejected rather than falling
	// back to a wider comparison — a wider comparison would look correct while
	// refusing addresses the narrow one was meant to allow, surfacing as
	// unexplained exhaustion rather than as a missing field.
	//
	// Immutable: a claim whose network or location changed after allocation is
	// incoherent.
	// +optional
	Scope map[string]ScopeRef `json:"scope,omitempty"`

	// ReclaimPolicy overrides the class default for this claim.
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// OwnerRef is the opaque consumer object this claim is made for.
	// +optional
	OwnerRef *ObjectRef `json:"ownerRef,omitempty"`
}

type IPClaimStatus struct {
	// +optional
	Phase ClaimPhase `json:"phase,omitempty"`
	// +optional
	AllocatedCIDR string `json:"allocatedCIDR,omitempty"`
	// Address is the single address form of AllocatedCIDR, set when the class
	// hands out host addresses rather than blocks.
	// +optional
	Address string `json:"address,omitempty"`
	// PoolRef names the pool the allocation was drawn from. It is resolved by
	// the allocator, not requested — this is status precisely because the
	// consumer does not choose it.
	// +optional
	PoolRef *LocalRef `json:"poolRef,omitempty"`
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
