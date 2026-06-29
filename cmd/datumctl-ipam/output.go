package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"
	"sigs.k8s.io/yaml"
)

// Output formats. The default human table is for a person at a terminal; json
// and yaml are the stable machine contract; wide adds columns; name emits bare
// identifiers for xargs/command substitution.
const (
	outputTable = "table"
	outputWide  = "wide"
	outputJSON  = "json"
	outputYAML  = "yaml"
	outputName  = "name"
)

func validOutputs() []string {
	return []string{outputTable, outputWide, outputJSON, outputYAML, outputName}
}

func isValidOutput(o string) bool {
	for _, v := range validOutputs() {
		if o == v {
			return true
		}
	}
	return false
}

// IOStreams bundles the process streams so commands and tests can be wired with
// buffers instead of os.Stdout/os.Stderr. Data goes to Out; all diagnostics,
// prompts, and progress go to ErrOut, keeping `-o json > file` clean.
type IOStreams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
}

func stdStreams() IOStreams {
	return IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
}

// ANSI color codes. Kept minimal and only applied when color is enabled.
const (
	colorReset  = "\x1b[0m"
	colorRed    = "\x1b[31m"
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
	colorBold   = "\x1b[1m"
)

// colorState is the resolved decision about whether to emit color, computed once
// from the --color flag, NO_COLOR, and TTY detection.
type colorState struct {
	enabled bool
}

func colorize(s, code string) string {
	return code + s + colorReset
}

// resolveColor decides whether to colorize output on the given writer. Precedence:
// --color=always|never wins; otherwise auto means "stdout is a TTY and NO_COLOR
// is unset". Machine output (json/yaml/name) is never colored regardless.
func resolveColor(mode string, out io.Writer, output string) colorState {
	if output == outputJSON || output == outputYAML || output == outputName {
		return colorState{enabled: false}
	}
	switch mode {
	case "always":
		return colorState{enabled: true}
	case "never":
		return colorState{enabled: false}
	}
	// auto
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return colorState{enabled: false}
	}
	return colorState{enabled: isTerminal(out)}
}

// termIsTerminal reports whether the given file descriptor is a terminal.
func termIsTerminal(fd int) bool {
	return term.IsTerminal(fd)
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// table is a small helper over text/tabwriter for aligned, column output.
type table struct {
	w       *tabwriter.Writer
	headers []string
}

func newTable(out io.Writer, headers []string) *table {
	t := &table{
		w:       tabwriter.NewWriter(out, 0, 2, 3, ' ', 0),
		headers: headers,
	}
	_, _ = fmt.Fprintln(t.w, strings.Join(headers, "\t"))
	return t
}

func (t *table) row(cells ...string) {
	_, _ = fmt.Fprintln(t.w, strings.Join(cells, "\t"))
}

func (t *table) flush() error {
	return t.w.Flush()
}

// encodeJSON writes obj as indented JSON to the data stream.
func encodeJSON(out io.Writer, obj any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(obj)
}

// encodeYAML writes obj as YAML to the data stream.
func encodeYAML(out io.Writer, obj any) error {
	b, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	return err
}

// successPrefix returns a "✓" mark, colored green when enabled. On non-TTY it is
// still a stable ASCII-friendly marker.
func successPrefix(c colorState) string {
	if c.enabled {
		return colorize("✓", colorGreen)
	}
	return "✓"
}
