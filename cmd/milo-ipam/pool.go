package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	clientset "go.miloapis.com/ipam/pkg/client/clientset/versioned"
)

const apiVersion = "ipam.miloapis.com/v1alpha1"

func setPoolGVK(p *ipamv1alpha1.IPPool) {
	p.APIVersion = apiVersion
	p.Kind = "IPPool"
}

func newPoolCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Create, inspect, and release allocatable address space (IPPool)",
		Long:  "Pools are allocatable blocks of address space. Root pools declare a CIDR;\nchild pools carve a sub-prefix from a parent.",
		// Reject unknown subcommands with a suggestion + usage exit, rather than
		// silently printing help and exiting 0 (which would let a typo'd scripted
		// command appear to succeed).
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return c.Help()
			}
			return unknownSubcommandError(c, args[0])
		},
	}
	// cobra only initializes this on the root command during Execute; set it here
	// so SuggestionsFor on this parent yields nearest-match subcommands.
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(
		newPoolCreateCommand(a),
		newPoolListCommand(a),
		newPoolShowCommand(a),
		newPoolTreeCommand(a),
		newPoolReleaseCommand(a),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// pool create
// ---------------------------------------------------------------------------

func newPoolCreateCommand(a *app) *cobra.Command {
	var (
		cidr       string
		family     string
		parent     string
		minLen     int
		maxLen     int
		prefixLen  int
		strategy   string
		visibility string
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a pool",
		Args:  cobra.ExactArgs(1),
		Example: `  # A root /8 pool
  datumctl ipam pool create prod-backbone --cidr 10.0.0.0/8

  # A child pool carved from a parent, allowing /24–/28 leaf claims
  datumctl ipam pool create us-west --parent prod-backbone --prefix-length 16 \
    --min-length 24 --max-length 28`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			pool := &ipamv1alpha1.IPPool{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: ipamv1alpha1.IPPoolSpec{
					CIDR:          cidr,
					ParentPoolRef: refOrNil(parent),
				},
			}
			setPoolGVK(pool)

			// Family: explicit flag wins, else infer from the CIDR.
			if family != "" {
				fam, err := parseFamily(family)
				if err != nil {
					return err
				}
				pool.Spec.IPFamily = fam
			} else if cidr != "" {
				_, fam, err := validateCIDR(cidr)
				if err != nil {
					return err
				}
				pool.Spec.IPFamily = fam
			}
			if cidr == "" && parent == "" {
				return usageErrorf("a root pool requires --cidr; a child pool requires --parent")
			}
			if prefixLen > 0 {
				pool.Spec.PrefixLength = prefixLen
			}
			if minLen > 0 {
				pool.Spec.Allocation.MinPrefixLength = minLen
			}
			if maxLen > 0 {
				pool.Spec.Allocation.MaxPrefixLength = maxLen
			}
			if strategy != "" {
				s, err := parseStrategy(strategy)
				if err != nil {
					return err
				}
				pool.Spec.Allocation.Strategy = s
			}
			if visibility != "" {
				pool.Spec.Visibility = visibility
			}

			if dryRun {
				_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — no pool was created.")
				_, _ = fmt.Fprintf(a.io.ErrOut, "Would create pool %q (cidr %s, family %s).\n",
					name, orDash(cidr), orDash(string(pool.Spec.IPFamily)))
				_, err := a.renderMachine(pool, func() string { return "ippool/" + name })
				return err
			}

			cs, _, err := a.client()
			if err != nil {
				return err
			}
			created, err := cs.IpamV1alpha1().IPPools().Create(context.Background(), pool, metav1.CreateOptions{})
			if err != nil {
				return classifyError(err)
			}
			setPoolGVK(created)
			if done, err := a.renderMachine(created, func() string { return "ippool/" + created.Name }); done {
				return err
			}
			_, _ = fmt.Fprintf(a.io.Out, "%s Created pool %q\n", successPrefix(a.color), created.Name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&cidr, "cidr", "", "CIDR for a root pool (e.g. 10.0.0.0/8)")
	f.StringVar(&family, "family", "", "Address family: ipv4|ipv6 (inferred from --cidr when omitted)")
	f.StringVar(&parent, "parent", "", "Parent pool name (creates a child pool)")
	f.IntVar(&minLen, "min-length", 0, "Minimum allocatable prefix length for claims against this pool")
	f.IntVar(&maxLen, "max-length", 0, "Maximum allocatable prefix length for claims against this pool")
	f.IntVar(&prefixLen, "prefix-length", 0, "Prefix length to carve from the parent (child pools)")
	f.StringVar(&strategy, "strategy", "", "Allocation strategy: FirstFit|BestFit|LeastUtilized")
	f.StringVar(&visibility, "visibility", "", "Pool visibility: platform|consumer|shared")
	f.BoolVar(&dryRun, "dry-run", false, "Preview the pool without creating it")
	return cmd
}

// ---------------------------------------------------------------------------
// pool list
// ---------------------------------------------------------------------------

func newPoolListCommand(a *app) *cobra.Command {
	var selector string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List pools with utilization",
		Args:    cobra.NoArgs,
		Example: `  datumctl ipam pool list
  datumctl ipam pool list --selector environment=staging -o wide`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, _, err := a.client()
			if err != nil {
				return err
			}
			list, err := cs.IpamV1alpha1().IPPools().List(context.Background(), metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return classifyError(err)
			}
			for i := range list.Items {
				setPoolGVK(&list.Items[i])
			}
			list.APIVersion = apiVersion
			list.Kind = "IPPoolList"

			switch a.opts.output {
			case outputJSON:
				return encodeJSON(a.io.Out, list)
			case outputYAML:
				return encodeYAML(a.io.Out, list)
			case outputName:
				for i := range list.Items {
					_, _ = fmt.Fprintf(a.io.Out, "ippool/%s\n", list.Items[i].Name)
				}
				return nil
			}
			return a.renderPoolTable(list.Items)
		},
	}
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "Label selector to filter pools")
	return cmd
}

