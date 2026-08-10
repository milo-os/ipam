package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"
)

// newRootCommand builds the full command tree. The plugin is dispatched by
// datumctl as `milo-ipam <args>`, presenting to the user as `datumctl ipam`.
func newRootCommand(io IOStreams) *cobra.Command {
	opts := &globalOptions{output: outputTable, color: "auto"}
	a := newApp(io, opts)

	root := &cobra.Command{
		Use:   "ipam",
		Short: "Manage IP address space on Datum",
		Long: `Manage IP address space on Datum.

The ipam plugin presents the IPAM service using the API's own nouns, so what you
read here and what you read in the API docs are the same vocabulary:

  class        a kind of address space you can claim from (IPClass)
  claim        a long-lived request for an address of a class (IPClaim)
  allocation   the record of an address handed out (IPAllocation)
  pool         an allocatable block of address space (IPPool)
  address      reverse lookup: who holds a given address

You claim by naming a class and the scope the address is for. You never name a
pool, a CIDR, or a location — those follow from the class and the scope, and the
server reports the pool it resolved in the claim's status:

  datumctl ipam claim create --class tenant-endpoint-ipv4 \
    --scope network=default --scope location=us-central-1

The allocated address comes back in that same call.

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
			if err := validateNamespaceFlag(opts.namespace); err != nil {
				return err
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

	root.AddCommand(newClassCommand(a))
	root.AddCommand(newClaimCommand(a))
	root.AddCommand(newAllocationCommand(a))
	root.AddCommand(newAddressCommand(a))
	root.AddCommand(newPoolCommand(a))
	root.AddCommand(newVersionCommand(io))

	return root
}

// validateNamespaceFlag rejects a -n value that cannot name a namespace.
//
// A LIST against a namespace that does not exist returns 200 and an empty list.
// That is the Kubernetes contract, not a deviation of ours — a stock
// kube-apiserver does the same, and the 404 you get from `kubectl get pod -n
// nope` comes from kubectl checking the namespace itself and rewriting the
// message, not from the server. So the confusion is real and the fix is
// client-side, which is where upstream puts it too.
//
// This covers the syntactic half only: `-n "NOT A NAMESPACE"` currently prints
// "No allocations found." and exits 0, indistinguishable from a tenant that
// genuinely holds nothing, and it is the half a shell-quoting slip produces.
// The other half — a well-formed name for a namespace that does not exist —
// needs a namespace GET, which means a core/v1 client this plugin does not
// build and a round trip on every empty list. Worth doing, but only once
// someone can verify it against the datum transport as well as a kubeconfig;
// asserting it works there without having run it is how the remedy this CLI
// already had to fix got written.
func validateNamespaceFlag(namespace string) error {
	if namespace == "" {
		return nil
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return usageErrorf("invalid -n %q: %s.\n"+
			"       a namespace that cannot exist lists empty rather than failing, so this\n"+
			"       is rejected here — otherwise it reads as a tenant holding nothing",
			namespace, errs[0])
	}
	return nil
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
