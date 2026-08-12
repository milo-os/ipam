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

// A 507 names the class and scope the caller asked for, plus the pools backing
// the class. The caller never named a pool, so "which one ran out" is the fact
// the request itself cannot supply.
func TestExhaustionError(t *testing.T) {
	ce := exhaustionError("tenant-endpoint-ipv4", "location=us-west network=default",
		[]string{"us-west-v4  10.1.0.0/16  98% used"}, nil)
	if ce.code != exitExhausted {
		t.Fatalf("code = %d, want %d", ce.code, exitExhausted)
	}
	for _, want := range []string{"tenant-endpoint-ipv4", "location=us-west", "us-west-v4", "98% used"} {
		if !strings.Contains(ce.msg, want) {
			t.Errorf("msg missing %q: %q", want, ce.msg)
		}
	}
	if !strings.Contains(ce.fix, "class show tenant-endpoint-ipv4") {
		t.Errorf("fix missing remediation command: %q", ce.fix)
	}
}

// With no --class, the claim asks for the family default, so the message must
// not invent a class name nobody gave it.
func TestExhaustionErrorDefaultClass(t *testing.T) {
	ce := exhaustionError("", "—", nil, nil)
	if !strings.Contains(ce.msg, "default class") {
		t.Errorf("msg missing default-class wording: %q", ce.msg)
	}
	if strings.Contains(ce.msg, "in scope") {
		t.Errorf("an em-dash scope must not be rendered: %q", ce.msg)
	}
	if !strings.Contains(ce.fix, "class list") {
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
