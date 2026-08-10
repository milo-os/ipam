// Package access holds the authorization checks the IPAM registries run before
// allocating.
//
// There is exactly one, and it is the class-consumption gate in class.go.
//
// That is a change worth recording, because the package used to hold a second:
// a cross-project check that ran when a claim named a pool in another project's
// space. Under the class model a claim names a class and carries a scope, and
// resolving that to a pool is entirely the allocator's business — a claim cannot
// name another project's pool, so there is nothing left for that check to guard.
// It was deleted rather than left in place. Dead authorization code is worse
// than absent authorization code, because it reads as a control that is running.
//
// The boundary moved rather than disappearing. Consuming a class is now the
// privilege, because the class name is the only thing a caller supplies that
// selects space — see AuthorizeClassConsumption, and the note in
// internal/allocator/cascade.go on why pool discovery is restricted to
// platform-scoped pools, which is the other half of the same property.
package access
