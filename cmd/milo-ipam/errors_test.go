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
	ce := exhaustionError("env-pool", "IPv4", 22, 98, 24, nil)
	if ce.code != exitExhausted {
		t.Fatalf("code = %d, want %d", ce.code, exitExhausted)
	}
	if !strings.Contains(ce.msg, "no free /22") {
		t.Errorf("msg missing requested length: %q", ce.msg)
	}
	if !strings.Contains(ce.msg, "/24") {
		t.Errorf("msg missing largest available: %q", ce.msg)
	}
	if !strings.Contains(ce.msg, "98%") {
		t.Errorf("msg missing utilization: %q", ce.msg)
	}
	if !strings.Contains(ce.fix, "prefix list --pool env-pool") {
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
	ce := exhaustionError("p", "IPv4", 22, 98, 24, nil)
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
