package main

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statusErr builds a Kubernetes API error carrying a specific HTTP code.
func statusErr(code int32, reason metav1.StatusReason, msg string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    code,
		Reason:  reason,
		Message: msg,
	}}
}

func TestClassifyErrorExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"forbidden", statusErr(403, metav1.StatusReasonForbidden, "no access"), exitForbidden},
		{"notfound", statusErr(404, metav1.StatusReasonNotFound, "nope"), exitNotFound},
		{"conflict", statusErr(409, metav1.StatusReasonConflict, "clash"), exitConflict},
		{"invalid", statusErr(400, metav1.StatusReasonBadRequest, "bad"), exitInvalid},
		{"exhausted", statusErr(507, "InsufficientStorage", "full"), exitExhausted},
		{"connrefused", errors.New("dial tcp 1.2.3.4:443: connection refused"), exitUnavailable},
		{"generic", errors.New("something odd"), exitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce := classifyError(tc.err)
			if ce.code != tc.want {
				t.Fatalf("classifyError(%s).code = %d, want %d", tc.name, ce.code, tc.want)
			}
		})
	}
}

// classifyError's fix text is printed for every noun and every verb, so it must
// not name a remedy that only holds for one of them. The 403 fix used to explain
// pool sharing — printed verbatim on a denied claim — and to point at the active
// org/project, which selects a control plane on the datum transport and does
// nothing at all on a kubeconfig.
func TestClassifyErrorFixesAreTransportAndNounAgnostic(t *testing.T) {
	banned := []string{"--project", "--org", "org/project", "shared into this project"}
	cases := []struct {
		name string
		err  error
	}{
		{"forbidden", statusErr(403, metav1.StatusReasonForbidden, `ipclaims "p1" is forbidden`)},
		{"unavailable", errors.New("dial tcp 1.2.3.4:443: connect: connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fix := classifyError(tc.err).fix
			for _, b := range banned {
				if strings.Contains(fix, b) {
					t.Errorf("fix names %q, which is not a remedy on every transport:\n%s", b, fix)
				}
			}
			// Continuation lines align under "Fix:  "; without the padding the
			// block renders flush left and stops reading as one paragraph.
			for _, line := range strings.Split(fix, "\n")[1:] {
				if line != "" && !strings.HasPrefix(line, "       ") {
					t.Errorf("continuation line is not indented to the Fix: block:\n%q", line)
				}
			}
		})
	}
}

func TestHTTPStatusCode(t *testing.T) {
	if got := httpStatusCode(statusErr(507, "InsufficientStorage", "full")); got != 507 {
		t.Fatalf("httpStatusCode = %d, want 507", got)
	}
	if got := httpStatusCode(errors.New("plain")); got != 0 {
		t.Fatalf("httpStatusCode(plain) = %d, want 0", got)
	}
	if got := httpStatusCode(nil); got != 0 {
		t.Fatalf("httpStatusCode(nil) = %d, want 0", got)
	}
}

func TestExhaustionError(t *testing.T) {
	// The caller named a class and a scope, never a pool, so the message has to
	// work backwards to the pools that back the class.
	ce := exhaustionError("tenant-endpoint-ipv4", "location=us-central-1 network=default",
		[]string{"us-central-1-tenant  10.4.0.0/14  98% used"}, nil)
	if ce.code != exitExhausted {
		t.Fatalf("code = %d, want %d", ce.code, exitExhausted)
	}
	if !strings.Contains(ce.msg, "tenant-endpoint-ipv4") {
		t.Errorf("msg missing class: %q", ce.msg)
	}
	if !strings.Contains(ce.msg, "location=us-central-1") {
		t.Errorf("msg missing scope: %q", ce.msg)
	}
	if !strings.Contains(ce.msg, "us-central-1-tenant") || !strings.Contains(ce.msg, "98% used") {
		t.Errorf("msg missing the pools backing the class: %q", ce.msg)
	}
	if !strings.Contains(ce.fix, "class show tenant-endpoint-ipv4") {
		t.Errorf("fix missing remediation command: %q", ce.fix)
	}
}

func TestToCLIErrorUsage(t *testing.T) {
	cases := []string{
		"unknown command \"foo\" for \"ipam\"",
		"unknown flag: --bogus",
		"unknown shorthand flag: 'x' in -x",
		"accepts 1 arg(s), received 0",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			ce := toCLIError(errors.New(msg))
			if ce.code != exitUsage {
				t.Fatalf("toCLIError(%q).code = %d, want %d", msg, ce.code, exitUsage)
			}
		})
	}
}

func TestRenderExitSuccess(t *testing.T) {
	io := IOStreams{Out: &strings.Builder{}, ErrOut: &strings.Builder{}}
	if code := renderExit(io, nil); code != exitOK {
		t.Fatalf("renderExit(nil) = %d, want 0", code)
	}
}

func TestRenderExitPrintsFixAndCode(t *testing.T) {
	var errBuf strings.Builder
	io := IOStreams{Out: &strings.Builder{}, ErrOut: &errBuf}
	ce := exhaustionError("tenant-endpoint-ipv4", "network=default", nil, nil)
	code := renderExit(io, ce)
	if code != exitExhausted {
		t.Fatalf("code = %d, want %d", code, exitExhausted)
	}
	out := errBuf.String()
	for _, want := range []string{"Error:", "Fix:", "IPAM_POOL_EXHAUSTED"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
}
