// Command datumctl-ipam is the IPAM plugin for datumctl. It presents the
// ipam.miloapis.com/v1alpha1 API as a small set of resource-oriented commands
// (pools and prefixes), reusing the user's datumctl identity/context in
// production and a standard kubeconfig for dev/e2e clusters.
package main

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func main() {
	io := stdStreams()
	root := newRootCommand(io)

	// Flag parse errors are usage errors (exit 2), not generic failures.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageErrorf("%v", err)
	})

	err := root.Execute()
	os.Exit(renderExit(io, err))
}

// renderExit prints a clean, IPAM-aware error (no Go stack trace) and returns
// the contractual exit code. Returns 0 on success.
func renderExit(io IOStreams, err error) int {
	if err == nil {
		return exitOK
	}

	ce := toCLIError(err)

	// Primary error line and optional remediation, all on stderr so machine
	// output on stdout stays clean.
	_, _ = io.ErrOut.Write([]byte("Error: " + ce.msg + "\n"))
	if ce.fix != "" {
		_, _ = io.ErrOut.Write([]byte("Fix:   " + ce.fix + "\n"))
	}
	// The symbolic exit-code name aids log readers; show it always so the
	// "pool full vs auth failed" distinction is visible in CI logs.
	if name := exitCodeNames[ce.code]; name != "" && ce.code != exitOK {
		_, _ = io.ErrOut.Write([]byte("exit status " + itoa(ce.code) + "   # " + name + "\n"))
	}
	if ce.cause != nil {
		// Stack-trace-equivalent detail only under --verbose/--debug, surfaced by
		// the cause being printed.
		if verboseEnabled() {
			_, _ = io.ErrOut.Write([]byte("cause: " + ce.cause.Error() + "\n"))
		}
	}
	return ce.code
}

// toCLIError normalizes any error into a *cliError. Cobra-origin command errors
// (unknown command/flag) are usage errors; everything else is classified by
// HTTP status or transport heuristics.
func toCLIError(err error) *cliError {
	if ce, isCLI := err.(*cliError); isCLI {
		return ce
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "unknown command") ||
		strings.HasPrefix(msg, "unknown flag") ||
		strings.HasPrefix(msg, "unknown shorthand flag") ||
		strings.Contains(msg, "requires") && strings.Contains(msg, "arg") ||
		strings.HasPrefix(msg, "accepts") {
		return newCLIError(exitUsage, msg)
	}
	return classifyError(err)
}

// verboseEnabled reports whether -v/--verbose or --debug appears in os.Args. We
// read os.Args directly because renderExit runs after cobra may have failed to
// parse flags, so the parsed flag value may be unavailable.
func verboseEnabled() bool {
	for _, a := range os.Args[1:] {
		if a == "-v" || a == "--verbose" || a == "--debug" {
			return true
		}
	}
	return false
}

// itoa is a tiny dependency-free int-to-string for exit codes (avoids importing
// strconv just for this).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
