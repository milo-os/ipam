package ipclaim

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/access"
	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// installSpanRecorder swaps in a fully-sampling SDK TracerProvider backed by a
// SpanRecorder for the duration of one test. tracing.Tracer() reads the GLOBAL
// provider (see internal/tracing doc comment), so the test must set the global
// provider — not pass one in — and restore it afterwards.
func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sr),
	)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return sr
}

// tracingAllocator wraps fakeAllocator so AllocatePrefix emits a find_block
// span exactly as the real PostgresPrefixAllocator does (the real one lives in
// internal/allocator and is bypassed by the fake). This lets the test assert
// find_block appears and nests under claim.allocate without a live Postgres.
// An optional err lets the test drive the allocator-failure span path.
type tracingAllocator struct {
	*fakeAllocator
	err error
}

func (a *tracingAllocator) AllocatePrefix(ctx context.Context, tx pgx.Tx, poolKey string, prefixLen int, ipFamily string, claimKey string, ownerProject string) (string, error) {
	_, fbSpan := tracing.Tracer().Start(ctx, tracing.SpanFindBlock)
	fbSpan.SetAttributes(attribute.String(tracing.AttrStrategy, "first-fit"))
	if a.err != nil {
		if a.err == allocator.ErrPoolExhausted {
			fbSpan.SetAttributes(attribute.Bool(tracing.AttrExhausted, true))
		}
		fbSpan.SetStatus(codes.Error, a.err.Error())
		fbSpan.End()
		return "", a.err
	}
	fbSpan.End()
	return a.fakeAllocator.AllocatePrefix(ctx, tx, poolKey, prefixLen, ipFamily, claimKey, ownerProject)
}

// newTracingREST mirrors newTestREST but injects the find_block-emitting
// allocator and an optional cross-project pool checker.
func newTracingREST(allocErr error, checker access.PoolAccessChecker) (*AllocatingREST, *tracingAllocator) {
	r, fa, _ := newTestREST()
	ta := &tracingAllocator{fakeAllocator: fa, err: allocErr}
	r.allocator = ta
	r.poolChecker = checker
	return r, ta
}

// projectContext builds a request context carrying a project tenant identity so
// tracing.Scope resolves to "project" and the cross-project gate can fire.
func projectContext(project string) context.Context {
	ctx := genericapirequest.WithNamespace(context.Background(), "default")
	ctx = genericapirequest.WithUser(ctx, &user.DefaultInfo{
		Name: "tenant",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {project},
		},
	})
	return ctx
}

// findSpan returns the first recorded span with the given name, or nil.
func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// attr returns the value of the named attribute on a span, and whether it was
// present.
func attr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestCreate_EmitsAllocateSpanTree(t *testing.T) {
	sr := installSpanRecorder(t)
	r, _ := newTracingREST(nil, nil)

	_, err := r.Create(projectContext("proj-a"), newClaim(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	spans := sr.Ended()

	root := findSpan(spans, tracing.SpanClaimAllocate)
	if root == nil {
		t.Fatalf("missing %q span; got spans: %v", tracing.SpanClaimAllocate, spanNames(spans))
	}
	if v, ok := attr(root, tracing.AttrTenantScope); !ok || v.AsString() != "project" {
		t.Errorf("%s = %q (present=%v), want %q", tracing.AttrTenantScope, v.AsString(), ok, "project")
	}
	if v, ok := attr(root, tracing.AttrPoolName); !ok || v.AsString() != "us-east" {
		t.Errorf("%s = %q (present=%v), want %q", tracing.AttrPoolName, v.AsString(), ok, "us-east")
	}

	// tenant.resolve must exist and be parented to claim.allocate.
	resolve := findSpan(spans, tracing.SpanTenantResolve)
	if resolve == nil {
		t.Fatalf("missing %q span; got: %v", tracing.SpanTenantResolve, spanNames(spans))
	}
	if resolve.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("%s parent = %v, want claim.allocate %v",
			tracing.SpanTenantResolve, resolve.Parent().SpanID(), root.SpanContext().SpanID())
	}

	// find_block must appear and nest under claim.allocate.
	fb := findSpan(spans, tracing.SpanFindBlock)
	if fb == nil {
		t.Fatalf("missing %q span; got: %v", tracing.SpanFindBlock, spanNames(spans))
	}
	if fb.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("%s parent = %v, want claim.allocate %v",
			tracing.SpanFindBlock, fb.Parent().SpanID(), root.SpanContext().SpanID())
	}

	// Successful allocation: root span has no error status.
	if root.Status().Code == codes.Error {
		t.Errorf("root span unexpectedly Error: %q", root.Status().Description)
	}
}

func TestCreate_CrossProjectAuthorizeSpan(t *testing.T) {
	sr := installSpanRecorder(t)
	// A nil PoolAccessChecker makes the cross-project gate fail closed at the
	// first branch (reason=no_checker), recording decision=denied before any DB
	// read — so the assertion runs without a live Postgres behind the fakeTx.
	// A poolRef whose ProjectRef differs from the caller triggers isCrossProject.
	r, _ := newTracingREST(nil, nil)

	claim := newClaim()
	claim.Spec.PoolRef.ProjectRef = &ipam.LocalRef{Name: "proj-b"}

	_, _ = r.Create(projectContext("proj-a"), claim, nil, &metav1.CreateOptions{})

	spans := sr.Ended()
	authz := findSpan(spans, tracing.SpanAuthorizeCrossPrj)
	if authz == nil {
		t.Fatalf("missing %q span; got: %v", tracing.SpanAuthorizeCrossPrj, spanNames(spans))
	}
	if v, ok := attr(authz, tracing.AttrDecision); !ok || v.AsString() != "denied" {
		t.Errorf("%s = %q (present=%v), want %q", tracing.AttrDecision, v.AsString(), ok, "denied")
	}
	if v, ok := attr(authz, tracing.AttrReason); !ok || v.AsString() == "" {
		t.Errorf("%s missing/empty (present=%v), want a reason", tracing.AttrReason, ok)
	}
}

func TestCreate_ErrorPathSetsSpanStatusAndReason(t *testing.T) {
	cases := []struct {
		name       string
		allocErr   error
		wantReason string
	}{
		{"pool exhausted", allocator.ErrPoolExhausted, tracing.ReasonExhausted},
		{"pool not found", allocator.ErrPoolNotFound, tracing.ReasonPoolNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sr := installSpanRecorder(t)
			r, _ := newTracingREST(tc.allocErr, nil)

			_, err := r.Create(projectContext("proj-a"), newClaim(), nil, &metav1.CreateOptions{})
			if err == nil {
				t.Fatal("Create succeeded, want error")
			}

			root := findSpan(sr.Ended(), tracing.SpanClaimAllocate)
			if root == nil {
				t.Fatalf("missing %q span", tracing.SpanClaimAllocate)
			}
			if root.Status().Code != codes.Error {
				t.Errorf("root span status = %v, want Error", root.Status().Code)
			}
			if v, ok := attr(root, tracing.AttrErrorReason); !ok || v.AsString() != tc.wantReason {
				t.Errorf("%s = %q (present=%v), want %q", tracing.AttrErrorReason, v.AsString(), ok, tc.wantReason)
			}
		})
	}
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

// compile-time guard: tracingAllocator must satisfy the allocator interface the
// REST depends on.
var _ allocator.PrefixAllocator = (*tracingAllocator)(nil)
