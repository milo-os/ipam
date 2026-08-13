// Package registryerrors provides apierror helpers shared across the IPAM
// registry storages.
//
// The taxonomy itself lives in go.miloapis.com/ipam/pkg/ipamerrors, where
// clients can import it. This package is the storages' shorthand for it, plus
// the Postgres-level predicates that never reach a client.
package registryerrors

import (
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// NewInsufficientStorage returns the exhaustion refusal with the supplied
// message. It serialises to HTTP 507, which is how IPAM signals that it has no
// space left.
func NewInsufficientStorage(message string) *apierrors.StatusError {
	return ipamerrors.New(ipamerrors.ReasonExhausted, message)
}

// NewPoolExhausted returns the exhaustion refusal for a pool that has no room
// left, naming the pool in Status.Details.
//
// A claim names a class, not a pool, so the caller cannot know which pool ran
// out. Without the name they get "IPPool exhausted" and no way to tell which of
// the class's pools to widen. The name goes in Details rather than only in the
// message so a client can read it without parsing prose.
func NewPoolExhausted(poolName string) *apierrors.StatusError {
	return ipamerrors.NewPoolExhausted(poolName, fmt.Sprintf("IPPool %q is exhausted", poolName))
}

// HolderConstraint is the unique index on ipam_cidr_allocations.claim_key:
// the holder of an allocation row, a claim or a child pool. A create under a
// name that already holds an allocation is refused there, before it reaches
// the allocation's own identity.
const HolderConstraint = "ipam_cidr_allocations_claim_key_key"

// IsUniqueViolation reports whether err is a Postgres unique violation on one
// of the named constraints.
func IsUniqueViolation(err error, constraints ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		return false
	}
	return slices.Contains(constraints, pgErr.ConstraintName)
}

const pgUniqueViolation = "23505"
