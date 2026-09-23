package tableconvertor

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// Columns marked Priority 1 are the wide set: a client shows them only for
// `-o wide`, which is where the fields that answer a follow-up question go,
// rather than the ones that identify the object.
const wide = 1

var (
	nameColumn = metav1.TableColumnDefinition{
		Name: "Name", Type: "string", Format: "name",
		Description: "Name of the object.",
	}
	ageColumn = metav1.TableColumnDefinition{
		Name: "Age", Type: "string",
		Description: "Time elapsed since the object was created.",
	}
)

// IPPools prints a pool as the address space it holds: what range, which
// family, which classes may draw on it, and how full it is.
func IPPools() rest.TableConvertor {
	return New(v1alpha1.Resource("ippools"), []metav1.TableColumnDefinition{
		nameColumn,
		{Name: "CIDR", Type: "string", Description: "Address range the pool holds."},
		{Name: "Family", Type: "string", Description: "Address family of the pool."},
		{Name: "Class", Type: "string", Description: "Class this pool serves: the classes it offers itself to, or the class that provisioned it."},
		{Name: "Utilization", Type: "string", Description: "Allocated share of the pool's address space."},
		ageColumn,
		{Name: "Parent", Type: "string", Priority: wide, Description: "Pool this one was carved from."},
		{Name: "Scope", Type: "string", Priority: wide, Description: "References this pool exists for."},
		{Name: "Phase", Type: "string", Priority: wide, Description: "Current lifecycle phase."},
	}, func(obj runtime.Object, name, age string) []interface{} {
		p, ok := obj.(*ipam.IPPool)
		if !ok {
			return []interface{}{name, "", "", "", "", age, "", "", ""}
		}
		// The carved range on a provisioned pool, the declared one on a root.
		cidr := p.Status.AllocatedCIDR
		if cidr == "" {
			cidr = p.Spec.CIDR
		}
		family := p.Status.IPFamily
		if family == "" {
			family = p.Spec.IPFamily
		}
		return []interface{}{
			name,
			orEmpty(cidr),
			orEmpty(string(family)),
			poolClass(p),
			poolUtilization(p),
			age,
			localRefName(p.Spec.ParentPoolRef),
			formatScope(p.Spec.Scope),
			orEmpty(string(p.Status.Phase)),
		}
	})
}

// IPClaims prints a claim as the request it makes and what came back: the class
// asked for, and the range and pool it was bound to.
func IPClaims() rest.TableConvertor {
	return New(v1alpha1.Resource("ipclaims"), []metav1.TableColumnDefinition{
		nameColumn,
		{Name: "Class", Type: "string", Description: "Class the claim draws from."},
		{Name: "Family", Type: "string", Description: "Address family requested."},
		{Name: "Allocated", Type: "string", Description: "Range or address bound to the claim."},
		{Name: "Pool", Type: "string", Description: "Pool the claim was bound to."},
		{Name: "Phase", Type: "string", Description: "Current lifecycle phase."},
		ageColumn,
		{Name: "Scope", Type: "string", Priority: wide, Description: "References the claim was made for."},
		{Name: "Allocation", Type: "string", Priority: wide, Description: "IPAllocation backing the claim."},
	}, func(obj runtime.Object, name, age string) []interface{} {
		c, ok := obj.(*ipam.IPClaim)
		if !ok {
			return []interface{}{name, "", "", "", "", "", age, "", ""}
		}
		return []interface{}{
			name,
			orEmpty(c.Spec.ClassName),
			orEmpty(string(c.Spec.IPFamily)),
			orEmpty(allocated(c.Status.AllocatedCIDR, c.Status.Address)),
			localRefName(c.Status.PoolRef),
			orEmpty(string(c.Status.Phase)),
			age,
			formatScope(c.Spec.Scope),
			localRefName(c.Status.BoundAllocationRef),
		}
	})
}

// IPAllocations prints an allocation as the record it is: what was handed out,
// from where, and on whose behalf.
func IPAllocations() rest.TableConvertor {
	return New(v1alpha1.Resource("ipallocations"), []metav1.TableColumnDefinition{
		nameColumn,
		{Name: "Allocated", Type: "string", Description: "Range or address held."},
		{Name: "Pool", Type: "string", Description: "Pool the allocation was drawn from."},
		{Name: "Class", Type: "string", Description: "Class the allocation was made under."},
		{Name: "Purpose", Type: "string", Description: "Why the allocation exists."},
		{Name: "Phase", Type: "string", Description: "Current lifecycle phase."},
		ageColumn,
		{Name: "Claim", Type: "string", Priority: wide, Description: "Claim holding this allocation, if any."},
		{Name: "Reclaim", Type: "string", Priority: wide, Description: "What happens to the range when the claim goes."},
		{Name: "Scope", Type: "string", Priority: wide, Description: "References the allocation was made for."},
	}, func(obj runtime.Object, name, age string) []interface{} {
		a, ok := obj.(*ipam.IPAllocation)
		if !ok {
			return []interface{}{name, "", "", "", "", "", age, "", "", ""}
		}
		return []interface{}{
			name,
			orEmpty(allocated(a.Status.AllocatedCIDR, a.Status.Address)),
			orEmpty(a.Spec.PoolRef.Name),
			orEmpty(a.Spec.ClassName),
			orEmpty(string(a.Spec.Purpose)),
			orEmpty(string(a.Status.Phase)),
			age,
			localRefName(a.Spec.ClaimRef),
			orEmpty(string(a.Spec.ReclaimPolicy)),
			formatScope(a.Spec.Scope),
		}
	})
}

