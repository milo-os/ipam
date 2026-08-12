package main

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	clientset "go.miloapis.com/ipam/pkg/client/clientset/versioned"
)

func setAllocationGVK(a *ipamv1alpha1.IPAllocation) {
	a.APIVersion = apiVersion
	a.Kind = "IPAllocation"
}

func newAllocationCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "allocation",
		Aliases: []string{"allocations", "alloc"},
		Short:   "Inspect and release the records of addresses handed out (IPAllocation)",
		Long: `An allocation is the record of an address handed out of a pool. It is created
by the system, never by you: claiming creates one, and so does a pool reserving
positions at its own edges.

An allocation can outlive the claim that created it. A claim released under
reclaim policy Retain leaves its allocation in place with no claim reference —
still held, still counted against its owner — which is the case this group
exists for.`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return c.Help()
			}
			return unknownSubcommandError(c, args[0])
		},
	}
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(
		newAllocationListCommand(a),
		newAllocationShowCommand(a),
		newAllocationReleaseCommand(a),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// allocation list
// ---------------------------------------------------------------------------

func newAllocationListCommand(a *app) *cobra.Command {
	var (
		class     string
		pool      string
		purpose   string
		unclaimed bool
		scopeArgs []string
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List allocations",
		Long: `List the addresses handed out of pools.

--unclaimed is the leak check: allocations with no claim bound. Under reclaim
policy Retain that is an address still held after its claim was deleted, and
nothing frees it until someone does so deliberately.

` + scopeGrammarHelp(),
		Args: cobra.NoArgs,
		Example: `  datumctl ipam allocation list
  datumctl ipam allocation list --pool us-central-1-tenant-v4 -o wide

  # Addresses still held with no claim behind them — the Retain leak check
  datumctl ipam allocation list --unclaimed`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			wantScope, err := buildScope(scopeArgs)
			if err != nil {
				return err
			}
			wantPurpose, err := parsePurpose(purpose)
			if err != nil {
				return err
			}
			list, err := cs.IpamV1alpha1().IPAllocations(ns).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return classifyError(err)
			}
			items := list.Items[:0:0]
			for i := range list.Items {
				al := &list.Items[i]
				if class != "" && al.Spec.ClassName != class {
					continue
				}
				if pool != "" && al.Spec.PoolRef.Name != pool {
					continue
				}
				if wantPurpose != "" && al.Spec.Purpose != wantPurpose {
					continue
				}
				if unclaimed && al.Spec.ClaimRef != nil {
					continue
				}
				if !scopeContains(al.Spec.Scope, wantScope) {
					continue
				}
				items = append(items, *al)
			}
			list.Items = items
			for i := range list.Items {
				setAllocationGVK(&list.Items[i])
			}
			list.APIVersion = apiVersion
			list.Kind = "IPAllocationList"

			switch a.opts.output {
			case outputJSON:
				return encodeJSON(a.io.Out, list)
			case outputYAML:
				return encodeYAML(a.io.Out, list)
			case outputName:
				for i := range items {
					_, _ = fmt.Fprintf(a.io.Out, "ipallocation/%s\n", items[i].Name)
				}
				return nil
			}
			return a.renderAllocationTable(items)
		},
	}
	f := cmd.Flags()
	f.StringVar(&class, "class", "", "Only show allocations of this class")
	f.StringVar(&pool, "pool", "", "Only show allocations drawn from this pool")
	f.StringVar(&purpose, "purpose", "", "Only show allocations of this purpose: Claim|Reservation|PoolCarve")
	f.BoolVar(&unclaimed, "unclaimed", false, "Only show allocations with no claim bound (retained or reserved)")
	f.StringArrayVar(&scopeArgs, "scope", nil, scopeFlagUsage("Only show allocations whose scope includes these references"))
	return cmd
}

