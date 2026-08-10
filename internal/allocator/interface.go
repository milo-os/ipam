// Package allocator wraps the pure CIDR allocation primitives in
// internal/allocation/ with PostgreSQL transaction support and the class
// model's pool resolution.
//
// # Two levels, two transaction shapes
//
// Handing out an address is one transaction: the caller opens it, the allocator
// locks the resolved pool with `SELECT ... FOR UPDATE`, reads the allocations in
// the claim's address space, chooses a block, and writes. That is
// AllocatePrefix, and the transaction's lifecycle belongs to the caller.
//
// Finding the pool in the first place is not one transaction, and must not be.
// A claim into a scope nothing has used yet provisions a pool at every level of
// its class's chain, and doing that in one transaction would hold a lock on the
// platform-wide root pool for the whole cascade. ResolvePool therefore takes a
// TxBeginner rather than a transaction and commits each level separately. The
// reasoning is in cascade.go, and it is worth reading before changing anything
// there.
package allocator

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrPoolNotFound is returned when a pool key does not exist in
	// ipam_objects.
	ErrPoolNotFound = errors.New("ipam: pool not found")

	// ErrPoolExhausted is returned when no free block of the requested size is
	// available. Callers map it to HTTP 507 (Insufficient Storage).
	ErrPoolExhausted = errors.New("ipam: pool exhausted")

	// ErrAddressTaken is returned when a claim names a specific address that is
	// already held in the same address space.
	ErrAddressTaken = errors.New("ipam: address already allocated")

	// ErrAddressOutOfRange is returned when a claim names an address that does
	// not fall inside the resolved pool.
	ErrAddressOutOfRange = errors.New("ipam: address outside pool")

	// ErrAddressInvalid is returned when a claim's spec.address does not parse.
	ErrAddressInvalid = errors.New("ipam: invalid address")

	// ErrAllocationExists is returned when an allocation row already exists
	// under the key being written.
	//
	// It is always a server-side fault: a caller never chooses an allocation key,
	// so reaching it means this service left a row behind that its own bookkeeping
	// should have removed — a pool deleted without releasing its carve, most
	// likely. It exists as a distinct sentinel so that failure is greppable and
	// so the driver's SQLSTATE never reaches an API client.
	ErrAllocationExists = errors.New("ipam: allocation already exists")

	// ErrObjectExists is returned by InsertObject when an API object row already
	// exists under the key being written.
	//
	// Unlike ErrAllocationExists this is normally the caller's doing: the key is
	// derived from the object's project, resource and name, so a second CREATE of
	// the same name lands on it. Callers must map it to 409 AlreadyExists —
	// registryerrors.MapWriteError does that and is the only thing that should.
	//
	// It exists because the registries that write objects by hand bypass the
	// generic Store, and with it the storage layer's own unique-violation
	// mapping. Before it, `kubectl create` of an existing IPPool answered 500
	// with `duplicate key value violates unique constraint "ipam_objects_pkey"
	// (SQLSTATE 23505)`, which is both unactionable and a description of our
	// schema. Translating at this boundary means a call site that forgets the
	// mapping still returns a wrong *code* rather than leaking driver internals.
	ErrObjectExists = errors.New("ipam: object already exists")

	// ErrInvalidReservation is returned when a pool's reservations cannot be
	// satisfied by the space it holds.
	//
	// It is deliberately distinct from ErrPoolExhausted and must not be mapped
	// to 507. A reservation that does not fit is operator misconfiguration, and
	// reporting it as a full pool would page someone about capacity when the
	// fix is a class field. From a claim's point of view the two look alike;
	// from an operator's they could not be less alike.
	ErrInvalidReservation = errors.New("ipam: invalid reservation")
)

