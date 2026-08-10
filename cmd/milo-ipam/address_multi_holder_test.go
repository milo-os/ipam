package main

// The reverse lookup must report every holder of an address.
//
// Two root pools over one range hand the same address to unrelated claims
// (#87), and `address show` — the command reached for precisely to answer "who
// has this address" — returned the first exact match and stopped. It named one
// claim and gave no hint a second existed. Both allocations being /32, the
// tightest-match tiebreak chose between them arbitrarily, so the answer was not
// merely incomplete but unstable between runs.
//
// A diagnostic that hides the condition it would be used to diagnose is worse
// than no diagnostic: it turns an investigation into a confident wrong
// conclusion.

import (
	"strings"
	"testing"
)

func TestAddressShowReportsEveryHolder(t *testing.T) {
	// Two allocations, same address, different pools — exactly what overlapping
	// root pools produce.
	cs := newFakeClientset(
		newAllocation("alloc-wide", "ov-class-a", "ov-pool-wide", "10.171.0.1/32", "10.171.0.1"),
		newAllocation("alloc-narrow", "ov-class-b", "ov-pool-narrow", "10.171.0.1/32", "10.171.0.1"),
	)
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"10.171.0.1"}); err != nil {
		t.Fatalf("address show: %v", err)
	}

	warning := ta.err.String()
	if !strings.Contains(warning, "held by 2 allocations") {
		t.Errorf("no multiple-holder warning; the command concealed the collision:\n%s", warning)
	}
	// Both pools must be named — comparing them is the operator's next step and
	// the whole reason the warning exists.
	for _, want := range []string{"ov-pool-wide", "ov-pool-narrow", "alloc-wide", "alloc-narrow"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning does not name %q:\n%s", want, warning)
		}
	}
	// Diagnostics never touch stdout: -o json|yaml is a data contract.
	if strings.Contains(ta.out.String(), "WARNING") {
		t.Errorf("warning leaked onto stdout:\n%s", ta.out.String())
	}
}

// The ordinary case must stay quiet, or the warning becomes noise and gets
// ignored on the day it matters.
func TestAddressShowSaysNothingForASingleHolder(t *testing.T) {
	cs := newFakeClientset(
		newAllocation("alloc-only", "cls", "pool", "10.171.0.1/32", "10.171.0.1"),
	)
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"10.171.0.1"}); err != nil {
		t.Fatalf("address show: %v", err)
	}
	if strings.Contains(ta.err.String(), "WARNING") {
		t.Errorf("warned about a single holder:\n%s", ta.err.String())
	}
}

// `allocation show <address>` reaches the same reverse lookup and concealed the
// same thing. Fixing only the command named in the report would have left the
// other entry point wrong — the set of paths is what gets missed.
func TestAllocationShowByAddressAlsoReportsEveryHolder(t *testing.T) {
	cs := newFakeClientset(
		newAllocation("alloc-wide", "ov-class-a", "ov-pool-wide", "10.171.0.1/32", "10.171.0.1"),
		newAllocation("alloc-narrow", "ov-class-b", "ov-pool-narrow", "10.171.0.1/32", "10.171.0.1"),
	)
	ta := newTestApp(cs, nil)
	cmd := newAllocationShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"10.171.0.1"}); err != nil {
		t.Fatalf("allocation show: %v", err)
	}
	if !strings.Contains(ta.err.String(), "held by 2 allocations") {
		t.Errorf("allocation show concealed the collision:\n%s", ta.err.String())
	}
}

// An enclosing block is still a single answer: asking about a host inside a
// claimed /24 names its holder and does not warn.
func TestEnclosingBlockIsNotAMultipleHolder(t *testing.T) {
	cs := newFakeClientset(
		newAllocation("alloc-block", "cls", "pool", "10.171.0.0/24", ""),
	)
	ta := newTestApp(cs, nil)
	cmd := newAddressShowCommand(ta.app)
	if err := cmd.RunE(cmd, []string{"10.171.0.9"}); err != nil {
		t.Fatalf("address show: %v", err)
	}
	if strings.Contains(ta.err.String(), "WARNING") {
		t.Errorf("an enclosing block is one holder, not two:\n%s", ta.err.String())
	}
}
