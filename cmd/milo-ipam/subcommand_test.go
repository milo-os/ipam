package main

import (
	"bytes"
	"strings"
	"testing"
)

// execRoot runs the full command tree with the given args and returns the error
// Execute produced (nil on success).
func execRoot(args ...string) (string, string, error) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	root := newRootCommand(IOStreams{In: strings.NewReader(""), Out: out, ErrOut: errOut})
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestUnknownSubcommandSuggestsAndExits2(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		suggest string
	}{
		{"pool typo", []string{"pool", "lst"}, "list"},
		{"pool typo, another", []string{"pool", "shw"}, "show"},
		{"claim typo", []string{"claim", "creat"}, "create"},
		{"allocation typo", []string{"allocation", "releas"}, "release"},
		{"class typo", []string{"class", "lst"}, "list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := execRoot(tc.args...)
			if err == nil {
				t.Fatal("expected an error for an unknown subcommand")
			}
			ce := toCLIError(err)
			if ce.code != exitUsage {
				t.Fatalf("exit code = %d, want usage(%d)", ce.code, exitUsage)
			}
			if !strings.Contains(ce.msg, "unknown command") {
				t.Errorf("message missing 'unknown command': %q", ce.msg)
			}
			if !strings.Contains(ce.msg, tc.suggest) {
				t.Errorf("message missing suggestion %q: %q", tc.suggest, ce.msg)
			}
		})
	}
}

func TestBareParentShowsHelpNoError(t *testing.T) {
	for _, noun := range []string{"class", "claim", "allocation", "address", "pool"} {
		t.Run(noun, func(t *testing.T) {
			_, _, err := execRoot(noun)
			if err != nil {
				t.Fatalf("bare %q should print help and not error, got: %v", noun, err)
			}
		})
	}
}

// The whole consumer workflow lives under these nouns; a root that registers
// only some of them ships a plugin that cannot claim an address.
func TestRootRegistersEveryNoun(t *testing.T) {
	root := newRootCommand(IOStreams{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}})
	registered := map[string]bool{}
	for _, c := range root.Commands() {
		registered[c.Name()] = true
	}
	for _, want := range []string{"class", "claim", "allocation", "address", "pool", "version"} {
		if !registered[want] {
			t.Errorf("root does not register %q", want)
		}
	}
}

// `prefix` was this group's name before the class model. It stays an alias so
// existing muscle memory and scripts still land somewhere.
func TestPrefixAliasResolvesToClaim(t *testing.T) {
	root := newRootCommand(IOStreams{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}})
	cmd, _, err := root.Find([]string{"prefix", "list"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name() != "list" || cmd.Parent().Name() != "claim" {
		t.Fatalf("prefix list resolved to %s under %s, want list under claim", cmd.Name(), cmd.Parent().Name())
	}
}

func TestUnknownNounAtRootExits2(t *testing.T) {
	_, _, err := execRoot("poool")
	if err == nil {
		t.Fatal("expected error for unknown noun")
	}
	if toCLIError(err).code != exitUsage {
		t.Fatalf("exit code = %d, want usage(%d)", toCLIError(err).code, exitUsage)
	}
}

func TestUnknownSubcommandErrorHelper(t *testing.T) {
	// Direct unit test of the helper's suggestion rendering via a real parent.
	root := newRootCommand(IOStreams{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}})
	var pool interface {
		CommandPath() string
		SuggestionsFor(string) []string
	}
	for _, c := range root.Commands() {
		if c.Name() == "pool" {
			pool = c
		}
	}
	if pool == nil {
		t.Fatal("pool command not found")
	}
	ce := unknownSubcommandError(pool, "lst")
	if ce.code != exitUsage {
		t.Fatalf("code = %d, want %d", ce.code, exitUsage)
	}
	if !strings.Contains(ce.msg, "list") {
		t.Errorf("expected 'list' suggestion: %q", ce.msg)
	}
}
