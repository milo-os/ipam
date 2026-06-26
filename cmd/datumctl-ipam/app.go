package main

import (
	"fmt"

	clientset "go.miloapis.com/ipam/pkg/client/clientset/versioned"
)

// globalOptions holds the flags shared by every subcommand.
type globalOptions struct {
	kubeconfig string
	namespace  string
	output     string
	quiet      bool
	color      string // auto | always | never
	verbose    bool
	assumeYes  bool // --yes / --force

	// Overridable context (flags > env > config).
	org     string
	project string
}

// app is the runtime context threaded through commands. It resolves the
// transport and clientset lazily so that read-only, no-API paths (help,
// --plugin-manifest, completion) never touch credentials or the network.
type app struct {
	io   IOStreams
	opts *globalOptions

	// clientFactory builds the clientset and resolves the effective namespace.
	// It is a field so tests can inject a fake clientset without a real cluster.
	clientFactory func() (clientset.Interface, string, error)

	// Memoized result of clientFactory so a command that fetches the client more
	// than once does not rebuild config, re-fetch a token, or re-log diagnostics.
	clientResolved bool
	cachedCS       clientset.Interface
	cachedNS       string
	cachedErr      error

	color colorState
}

// newApp wires the default, real transport-backed client factory.
func newApp(io IOStreams, opts *globalOptions) *app {
	a := &app{io: io, opts: opts}
	a.clientFactory = a.defaultClientFactory
	return a
}

func (a *app) defaultClientFactory() (clientset.Interface, string, error) {
	env := readDatumEnv()
	mode := chooseMode(a.opts.kubeconfig, env)
	cfg, ns, err := restConfigFor(mode, a.opts.kubeconfig, env)
	if err != nil {
		return nil, "", err
	}
	// Flag/env override precedence for the namespace (project): explicit
	// --namespace wins, then --project, then the transport's default.
	if a.opts.namespace != "" {
		ns = a.opts.namespace
	} else if a.opts.project != "" {
		ns = a.opts.project
	}
	// --verbose: surface the resolved scope, transport, and API host on stderr,
	// and wrap the transport so every API call (method + path) is logged. stdout
	// (the json/yaml data contract) is untouched.
	if a.opts.verbose {
		a.vlogf("resolved scope: %s", a.scopeLine(ns))
		a.vlogf("transport: %s", mode)
		a.vlogf("API host: %s", cfg.Host)
		cfg.Wrap(verboseTransport(a.io.ErrOut))
	}
	cs, err := newClientset(cfg)
	if err != nil {
		return nil, "", err
	}
	return cs, ns, nil
}

// client resolves the clientset and effective namespace once, memoizing the
// result so repeat callers within a command don't rebuild config, re-fetch a
// token, or re-emit verbose diagnostics.
func (a *app) client() (clientset.Interface, string, error) {
	if !a.clientResolved {
		a.cachedCS, a.cachedNS, a.cachedErr = a.clientFactory()
		a.clientResolved = true
	}
	return a.cachedCS, a.cachedNS, a.cachedErr
}

// resolveColor computes the color decision for this invocation against the data
// stream, the requested output format, and the --color mode.
func (a *app) resolveColor() {
	a.color = resolveColor(a.opts.color, a.io.Out, a.opts.output)
}

// scopeLine returns a short "org / project" descriptor for success output and
// verbose diagnostics, falling back to the namespace when org/project are unset.
func (a *app) scopeLine(namespace string) string {
	switch {
	case a.opts.org != "" && a.opts.project != "":
		return fmt.Sprintf("%s / %s", a.opts.org, a.opts.project)
	case a.opts.project != "":
		return a.opts.project
	default:
		return namespace
	}
}

// vlogf writes a diagnostic line to stderr only when --verbose is set.
func (a *app) vlogf(format string, args ...any) {
	if a.opts.verbose {
		fmt.Fprintf(a.io.ErrOut, format+"\n", args...)
	}
}