// parsePurpose validates the --purpose value against the API enum, accepting
// any casing.
func parsePurpose(s string) (ipamv1alpha1.AllocationPurpose, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "claim":
		return ipamv1alpha1.PurposeClaim, nil
	case "reservation":
		return ipamv1alpha1.PurposeReservation, nil
	case "poolcarve", "pool-carve":
		return ipamv1alpha1.PurposePoolCarve, nil
	default:
		return "", usageErrorf("invalid --purpose %q: must be %s, %s, or %s",
			s, ipamv1alpha1.PurposeClaim, ipamv1alpha1.PurposeReservation, ipamv1alpha1.PurposePoolCarve)
	}
}

// allocationAddress is the address an allocation holds: the single-address form
// when the class hands out hosts, the block otherwise.
func allocationAddress(al *ipamv1alpha1.IPAllocation) string {
	if al.Status.Address != "" {
		return al.Status.Address
	}
	return al.Status.AllocatedCIDR
}

// allocationClaimName names the claim bound to an allocation, or says why there
// is none — the distinction between reserved space and a retained address is
// what an operator is scanning this column for.
func allocationClaimName(al *ipamv1alpha1.IPAllocation) string {
	if al.Spec.ClaimRef != nil {
		return al.Spec.ClaimRef.Name
	}
	switch al.Spec.Purpose {
	case ipamv1alpha1.PurposeReservation:
		return "— (reserved)"
	case ipamv1alpha1.PurposePoolCarve:
		return "— (child pool)"
	default:
		return "— (retained)"
	}
}

func (a *app) renderAllocationTable(allocs []ipamv1alpha1.IPAllocation) error {
	if len(allocs) == 0 {
		if !a.opts.quiet {
			_, _ = fmt.Fprintln(a.io.ErrOut, "No allocations found.")
		}
		return nil
	}
	sort.Slice(allocs, func(i, j int) bool { return allocs[i].Name < allocs[j].Name })

	wide := a.opts.output == outputWide
	headers := []string{"NAME", "ADDRESS", "CLASS", "POOL", "CLAIM", "PHASE", "AGE"}
	if wide {
		headers = []string{"NAME", "ADDRESS", "CLASS", "POOL", "CLAIM", "PURPOSE", "SCOPE", "OWNER", "RECLAIM", "PHASE", "AGE"}
	}
	t := newTable(a.io.Out, headers)
	for i := range allocs {
		al := &allocs[i]
		if wide {
			t.row(al.Name, orDash(allocationAddress(al)), orDash(al.Spec.ClassName), al.Spec.PoolRef.Name,
				allocationClaimName(al), orDash(string(al.Spec.Purpose)), formatScope(al.Spec.Scope),
				formatObjectRef(al.Spec.OwnerRef), orDash(string(al.Spec.ReclaimPolicy)),
				phaseText(string(al.Status.Phase)), humanDuration(al.CreationTimestamp))
		} else {
			t.row(al.Name, orDash(allocationAddress(al)), orDash(al.Spec.ClassName), al.Spec.PoolRef.Name,
				allocationClaimName(al), phaseText(string(al.Status.Phase)),
				humanDuration(al.CreationTimestamp))
		}
	}
	return t.flush()
}

// ---------------------------------------------------------------------------
// allocation show
// ---------------------------------------------------------------------------

func newAllocationShowCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "show <name|address>",
		Aliases: []string{"get", "describe"},
		Short:   "Show an allocation by name, or by the address it holds",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			holders, err := a.resolveAllocation(cs, ns, args[0])
			if err != nil {
				return err
			}
			al := holders[0]
			setAllocationGVK(al)
			if done, mErr := a.renderMachine(al, func() string { return "ipallocation/" + al.Name }); done {
				return mErr
			}
			if err := a.renderAllocationDetail(al); err != nil {
				return err
			}
			a.warnMultipleHolders(args[0], holders)
			return nil
		},
	}
}

