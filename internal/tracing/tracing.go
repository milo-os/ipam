// Package tracing defines the tracer name, span names, and attribute keys
// shared across IPAM's allocation paths, so dashboards and the runbook rely on
// a single source of truth.
//
// IPAM's spans come from the global OpenTelemetry provider. The apiserver
// builds that provider from its tracing config but does not register it
// globally, so serve.go publishes it at startup. Until then Tracer() is a
// no-op, which keeps these calls safe and free when tracing is off.
package tracing

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope reported on every IPAM domain span.
// Kept stable so operators can filter Tempo/Grafana by instrumentation scope.
const TracerName = "go.miloapis.com/ipam"

// Tracer returns the IPAM domain tracer. It is a no-op until the global
// provider is wired at startup, so call sites never need a nil check.
func Tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// Span names. One constant per node in the span tree so dashboards and the
// runbook reference a single source of truth.
const (
	SpanClaimAllocate     = "ipam.claim.allocate"
	SpanClaimRelease      = "ipam.claim.release"
	SpanPoolChildAllocate = "ipam.pool.child_allocate"
	SpanTenantResolve     = "ipam.tenant.resolve"
	SpanAuthorizeCrossPrj = "ipam.pool.authorize_cross_project"
	// SpanAuthorizeClass covers the class-consumption gate — the only
	// authorization boundary a claim crosses, now that nobody names a pool.
	SpanAuthorizeClass = "ipam.class.authorize"
	SpanFindBlock      = "ipam.allocate.find_block"
)

// Attribute keys. Low-cardinality identity + decision signals only — never a
// per-claim name or a raw CIDR on a high-cardinality dimension (mirrors the
// metrics-label discipline in internal/metrics).
const (
	AttrTenantScope   = "ipam.tenant.scope"   // "platform" | "project"
	AttrTenantProject = "ipam.tenant.project" // project ID, "" when platform
	AttrTenantOrg     = "ipam.tenant.org"     // org ID when known, else ""
	AttrPoolName      = "ipam.pool.name"
	AttrClaimPrefix   = "ipam.claim.prefix_length"
	AttrClaimIPFamily = "ipam.claim.ip_family"
	// AttrClassName is the class a claim allocated under. It is the one
	// consumer-facing name in the model and is bounded by the operator-authored
	// catalog, so it is safe as a span attribute where a claim or pool name
	// would not be.
	AttrClassName = "ipam.class.name"
	AttrDryRun    = "ipam.dry_run"

	// tenant.resolve
	AttrHasParentExtras = "has_parent_extras"
	AttrScope           = "scope"
	AttrProject         = "project"

	// pool.authorize_cross_project
	AttrCrossProject = "cross_project"
	AttrDecision     = "decision" // "allowed" | "denied"
	AttrReason       = "reason"   // not_shared | sar_denied | no_checker | allowed

	// allocate.find_block
	AttrStrategy      = "strategy"
	AttrExistingCount = "existing_count"
	AttrResultCIDR    = "result_cidr"
	AttrExhausted     = "exhausted"

	// failure
	AttrErrorReason = "ipam.error.reason" // pool_not_found | exhausted | cross_project_denied | tx_error
)

// Scope returns the canonical scope attribute value for an identity.
func Scope(isPlatform bool) string {
	if isPlatform {
		return "platform"
	}
	return "project"
}

// Error-reason attribute values.
const (
	ReasonPoolNotFound = "pool_not_found"
	// ReasonBadRequest covers a claim rejected against its class: a size outside
	// the allowed range, a family disagreement, or a scope missing a role the
	// class requires.
	ReasonBadRequest = "bad_request"
	// ReasonNamespaceUnusable covers a claim refused because its namespace
	// cannot collect what would be bound into it — Terminating, or absent.
	// Distinct from a lookup that produced no answer, which admits the claim
	// and is not a refusal reason at all.
	ReasonNamespaceUnusable = "namespace_unusable"
	// ReasonClassDenied covers a claim refused at the class-consumption gate:
	// a container class, a platform-only class, or a failed "use" check.
	ReasonClassDenied        = "class_denied"
	ReasonExhausted          = "exhausted"
	ReasonCrossProjectDenied = "cross_project_denied"
	ReasonTxError            = "tx_error"
)