// childCounts tallies, per pool name, how many child pools and how many claims
// reference it. Best-effort; returns empty maps on listing failure.
func childCounts(cs clientset.Interface, namespace string, pools []ipamv1alpha1.IPPool) (children map[string]int, prefixes map[string]int) {
	children = map[string]int{}
	prefixes = map[string]int{}
	for i := range pools {
		if p := pools[i].Spec.ParentPoolRef; p != nil {
			children[p.Name]++
		}
	}
	claims, err := cs.IpamV1alpha1().IPClaims(namespace).List(context.Background(), metav1.ListOptions{})
	if err == nil {
		for i := range claims.Items {
			if ref := claims.Items[i].Status.PoolRef; ref != nil {
				prefixes[ref.Name]++
			}
		}
	}
	return children, prefixes
}

func (a *app) renderPoolTable(pools []ipamv1alpha1.IPPool) error {
	if len(pools) == 0 {
		if !a.opts.quiet {
			_, _ = fmt.Fprintln(a.io.ErrOut, "No pools found.")
		}
		return nil
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })

	wide := a.opts.output == outputWide
	headers := []string{"NAME", "CIDR", "FAMILY", "UTILIZATION", "AGE"}
	if wide {
		headers = []string{"NAME", "CIDR", "FAMILY", "UTILIZATION", "CHILDREN", "PREFIXES", "PHASE", "AGE"}
	}
	t := newTable(a.io.Out, headers)

	var children, prefixes map[string]int
	if wide {
		if cs, ns, err := a.client(); err == nil {
			children, prefixes = childCounts(cs, ns, pools)
		}
	}

	for i := range pools {
		p := &pools[i]
		cidr := p.Status.AllocatedCIDR
		if cidr == "" {
			cidr = p.Spec.CIDR
		}
		util := utilizationCell(poolUtilization(p), 10, a.color.enabled)
		family := orDash(string(poolFamily(p)))
		if wide {
			t.row(p.Name, orDash(cidr), family, util,
				itoa(children[p.Name]), itoa(prefixes[p.Name]), orDash(string(p.Status.Phase)),
				humanDuration(p.CreationTimestamp))
		} else {
			t.row(p.Name, orDash(cidr), family, util,
				humanDuration(p.CreationTimestamp))
		}
	}
	return t.flush()
}