// resolveAllocation accepts a name or an address. It returns every holder, not
// just one, because `allocation show <address>` reaches the same reverse lookup
// as `address show` and would conceal a doubly-held address in the same way. A
// name resolves to at most one object by construction.
func (a *app) resolveAllocation(cs clientset.Interface, ns, arg string) ([]*ipamv1alpha1.IPAllocation, error) {
	if looksLikeAddress(arg) {
		return a.lookupAllocationsByAddress(cs, ns, arg)
	}
	al, err := cs.IpamV1alpha1().IPAllocations(ns).Get(context.Background(), arg, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, newCLIError(exitNotFound, fmt.Sprintf("allocation %q not found in this project", arg)).
				withFix("list allocations:\n       datumctl ipam allocation list").withCause(err)
		}
		return nil, classifyError(err)
	}
	return []*ipamv1alpha1.IPAllocation{al}, nil
}

func (a *app) renderAllocationDetail(al *ipamv1alpha1.IPAllocation) error {
	t := newTable(a.io.Out, []string{"FIELD", "VALUE"})
	t.row("Name", al.Name)
	t.row("Address", orDash(allocationAddress(al)))
	if al.Status.Address != "" && al.Status.AllocatedCIDR != "" && al.Status.Address != al.Status.AllocatedCIDR {
		t.row("Block", al.Status.AllocatedCIDR)
	}
	t.row("Class", orDash(al.Spec.ClassName))
	t.row("Family", orDash(string(al.Spec.IPFamily)))
	t.row("Pool", al.Spec.PoolRef.Name)
	t.row("Purpose", orDash(string(al.Spec.Purpose)))
	t.row("Claim", allocationClaimName(al))
	t.row("Phase", phaseText(string(al.Status.Phase)))
	for _, role := range sortedScopeRoles(al.Spec.Scope) {
		t.row("Scope "+role, formatScopeRef(al.Spec.Scope[role]))
	}
	if al.Status.ScopeDigest != "" {
		t.row("Scope digest", al.Status.ScopeDigest)
	}
	if al.Spec.ReclaimPolicy != "" {
		t.row("Reclaim policy", string(al.Spec.ReclaimPolicy))
	}
	if al.Spec.OwnerRef != nil {
		t.row("Held for", formatObjectRef(al.Spec.OwnerRef))
	}
	t.row("Age", humanDuration(al.CreationTimestamp))
	if err := t.flush(); err != nil {
		return err
	}
	return a.renderConditions(al.Status.Conditions)
}

// ---------------------------------------------------------------------------
// allocation release
// ---------------------------------------------------------------------------

func newAllocationReleaseCommand(a *app) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "release <name>",
		Aliases: []string{"rm", "delete"},
		Short:   "Release a held allocation back to its pool",
		Long: `Release an allocation. This is the deliberate hand-back that reclaim policy
Retain defers to: an allocation whose claim is gone stays held until something
releases it here.

Releasing an allocation that still has a claim bound to it is refused — release
the claim instead, so the claim and its address do not disagree.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			al, err := cs.IpamV1alpha1().IPAllocations(ns).Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return newCLIError(exitNotFound, fmt.Sprintf("allocation %q not found in this project", name)).
						withFix("list allocations:\n       datumctl ipam allocation list").withCause(err)
				}
				return classifyError(err)
			}
			if al.Spec.ClaimRef != nil {
				return newCLIError(exitConflict, fmt.Sprintf(
					"allocation %q is still bound to claim %q", name, al.Spec.ClaimRef.Name)).
					withFix(fmt.Sprintf("release the claim instead:\n       datumctl ipam claim release %s", al.Spec.ClaimRef.Name))
			}

			addr := orDash(allocationAddress(al))
			if dryRun {
				_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — nothing was released.")
				_, _ = fmt.Fprintf(a.io.ErrOut, "Would release %s back to pool %q.\n", addr, al.Spec.PoolRef.Name)
				return nil
			}
			if !a.confirmYesNo(fmt.Sprintf("Release %s back to pool %q?", addr, al.Spec.PoolRef.Name)) {
				return newCLIError(exitAborted, "aborted")
			}
			if err := cs.IpamV1alpha1().IPAllocations(ns).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
				return classifyError(err)
			}
			if a.opts.output == outputName {
				_, _ = fmt.Fprintf(a.io.Out, "ipallocation/%s\n", name)
				return nil
			}
			_, _ = fmt.Fprintf(a.io.Out, "%s Released %s back to pool %q\n", successPrefix(a.color), addr, al.Spec.PoolRef.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be released without releasing it")
	return cmd
}

// ---------------------------------------------------------------------------
// address show — reverse lookup
// ---------------------------------------------------------------------------

func newAddressCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "address",
		Aliases: []string{"addr"},
		Short:   "Answer questions about a specific address",
		Long: `Reverse lookup: given an address, say what it is and who holds it.

