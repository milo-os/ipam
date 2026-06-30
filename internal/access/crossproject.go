package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"go.miloapis.com/ipam/internal/tracing"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ErrCrossProjectDenied is returned by AuthorizeCrossProjectPrefix when
// the caller is not allowed to use a foreign-project prefix pool — whether
// because the pool does not exist, its class is not visibility=shared,
// the SAR returns deny, or no PoolAccessChecker is configured (fail-
// closed). It is a sentinel so claim Create handlers can mask the failure
// as "no pool matches" on the selector path and not leak fingerprintable
// existence/label information about another project's pools.
//
// Two failure modes are intentionally collapsed into this one sentinel:
// pool-doesn't-exist and pool-exists-but-not-authorised. Distinguishing
// them at the API surface is the same information leak — an attacker
// looking for which projects have which pool labels could trial a name
// and infer existence from the difference.
var ErrCrossProjectDenied = errors.New("ipam: cross-project pool not accessible")

// AuthorizeCrossProjectPrefix enforces the gates that a cross-project
// IPPool claim must clear before allocation:
//
//  1. A SAR-capable PoolAccessChecker must be configured. When checker
//     is nil (e.g. the apiserver was started without an authorizer, or
//     the authorizer is AlwaysAllow) cross-project claims fail closed —
//     the visibility=shared marker on the IPPool is intent-only and is
//     never sufficient on its own.
//  2. The source IPPool must declare spec.visibility=shared.
//  3. The caller must pass a "use" SubjectAccessReview against the pool.
//
// All lookups happen inside the supplied transaction so they share the
// same view of the database as the allocation that follows. On any
// denial path it returns ErrCrossProjectDenied; on infrastructure errors
// (DB read failure, SAR error) it returns the underlying error wrapped.
// Callers translate the sentinel into a 400 "no pool matches" for
// selector lookups and a 403 Forbidden for direct poolRef lookups.
func AuthorizeCrossProjectPrefix(ctx context.Context, tx pgx.Tx, poolKey string, checker PoolAccessChecker) error {
	ctx, span := tracing.Tracer().Start(ctx, tracing.SpanAuthorizeCrossPrj)
	defer span.End()
	// This gate is only reached for genuinely cross-project claims.
	span.SetAttributes(attribute.Bool(tracing.AttrCrossProject, true))

	// deny records a denial decision (decision=denied + the precise reason)
	// before returning the sentinel, so a trace shows *why* a cross-project
	// claim was refused without leaking it onto the API error.
	deny := func(reason string) error {
		span.SetAttributes(
			attribute.String(tracing.AttrDecision, "denied"),
			attribute.String(tracing.AttrReason, reason),
		)
		return ErrCrossProjectDenied
	}

	if checker == nil {
		return deny("no_checker")
	}

	pool, err := loadIPPool(ctx, tx, poolKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Collapsed into not_shared at the API surface; the trace keeps the
			// real reason for the operator.
			return deny("not_shared")
		}
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("load pool for access check: %w", err)
	}
	if pool.Spec.Visibility != "shared" {
		return deny("not_shared")
	}

	allowed, err := checker.CanUsePool(ctx, poolKey)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("authorize pool access: %w", err)
	}
	if !allowed {
		return deny("sar_denied")
	}
	span.SetAttributes(
		attribute.String(tracing.AttrDecision, "allowed"),
		attribute.String(tracing.AttrReason, "allowed"),
	)
	return nil
}

// loadIPPool decodes the IPPool object at poolKey from ipam_objects
// without acquiring FOR UPDATE — the SELECT runs inside the same
// transaction the allocator will reuse, so the row will be locked when
// AllocatePrefix fires its own SELECT FOR UPDATE on the same key.
func loadIPPool(ctx context.Context, tx pgx.Tx, poolKey string) (*ipamv1alpha1.IPPool, error) {
	var data []byte
	err := tx.QueryRow(ctx,
		`SELECT data FROM ipam_objects WHERE key = $1`,
		poolKey,
	).Scan(&data)
	if err != nil {
		return nil, fmt.Errorf("load pool object: %w", err)
	}
	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, fmt.Errorf("decode pool: %w", err)
	}
	return &pool, nil
}
