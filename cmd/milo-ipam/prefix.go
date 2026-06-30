package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	clientset "go.miloapis.com/ipam/pkg/client/clientset/versioned"
)

func setClaimGVK(c *ipamv1alpha1.IPClaim) {
	c.APIVersion = apiVersion
	c.Kind = "IPClaim"
}

func newPrefixCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prefix",
		Short: "Claim, inspect, and release sub-blocks of address space (IPClaim)",
		Long: `A prefix is a sub-block claimed from a pool. Claiming returns the allocated
CIDR synchronously. Under the hood a prefix is an IPClaim with a system-created
IPAllocation; -o yaml shows the real resource.`,
		// Reject unknown subcommands with a suggestion + usage exit, rather than
		// silently printing help and exiting 0.
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return c.Help()
			}
			return unknownSubcommandError(c, args[0])
		},
	}
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(
		newPrefixClaimCommand(a),
		newPrefixListCommand(a),
		newPrefixShowCommand(a),
		newPrefixReleaseCommand(a),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// prefix claim
// ---------------------------------------------------------------------------

type claimOptions struct {
	pool          string
	length        int
	cidr          string
	family        string
	name          string
	selector      string
	childPool     string
	strategy      string
	reclaimPolicy string
	dryRun        bool
}

func newPrefixClaimCommand(a *app) *cobra.Command {
	o := &claimOptions{}
	cmd := &cobra.Command{
		Use:   "claim",
		Short: "Claim a prefix from a pool and get the CIDR back synchronously",
		Long: `Claim a prefix from a pool. The allocated CIDR is returned in the same call.

Allocation is not idempotent: each claim consumes space. Pass a stable --name to
make retries safe — a retried claim with the same name returns the existing
allocation instead of consuming a second block.`,
		Example: `  # Claim a /24 by size
  datumctl ipam prefix claim --pool prod-backbone --length 24

  # Idempotent claim (safe to retry)
  datumctl ipam prefix claim --pool prod-backbone --length 24 --name app-net-3

  # Preview without consuming space
  datumctl ipam prefix claim --pool prod-backbone --length 14 --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrefixClaim(a, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.pool, "pool", "", "Pool to claim from (by name)")
	f.IntVar(&o.length, "length", 0, "Requested prefix length in bits (e.g. 24)")
	f.StringVar(&o.cidr, "cidr", "", "Request a specific block; sets length and family from the CIDR")
	f.StringVar(&o.family, "family", "", "Address family: ipv4|ipv6 (inferred from the pool or --cidr)")
	f.StringVar(&o.name, "name", "", "Stable claim name; reusing it makes retries idempotent")
	f.StringVarP(&o.selector, "selector", "l", "", "Select the pool by label instead of --pool")
	f.StringVar(&o.childPool, "child-pool", "", "Also stand up a child pool over the claimed block (childPrefixTemplate)")
	f.StringVar(&o.strategy, "strategy", "", "Allocation strategy override: FirstFit|BestFit|LeastUtilized")
	f.StringVar(&o.reclaimPolicy, "reclaim-policy", "", "Reclaim policy: Delete|Retain")
	f.BoolVar(&o.dryRun, "dry-run", false, "Preview the claim and projected utilization without consuming space")
	return cmd
}

func runPrefixClaim(a *app, o *claimOptions) error {
	if o.pool == "" && o.selector == "" {
		return usageErrorf("a claim needs a pool: pass --pool <name> or --selector <labels>")
	}
	if o.pool != "" && o.selector != "" {
		return usageErrorf("--pool and --selector are mutually exclusive")
	}
	if o.childPool != "" {
		// The current IPAM API (IPClaimSpec) does not expose childPrefixTemplate.
		// Fail loudly rather than silently dropping the user's intent.
		return usageErrorf("--child-pool is not supported by the current IPAM API on this cluster")
	}

	var family ipamv1alpha1.IPFamily
	if o.family != "" {
		fam, err := parseFamily(o.family)
		if err != nil {
			return err
		}
		family = fam
	}

	// --cidr sets length and family. The API allocates by length (it has no
	// "pin this exact block" field), so --cidr is a convenience for length+family.
	if o.cidr != "" {
		p, fam, err := validateCIDR(o.cidr)
		if err != nil {
			return err
		}
		o.length = p.Bits()
		if family == "" {
			family = fam
		}
		a.vlogf("note: --cidr sets length /%d and family %s; the server allocates by length", o.length, fam)
	}

	cs, ns, err := a.client()
	if err != nil {
		return err
	}

	// Fetch the named pool up front: it gives us the family default, the
	// before-utilization for the success line, and existence/visibility checking.
	var pool *ipamv1alpha1.IPPool
	if o.pool != "" {
		p, gErr := cs.IpamV1alpha1().IPPools().Get(context.Background(), o.pool, metav1.GetOptions{})
		if gErr != nil {
			if apierrors.IsNotFound(gErr) {
				return noMatchingPoolError(o.pool, "", visiblePoolNames(cs))
			}
			return classifyError(gErr)
		}
		pool = p
		if family == "" {
			family = p.Spec.IPFamily
		}
	}
	if family == "" {
		if o.selector != "" {
			return usageErrorf("could not infer address family from a selector; pass --family ipv4|ipv6")
		}
		family = ipamv1alpha1.IPv4
	}
	if o.length <= 0 {
		return usageErrorf("a claim needs a size: pass --length <n> or --cidr <cidr>")
	}
	if o.length > familyBits(family) {
		return usageErrorf("--length /%d is out of range for %s (max /%d)", o.length, family, familyBits(family))
	}

	// Idempotency: a named claim that already exists is returned as-is.
	if o.name != "" {
		existing, gErr := cs.IpamV1alpha1().IPClaims(ns).Get(context.Background(), o.name, metav1.GetOptions{})
		if gErr == nil {
			a.vlogf("claim %q already exists; returning the existing allocation (idempotent)", o.name)
			return a.renderClaimResult(existing, pool, cs, true)
		}
		if !apierrors.IsNotFound(gErr) {
			return classifyError(gErr)
		}
	}

	claim := buildClaim(o, ns, family)

	if o.dryRun {
		// Server-side dry-run: the apiserver computes the real next CIDR inside
		// the allocation transaction and rolls back, persisting nothing and
		// consuming no capacity. This lets the preview show the exact block.
		dryClaim, err := cs.IpamV1alpha1().IPClaims(ns).Create(context.Background(), claim,
			metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err != nil {
			return a.claimCreateError(err, o, family, cs)
		}
		return a.renderClaimDryRun(o, pool, family, dryClaim)
	}

	created, cErr := cs.IpamV1alpha1().IPClaims(ns).Create(context.Background(), claim, metav1.CreateOptions{})
	if cErr != nil {
		return a.claimCreateError(cErr, o, family, cs)
	}
	return a.renderClaimResult(created, pool, cs, false)
}

func buildClaim(o *claimOptions, ns string, family ipamv1alpha1.IPFamily) *ipamv1alpha1.IPClaim {
	claim := &ipamv1alpha1.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns},
		Spec: ipamv1alpha1.IPClaimSpec{
			IPFamily:     family,
			PrefixLength: o.length,
		},
	}
	// A stable --name makes retries idempotent. When omitted, synthesize a unique
	// client-side name: the IPAM aggregated apiserver does not implement
	// server-side metadata.generateName (a generateName-only claim is rejected
	// with "metadata.name was not generated").
	if o.name != "" {
		claim.Name = o.name
	} else {
		claim.Name = generateResourceName("prefix")
	}
	if o.pool != "" {
		claim.Spec.PoolRef = &ipamv1alpha1.NamespacedRef{Name: o.pool}
	}
	if o.selector != "" {
		claim.Spec.PoolSelector = &ipamv1alpha1.PoolSelector{
			LabelSelector: parseLabelSelector(o.selector),
		}
	}
	if o.reclaimPolicy != "" {
		claim.Spec.ReclaimPolicy = ipamv1alpha1.ReclaimPolicy(o.reclaimPolicy)
	}
	setClaimGVK(claim)
	return claim
}

// renderClaimResult prints the success output for a created or pre-existing
// claim. before/after utilization come from the pool status when available.
func (a *app) renderClaimResult(claim *ipamv1alpha1.IPClaim, poolBefore *ipamv1alpha1.IPPool, cs clientset.Interface, idempotent bool) error {
	setClaimGVK(claim)
	if done, err := a.renderMachine(claim, func() string { return "ipclaim/" + claim.Name }); done {
		return err
	}
	cidr := claim.Status.AllocatedCIDR
	if a.opts.quiet {
		// Script-friendly: just the CIDR (the one fact the caller came for).
		_, _ = fmt.Fprintln(a.io.Out, cidr)
		return nil
	}

	poolName := "—"
	if claim.Spec.PoolRef != nil {
		poolName = claim.Spec.PoolRef.Name
	}

	utilNote := ""
	if poolBefore != nil {
		before := utilizationPercent(poolBefore.Status.Capacity)
		after := before
		if poolAfter, err := cs.IpamV1alpha1().IPPools().Get(context.Background(), poolBefore.Name, metav1.GetOptions{}); err == nil {
			after = utilizationPercent(poolAfter.Status.Capacity)
		}
		utilNote = fmt.Sprintf("  (utilization %.0f%% → %.0f%%)", before, after)
	}

	verb := "Claimed"
	if idempotent {
		verb = "Reused existing claim for"
	}
	_, _ = fmt.Fprintf(a.io.Out, "%s %s %s from pool %q%s\n", successPrefix(a.color), verb, orDash(cidr), poolName, utilNote)
	_, _ = fmt.Fprintf(a.io.Out, "  prefix:      %s\n", claim.Name)
	if claim.Status.BoundAllocationRef != nil {
		_, _ = fmt.Fprintf(a.io.Out, "  allocation:  %s\n", claim.Status.BoundAllocationRef.Name)
	}
	if poolBefore != nil {
		poolCIDR := poolBefore.Status.AllocatedCIDR
		if poolCIDR == "" {
			poolCIDR = poolBefore.Spec.CIDR
		}
		_, _ = fmt.Fprintf(a.io.Out, "  pool:        %s (%s, %s)\n", poolName, orDash(poolCIDR), orDash(string(claim.Spec.IPFamily)))
	}
	_, _ = fmt.Fprintf(a.io.Out, "  org/project: %s\n", a.scopeLine(claim.Namespace))
	return nil
}

func (a *app) renderClaimDryRun(o *claimOptions, pool *ipamv1alpha1.IPPool, family ipamv1alpha1.IPFamily, dryClaim *ipamv1alpha1.IPClaim) error {
	// Machine formats emit the would-be claim object (carrying the computed CIDR).
	setClaimGVK(dryClaim)
	if done, err := a.renderMachine(dryClaim, func() string { return "ipclaim/" + dryClaim.Name }); done {
		return err
	}
	_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — no allocation was made.")
	// The server (honoring DryRun) returns the exact block it would allocate.
	if cidr := dryClaim.Status.AllocatedCIDR; cidr != "" {
		_, _ = fmt.Fprintf(a.io.ErrOut, "Would claim:   %s from pool %q\n", cidr, poolDisplay(o))
	} else {
		_, _ = fmt.Fprintf(a.io.ErrOut, "Would claim:   a /%d from pool %q\n", o.length, poolDisplay(o))
	}
	if pool != nil {
		before, after := projectedUtilization(pool.Status.Capacity, family, o.length)
		freeAfter := largestFreePrefix(family, pool.Status.Capacity.Available-(int64(1)<<uint(familyBits(family)-o.length)))
		freeStr := "—"
		if freeAfter > 0 {
			freeStr = fmt.Sprintf("/%d", freeAfter)
		}
		_, _ = fmt.Fprintf(a.io.ErrOut, "Pool after:    utilization %.0f%% → %.0f%%, largest free block %s\n", before, after, freeStr)
	}
	return nil
}

func poolDisplay(o *claimOptions) string {
	if o.pool != "" {
		return o.pool
	}
	return "(selector " + o.selector + ")"
}

// claimCreateError turns a failed claim Create into an IPAM-aware error. The
// signature case is 507 (exhaustion), which is re-decorated with the pool's
// utilization and largest free block.
func (a *app) claimCreateError(err error, o *claimOptions, family ipamv1alpha1.IPFamily, cs clientset.Interface) error {
	switch httpStatusCode(err) {
	case 507:
		var util float64
		largest := 0
		if o.pool != "" {
			if p, gErr := cs.IpamV1alpha1().IPPools().Get(context.Background(), o.pool, metav1.GetOptions{}); gErr == nil {
				util = utilizationPercent(p.Status.Capacity)
				largest = largestFreePrefix(family, p.Status.Capacity.Available)
			}
		}
		return exhaustionError(poolDisplay(o), string(family), o.length, util, largest, err)
	case 404:
		return noMatchingPoolError(o.pool, o.selector, visiblePoolNames(cs))
	case 409:
		return newCLIError(exitConflict, fmt.Sprintf("conflict: %s", apiMessage(err))).
			withFix("a claim with that name or block already exists; pick a different --name.").withCause(err)
	}
	return classifyError(err)
}

// ---------------------------------------------------------------------------
// prefix list
// ---------------------------------------------------------------------------

func newPrefixListCommand(a *app) *cobra.Command {
	var pool string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List claimed prefixes",
		Args:    cobra.NoArgs,
		Example: `  datumctl ipam prefix list
  datumctl ipam prefix list --pool prod-backbone -o wide`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			list, err := cs.IpamV1alpha1().IPClaims(ns).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return classifyError(err)
			}
			items := list.Items
			if pool != "" {
				filtered := items[:0:0]
				for i := range items {
					if r := items[i].Spec.PoolRef; r != nil && r.Name == pool {
						filtered = append(filtered, items[i])
					}
				}
				items = filtered
				list.Items = items
			}
			for i := range list.Items {
				setClaimGVK(&list.Items[i])
			}
			list.APIVersion = apiVersion
			list.Kind = "IPClaimList"

			switch a.opts.output {
			case outputJSON:
				return encodeJSON(a.io.Out, list)
			case outputYAML:
				return encodeYAML(a.io.Out, list)
			case outputName:
				for i := range items {
					_, _ = fmt.Fprintf(a.io.Out, "ipclaim/%s\n", items[i].Name)
				}
				return nil
			}
			return a.renderPrefixTable(items)
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "Only show prefixes claimed from this pool")
	return cmd
}

func (a *app) renderPrefixTable(claims []ipamv1alpha1.IPClaim) error {
	if len(claims) == 0 {
		if !a.opts.quiet {
			_, _ = fmt.Fprintln(a.io.ErrOut, "No prefixes found.")
		}
		return nil
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Name < claims[j].Name })

	wide := a.opts.output == outputWide
	headers := []string{"NAME", "CIDR", "POOL", "FAMILY", "LENGTH", "PHASE", "AGE"}
	if wide {
		headers = []string{"NAME", "CIDR", "POOL", "FAMILY", "LENGTH", "PHASE", "ALLOCATION", "OWNER", "AGE"}
	}
	t := newTable(a.io.Out, headers)
	for i := range claims {
		c := &claims[i]
		poolName := "—"
		if c.Spec.PoolRef != nil {
			poolName = c.Spec.PoolRef.Name
		} else if c.Spec.PoolSelector != nil {
			poolName = "(selector)"
		}
		phase := phaseText(string(c.Status.Phase))
		if wide {
			alloc := "—"
			if c.Status.BoundAllocationRef != nil {
				alloc = c.Status.BoundAllocationRef.Name
			}
			owner := "—"
			if c.Spec.OwnerRef != nil && c.Spec.OwnerRef.Name != "" {
				owner = c.Spec.OwnerRef.Name
			}
			t.row(c.Name, orDash(c.Status.AllocatedCIDR), poolName, orDash(string(c.Spec.IPFamily)),
				fmt.Sprintf("/%d", c.Spec.PrefixLength), phase, alloc, owner, humanDuration(c.CreationTimestamp))
		} else {
			t.row(c.Name, orDash(c.Status.AllocatedCIDR), poolName, orDash(string(c.Spec.IPFamily)),
				fmt.Sprintf("/%d", c.Spec.PrefixLength), phase, humanDuration(c.CreationTimestamp))
		}
	}
	return t.flush()
}

// phaseText renders a claim phase as plain, color-independent text.
func phaseText(p string) string {
	if p == "" {
		return "—"
	}
	return strings.ToUpper(p)
}

// ---------------------------------------------------------------------------
// prefix show
// ---------------------------------------------------------------------------

func newPrefixShowCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "show <cidr|name>",
		Aliases: []string{"get", "describe"},
		Short:   "Show a claimed prefix by name or allocated CIDR",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			claim, err := resolveClaim(cs, ns, args[0])
			if err != nil {
				return err
			}
			setClaimGVK(claim)
			if done, err := a.renderMachine(claim, func() string { return "ipclaim/" + claim.Name }); done {
				return err
			}
			return a.renderPrefixDetail(claim)
		},
	}
	return cmd
}

// resolveClaim looks up a claim by name, or by allocated CIDR when the argument
// parses as a CIDR.
func resolveClaim(cs clientset.Interface, ns, arg string) (*ipamv1alpha1.IPClaim, error) {
	if strings.Contains(arg, "/") {
		list, err := cs.IpamV1alpha1().IPClaims(ns).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return nil, classifyError(err)
		}
		for i := range list.Items {
			if list.Items[i].Status.AllocatedCIDR == arg {
				return &list.Items[i], nil
			}
		}
		return nil, newCLIError(exitNotFound, fmt.Sprintf("no prefix with allocated CIDR %q in this project", arg)).
			withFix("list prefixes:\n       datumctl ipam prefix list")
	}
	claim, err := cs.IpamV1alpha1().IPClaims(ns).Get(context.Background(), arg, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, newCLIError(exitNotFound, fmt.Sprintf("prefix %q not found in this project", arg)).
				withFix("list prefixes:\n       datumctl ipam prefix list").withCause(err)
		}
		return nil, classifyError(err)
	}
	return claim, nil
}

func (a *app) renderPrefixDetail(c *ipamv1alpha1.IPClaim) error {
	t := newTable(a.io.Out, []string{"FIELD", "VALUE"})
	t.row("Name", c.Name)
	t.row("CIDR", orDash(c.Status.AllocatedCIDR))
	t.row("Family", orDash(string(c.Spec.IPFamily)))
	t.row("Length", fmt.Sprintf("/%d", c.Spec.PrefixLength))
	t.row("Phase", phaseText(string(c.Status.Phase)))
	if c.Spec.PoolRef != nil {
		t.row("Pool", c.Spec.PoolRef.Name)
	} else if c.Spec.PoolSelector != nil {
		t.row("Pool", "(by selector)")
	}
	if c.Status.BoundAllocationRef != nil {
		t.row("Allocation", c.Status.BoundAllocationRef.Name)
	}
	if c.Spec.ReclaimPolicy != "" {
		t.row("Reclaim policy", string(c.Spec.ReclaimPolicy))
	}
	if c.Spec.OwnerRef != nil && c.Spec.OwnerRef.Name != "" {
		t.row("Owner", fmt.Sprintf("%s/%s", c.Spec.OwnerRef.Kind, c.Spec.OwnerRef.Name))
	}
	t.row("Age", humanDuration(c.CreationTimestamp))
	return t.flush()
}

// ---------------------------------------------------------------------------
// prefix release
// ---------------------------------------------------------------------------

func newPrefixReleaseCommand(a *app) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "release <name>",
		Aliases: []string{"rm", "delete"},
		Short:   "Release (delete) a claimed prefix",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			claim, err := cs.IpamV1alpha1().IPClaims(ns).Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return newCLIError(exitNotFound, fmt.Sprintf("prefix %q not found in this project", name)).
						withFix("list prefixes:\n       datumctl ipam prefix list").withCause(err)
				}
				return classifyError(err)
			}

			if dryRun {
				_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — nothing was released.")
				_, _ = fmt.Fprintf(a.io.ErrOut, "Would release prefix %q (%s).\n", name, orDash(claim.Status.AllocatedCIDR))
				if claim.Status.BoundAllocationRef != nil {
					_, _ = fmt.Fprintf(a.io.ErrOut, "Would free allocation %q.\n", claim.Status.BoundAllocationRef.Name)
				}
				return nil
			}

			prompt := fmt.Sprintf("Release prefix %q (%s)?", name, orDash(claim.Status.AllocatedCIDR))
			if !a.confirmYesNo(prompt) {
				return newCLIError(exitAborted, "aborted")
			}
			if err := cs.IpamV1alpha1().IPClaims(ns).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
				return classifyError(err)
			}
			if a.opts.output == outputName {
				_, _ = fmt.Fprintf(a.io.Out, "ipclaim/%s\n", name)
				return nil
			}
			_, _ = fmt.Fprintf(a.io.Out, "%s Released prefix %q\n", successPrefix(a.color), name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be released without releasing it")
	return cmd
}

// visiblePoolNames returns the names of pools visible to the caller, for
// "did you mean" style error context. Best-effort; returns nil on failure.
func visiblePoolNames(cs clientset.Interface) []string {
	list, err := cs.IpamV1alpha1().IPPools().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	sort.Strings(names)
	return names
}