// ---------------------------------------------------------------------------
// pool show
// ---------------------------------------------------------------------------

func newPoolShowCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "show <name>",
		Aliases: []string{"get", "describe"},
		Short:   "Show a pool in detail",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, _, err := a.client()
			if err != nil {
				return err
			}
			pool, err := cs.IpamV1alpha1().IPPools().Get(context.Background(), args[0], metav1.GetOptions{})
			if err != nil {
				return poolGetError(err, args[0])
			}
			setPoolGVK(pool)
			if done, err := a.renderMachine(pool, func() string { return "ippool/" + pool.Name }); done {
				return err
			}
			return a.renderPoolDetail(pool)
		},
	}
	return cmd
}

func (a *app) renderPoolDetail(p *ipamv1alpha1.IPPool) error {
	cidr := p.Status.AllocatedCIDR
	if cidr == "" {
		cidr = p.Spec.CIDR
	}
	pct := poolUtilization(p)
	t := newTable(a.io.Out, []string{"FIELD", "VALUE"})
	t.row("Name", p.Name)
	t.row("CIDR", orDash(cidr))
	t.row("Family", orDash(string(poolFamily(p))))
	t.row("Phase", orDash(string(p.Status.Phase)))
	if p.Spec.ParentPoolRef != nil {
		t.row("Parent", p.Spec.ParentPoolRef.Name)
	}
	if p.Spec.Visibility != "" {
		t.row("Visibility", p.Spec.Visibility)
	}
	label := utilizationLabel(pct)
	utilText := fmt.Sprintf("%.0f%%", pct)
	if label != "" {
		utilText += " (" + label + ")"
	}
	t.row("Utilization", utilText)
	// The int64 capacity counts are exact for IPv4 but saturate for IPv6
	// address spaces, so only show the raw totals when they're meaningful;
	// IPv6 pools are summarized by utilization and largest-free instead.
	if poolFamily(p) != ipamv1alpha1.IPv6 {
		t.row("Capacity", fmt.Sprintf("total=%d allocated=%d available=%d",
			p.Status.Capacity.Total, p.Status.Capacity.Allocated, p.Status.Capacity.Available))
	}
	if alloc := p.Spec.Allocation; alloc.MinPrefixLength != 0 || alloc.MaxPrefixLength != 0 || alloc.Strategy != "" {
		t.row("Allocation", fmt.Sprintf("min=/%d max=/%d strategy=%s",
			alloc.MinPrefixLength, alloc.MaxPrefixLength, orDash(string(alloc.Strategy))))
	}
	t.row("Age", humanDuration(p.CreationTimestamp))
	return t.flush()
}

// ---------------------------------------------------------------------------
// pool release
// ---------------------------------------------------------------------------

