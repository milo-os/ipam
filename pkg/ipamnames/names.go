// Package ipamnames derives the names IPAM gives the objects it creates.
//
// IPAM names a claim's IPAllocation from the claim's namespace and name, and
// the allocation keeps that name for its whole life, including after a claim
// under reclaim policy Retain is deleted. A client that knows a claim's name
// can therefore find its allocation, retained or not, with a single get,
// without holding the claim and without asking IPAM to allocate anything.
//
// The package imports nothing but the standard library, so a consumer can
// depend on it without pulling in an apiserver.
package ipamnames

import (
	"crypto/sha256"
	"encoding/hex"
)

// AllocationPrefix starts the name of every IPAllocation IPAM creates for a
// claim, so system-generated names are obvious at a glance.
const AllocationPrefix = "alloc-"

// AllocationName returns the name of the IPAllocation that a claim named
// claimName in claimNamespace is bound to. The allocation lives in the same
// namespace as the claim.
//
// The name is a truncated SHA-256 of "namespace/name": stable, fixed in
// length whatever the claim's name, and the same before and after the claim
// is deleted. IPAM refuses to change it, because existing allocations are
// already stored under it.
func AllocationName(claimNamespace, claimName string) string {
	h := sha256.Sum256([]byte(claimNamespace + "/" + claimName))
	return AllocationPrefix + hex.EncodeToString(h[:8])
}
