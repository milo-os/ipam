package allocator

// Reclaiming a retained allocation.
//
// This is the half of retention that makes it worth having. Retention keeps an
// address held when its claim goes away; reclaim is how the replacement gets
// that *same* address back. Without it, `Retain` only means "nobody else may
// have this either", and the only route to the address is to release it and
// allocate again — which returns a different address and opens exactly the
// window that retaining-by-not-unbinding was chosen to avoid.
//
// # What entitles a claim to a retained address
//
// Not the address. A claim naming an address it wants is a separate and more
// privileged operation, and binding on address alone would let any claim seize
// any retained address in a pool it can reach.
//
// The key is **claim identity**. A claim's name is deterministic — derived from
// the slot, the interface and the family — so a replacement instance filling the
// same slot produces the same claim name, and the allocation's own name is a
// pure function of that. So the replacement asks for its allocation by the key
// it would have written anyway, and finding a retained row there *is* the proof
// of entitlement: only a claim of that identity could have derived that key.
//
// # Why a mismatch is refused rather than reinterpreted
//
// A replacement claim of a different class, size, or address space is not the
// same claim wearing the same name. Quietly allocating it something else would
// be the worst outcome available: the caller believes it recovered its address,
// the old one stays retained and unreferenced, and nothing says so. Every
// disagreement is an error naming the field that differs.
//
// # Ordering against the sweeper
//
// The reclaim takes the pool's row lock before touching the allocation, which is
// the same lock the sweeper takes. That is what makes the two safe against each
// other: a reclaim either happens entirely before a release or entirely after
// it, and after it the row is simply gone and the caller allocates fresh.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
)

// ErrRetainedMismatch is returned when a retained allocation exists under the
// claim's identity but does not agree with what the claim is asking for.
//
// Deliberately an error rather than a fallback to fresh allocation. The claim
// believes it is recovering a specific address; handing it a different one
// silently would leave the retained address held by nobody, invisible, and
// consuming capacity — while the caller had every reason to think it had
// recovered.
var ErrRetainedMismatch = errors.New("ipam: retained allocation does not match this claim")

// ReclaimRequest describes a claim attempting to recover its retained address.
type ReclaimRequest struct {
	// AllocationKey is the storage key the claim's identity derives. Its
	// presence in the table is the entitlement: only a claim of this identity
	// could have produced it.
	AllocationKey string
	// ClaimKey is the claim to bind the allocation to.
	ClaimKey string
	// ClassName the claim allocates under. Must match what was retained.
	ClassName string
	// ScopeDigest of the address space the claim belongs to. Must match — a
	// network deleted and recreated under the same name is a different address
	// space, and a claim in the new one must not inherit the old one's addresses.
	ScopeDigest string
	// PrefixLength the claim is asking for. Must match the retained block.
	PrefixLength int
}

