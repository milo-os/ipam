package main

import (
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Exit codes are a stable contract: automation branches on them. 0 is success;
// each documented non-zero code names a distinct failure class. A bulk
// operation that partially fails must never exit 0.
const (
	exitOK          = 0 // success
	exitError       = 1 // generic / unexpected error
	exitUsage       = 2 // invalid flags or arguments (IPAM_USAGE)
	exitForbidden   = 3 // HTTP 403 RBAC denial (IPAM_FORBIDDEN)
	exitNotFound    = 4 // HTTP 404 / no matching pool (IPAM_NOT_FOUND)
	exitConflict    = 5 // HTTP 409 overlap / conflict (IPAM_CONFLICT)
	exitInvalid     = 6 // HTTP 400 validation error (IPAM_INVALID)
	exitExhausted   = 7 // HTTP 507 pool exhausted (IPAM_POOL_EXHAUSTED)
	exitUnavailable = 8 // transport / connection failure (IPAM_UNAVAILABLE)
	exitAborted     = 9 // user declined a confirmation (IPAM_ABORTED)
)

// exitCodeNames maps each exit code to its documented symbolic name, used in
// help text and --verbose diagnostics.
var exitCodeNames = map[int]string{
	exitOK:          "OK",
	exitError:       "IPAM_ERROR",
	exitUsage:       "IPAM_USAGE",
	exitForbidden:   "IPAM_FORBIDDEN",
	exitNotFound:    "IPAM_NOT_FOUND",
	exitConflict:    "IPAM_CONFLICT",
	exitInvalid:     "IPAM_INVALID",
	exitExhausted:   "IPAM_POOL_EXHAUSTED",
	exitUnavailable: "IPAM_UNAVAILABLE",
	exitAborted:     "IPAM_ABORTED",
}

// cliError carries a rendered, human-facing message plus a precise exit code.
// It is what RunE handlers return so that main() can both print a clean message
// (no Go stack trace) and exit with the contractual code.
type cliError struct {
	code int
	// msg is the primary "Error:" line.
	msg string
	// fix is an optional remediation block printed under "Fix:".
	fix string
	// cause is retained for --verbose/--debug rendering only.
	cause error
}

func (e *cliError) Error() string { return e.msg }
func (e *cliError) Unwrap() error { return e.cause }

func newCLIError(code int, msg string) *cliError {
	return &cliError{code: code, msg: msg}
}

func (e *cliError) withFix(fix string) *cliError {
	e.fix = fix
	return e
}

func (e *cliError) withCause(err error) *cliError {
	e.cause = err
	return e
}

// usageErrorf builds a usage (exit 2) error.
func usageErrorf(format string, a ...any) *cliError {
	return newCLIError(exitUsage, fmt.Sprintf(format, a...))
}

// unknownSubcommandError rejects an unrecognized subcommand of a parent command
// (e.g. `pool lst`) with a usage exit code and a nearest-match suggestion,
// matching the noun-level "did you mean" behavior cobra gives at the root. A
// typo'd scripted command must fail (exit 2), not silently print help and exit 0.
func unknownSubcommandError(cmd interface {
	CommandPath() string
	SuggestionsFor(string) []string
}, name string) *cliError {
	msg := fmt.Sprintf("unknown command %q for %q", name, cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n"
		for _, s := range suggestions {
			msg += "\t" + s + "\n"
		}
	}
	return newCLIError(exitUsage, strings.TrimRight(msg, "\n"))
}

// httpStatusCode extracts the HTTP status code from a Kubernetes API error,
// returning 0 when the error does not carry one (e.g. a transport failure).
func httpStatusCode(err error) int {
	if status, isStatus := asAPIStatus(err); isStatus {
		return int(status.Status().Code)
	}
	return 0
}

// asAPIStatus unwraps err to a Kubernetes APIStatus when possible.
func asAPIStatus(err error) (apierrors.APIStatus, bool) {
	if err == nil {
		return nil, false
	}
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return s, true
	}
	return nil, false
}

