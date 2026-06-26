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
		return newCLIError(exitForbidden, fmt.Sprintf("not authorized: %s", apiMessage(err))).
			withFix("verify the active org/project and your RBAC. A pool that exists but\n" +
				"isn't shared into this project reports forbidden, not found.").
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
			withFix("check connectivity and that you are logged in (datumctl login), or\n" +
				"that --kubeconfig / KUBECONFIG points at a reachable cluster.").
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

// exhaustionError renders the signature IPAM failure (HTTP 507) with the full
// remediation context the proposal calls for: the requested length, the
// largest block that is actually available, current utilization, and a concrete
// next command. poolName/family/requested describe the claim; cap (which may be
// zero-valued if the pool could not be re-fetched) supplies utilization.
func exhaustionError(poolName string, family string, requested int, util float64, largestFree int, cause error) *cliError {
	var b strings.Builder
	fmt.Fprintf(&b, "pool %q has no free /%d block (requested length %d).", poolName, requested, requested)
	if largestFree > 0 {
		fmt.Fprintf(&b, "\n       Largest available block is /%d", largestFree)
		if util > 0 {
			fmt.Fprintf(&b, "; utilization is %.0f%%.", util)
		} else {
			b.WriteString(".")
		}
	} else if util > 0 {
		fmt.Fprintf(&b, "\n       Utilization is %.0f%%.", util)
	}

	fix := "request a smaller prefix"
	if largestFree > requested {
		fix = fmt.Sprintf("request a smaller prefix (--length %d) or free space:", largestFree)
	} else {
		fix = "free space in the pool or pick a different pool:"
	}
	fix += fmt.Sprintf("\n       datumctl ipam prefix list --pool %s", poolName)

	return newCLIError(exitExhausted, b.String()).withFix(fix).withCause(cause)
}

// noMatchingPoolError renders the "selector matched nothing" / "named pool not
// visible" failure with the pools that do exist nearby.
func noMatchingPoolError(ref string, selector string, candidates []string) *cliError {
	var b strings.Builder
	if selector != "" {
		fmt.Fprintf(&b, "no pool matches selector %q in the active project.", selector)
	} else {
		fmt.Fprintf(&b, "pool %q is not visible in the active project.", ref)
	}
	ce := newCLIError(exitNotFound, b.String())
	if len(candidates) > 0 {
		ce.withFix("pools that are visible here:\n       " + strings.Join(candidates, "\n       "))
	} else {
		ce.withFix("list visible pools:\n       datumctl ipam pool list")
	}
	return ce
}
