package access

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/authorization/authorizer"

	"go.miloapis.com/ipam/internal/tenant"
)

// verbAuthorizer allows the verbs in allow, fails the verbs in fail, denies
// the rest, and records every question.
type verbAuthorizer struct {
	allow map[string]bool
	fail  map[string]error
	asked []authorizer.Attributes
}

func (a *verbAuthorizer) Authorize(_ context.Context, attrs authorizer.Attributes) (authorizer.Decision, string, error) {
	a.asked = append(a.asked, attrs)
	if err := a.fail[attrs.GetVerb()]; err != nil {
		return authorizer.DecisionNoOpinion, "", err
	}
	if a.allow[attrs.GetVerb()] {
		return authorizer.DecisionAllow, "", nil
	}
	return authorizer.DecisionDeny, "", nil
}

func (a *verbAuthorizer) ConditionsAwareAuthorize(ctx context.Context, attrs authorizer.Attributes) authorizer.ConditionsAwareDecision {
	return authorizer.ConditionsAwareDecisionFromParts(a.Authorize(ctx, attrs))
}

func (a *verbAuthorizer) EvaluateConditions(_ context.Context, _ authorizer.ConditionsAwareDecision, _ authorizer.ConditionsData) (authorizer.Decision, string, error) {
	return authorizer.DecisionDeny, "", authorizer.ErrorConditionEvaluationNotSupported
}

func allowing(verbs ...string) *verbAuthorizer {
	a := &verbAuthorizer{allow: map[string]bool{}}
	for _, v := range verbs {
		a.allow[v] = true
	}
	return a
}

// release-unleased is asked first, about the allocation itself, in the
// caller's own project. An allow settles it without a second question.
func TestCanReleaseUnleasedAsksAboutTheAllocationInTheCallersProject(t *testing.T) {
	authz := allowing(VerbReleaseUnleased)
	allowed, err := NewUnleasedReleaseChecker(authz).CanReleaseUnleased(callerCtx("tenant-a"), "default", "vpc-48")
	if err != nil || !allowed {
		t.Fatalf("CanReleaseUnleased = (%v, %v), want (true, nil)", allowed, err)
	}
	if len(authz.asked) != 1 {
		t.Fatalf("asked %d questions, want 1", len(authz.asked))
	}
	got := authz.asked[0]
	if got.GetVerb() != VerbReleaseUnleased || got.GetAPIGroup() != "ipam.miloapis.com" ||
		got.GetResource() != "ipallocations" || got.GetNamespace() != "default" ||
		got.GetName() != "vpc-48" || !got.IsResourceRequest() {
		t.Errorf("checked %s %s/%s %s/%q, want %s ipam.miloapis.com/ipallocations default/%q",
			got.GetVerb(), got.GetAPIGroup(), got.GetResource(), got.GetNamespace(), got.GetName(),
			VerbReleaseUnleased, "vpc-48")
	}
	if p := got.GetUser().GetExtra()[tenant.ExtraParentName]; len(p) != 1 || p[0] != "tenant-a" {
		t.Errorf("parent-name = %v, want [tenant-a]: the release is in the caller's own project", p)
	}
}

// The namespace controller holds deletecollection on every resource, and
// deleting a namespace frees what its allocations hold. That verb alone
// admits the release; neither verb denies it.
func TestCanReleaseUnleasedAdmitsDeleteCollection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		authz *verbAuthorizer
		want  bool
	}{
		{"deletecollection only", allowing(VerbDeleteCollection), true},
		{"plain delete only, as a consumer", allowing("delete"), false},
		{"nothing", allowing(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed, err := NewUnleasedReleaseChecker(tc.authz).CanReleaseUnleased(callerCtx("tenant-a"), "default", "vpc-48")
			if err != nil || allowed != tc.want {
				t.Fatalf("CanReleaseUnleased = (%v, %v), want (%v, nil)", allowed, err, tc.want)
			}
			if len(tc.authz.asked) != 2 {
				t.Fatalf("asked %d questions, want release-unleased then deletecollection", len(tc.authz.asked))
			}
			second := tc.authz.asked[1]
			if second.GetVerb() != VerbDeleteCollection || second.GetResource() != "ipallocations" ||
				second.GetNamespace() != "default" || second.GetName() != "" {
				t.Errorf("second question was %s ipallocations %s/%q, want deletecollection on the namespace's collection",
					second.GetVerb(), second.GetNamespace(), second.GetName())
			}
		})
	}
}

// An authorizer failure on one question does not hide an allow on the
// other; with no allow, it is reported.
func TestCanReleaseUnleasedReportsAFailureOnlyWithoutAnAllow(t *testing.T) {
	boom := errors.New("webhook down")
	authz := allowing(VerbDeleteCollection)
	authz.fail = map[string]error{VerbReleaseUnleased: boom}
	if allowed, err := NewUnleasedReleaseChecker(authz).CanReleaseUnleased(callerCtx("tenant-a"), "default", "x"); !allowed || err != nil {
		t.Errorf("failure then allow = (%v, %v), want (true, nil)", allowed, err)
	}

	authz = allowing()
	authz.fail = map[string]error{VerbReleaseUnleased: boom}
	if allowed, err := NewUnleasedReleaseChecker(authz).CanReleaseUnleased(callerCtx("tenant-a"), "default", "x"); allowed || !errors.Is(err, boom) {
		t.Errorf("failure then deny = (%v, %v), want (false, the failure)", allowed, err)
	}
}

// No user on the context is a denial, not an error.
func TestCanReleaseUnleasedWithNoUserIsDenied(t *testing.T) {
	authz := allowing(VerbReleaseUnleased, VerbDeleteCollection)
	allowed, err := NewUnleasedReleaseChecker(authz).CanReleaseUnleased(context.Background(), "default", "vpc-48")
	if err != nil || allowed {
		t.Errorf("CanReleaseUnleased with no user = (%v, %v), want (false, nil)", allowed, err)
	}
}

// countingChecker counts how often it is asked.
type countingChecker struct {
	allow bool
	err   error
	calls int
}

func (c *countingChecker) CanReleaseUnleased(context.Context, string, string) (bool, error) {
	c.calls++
	return c.allow, c.err
}

// The answer is asked for once and remembered, errors included; with no
// checker it is no.
func TestUnleasedReleaseAskAsksOnce(t *testing.T) {
	ctx := context.Background()
	for _, allow := range []bool{true, false} {
		c := &countingChecker{allow: allow}
		ask := NewUnleasedReleaseAsk(c, "default", "x")
		for range 3 {
			if got, err := ask.Allowed(ctx); got != allow || err != nil {
				t.Fatalf("Allowed = (%v, %v), want (%v, nil)", got, err, allow)
			}
		}
		if c.calls != 1 {
			t.Errorf("checker asked %d times, want 1", c.calls)
		}
	}

	c := &countingChecker{err: errors.New("down")}
	ask := NewUnleasedReleaseAsk(c, "default", "x")
	for range 2 {
		if _, err := ask.Allowed(ctx); !apierrors.IsInternalError(err) {
			t.Fatalf("Allowed error = %v, want a 500", err)
		}
	}
	if c.calls != 1 {
		t.Errorf("failing checker asked %d times, want 1", c.calls)
	}

	if got, err := NewUnleasedReleaseAsk(nil, "default", "x").Allowed(ctx); got || err != nil {
		t.Errorf("nil checker = (%v, %v), want (false, nil)", got, err)
	}
}