// ReclaimRetained re-binds a retained allocation to a new claim, returning the
// address it recovered.
//
// The bool distinguishes "there was nothing to reclaim" — the ordinary case, and
// not an error — from a successful reclaim. A mismatch is neither: it returns
// ErrRetainedMismatch, because something *is* there and it is not what the
// caller asked for.
func (a *PostgresPrefixAllocator) ReclaimRetained(ctx context.Context, tx pgx.Tx, req ReclaimRequest) (string, bool, error) {
	defer metrics.ObserveQuery("reclaim_retained", time.Now())

	// Discover the pool without a lock, so the lock can be taken in the same
	// order every other path takes it: pool first, then the rows in it.
	var poolKey string
	err := tx.QueryRow(ctx,
		`SELECT pool_key FROM ipam_cidr_allocations WHERE allocation_key = $1`,
		req.AllocationKey,
	).Scan(&poolKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing retained under this identity. The overwhelmingly common
			// case — a first claim, or one whose predecessor released.
			return "", false, nil
		}
		return "", false, fmt.Errorf("look up retained allocation %q: %w", req.AllocationKey, err)
	}

	if _, err := lockAndDecodeIPPool(ctx, tx, poolKey); err != nil {
		if errors.Is(err, ErrPoolNotFound) {
			// The row outlived its pool. Not reclaimable, and not this path's
			// problem to repair.
			return "", false, nil
		}
		return "", false, err
	}

	// Re-read under the pool lock. Between the discovery read and here the
	// sweeper may have released it, or another claim may have taken it.
	var cidr, className, scopeDigest string
	var claimKey *string
	err = tx.QueryRow(ctx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr),
		        class_name, scope_digest, claim_key, purpose
		   FROM ipam_cidr_allocations
		  WHERE allocation_key = $1
		  FOR UPDATE`,
		req.AllocationKey,
	).Scan(&cidr, &className, &scopeDigest, &claimKey, new(string))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("re-read retained allocation %q: %w", req.AllocationKey, err)
	}

	if claimKey != nil {
		// Already bound. Either this claim is being created twice, or a
		// different one holds the identity — both are the caller's business,
		// and neither is a reclaim.
		return "", false, fmt.Errorf("%w: an allocation under this identity is already bound to %q",
			ErrRetainedMismatch, *claimKey)
	}

	if err := reclaimAgrees(req, cidr, className, scopeDigest); err != nil {
		return "", false, err
	}

	// Bind it, as a compare-and-set.
	//
	// `claim_key IS NULL` looks redundant here — the row is held FOR UPDATE
	// under the pool lock, so nothing can bind it between the read above and
	// this write — and it must not be removed on that reasoning. It is what
	// makes the statement correct *independently* of the locking above it, and
	// the failure it prevents is the quiet kind: without it, two replacement
	// claims for one retained address both report success and the second
	// silently takes it from the first. The lock is why that cannot happen
	// today; the predicate is why it cannot happen if the lock ever moves.
	//
	// Migration 004's trigger clears retained_at, so the lease clock stops here
	// and restarts from scratch if this allocation is ever released again — an
	// address that has been round-tripped once does not inherit its first
	// retention.
	tag, err := tx.Exec(ctx,
		`UPDATE ipam_cidr_allocations
		    SET claim_key = $2
		  WHERE allocation_key = $1 AND claim_key IS NULL`,
		req.AllocationKey, req.ClaimKey,
	)
	if err != nil {
		return "", false, fmt.Errorf("rebind retained allocation %q: %w", req.AllocationKey, err)
	}
	if tag.RowsAffected() == 0 {
		// Unreachable while the lock above is held, and reported rather than
		// assumed away: returning success here would hand the caller an address
		// it does not hold, which is the worst failure this function has
		// available to it.
		return "", false, fmt.Errorf(
			"rebind retained allocation %q: another claim bound it first", req.AllocationKey)
	}

	metrics.RecordReclaimRetained(req.ClassName)
	return cidr, true, nil
}

// reclaimAgrees checks that a retained allocation is the same thing the claim is
// asking for, naming the field that differs.
//
// The scope digest is the subtle one and the reason it is checked at all: a
// network deleted and recreated under the same name is a *different* address
// space by the model's own rule, so a claim made in the new one carries a
// different digest and must not inherit the old space's addresses even though it
// derives an identical claim name.
func reclaimAgrees(req ReclaimRequest, cidr, className, scopeDigest string) error {
	if className != req.ClassName {
		return fmt.Errorf("%w: it was allocated under class %q and this claim names %q",
			ErrRetainedMismatch, className, req.ClassName)
	}
	if scopeDigest != req.ScopeDigest {
		return fmt.Errorf("%w: it belongs to a different address space; a reference deleted and recreated under the same name is a different space and does not inherit its predecessor's addresses",
			ErrRetainedMismatch)
	}
	if got := prefixLenOf(cidr); got != req.PrefixLength {
		return fmt.Errorf("%w: it is a /%d and this claim asks for a /%d",
			ErrRetainedMismatch, got, req.PrefixLength)
	}
	return nil
}

// prefixLenOf extracts the mask length from a CIDR string the database rendered.
// Returns -1 when it cannot, which no comparison will match.
func prefixLenOf(cidr string) int {
	for i := len(cidr) - 1; i >= 0; i-- {
		if cidr[i] == '/' {
			n := 0
			for _, c := range cidr[i+1:] {
				if c < '0' || c > '9' {
					return -1
				}
				n = n*10 + int(c-'0')
			}
			return n
		}
	}
	return -1
}