func newPoolReleaseCommand(a *app) *cobra.Command {
	var (
		cascade bool
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:     "release <name>",
		Aliases: []string{"rm", "delete"},
		Short:   "Release (delete) a pool",
		Args:    cobra.ExactArgs(1),
		Long: `Release a pool. This is the highest blast-radius action: by default it
confirms by requiring you to type the pool name. A pool that still has child
pools or active prefixes is protected — releasing it fails unless --cascade is
given (mirroring the server's deletion protection).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			pool, err := cs.IpamV1alpha1().IPPools().Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				return poolGetError(err, name)
			}

			// Blast radius: child pools + claims referencing this pool.
			allPools, _ := cs.IpamV1alpha1().IPPools().List(context.Background(), metav1.ListOptions{})
			var childPools []string
			if allPools != nil {
				for i := range allPools.Items {
					if r := allPools.Items[i].Spec.ParentPoolRef; r != nil && r.Name == name {
						childPools = append(childPools, allPools.Items[i].Name)
					}
				}
			}
			claims, _ := cs.IpamV1alpha1().IPClaims(ns).List(context.Background(), metav1.ListOptions{})
			var heldPrefixes []string
			if claims != nil {
				for i := range claims.Items {
					if r := claims.Items[i].Status.PoolRef; r != nil && r.Name == name {
						heldPrefixes = append(heldPrefixes, claims.Items[i].Name)
					}
				}
			}
			blast := len(childPools) + len(heldPrefixes)

			if dryRun {
				_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — nothing was released.")
				_, _ = fmt.Fprintf(a.io.ErrOut, "Would release pool %q.\n", name)
				if blast == 0 {
					_, _ = fmt.Fprintln(a.io.ErrOut, "Blast radius: none (no child pools or active prefixes).")
				} else {
					_, _ = fmt.Fprintf(a.io.ErrOut, "Blast radius: %d child pool(s), %d active prefix(es):\n", len(childPools), len(heldPrefixes))
					for _, c := range childPools {
						_, _ = fmt.Fprintf(a.io.ErrOut, "  child pool: %s\n", c)
					}
					for _, p := range heldPrefixes {
						_, _ = fmt.Fprintf(a.io.ErrOut, "  prefix:     %s\n", p)
					}
				}
				return nil
			}

			if blast > 0 && !cascade {
				return newCLIError(exitConflict, fmt.Sprintf(
					"pool %q still has %d child pool(s) and %d active prefix(es)", name, len(childPools), len(heldPrefixes))).
					withFix("release them first, or pass --cascade to release everything under this pool.")
			}

			prompt := fmt.Sprintf("About to release pool %q.", name)
			if blast > 0 {
				prompt += fmt.Sprintf(" This releases %d child pool(s) and %d prefix(es).", len(childPools), len(heldPrefixes))
			}
			ok, cErr := a.confirmTyped(name, prompt)
			if cErr != nil {
				return cErr
			}
			if !ok {
				return newCLIError(exitAborted, "aborted")
			}

			if cascade {
				for _, pn := range heldPrefixes {
					if delErr := cs.IpamV1alpha1().IPClaims(ns).Delete(context.Background(), pn, metav1.DeleteOptions{}); delErr != nil {
						return classifyError(delErr)
					}
				}
				for _, cp := range childPools {
					if delErr := cs.IpamV1alpha1().IPPools().Delete(context.Background(), cp, metav1.DeleteOptions{}); delErr != nil {
						return classifyError(delErr)
					}
				}
			}
			if err := cs.IpamV1alpha1().IPPools().Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
				return classifyError(err)
			}
			_ = pool
			if a.opts.output == outputName {
				_, _ = fmt.Fprintf(a.io.Out, "ippool/%s\n", name)
				return nil
			}
			_, _ = fmt.Fprintf(a.io.Out, "%s Released pool %q\n", successPrefix(a.color), name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&cascade, "cascade", false, "Also release child pools and active prefixes under this pool")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "List what would be released without releasing it")
	return cmd
}

// poolGetError adds IPAM context to a failed pool Get: a 404 becomes a clear
// "not visible in the active project" message rather than a bare Kubernetes
// NotFound.
func poolGetError(err error, name string) error {
	if apierrors.IsNotFound(err) {
		return newCLIError(exitNotFound, fmt.Sprintf("pool %q is not visible in the active project", name)).
			withFix("list visible pools:\n       datumctl ipam pool list").withCause(err)
	}
	return classifyError(err)
}