// classifyError maps an arbitrary error from an API call into a cliError with
// the right exit code. It is deliberately generic; callers that can add IPAM
// context (the pool, requested length, …) should prefer the richer helpers
// below and only fall back here for the unexpected.
func classifyError(err error) *cliError {
	if err == nil {
		return nil
	}
	if ce, isCLI := err.(*cliError); isCLI {
		return ce
	}
	code := httpStatusCode(err)
	switch code {
	case 403:
		// classifyError is reached from every verb on every noun, so the fix here
		// must hold for all of them. It used to explain that "a pool that exists
		// but isn't shared into this project reports forbidden, not found" —
		// printed verbatim on a denied claim, where it is about the wrong noun,
		// and asserting a per-project sharing mechanism for a cluster-scoped kind.
		// It also told the reader to "verify the active org/project", which reads
		// as a pointer at --org/--project; those select a control plane on the
		// datum transport and do nothing at all on a kubeconfig, so as a remedy
		// they are a coin flip.
		//
		// The server's own message names the user, the verb, the resource and the
		// scope, which is the whole of what the caller can act on. Say what the
		// code means and stop.
		return newCLIError(exitForbidden, fmt.Sprintf("not authorized: %s", apiMessage(err))).
			withFix("the message above names the identity, the verb and the scope that were\n" +
				"       refused. A denial is authorization, never absence: the object may well\n" +
				"       exist. Grant that identity the access, or ask someone who can.").
			withCause(err)
	case 404:
		return newCLIError(exitNotFound, apiMessage(err)).withCause(err)
	case 409:
		return newCLIError(exitConflict, fmt.Sprintf("conflict: %s", apiMessage(err))).withCause(err)
	case 400, 422:
		return newCLIError(exitInvalid, fmt.Sprintf("invalid request: %s", apiMessage(err))).withCause(err)
	case 507:
		// Generic 507 without pool context. Callers in the claim path render a
		// far richer message via exhaustionError.
		return newCLIError(exitExhausted, fmt.Sprintf("pool exhausted: %s", apiMessage(err))).withCause(err)
	}
	// No HTTP status: most likely a transport/connection problem or auth/setup.
	msg := err.Error()
	if isConnectionError(msg) {
		return newCLIError(exitUnavailable, fmt.Sprintf("cannot reach the IPAM API: %s", msg)).
			// Continuation lines carry seven spaces so they align under the "Fix:  "
			// label; without them the block renders flush left and stops reading as
			// one paragraph. classifyError is transport-blind, so both candidates
			// are offered as alternatives rather than one being asserted.
			withFix("check connectivity, then whichever applies: that you are logged in\n" +
				"       (datumctl login), or that --kubeconfig / KUBECONFIG points at a\n" +
				"       reachable cluster.").
			withCause(err)
	}
	return newCLIError(exitError, msg).withCause(err)
}

func isConnectionError(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{
		"connection refused", "no such host", "i/o timeout", "deadline exceeded",
		"tls", "dial tcp", "eof", "connect:", "unable to connect",
	} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// apiMessage returns the server-provided status message when available, else
// the raw error string.
func apiMessage(err error) string {
	if status, isStatus := asAPIStatus(err); isStatus {
		if m := status.Status().Message; m != "" {
			return m
		}
	}
	return err.Error()
}

// exhaustionError renders the signature IPAM failure (HTTP 507). Under the class
// model the caller never named a pool, so the message has to work backwards: it
// names the class and the scope that were asked for, then the pools that back
// that class and how full they are, because "which pool ran out" is the first
// thing the reader needs and the one thing the request did not say.
func exhaustionError(className, scope string, poolLines []string, cause error) *cliError {
	var b strings.Builder
	if className != "" {
		fmt.Fprintf(&b, "no address is available for class %q", className)
	} else {
		b.WriteString("no address is available for the default class")
	}
	if scope != "" && scope != "—" {
		fmt.Fprintf(&b, " in scope %s", scope)
	}
	b.WriteString(".")
	if len(poolLines) > 0 {
		b.WriteString("\n       Pools backing this class:")
		for _, line := range poolLines {
			b.WriteString("\n         " + line)
		}
	}

	fix := "free space in the pools above, or have an operator add capacity by offering\n" +
		"       another pool to the class:"
	if className != "" {
		fix += fmt.Sprintf("\n       datumctl ipam class show %s", className)
	} else {
		fix += "\n       datumctl ipam class list"
	}
	return newCLIError(exitExhausted, b.String()).withFix(fix).withCause(cause)
}

// noMatchingPoolError renders the "named pool not visible" failure with the
// pools that do exist nearby. Pools are an operator surface now — a claim never
// names one — so this is reached from `pool` commands, not the claim path.
func noMatchingPoolError(ref string, candidates []string) *cliError {
	ce := newCLIError(exitNotFound, fmt.Sprintf("pool %q is not visible in the active project.", ref))
	if len(candidates) > 0 {
		return ce.withFix("pools that are visible here:\n       " + strings.Join(candidates, "\n       "))
	}
	return ce.withFix("list visible pools:\n       datumctl ipam pool list")
}
