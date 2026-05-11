package allocation

import (
	"errors"
	"sort"
)

// ErrInvalidASNRange indicates a range with End < Start or non-positive ASN.
var ErrInvalidASNRange = errors.New("ipam: invalid asn range")

// ASNRange is an inclusive [Start, End] range of ASN values.
type ASNRange struct {
	Start int64
	End   int64
}

// ASNPool holds the configured ranges and the currently allocated ASNs.
// Existing must be sorted ascending; callers wishing to mutate the pool
// should call Allocate or Release and use the returned slice.
type ASNPool struct {
	Ranges   []ASNRange
	Existing []int64
}

// Allocate returns the lowest free ASN across the configured ranges, scanning
// ranges in order. Returns ErrPoolExhausted if no ASN is free.
func (p *ASNPool) Allocate() (int64, error) {
	for _, r := range p.Ranges {
		if r.End < r.Start {
			return 0, ErrInvalidASNRange
		}
		// Index of the first existing ASN >= r.Start.
		i := sort.Search(len(p.Existing), func(j int) bool { return p.Existing[j] >= r.Start })
		next := r.Start
		for i < len(p.Existing) && p.Existing[i] <= r.End {
			if p.Existing[i] > next {
				return next, nil
			}
			// p.Existing[i] == next: advance past it.
			next = p.Existing[i] + 1
			i++
		}
		if next <= r.End {
			return next, nil
		}
	}
	return 0, ErrPoolExhausted
}

// Release returns a copy of Existing with asn removed. The pool itself is not
// mutated. Returns ErrNotInPool if asn is not currently allocated.
func (p *ASNPool) Release(asn int64) ([]int64, error) {
	i := sort.Search(len(p.Existing), func(j int) bool { return p.Existing[j] >= asn })
	if i >= len(p.Existing) || p.Existing[i] != asn {
		return p.Existing, ErrNotInPool
	}
	out := make([]int64, 0, len(p.Existing)-1)
	out = append(out, p.Existing[:i]...)
	out = append(out, p.Existing[i+1:]...)
	return out, nil
}

// Available reports the number of ASNs that are currently free across all
// configured ranges. Caller is responsible for ensuring Existing values fall
// within the configured ranges (otherwise the result is undefined).
func (p *ASNPool) Available() int64 {
	var total int64
	for _, r := range p.Ranges {
		if r.End < r.Start {
			continue
		}
		total += r.End - r.Start + 1
	}
	// Subtract existing entries that fall within any range.
	for _, a := range p.Existing {
		if asnInAnyRange(a, p.Ranges) {
			total--
		}
	}
	if total < 0 {
		return 0
	}
	return total
}

func asnInAnyRange(a int64, rs []ASNRange) bool {
	for _, r := range rs {
		if a >= r.Start && a <= r.End {
			return true
		}
	}
	return false
}