// PrefixAllocator atomically reserves a block for a claim, and maintains the
// generic object rows the synchronous-allocation registries write directly.
//
// The object methods are on this interface rather than on a separate storage
// abstraction because they must run in the *same* transaction as the
// allocation: a claim whose row committed without its allocation row, or the
// reverse, is exactly the inconsistency the synchronous design exists to
// prevent.
type PrefixAllocator interface {
	// AllocatePrefix reserves a block inside the pool named by the request and
	// returns its CIDR.
	AllocatePrefix(ctx context.Context, tx pgx.Tx, req AllocateRequest) (string, error)

	// CarveChildPool reserves a block for a child pool out of parentPoolKey and
	// returns its CIDR.
	//
	// This is not AllocatePrefix with a different label on the row, and reaching
	// for AllocatePrefix here is the mistake this method exists to prevent. A
	// claim is an allocation *within* an address space and only has to avoid the
	// blocks in that space; a child pool is address space *leaving* the parent,
	// so it must avoid every block the parent holds no matter whose space it
	// belongs to. Carving with a scoped search places a sub-pool on top of a live
	// claim in another space, and the exclusion constraint does not catch it
	// because the two rows differ in scope_digest.
	CarveChildPool(ctx context.Context, tx pgx.Tx, parentPoolKey string, prefixLen int, rec PoolCarveRecord) (string, error)

	// Release disposes of the allocations bound to claimKey according to the
	// reclaim policy each was recorded with, and reports what became of each so
	// the caller can keep the IPAllocation objects in step. A retained
	// allocation is unbound rather than deleted.
	Release(ctx context.Context, tx pgx.Tx, claimKey string) ([]ReleaseOutcome, error)

	// ReclaimRetained re-binds a retained allocation to a new claim, returning
	// the address recovered and whether there was anything to recover.
	//
	// This is the half of retention that makes it worth having: without it,
	// Retain only means "nobody else may have this", and the replacement gets a
	// different address than the one held for it.
	ReclaimRetained(ctx context.Context, tx pgx.Tx, req ReclaimRequest) (string, bool, error)

	// ForceRelease deletes one allocation by its own key, reporting whether a row
	// was actually there. It is how a retained allocation — which has no claim
	// left to release it through — returns to a claimable state.
	//
	// The bool matters. Releasing a key no row matches is legitimate (a root pool
	// has no carve; a repeated delete is idempotent) but it is also exactly what a
	// drifted key looks like, and in that case the caller reports success while
	// the address stays consumed forever. A caller that knows a row should exist
	// must check it.
	ForceRelease(ctx context.Context, tx pgx.Tx, allocationKey string) (bool, error)

	// InsertObject writes a generic API object row into ipam_objects inside the
	// supplied transaction and returns the assigned resource_version. Callers
	// stamp the returned rv on the in-memory object so the response body matches
	// what readers will see on subsequent GETs.
	InsertObject(ctx context.Context, tx pgx.Tx, key, kind, namespace, name string, data []byte) (int64, error)

	// DeleteObject removes the API object row at key and records a DELETED
	// changelog entry. Returns the resource_version stamped on that entry, or 0
	// if the object was already gone.
	DeleteObject(ctx context.Context, tx pgx.Tx, key string) (int64, error)

	// UpdateObject rewrites the API object row at key with a fresh
	// resource_version and records a MODIFIED changelog entry, so watchers
	// observe intermediate states — the Releasing phase, and an allocation
	// losing its claim reference under Retain.
	UpdateObject(ctx context.Context, tx pgx.Tx, key string, data []byte) (int64, error)
}

// Compile-time assertion that the Postgres implementation satisfies the
// interface the registries depend on.
var _ PrefixAllocator = (*PostgresPrefixAllocator)(nil)

// ExhaustionError reports which pool ran out and what state it was in.
//
// It exists because of what the class model took away. A claim no longer names
// a pool, so a bare "pool exhausted" leaves the caller unable to say what filled
// up — and it cannot find out afterwards: listing the pools that offer the class
// fans out, and misses cascade-provisioned pools entirely, since those carry a
// classRef rather than classNames. Every fact here is in hand at the moment of
// failure and nowhere else, so it travels with the error.
//
// It used to also carry the largest free aligned block, which is the figure that
// distinguishes "needs more capacity" from "is fragmented". That was removed
// because it was not free: finding it costs a scan of every free region, and the
// same computation sat on the success path too, where it was ~99% of measure().
// Utilization answers the common case — a pool at 100% needs capacity — and the
// fragmented case is diagnosable from the allocation list when it arises.
//
// It wraps ErrPoolExhausted, so existing errors.Is checks keep working and
// callers that want the detail use errors.As.
type ExhaustionError struct {
	// PoolKey is the storage key of the pool that ran out. For a multi-level
	// cascade this names the level that actually exhausted, which is frequently
	// not the level the claim asked for.
	PoolKey string
	// RequestedPrefixLength is the block size that could not be satisfied.
	RequestedPrefixLength int
	// UtilizationPercent is the pool's allocated share, 0–100.
	UtilizationPercent float64
}

func (e *ExhaustionError) Error() string {
	return fmt.Sprintf("ipam: pool %q cannot satisfy a /%d (%g%% utilized)",
		e.PoolKey, e.RequestedPrefixLength, e.UtilizationPercent)
}

// Unwrap makes errors.Is(err, ErrPoolExhausted) true, so every existing
// exhaustion check keeps working and only the callers that want the numbers
// need to know this type exists.
func (e *ExhaustionError) Unwrap() error { return ErrPoolExhausted }
