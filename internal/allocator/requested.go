package allocator

// The specific-address path: a claim that names the block it wants rather than
// a size. It asks "is this exact block free here", which either holds or does
// not, so it never searches and never moves the claim to another block.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
)

var (
	// ErrAddressOutsidePool is returned when a requested address lies outside
	// the pool a claim draws from, or outside every pool offering its class.
	ErrAddressOutsidePool = errors.New("ipam: address is outside the pool")

	// ErrAddressReserved is returned when a requested address sits in a
	// position the pool withholds.
	ErrAddressReserved = errors.New("ipam: address is reserved by the pool")

	// ErrAddressInUse is returned, wrapped in an *AddressInUseError, when a
	// requested address is already held.
	ErrAddressInUse = errors.New("ipam: address is in use")
)

// AddressInUseError says what holds a requested address.
type AddressInUseError struct {
	// Address is the block the claim asked for.
	Address string
	// Held is the block that overlaps it, which may be larger or smaller.
	Held string

	// Carved reports that the block was carved to a child pool rather than
	// handed to a claim.
	Carved bool

	// AllocationNamespace and AllocationName identify the IPAllocation holding
	// the block. Empty for a carve, and for an allocation in another project:
	// that project's namespace names are not the caller's to read.
	AllocationNamespace string
	AllocationName      string

	// Retained reports that the holding allocation outlived its claim under
	// reclaimPolicy Retain, so only deleting the IPAllocation frees it.
	Retained bool
}

func (e *AddressInUseError) Error() string {
	switch {
	case e.Carved:
		return fmt.Sprintf("address %s is in use: %s is carved to a child pool", e.Address, e.Held)
	case e.AllocationName == "":
		return fmt.Sprintf("address %s is in use: %s is held by another project's allocation", e.Address, e.Held)
	case e.Retained:
		return fmt.Sprintf("address %s is in use: %s is held by IPAllocation %s/%s, retained after its claim was deleted; delete that IPAllocation to free the address",
			e.Address, e.Held, e.AllocationNamespace, e.AllocationName)
	default:
		return fmt.Sprintf("address %s is in use: %s is held by IPAllocation %s/%s",
			e.Address, e.Held, e.AllocationNamespace, e.AllocationName)
	}
}

func (e *AddressInUseError) Is(target error) bool { return target == ErrAddressInUse }

// requestedBlock returns the block req.Address names if the pool can bind it.
// The caller holds the pool row's lock and has materialised its reservations.
//
// Containment is checked apart from overlap because the two point at
// different people: an address outside the pool means the claim reached the
// wrong pool, an address inside it that is taken means someone holds it.
//
// The search floor is not read or moved. The floor promises only that nothing
// below it is free, and binding a block above or below it keeps that promise;
// touching it would also take the floor's lock on a path that does not need
// it.
func requestedBlock(ctx context.Context, tx pgx.Tx, req PrefixRequest, parents []net.IPNet) (*net.IPNet, error) {
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanFindBlock)
	defer span.End()
	span.SetAttributes(attribute.String(tracing.AttrStrategy, "Requested"))
	fail := func(err error) (*net.IPNet, error) {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	want, err := allocation.RequestedBlock(req.Address, req.PrefixLen)
	if err != nil {
		return fail(err)
	}
	own := req.AllocationKey
	if own == "" {
		own = req.ClaimKey
	}
	if err := checkRequestedBlock(ctx, tx, req.PoolKey, req.ScopeDigest, parents, want, own); err != nil {
		return fail(err)
	}

	span.SetAttributes(attribute.String(tracing.AttrResultCIDR, want.String()))
	return &want, nil
}

