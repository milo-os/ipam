package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// nonInteractive reports whether prompts must be auto-suppressed: stdin is not a
// terminal, or CI is set. In that state a yes/no prompt cannot be answered, so
// callers either proceed (low blast radius) or require --yes (high blast radius).
func (a *app) nonInteractive() bool {
	if _, ci := os.LookupEnv("CI"); ci {
		return true
	}
	f, ok := a.io.In.(*os.File)
	if !ok {
		return true
	}
	return !isTerminalFd(int(f.Fd()))
}

// confirmYesNo asks a yes/no question scaled to a low blast radius (a single
// claim or allocation release). Returns true to proceed. --yes bypasses; a
// non-interactive session proceeds, since the prompt cannot be answered and the
// action is recoverable.
func (a *app) confirmYesNo(prompt string) bool {
	if a.opts.assumeYes {
		return true
	}
	if a.nonInteractive() {
		return true
	}
	_, _ = fmt.Fprintf(a.io.ErrOut, "%s [y/N]: ", prompt)
	reader := bufio.NewReader(a.io.In)
	line, _ := reader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// confirmTyped is the high-blast-radius gate, used by pool release: the user
// must type the exact resource name. --yes bypasses it. A non-interactive
// session without --yes refuses, because nobody can type the name there. The
// friction is intentional for the most destructive action.
func (a *app) confirmTyped(name, prompt string) (bool, error) {
	if a.opts.assumeYes {
		return true, nil
	}
	if a.nonInteractive() {
		return false, newCLIError(exitAborted,
			"refusing to perform a destructive action non-interactively without confirmation").
			withFix("re-run with --yes to confirm releasing " + name + ".")
	}
	_, _ = fmt.Fprintln(a.io.ErrOut, prompt)
	_, _ = fmt.Fprintf(a.io.ErrOut, "Type the name %q to confirm: ", name)
	reader := bufio.NewReader(a.io.In)
	line, _ := reader.ReadString('\n')
	if strings.TrimSpace(line) != name {
		return false, newCLIError(exitAborted, "confirmation did not match; aborted")
	}
	return true, nil
}

// isTerminalFd is split out so confirm.go does not depend on output.go's writer
// shape.
func isTerminalFd(fd int) bool {
	return termIsTerminal(fd)
}
