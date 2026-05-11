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
)

// ErrPoolNotFound is returned when the supplied poolKey does not exist in
// ipam_objects.
var ErrPoolNotFound = errors.New("ipam: pool not found")

// ErrPoolExhausted is returned when no free block of the requested size is
// available in the pool. Callers should map this to HTTP 507 (Insufficient
// Storage) at the registry boundary.
var ErrPoolExhausted = errors.New("ipam: pool exhausted")

// PrefixAllocator atomically reserves a sub-CIDR from an IPPrefix pool.
//
// ownerProject scopes the allocation to a single tenant project so per-project
// capacity queries (used by the quota integration) can sum allocations
// belonging to a given project. Pass "" for platform-scoped allocations.
type PrefixAllocator interface {
	// AllocatePrefix reserves a sub-prefix of prefixLen bits within the
	// pool identified by poolKey and returns its CIDR string.
	AllocatePrefix(ctx context.Context, tx pgx.Tx, poolKey string, prefixLen int, ipFamily string, claimKey string, ownerProject string) (string, error)

	// AllocateSingleAddress reserves a single host address within the pool
	// identified by poolKey and returns its IP string (without prefix).
	AllocateSingleAddress(ctx context.Context, tx pgx.Tx, poolKey string, ipFamily string, claimKey string, ownerProject string) (string, error)

	// InsertObject writes a generic API object row into ipam_objects inside
	// the supplied transaction and returns the assigned resource_version.
	// Callers use the returned rv to populate metadata.resourceVersion on
	// the in-memory object so the response body matches what readers will
	// see on subsequent GETs.
	InsertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error)

	// InsertChildPrefix writes a child IPPrefix object row into ipam_objects
	// inside the supplied transaction. Used by IPPrefixClaim creates with
	// CreateChildPrefix=true so the child pool object materialises atomically
	// with the parent allocation.
	InsertChildPrefix(ctx context.Context, tx pgx.Tx, key, namespace, name string, data []byte) error

	// Release removes the prefix allocation record matching claimKey.
	Release(ctx context.Context, tx pgx.Tx, claimKey string) error

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

// ASNAllocator atomically reserves an ASN from an ASNPool.
//
// ownerProject scopes the allocation to a single tenant project so per-project
// capacity queries can sum allocations belonging to a given project. Pass ""
// for platform-scoped allocations.
type ASNAllocator interface {
	AllocateASN(ctx context.Context, tx pgx.Tx, poolKey string, claimKey string, ownerProject string) (int64, error)

	// InsertObject writes a generic API object row into ipam_objects inside
	// the supplied transaction and returns the assigned resource_version.
	InsertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error)

	Release(ctx context.Context, tx pgx.Tx, claimKey string) error

	// DeleteObject removes the API object row at key from ipam_objects and
	// records a DELETED changelog entry inside the supplied transaction.
	// Returns the resource_version stamped on the DELETED changelog row, or
	// 0 if the object was already gone.
	DeleteObject(ctx context.Context, tx pgx.Tx, key string) (int64, error)

	// UpdateObject rewrites the API object row at key with a fresh
	// resource_version and records a MODIFIED changelog entry inside the
	// supplied transaction. Used by the AllocatingREST Delete handlers to
	// publish phase=Releasing as a discrete watch event before the object is
	// removed. Returns the assigned resource_version, or an error if the row
	// does not exist.
	UpdateObject(ctx context.Context, tx pgx.Tx, key string, data []byte) (int64, error)
}