// checkRequestedBlock reports whether want can be taken from the pool at
// poolKey, whose range is parents, in the address space scopeDigest: nil when
// it can, otherwise the refusal that says why not. ownKey is the storage key of
// whoever is asking, and decides only whether a holding allocation is named.
//
// It is the one classification both a named block and a named carve use, so a
// claim and a ScopeRange placing a pool are refused in the same words for the
// same collision. A carve passes spaceAll: the block leaves the pool, so it
// must be free in every space.
func checkRequestedBlock(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, parents []net.IPNet, want net.IPNet, ownKey string) error {
	if !allocation.IsBlockAvailable(parents, nil, want) {
		return fmt.Errorf("%w: %s is not inside pool %q (%s)", ErrAddressOutsidePool,
			want.String(), poolNameFromKey(poolKey), parentsString(parents))
	}
	holder, err := firstOverlap(ctx, tx, poolKey, scopeDigest, want)
	if err != nil {
		return err
	}
	if holder != nil {
		return holder.refusal(want, poolKey, ownKey)
	}
	return nil
}

// overlappingRow is the one allocation row a requested block collides with.
type overlappingRow struct {
	purpose       string
	allocationKey string
	retained      bool
	cidr          string
}

// firstOverlap returns the row the requested block collides with in its
// address space, or nil when it is free.
//
// searchFilter is the predicate the search applies, so a block this reports
// free is one the search would also have treated as free: claims of the same
// space, plus the reservations and carves that hold their space in every one.
//
// A reservation is reported ahead of anything else it overlaps, then a carve:
// those are what an operator fixes, and naming a claim instead would send the
// caller after the wrong holder.
func firstOverlap(ctx context.Context, tx pgx.Tx, poolKey, scopeDigest string, want net.IPNet) (*overlappingRow, error) {
	defer metrics.ObserveQuery("requested_block_overlap", time.Now())
	var row overlappingRow
	err := tx.QueryRow(ctx,
		`SELECT purpose, allocation_key, claim_key IS NULL,
		        host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE `+searchFilter+`
		    AND allocated_cidr && $3::cidr
		  ORDER BY CASE purpose WHEN 'Reservation' THEN 0 WHEN 'PoolCarve' THEN 1 ELSE 2 END,
		           allocated_cidr
		  LIMIT 1`,
		poolKey, scopeDigest, want.String(),
	).Scan(&row.purpose, &row.allocationKey, &row.retained, &row.cidr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("check requested block %s: %w", want.String(), err)
	}
	return &row, nil
}

// refusal classifies the collision by what holds the block. ownKey is the
// requester's storage key: a holding allocation is named only when it is in
// the requester's own project.
func (r *overlappingRow) refusal(want net.IPNet, poolKey, ownKey string) error {
	switch r.purpose {
	case "Reservation":
		return fmt.Errorf("%w: %s overlaps %s, which pool %q withholds", ErrAddressReserved,
			want.String(), r.cidr, poolNameFromKey(poolKey))
	case "PoolCarve":
		return &AddressInUseError{Address: want.String(), Held: r.cidr, Carved: true}
	}
	e := &AddressInUseError{Address: want.String(), Held: r.cidr, Retained: r.retained}
	if tenant.ProjectFromKey(r.allocationKey) == tenant.ProjectFromKey(ownKey) {
		e.AllocationNamespace, e.AllocationName = allocationRefFromKey(r.allocationKey)
	}
	return e
}

// allocationRefFromKey returns the namespace and name an IPAllocation storage
// key ends in: ".../ipallocations/<namespace>/<name>".
func allocationRefFromKey(key string) (namespace, name string) {
	const marker = "/ipallocations/"
	i := strings.LastIndex(key, marker)
	if i < 0 {
		return "", ""
	}
	ns, n, ok := strings.Cut(key[i+len(marker):], "/")
	if !ok {
		return "", ""
	}
	return ns, n
}

func poolNameFromKey(key string) string {
	return key[strings.LastIndex(key, "/")+1:]
}

func parentsString(parents []net.IPNet) string {
	out := make([]string, len(parents))
	for i, p := range parents {
		out[i] = p.String()
	}
	return strings.Join(out, ", ")
}
