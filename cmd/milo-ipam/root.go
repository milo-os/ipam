package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newRootCommand builds the full command tree. The plugin is dispatched by
// datumctl as `milo-ipam <args>`, presenting to the user as `datumctl ipam`.
func newRootCommand(io IOStreams) *cobra.Command {
	opts := &globalOptions{output: outputTable, color: "auto"}
	a := newApp(io, opts)

	root := &cobra.Command{
		Use:   "ipam",
		Short: "Manage IP address space (pools and prefixes) on Datum",
		Long: `Manage IP address space on Datum.

The ipam plugin presents the IPAM service as a small set of resource-oriented
commands. The two nouns that matter most:

  pool     an allocatable block of address space (IPPool)
  prefix   a sub-block claimed from a pool (IPClaim / IPAllocation)

Claiming a prefix returns the allocated CIDR synchronously:

  datumctl ipam prefix claim --pool prod-backbone --length 24

Output is a human table by default; -o json|yaml is a stable contract for
scripts (data on stdout, diagnostics on stderr). Exit codes are documented and
distinct per failure class (notably 7 = IPAM_POOL_EXHAUSTED).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if !isValidOutput(opts.output) {
				return usageErrorf("invalid -o %q: must be one of %v", opts.output, validOutputs())
			}
			switch opts.color {
			case "auto", "always", "never":
			default:
				return usageErrorf("invalid --color %q: must be auto, always, or never", opts.color)
			}
			a.resolveColor()
			// Service-entitlement preflight: every command that actually talks to
			// the IPAM API must run against a project entitled to IPAM. We gate
			// here (like the compute plugin) but skip the read-only/no-API
			// commands so `version`, `completion`, and `help` work without a live
			// API call. Prompts go to the command's stderr; input from its stdin.
			if skipEntitlementCheck(cmd) {
				return nil
			}
			return a.ensureEntitlement(cmd.InOrStdin(), cmd.ErrOrStderr())
		},
	}

	// Note: --plugin-manifest is handled by plugin.ServeManifest in main() before
	// cobra runs, so it is intentionally not registered as a cobra flag here.
	pf := root.PersistentFlags()
	pf.StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to a kubeconfig (forces kubeconfig transport; for dev/e2e clusters)")
	pf.StringVarP(&opts.namespace, "namespace", "n", "", "Namespace for namespaced resources (claims/allocations); defaults to the active context")
	pf.StringVarP(&opts.output, "output", "o", outputTable, "Output format: table|wide|json|yaml|name")
	pf.BoolVarP(&opts.quiet, "quiet", "q", false, "Suppress decorative output; print only essential data")
	pf.StringVar(&opts.color, "color", "auto", "Colorize output: auto|always|never")
	pf.BoolVarP(&opts.verbose, "verbose", "v", false, "Verbose diagnostics on stderr (resolved scope, API host, calls)")
	pf.BoolVar(&opts.assumeYes, "yes", false, "Assume yes; skip confirmation prompts")
	pf.BoolVar(&opts.assumeYes, "force", false, "Alias for --yes")
	pf.StringVar(&opts.org, "org", "", "Override the active organization for this invocation")
	pf.StringVar(&opts.project, "project", "", "Override the active project for this invocation")

	root.AddCommand(newPoolCommand(a))
	root.AddCommand(newVersionCommand(io))

	return root
}

// skipEntitlementCheck reports whether the matched command should bypass the
// service-entitlement preflight. These commands either don't touch the IPAM API
// (version, completion, help) or are help/diagnostic surfaces that must work
// without a live API call or a configured project.
func skipEntitlementCheck(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "version", "completion", "help", "__complete", "__completeNoDesc":
		return true
	}
	// `--help` on any command: cobra normally short-circuits before
	// PersistentPreRunE, but guard explicitly so help is never gated.
	if help, err := cmd.Flags().GetBool("help"); err == nil && help {
		return true
	}
	return false
}

func newVersionCommand(io IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintf(io.Out, "milo-ipam %s (IPAM API %s)\n", pluginVersion, ipamAPIGroupVersion)
			return nil
		},
	}
}
