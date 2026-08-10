package allocator

import (
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// The allocator straddles two representations of the same scope, and this file
// is the seam.
//
// Scopes arrive from the registry as internal types, because that is what a
// decoded request object carries and what internal/scope digests. They leave as
// v1alpha1 types, because ipam_objects stores the versioned wire form and a
// pool written any other way would not decode on read. The two structs are
// field-identical; keeping the conversion explicit and in one place is what
// stops a future field being added to one and silently dropped by the other.

func scopeToVersioned(in map[string]ipam.ScopeRef) map[string]ipamv1alpha1.ScopeRef {
	if in == nil {
		return nil
	}
	out := make(map[string]ipamv1alpha1.ScopeRef, len(in))
	for role, ref := range in {
		out[role] = ipamv1alpha1.ScopeRef{
			APIGroup: ref.APIGroup,
			Kind:     ref.Kind,
			Name:     ref.Name,
			UID:      ref.UID,
		}
	}
	return out
}

func scopeToInternal(in map[string]ipamv1alpha1.ScopeRef) map[string]ipam.ScopeRef {
	if in == nil {
		return nil
	}
	out := make(map[string]ipam.ScopeRef, len(in))
	for role, ref := range in {
		out[role] = ipam.ScopeRef{
			APIGroup: ref.APIGroup,
			Kind:     ref.Kind,
			Name:     ref.Name,
			UID:      ref.UID,
		}
	}
	return out
}
