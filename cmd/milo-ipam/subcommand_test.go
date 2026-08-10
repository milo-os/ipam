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
		{"claim typo", []string{"claim", "creat"}, "create"},
		{"class typo", []string{"class", "lst"}, "list"},
		{"allocation typo", []string{"allocation", "lst"}, "list"},
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
	for _, noun := range []string{"pool", "class", "claim", "allocation", "address"} {
		t.Run(noun, func(t *testing.T) {
			_, _, err := execRoot(noun)
			if err != nil {
				t.Fatalf("bare %q should print help and not error, got: %v", noun, err)
			}
		})
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

// A LIST against a namespace that cannot exist returns 200 and an empty list —
// the Kubernetes contract, not our deviation — so `-n "NOT A NAMESPACE"` used to
// print "No allocations found." and exit 0, indistinguishable from a tenant that
// genuinely holds nothing. Upstream's remedy is client-side and so is this one.
func TestInvalidNamespaceFlagIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		wantErr   bool
	}{
		{"spaces", "NOT A NAMESPACE", true},
		// The way this was actually hit: an unquoted shell expansion under zsh,
		// which does not word-split, so "-n ipam-cli-a" arrived as one argument
		// and the shorthand took " ipam-cli-a" as its value.
		{"leading space from a quoting slip", " ipam-cli-a", true},
		{"uppercase", "Default", true},
		{"trailing dash", "ns-", true},
		{"unset means the context default", "", false},
		{"ordinary name", "ipam-system", false},
		{"digits and dashes", "123-abc", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNamespaceFlag(tc.namespace)
			if tc.wantErr && err == nil {
				t.Fatalf("validateNamespaceFlag(%q) accepted a name that cannot exist", tc.namespace)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateNamespaceFlag(%q) = %v, want nil", tc.namespace, err)
			}
			if !tc.wantErr {
				return
			}
			ce, ok := err.(*cliError)
			if !ok {
				t.Fatalf("error is %T, want *cliError", err)
			}
			if ce.code != exitUsage {
				t.Errorf("exit code = %d, want %d", ce.code, exitUsage)
			}
		})
	}
}