This is the operator's first question during an incident, and it is answerable
because every address the platform hands out is a tracked allocation carrying
its class, its scope, and the consumer object it is held for.`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return c.Help()
			}
			return unknownSubcommandError(c, args[0])
		},
	}
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(newAddressShowCommand(a))
	return cmd
}

func newAddressShowCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "show <address>",
		Aliases: []string{"get", "who", "lookup"},
		Short:   "Say what an address is and who holds it",
		Args:    cobra.ExactArgs(1),
		Example: `  datumctl ipam address show 198.51.100.11
  datumctl ipam address show 2001:db8:1::/96`,
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := args[0]
			if !looksLikeAddress(arg) {
				return usageErrorf("%q is not an IP address or CIDR", arg)
			}
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			holders, err := a.lookupAllocationsByAddress(cs, ns, arg)
			if err != nil {
				return err
			}
			al := holders[0]
			// More than one holder means the address is doubly allocated. The
			// machine form emits every holder rather than the first, for the same
			// reason the human form warns: a caller that receives one object cannot
			// tell this state from a healthy one.
			if len(holders) > 1 {
				list := &ipamv1alpha1.IPAllocationList{}
				list.APIVersion = apiVersion
				list.Kind = "IPAllocationList"
				for _, h := range holders {
					setAllocationGVK(h)
					list.Items = append(list.Items, *h)
				}
				if done, mErr := a.renderMachine(list, func() string { return "ipallocation/" + al.Name }); done {
					return mErr
				}
			} else {
				setAllocationGVK(al)
				if done, mErr := a.renderMachine(al, func() string { return "ipallocation/" + al.Name }); done {
					return mErr
				}
			}
			if err := a.renderAddressAnswer(arg, al, cs); err != nil {
				return err
			}
			a.warnMultipleHolders(arg, holders)
			return nil
		},
	}
}

// lookupAllocationsByAddress finds every allocation holding an address. An exact
// match on the address or the block wins; failing that, the most specific block
// containing the address does, so asking about a host inside a claimed /24 still
// names its holder. The result is never empty — a miss is returned as an error.
//
// It returns every match rather than the first because two holders of one
// address is precisely the condition this command is reached for. Two root pools
// over one range hand the same address to unrelated claims; returning one of
// them names a single claim, gives no hint a second exists, and picks between
// equally specific blocks arbitrarily — an unstable answer, not merely an
// incomplete one.
func (a *app) lookupAllocationsByAddress(cs clientset.Interface, ns, arg string) ([]*ipamv1alpha1.IPAllocation, error) {
	list, err := cs.IpamV1alpha1().IPAllocations(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, classifyError(err)
	}

	var exact, best []*ipamv1alpha1.IPAllocation
	bestBits := -1
	for i := range list.Items {
		al := &list.Items[i]
		if al.Status.Address == arg || al.Status.AllocatedCIDR == arg {
			exact = append(exact, al)
			continue
		}
		bits, ok := allocationCovers(al, arg)
		if !ok {
			continue
		}
		// Ties are kept, not discarded. Two blocks of equal specificity covering
		// one address is the same collision as two exact matches.
		switch {
		case bits > bestBits:
			best, bestBits = []*ipamv1alpha1.IPAllocation{al}, bits
		case bits == bestBits:
			best = append(best, al)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	if len(best) > 0 {
		return best, nil
	}
	return nil, newCLIError(exitNotFound,
		fmt.Sprintf("%s is not allocated in namespace %q of this control plane", arg, ns)).
		withFix(a.addressNotFoundFix(arg))
}

// warnMultipleHolders reports an address held by more than one allocation.
//
// This is not a formatting nicety. Two holders of one address means two systems
// can be configured with it and put on one wire, and nothing else surfaces the
// condition: no condition, no event, no metric, and `allocation list` shows it
// only to someone who already suspects it and sorts by address.
//
// It goes to stderr so the -o json|yaml stdout contract is untouched; the
// machine form carries the same fact as a list of every holder.
func (a *app) warnMultipleHolders(arg string, holders []*ipamv1alpha1.IPAllocation) {
	if len(holders) < 2 {
		return
	}
	_, _ = fmt.Fprintf(a.io.ErrOut,
		"\nWARNING: %s is held by %d allocations at once. An address is meant to have\n"+
			"         one holder within an address space, so this is a defect in the data,\n"+
			"         not a form of sharing. The usual cause is two root IPPools whose\n"+
			"         ranges overlap; compare the pools below.\n",
		arg, len(holders))
	for _, h := range holders {
		claim := "(no claim — retained or reserved)"
		if h.Spec.ClaimRef != nil {
			claim = "claim " + h.Spec.ClaimRef.Name
		}
		_, _ = fmt.Fprintf(a.io.ErrOut, "         %-24s pool %-20s class %-20s %s\n",
			h.Name, h.Spec.PoolRef.Name, orDash(h.Spec.ClassName), claim)
	}
}

// addressNotFoundFix builds the remedy for a reverse lookup that found nothing.
//
// This is the command an operator reaches for mid-incident, so a remedy that
// cannot work is worse than none: it has the same shape as a runbook step and
// costs a real attempt to disprove. Every line below has to be something that
// actually changes the search.
//
// Two things bound the search, and the message names them in the order they
// bite. The lookup reads one namespace, which is the failure people hit first
// and the one always worth retrying. Beyond that the caller's tenant identity
// bounds it — and whether that can be changed from here depends entirely on the
// transport, so the message asks which one is in use rather than guessing.
//
// There is deliberately no "widen the search" suggestion: no cluster-wide
// reverse lookup exists, and pointing at `allocation list` implies one does.
func (a *app) addressNotFoundFix(arg string) string {
	var b strings.Builder
	b.WriteString("the reverse lookup reads one namespace. If the address belongs to a\n")
	b.WriteString("       different one, name it:\n")
	fmt.Fprintf(&b, "       datumctl ipam address show %s -n <namespace>", arg)

	_, mode := a.resolveDatum()
	if mode == modeDatum {
		// The active project is carried in the control-plane URL path, and Milo's
		// front gate turns that path into the caller's tenant identity. So here
		// --project genuinely re-targets the lookup, for a project you can read.
		b.WriteString("\n\n       An address held by another project is not visible from this one.\n")
		b.WriteString("       Re-run against a project you have access to:\n")
		fmt.Fprintf(&b, "       datumctl ipam address show %s --project <id>", arg)
	} else {
		// Direct to one aggregated apiserver: no Milo front gate, so nothing on
		// this side can stamp the tenant extras. --project only changes what the
		// output prints, which is precisely why it must not be offered as a fix.
		b.WriteString("\n\n       This kubeconfig reaches one control plane directly, so the addresses\n")
		b.WriteString("       visible here are all there are. --project will not widen it — on this\n")
		b.WriteString("       transport it affects only what the output prints. To look in another\n")
		b.WriteString("       tenant's space, point --kubeconfig at that control plane.")
	}
	return b.String()
}

// allocationCovers reports whether an allocation's block contains the queried
// address or block, and how specific that block is (its prefix length), so the
// tightest enclosing allocation can win.
func allocationCovers(al *ipamv1alpha1.IPAllocation, arg string) (int, bool) {
	block, err := netip.ParsePrefix(al.Status.AllocatedCIDR)
	if err != nil {
		return 0, false
	}
	if addr, aErr := netip.ParseAddr(arg); aErr == nil {
		return block.Bits(), block.Contains(addr)
	}
	if p, pErr := netip.ParsePrefix(arg); pErr == nil {
		return block.Bits(), block.Bits() <= p.Bits() && block.Contains(p.Addr())
	}
	return 0, false
}

// renderAddressAnswer prints the reverse-lookup answer: what the address is,
// who holds it, and what happens to it when that holder goes away.
func (a *app) renderAddressAnswer(arg string, al *ipamv1alpha1.IPAllocation, cs clientset.Interface) error {
	exact := al.Status.Address == arg || al.Status.AllocatedCIDR == arg
	if !exact {
		_, _ = fmt.Fprintf(a.io.Out, "%s falls inside %s, allocated as:\n\n", arg, al.Status.AllocatedCIDR)
	}
	_, _ = fmt.Fprintf(a.io.Out, "  address:    %s\n", orDash(allocationAddress(al)))
	_, _ = fmt.Fprintf(a.io.Out, "  class:      %s\n", orDash(al.Spec.ClassName))
	_, _ = fmt.Fprintf(a.io.Out, "  pool:       %s\n", al.Spec.PoolRef.Name)

	switch {
	case al.Spec.Purpose == ipamv1alpha1.PurposeReservation:
		_, _ = fmt.Fprintf(a.io.Out, "  held by:    pool %s itself (reserved, never handed out)\n", al.Spec.PoolRef.Name)
	case al.Spec.Purpose == ipamv1alpha1.PurposePoolCarve:
		_, _ = fmt.Fprintf(a.io.Out, "  held by:    a child pool carved from %s\n", al.Spec.PoolRef.Name)
	case al.Spec.OwnerRef != nil:
		_, _ = fmt.Fprintf(a.io.Out, "  claimed by: %s\n", formatObjectRef(al.Spec.OwnerRef))
		if al.Spec.ClaimRef != nil {
			_, _ = fmt.Fprintf(a.io.Out, "              via claim %s\n", al.Spec.ClaimRef.Name)
		}
		_, _ = fmt.Fprintf(a.io.Out, "              project %s\n", a.scopeLine(al.Namespace))
	case al.Spec.ClaimRef != nil:
		_, _ = fmt.Fprintf(a.io.Out, "  claimed by: claim %s\n", al.Spec.ClaimRef.Name)
		_, _ = fmt.Fprintf(a.io.Out, "              project %s\n", a.scopeLine(al.Namespace))
	default:
		_, _ = fmt.Fprintf(a.io.Out, "  claimed by: nothing — retained after its claim was deleted\n")
	}

	for _, role := range sortedScopeRoles(al.Spec.Scope) {
		_, _ = fmt.Fprintf(a.io.Out, "  %-11s %s\n", role+":", al.Spec.Scope[role].Name)
	}

	switch al.Spec.ReclaimPolicy {
	case ipamv1alpha1.ReclaimRetain:
		_, _ = fmt.Fprintf(a.io.Out, "  policy:     Retain — survives deletion of its holder\n")
	case ipamv1alpha1.ReclaimDelete:
		_, _ = fmt.Fprintf(a.io.Out, "  policy:     Delete — freed when its claim is released\n")
	}

	// The class explains what routing does with this address, which is the other
	// half of the incident question.
	if al.Spec.ClassName != "" {
		if class, err := cs.IpamV1alpha1().IPClasses().Get(context.Background(), al.Spec.ClassName, metav1.GetOptions{}); err == nil {
			if r := class.Spec.Routing; r.Internal != "" || r.External != "" {
				_, _ = fmt.Fprintf(a.io.Out, "  routing:    internal=%s external=%s\n",
					orDash(string(r.Internal)), orDash(string(r.External)))
			}
		}
	}
	return nil
}