// IPClasses prints a class as the policy it is: the family it hands out, where
// it sits in the chain, and what size blocks it cuts.
func IPClasses() rest.TableConvertor {
	return New(v1alpha1.Resource("ipclasses"), []metav1.TableColumnDefinition{
		nameColumn,
		{Name: "Family", Type: "string", Description: "Address family this class hands out."},
		{Name: "Parent", Type: "string", Description: "Class this one draws from."},
		{Name: "Prefix", Type: "string", Description: "Default prefix length handed out."},
		{Name: "Pool Per", Type: "string", Description: "Scope roles a separate pool is provisioned for."},
		{Name: "Phase", Type: "string", Description: "Current lifecycle phase."},
		ageColumn,
		{Name: "Source", Type: "string", Priority: wide, Description: "Platform class this one references, if any."},
		{Name: "Strategy", Type: "string", Priority: wide, Description: "How a pool is chosen."},
		{Name: "Unique Within", Type: "string", Priority: wide, Description: "Scope roles an allocation must be unique within."},
	}, func(obj runtime.Object, name, age string) []interface{} {
		c, ok := obj.(*ipam.IPClass)
		if !ok {
			return []interface{}{name, "", "", "", "", "", age, "", "", ""}
		}
		prefix := ""
		if c.Spec.DefaultPrefixLength != 0 {
			prefix = fmt.Sprintf("/%d", c.Spec.DefaultPrefixLength)
		}
		source := ""
		if c.Spec.Source != nil {
			source = c.Spec.Source.Name
		}
		return []interface{}{
			name,
			orEmpty(string(c.Spec.IPFamily)),
			orEmpty(c.Spec.ParentClassName),
			orEmpty(prefix),
			orEmpty(strings.Join(c.Spec.PoolPer, ",")),
			orEmpty(string(c.Status.Phase)),
			age,
			orEmpty(source),
			orEmpty(string(c.Spec.Strategy)),
			orEmpty(strings.Join(c.Spec.UniqueWithin, ",")),
		}
	})
}

// allocated prefers the range over the single address: a claim for one address
// reports it in both fields, and the range is the more specific answer.
func allocated(cidr, address string) string {
	if cidr != "" {
		return cidr
	}
	return address
}

func localRefName(ref *ipam.LocalRef) string {
	if ref == nil {
		return "<none>"
	}
	return orEmpty(ref.Name)
}

// formatScope renders scope the way the milo-ipam plugin does, so a role=name
// pair reads the same whichever surface printed it.
func formatScope(scope map[string]ipam.ScopeRef) string {
	if len(scope) == 0 {
		return "<none>"
	}
	roles := make([]string, 0, len(scope))
	for role := range scope {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	cells := make([]string, 0, len(roles))
	for _, role := range roles {
		cells = append(cells, role+"="+scope[role].Name)
	}
	return strings.Join(cells, " ")
}

// poolClass names the class a pool serves. Exactly one of these is set on any
// given pool: an operator-authored pool lists the classes it offers itself to,
// and a cascade-provisioned pool names the class that carved it. Reading only
// the first would print "<none>" for every provisioned pool, which is most of
// them.
func poolClass(p *ipam.IPPool) string {
	if len(p.Spec.ClassNames) > 0 {
		return strings.Join(p.Spec.ClassNames, ",")
	}
	return localRefName(p.Spec.ClassRef)
}

// poolUtilization renders the allocated share of a pool.
//
// status.utilizationPercent is rounded to four decimal places, and an IPv6 pool
// is big enough that a real allocation rounds to zero: a /48 carved from a /32
// is 0.0000%. Whether the pool has been drawn on at all is therefore read from
// the capacity, which is exact, and the percentage is only used for how much.
func poolUtilization(p *ipam.IPPool) string {
	if !allocatedAny(p.Status.Capacity.Allocated) {
		return "0%"
	}
	if p.Status.UtilizationPercent < 0.05 {
		return "<0.1%"
	}
	return fmt.Sprintf("%.1f%%", p.Status.UtilizationPercent)
}

// allocatedAny reports whether a capacity count is a non-zero number. The counts
// are decimal strings because an IPv6 pool overflows every integer type.
func allocatedAny(s string) bool {
	n, ok := new(big.Int).SetString(s, 10)
	return ok && n.Sign() > 0
}

func orEmpty(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}
