// Package allocator wraps the pure CIDR/ASN allocation primitives in
// internal/allocation/ with PostgreSQL transaction support.
//
// The allocator runs inside a pgx.Tx supplied by the caller (the registry
// layer's AllocatingREST wrapper). The transaction's lifecycle — BEGIN,
// COMMIT, ROLLBACK — is the caller's responsibility; the allocator only
// reads from and writes to the supplied transaction.
//
// Allocation is synchronous: the request thread holds a row-level lock on
// the pool object's row in ipam_objects (`SELECT ... FOR UPDATE`) for the
// duration of the read-decide-write sequence. This serialises concurrent
// claims against the same pool and guarantees no overlapping allocations.
package allocator

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ErrPoolNotFound is returned when the supplied poolKey does not exist in
// ipam_objects.
var ErrPoolNotFound = errors.New("ipam: pool not found")

// ErrPoolExhausted is returned when no free block of the requested size is
// available in the pool. Callers should map this to HTTP 507 (Insufficient
// Storage) at the registry boundary.
var ErrPoolExhausted = errors.New("ipam: pool exhausted")

// ErrObjectNotFound is returned by GetObject when no row exists at the key.
var ErrObjectNotFound = errors.New("ipam: object not found")

// PrefixRequest describes one allocation against a pool.
//
// OwnerProject scopes the allocation to a single tenant project so per-project
// capacity queries (used by the quota integration) can sum allocations
// belonging to a given project. Leave it empty for platform-scoped
// allocations.
type PrefixRequest struct {
	PoolKey   string
	PrefixLen int
	IPFamily  string

	// ClaimKey is the claim the allocation is bound to. Retention clears it,
	// so it is not the row's identity.
	ClaimKey string

	// AllocationKey is the storage key of the IPAllocation the row backs, and
	// the identity every release path finds the row by. Defaults to ClaimKey.
	AllocationKey string

	OwnerProject string

	// ScopeDigest is the address space this allocation must be unique in: the
	// claim's scope projected onto its class's uniqueWithin, digested by
	// internal/scope. It is both what the search excludes other claims by and
	// what the row records, so the block handed out is free in the space it is
	// written into.
	//
	// Empty means the address space no refs separate — the single space a
	// `uniqueWithin: []` class hands out from.
	ScopeDigest string

	// ClassName and ReclaimPolicy are recorded on the row because the
	// allocation outlives the claim that chose them. An empty ReclaimPolicy
	// is Delete.
	ClassName     string
	ReclaimPolicy ipamv1alpha1.ReclaimPolicy
}

// PrefixAllocator atomically reserves a sub-CIDR from an IPPool.
type PrefixAllocator interface {
	// AllocatePrefix reserves a sub-prefix of req.PrefixLen bits within the
	// pool identified by req.PoolKey and returns its CIDR string.
	AllocatePrefix(ctx context.Context, tx pgx.Tx, req PrefixRequest) (string, error)

	// InsertObject writes a generic API object row into ipam_objects inside
	// the supplied transaction and returns the assigned resource_version.
	// Callers use the returned rv to populate metadata.resourceVersion on
	// the in-memory object so the response body matches what readers will
	// see on subsequent GETs.
	InsertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error)

	// Release disposes of the allocations bound to claimKey according to the
	// reclaim policy recorded on each row: Delete frees the address, Retain
	// unbinds the claim and leaves the address held. It returns the
	// allocation keys of the rows that were retained, so the caller can keep
	// their IPAllocation objects instead of deleting them.
	Release(ctx context.Context, tx pgx.Tx, claimKey string) ([]string, error)

	// ReleaseAllocation frees the allocation row identified by allocationKey
	// whatever its claim binding. It is the release path for a retained
	// allocation, which no longer has a claim to release through.
	ReleaseAllocation(ctx context.Context, tx pgx.Tx, allocationKey string) error

	// ReleasePoolReservations frees the rows a pool holds for its own
	// reserved edge positions. They belong to no claim, so no claim-driven
	// release path reaches them, and a pool cannot be deleted while they
	// remain.
	ReleasePoolReservations(ctx context.Context, tx pgx.Tx, poolKey string) error

	// GetObject reads the stored API object at key inside the supplied
	// transaction, returning ErrObjectNotFound if no row exists.
	GetObject(ctx context.Context, tx pgx.Tx, key string) ([]byte, error)

	// DeleteObject removes the API object row at key from ipam_objects and
	// records a DELETED changelog entry inside the supplied transaction. The
	// AllocatingREST Delete handlers call this alongside Release so the claim
	// itself disappears from storage instead of leaking after its allocation
	// row is freed. Returns the resource_version stamped on the DELETED
	// changelog row, or 0 if the object was already gone.
	DeleteObject(ctx context.Context, tx pgx.Tx, key string) (int64, error)

	// UpdateObject rewrites the API object row at key with a fresh
	// resource_version and records a MODIFIED changelog entry inside the
	// supplied transaction. Used by the AllocatingREST Delete handlers to
	// publish phase=Releasing as a discrete watch event before the object is
	// removed. Returns the assigned resource_version, or an error if the row
	// does not exist.
	UpdateObject(ctx context.Context, tx pgx.Tx, key string, data []byte) (int64, error)
}
