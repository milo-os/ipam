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

const apiVersion = "ipam.miloapis.com/v1alpha1"

func setPoolGVK(p *ipamv1alpha1.IPPool) {
	p.APIVersion = apiVersion
	p.Kind = "IPPool"
}

func newPoolCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Create, inspect, and release allocatable address space (IPPool)",
		Long: `Pools are allocatable blocks of address space. Root pools declare a CIDR; child
pools carve a sub-block from a parent.

Pools are an operator surface. A consumer never names one — a claim names a
class, and the allocator resolves the pool from that class and the claim's
scope. What connects the two is spec.classNames: the classes a pool offers
itself to.`,
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
		cidr        string
		family      string
		parent      string
		classNames  []string
		scopeArgs   []string
		reserveHead int
		reserveTail int
		reserveUnit int
		minLen      int
		maxLen      int
		prefixLen   int
		strategy    string
		visibility  string
		dryRun      bool
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a pool and offer it to classes",
		Long: `Create an allocatable block of address space.

A pool is only reachable by consumers once it offers itself to a class: --class
is what publishes capacity, and a class no pool offers is a class whose every
claim fails. --scope narrows who the pool serves — a pool scoped to a location
serves only claims from that location, and a pool with no scope serves
everywhere.

` + scopeGrammarHelp(),
		Args: cobra.ExactArgs(1),
		Example: `  # A root /8 offered to a class
  datumctl ipam pool create prod-backbone --cidr 10.0.0.0/8 --class tenant-subnet-ipv4

  # A pool that serves one location only
  datumctl ipam pool create us-central-1-tenant --cidr 10.4.0.0/14 \
    --class tenant-endpoint-ipv4 --scope location=us-central-1

  # A child pool carved from a parent, allowing /24–/28 leaf claims
  datumctl ipam pool create us-west --parent prod-backbone --prefix-length 16 \
    --min-length 24 --max-length 28

  # Withhold the first and last /32 of the block from circulation
  datumctl ipam pool create link-net --cidr 192.0.2.0/24 \
    --reserve-leading 1 --reserve-trailing 1 --reserve-unit 32`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			scope, err := buildScope(scopeArgs)
			if err != nil {
				return err
			}
			pool := &ipamv1alpha1.IPPool{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: ipamv1alpha1.IPPoolSpec{
					CIDR:          cidr,
					ParentPoolRef: refOrNil(parent),
					ClassNames:    classNames,
					Scope:         scope,
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
			if reserveHead > 0 || reserveTail > 0 {
				// The unit cannot be inferred: a pool serves classes of differing
				// allocation sizes, so "one reserved position" is meaningless until
				// the caller says how big a position is.
				if reserveUnit <= 0 {
					return usageErrorf("--reserve-leading/--reserve-trailing need --reserve-unit <bits>: " +
						"a reserved position has no size until you name one")
				}
				pool.Spec.Reservations = &ipamv1alpha1.ReservationSpec{
					Leading:          int32(reserveHead),
					Trailing:         int32(reserveTail),
					UnitPrefixLength: int32(reserveUnit),
				}
			} else if reserveUnit > 0 {
				return usageErrorf("--reserve-unit has no effect without --reserve-leading or --reserve-trailing")
			}

			if dryRun {
				_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — no pool was created.")
				_, _ = fmt.Fprintf(a.io.ErrOut, "Would create pool %q (cidr %s, family %s), offering %s.\n",
					name, orDash(cidr), orDash(string(pool.Spec.IPFamily)), orDashList(classNames))
				_, mErr := a.renderMachine(pool, func() string { return "ippool/" + name })
				return mErr
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
			if len(created.Spec.ClassNames) == 0 && !a.opts.quiet {
				_, _ = fmt.Fprintln(a.io.ErrOut,
					"This pool offers itself to no class, so no claim can draw on it.\n"+
						"Add one with --class <name> at create time.")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&cidr, "cidr", "", "CIDR for a root pool (e.g. 10.0.0.0/8)")
	f.StringVar(&family, "family", "", "Address family: ipv4|ipv6 (inferred from --cidr when omitted)")
	f.StringVar(&parent, "parent", "", "Parent pool name (creates a child pool)")
	f.StringArrayVar(&classNames, "class", nil, "Offer this pool to a class (repeatable). Capacity is unreachable without one")
	f.StringArrayVar(&scopeArgs, "scope", nil, scopeFlagUsage("References this pool serves; a scoped pool serves only matching claims"))
	f.IntVar(&reserveHead, "reserve-leading", 0, "Withhold this many positions at the start of the block")
	f.IntVar(&reserveTail, "reserve-trailing", 0, "Withhold this many positions at the end of the block")
	f.IntVar(&reserveUnit, "reserve-unit", 0, "Prefix length of one reserved position; required with --reserve-leading/--reserve-trailing")
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

// childCounts tallies, per pool name, how many child pools and how many
// allocations draw on it. Allocations rather than claims: a retained allocation
// whose claim is gone still holds space, and a reservation never had a claim at
// all, so counting claims would under-report what a pool is carrying.
//
// Best-effort, and the counts are a decoration on `-o wide` rather than an input
// to any decision — unlike the release path, which needs `listPoolAllocations`
// because it must not act on an undercount. The one thing this shares with that
// path is the search: an IPPool is cluster-scoped and its allocations are not,
// so a single-namespace list undercounts every pool with consumers elsewhere.
func childCounts(cs clientset.Interface, namespace string, pools []ipamv1alpha1.IPPool) (children map[string]int, allocations map[string]int) {
	children = map[string]int{}
	allocations = map[string]int{}
	for i := range pools {
		if p := pools[i].Spec.ParentPoolRef; p != nil {
			children[p.Name]++
		}
	}
	items, _, err := listPoolAllocations(cs, namespace)
	if err == nil {
		for i := range items {
			allocations[items[i].Spec.PoolRef.Name]++
		}
	}
	return children, allocations
}

// listPoolAllocations returns the allocations to search when reasoning about a
// pool, and reports whether that search covered every namespace.
//
// IPPool is cluster-scoped; IPAllocation is not. A pool hands addresses to any
// namespace that claims from it, so "the allocations of this pool" is a
// cluster-wide question and listing one namespace answers a different, smaller
// one. Passing "" lists across all namespaces, which is what the question needs.
//
// A consumer identity will not be allowed that, so a denial falls back to the
// active namespace and reports complete=false. The fallback exists to keep the
// command working for a caller who can only see their own namespace; the flag
// exists because an undercount must never be rendered as an absence. Callers
// that act on the result have to branch on it — see poolBlastRadius.
func listPoolAllocations(cs clientset.Interface, namespace string) (items []ipamv1alpha1.IPAllocation, complete bool, err error) {
	all, err := cs.IpamV1alpha1().IPAllocations(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	if err == nil {
		return all.Items, true, nil
	}
	if !apierrors.IsForbidden(err) {
		return nil, false, err
	}
	scoped, scopedErr := cs.IpamV1alpha1().IPAllocations(namespace).List(context.Background(), metav1.ListOptions{})
	if scopedErr != nil {
		return nil, false, scopedErr
	}
	return scoped.Items, false, nil
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
	headers := []string{"NAME", "CIDR", "FAMILY", "CLASSES", "SCOPE", "UTILIZATION", "LARGEST FREE", "AGE"}
	if wide {
		headers = []string{"NAME", "CIDR", "FAMILY", "CLASSES", "SCOPE", "UTILIZATION", "LARGEST FREE", "CHILDREN", "ALLOCATIONS", "PHASE", "AGE"}
	}
	t := newTable(a.io.Out, headers)

	var children, allocations map[string]int
	if wide {
		if cs, ns, err := a.client(); err == nil {
			children, allocations = childCounts(cs, ns, pools)
		}
	}

	for i := range pools {
		p := &pools[i]
		cidr := p.Status.AllocatedCIDR
		if cidr == "" {
			cidr = p.Spec.CIDR
		}
		util := utilizationCell(poolUtilization(p), 10, a.color.enabled)
		free := poolLargestFreeCell(p)
		family := orDash(string(poolFamily(p)))
		classes := poolClassesCell(p)
		if wide {
			t.row(p.Name, orDash(cidr), family, classes, formatScope(p.Spec.Scope), util, free,
				itoa(children[p.Name]), itoa(allocations[p.Name]), orDash(string(p.Status.Phase)),
				humanDuration(p.CreationTimestamp))
		} else {
			t.row(p.Name, orDash(cidr), family, classes, formatScope(p.Spec.Scope), util, free,
				humanDuration(p.CreationTimestamp))
		}
	}
	return t.flush()
}

// poolClassesCell renders the classes a pool offers itself to. A pool that
// offers none is capacity nobody can reach, so it says so rather than showing a
// blank cell.
func poolClassesCell(p *ipamv1alpha1.IPPool) string {
	if len(p.Spec.ClassNames) > 0 {
		return strings.Join(p.Spec.ClassNames, ",")
	}
	if p.Spec.ClassRef != nil {
		return "(provisioned by " + p.Spec.ClassRef.Name + ")"
	}
	return "— (unreachable)"
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
				return poolGetError(err, args[0], cs)
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
	t.row("Offered to classes", poolClassesCell(p))
	if p.Spec.ClassRef != nil {
		t.row("Provisioned by", p.Spec.ClassRef.Name)
	}
	for _, role := range sortedScopeRoles(p.Spec.Scope) {
		t.row("Serves "+role, formatScopeRef(p.Spec.Scope[role]))
	}
	if p.Status.ScopeDigest != "" {
		t.row("Scope digest", p.Status.ScopeDigest)
	}
	if r := p.Spec.Reservations; r != nil {
		t.row("Reservations", fmt.Sprintf("leading=%d trailing=%d unit=/%d",
			r.Leading, r.Trailing, r.UnitPrefixLength))
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
		t.row("Capacity", fmt.Sprintf("total=%s allocated=%s available=%s",
			p.Status.Capacity.Total, p.Status.Capacity.Allocated, p.Status.Capacity.Available))
	}
	t.row("Largest free", poolLargestFreeCell(p))
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

// poolBlastRadius is what releasing a pool would take with it: the child pools
// carved from it and the allocations still held out of it.
//
// `complete` is the field that earns this a type rather than two slices. The
// allocation search can be forced back to one namespace by authorization (see
// listPoolAllocations), and an undercount rendered as "Blast radius: none" is a
// dry run reporting safe for a call the server then refuses — the one output an
// operator reaches for *because* they are unsure. So the shortfall travels with
// the counts and every consumer has to decide what to do about it, instead of
// being dropped at the call site the way a discarded error was.
type poolBlastRadius struct {
	childPools      []string
	heldAllocations []ipamv1alpha1.IPAllocation

	// complete reports that every namespace was searched. When false the counts
	// are a floor, not a total, and searchedNamespace names all that was covered.
	complete          bool
	searchedNamespace string
}

func (b poolBlastRadius) total() int { return len(b.childPools) + len(b.heldAllocations) }

// poolBlast computes what releasing the named pool would take with it.
//
// Unlike the code this replaced, no listing error is discarded. A failure to
// enumerate is not evidence of an empty blast radius, and rendering it as one
// turned a 403 into "nothing here" on the most destructive command in the CLI.
func poolBlast(cs clientset.Interface, namespace, name string) (poolBlastRadius, error) {
	b := poolBlastRadius{searchedNamespace: namespace}

	allPools, err := cs.IpamV1alpha1().IPPools().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return b, classifyError(err)
	}
	for i := range allPools.Items {
		if r := allPools.Items[i].Spec.ParentPoolRef; r != nil && r.Name == name {
			b.childPools = append(b.childPools, allPools.Items[i].Name)
		}
	}

	allocs, complete, err := listPoolAllocations(cs, namespace)
	if err != nil {
		return b, classifyError(err)
	}
	b.complete = complete
	for i := range allocs {
		if allocs[i].Spec.PoolRef.Name == name {
			b.heldAllocations = append(b.heldAllocations, allocs[i])
		}
	}
	return b, nil
}

// renderBlastRadius prints what a release would take with it. It never prints
// "none" from an incomplete search: the whole point of the preview is that a
// reader can act on it, and "none" is the one word that would let them.
func (a *app) renderBlastRadius(b poolBlastRadius) {
	switch {
	case b.total() == 0 && b.complete:
		_, _ = fmt.Fprintln(a.io.ErrOut, "Blast radius: none (no child pools or live allocations).")
	case b.total() == 0:
		_, _ = fmt.Fprintf(a.io.ErrOut,
			"Blast radius: UNKNOWN. Nothing found in namespace %q, but this identity may\n"+
				"not list allocations in every namespace, and a pool serves any namespace\n"+
				"that claims from it. Absence here is not absence.\n", b.searchedNamespace)
		return
	default:
		if !b.complete {
			_, _ = fmt.Fprintf(a.io.ErrOut,
				"Blast radius: AT LEAST %d child pool(s), %d allocation(s) — namespace %q only,\n"+
					"there may be more elsewhere:\n",
				len(b.childPools), len(b.heldAllocations), b.searchedNamespace)
		} else {
			_, _ = fmt.Fprintf(a.io.ErrOut, "Blast radius: %d child pool(s), %d allocation(s):\n",
				len(b.childPools), len(b.heldAllocations))
		}
	}
	for _, c := range b.childPools {
		_, _ = fmt.Fprintf(a.io.ErrOut, "  child pool: %s\n", c)
	}
	for i := range b.heldAllocations {
		al := &b.heldAllocations[i]
		_, _ = fmt.Fprintf(a.io.ErrOut, "  allocation: %s/%s (%s, %s)\n",
			al.Namespace, al.Name, orDash(allocationAddress(al)), allocationClaimName(al))
	}
}

// incompleteError refuses a release whose blast radius could not be established.
//
// The alternative is to proceed and let the server's deletion protection catch
// it, which is where this used to end up: the cascade deleted what it could see,
// the pool delete was then refused for what it could not, and the caller was
// left with a partly dismantled pool and a 409 naming objects the CLI had just
// told them did not exist.
func (b poolBlastRadius) incompleteError(name string) *cliError {
	return newCLIError(exitForbidden, fmt.Sprintf(
		"cannot establish what pool %q is carrying: this identity may only list "+
			"allocations in namespace %q, and a pool serves every namespace that claims from it",
		name, b.searchedNamespace)).
		withFix("releasing a pool on a partial view can strand allocations in namespaces\n" +
			"       this command cannot see. Re-run as an identity that can list allocations\n" +
			"       in all namespaces, or have the pool's operator release it.")
}

// poolInUseError refuses a release and names what is holding the pool.
//
// The server's own refusal says "release all claims and child pools first",
// which names nothing the caller can go and look at — and for a retained
// allocation there is no claim to release at all. Since the CLI has just
// enumerated the holders, it says which they are.
func poolInUseError(name string, b poolBlastRadius) *cliError {
	ce := newCLIError(exitConflict, fmt.Sprintf(
		"pool %q still has %d child pool(s) and %d allocation(s)",
		name, len(b.childPools), len(b.heldAllocations)))

	var fix strings.Builder
	fix.WriteString("release these first, or pass --cascade to release everything under this\n       pool:")
	for _, c := range b.childPools {
		fix.WriteString("\n       child pool " + c)
	}
	for i := range b.heldAllocations {
		al := &b.heldAllocations[i]
		// A retained allocation has no claim to release, so name the allocation
		// itself — telling someone to "release the claim" for one of these is the
		// remedy that cannot work.
		if al.Spec.ClaimRef != nil {
			fmt.Fprintf(&fix, "\n       claim %s/%s (holds %s)",
				al.Namespace, al.Spec.ClaimRef.Name, orDash(allocationAddress(al)))
			continue
		}
		fmt.Fprintf(&fix, "\n       allocation %s/%s (holds %s, no claim — retained)",
			al.Namespace, al.Name, orDash(allocationAddress(al)))
	}
	return ce.withFix(fix.String())
}

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
pools or live allocations is protected — releasing it fails unless --cascade is
given (mirroring the server's deletion protection).

Allocations, not claims, are what count here: a retained allocation whose claim
is long gone still holds an address out of this pool, and releasing the pool
underneath it destroys a record something else may still be relying on.

A pool is cluster-scoped and its allocations are not, so the blast radius is
searched across every namespace. If your access does not reach that far the
blast radius is reported as UNKNOWN and the release is refused, rather than an
unsearchable namespace being reported as an empty one.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			pool, err := cs.IpamV1alpha1().IPPools().Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				return poolGetError(err, name, cs)
			}

			blast, brErr := poolBlast(cs, ns, name)
			if brErr != nil {
				return brErr
			}
			childPools, heldAllocations := blast.childPools, blast.heldAllocations

			if dryRun {
				_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — nothing was released.")
				_, _ = fmt.Fprintf(a.io.ErrOut, "Would release pool %q.\n", name)
				a.renderBlastRadius(blast)
				return nil
			}

			// An incomplete search cannot support either decision below: a zero
			// count does not mean the pool is free, and a cascade would delete the
			// allocations it can see and then be refused by the server for the ones
			// it cannot. Refuse while the answer is still honest rather than part
			// way through. See poolBlastRadius for why the search can be partial.
			if !blast.complete {
				return blast.incompleteError(name)
			}

			if blast.total() > 0 && !cascade {
				return poolInUseError(name, blast)
			}

			prompt := fmt.Sprintf("About to release pool %q.", name)
			if blast.total() > 0 {
				prompt += fmt.Sprintf(" This releases %d child pool(s) and %d allocation(s).", len(childPools), len(heldAllocations))
			}
			ok, cErr := a.confirmTyped(name, prompt)
			if cErr != nil {
				return cErr
			}
			if !ok {
				return newCLIError(exitAborted, "aborted")
			}

			if cascade {
				// Each object is addressed in its OWN namespace, not the caller's.
				// The search is cluster-wide (a cluster-scoped pool serves every
				// namespace that claims from it), so `ns` is merely where the CLI
				// happens to be pointed and using it here would issue every delete
				// against the wrong namespace — 404s on someone else's allocation,
				// swallowed by the IsNotFound guard, and the pool delete refused
				// afterwards for objects the cascade reported as handled.
				for i := range heldAllocations {
					al := &heldAllocations[i]
					// Claim first where there is one, so the claim and its address
					// never disagree: releasing an allocation out from under a live
					// claim is refused, and rightly.
					if al.Spec.ClaimRef != nil {
						if delErr := cs.IpamV1alpha1().IPClaims(al.Namespace).Delete(context.Background(), al.Spec.ClaimRef.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
							return classifyError(delErr)
						}
					}
					// Then the allocation, unconditionally. Deleting the claim does
					// NOT free the address under reclaim policy Retain — that is what
					// Retain means — so stopping at the claim left the allocation
					// holding the block, the pool delete refused, and the caller with
					// their claim destroyed and the pool still standing: a partial,
					// irreversible outcome produced by the flag that promises to
					// release everything under the pool. A 404 here is the ordinary
					// Delete case, where the claim already took the allocation with it.
					if delErr := cs.IpamV1alpha1().IPAllocations(al.Namespace).Delete(context.Background(), al.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
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
	cmd.Flags().BoolVar(&cascade, "cascade", false, "Also release child pools and the allocations held under this pool")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "List what would be released without releasing it")
	return cmd
}

// poolGetError adds IPAM context to a failed pool Get: a 404 becomes a clear
// "not visible in the active project" message, naming the pools that are
// visible, rather than a bare Kubernetes NotFound.
func poolGetError(err error, name string, cs clientset.Interface) error {
	if apierrors.IsNotFound(err) {
		return noMatchingPoolError(name, visiblePoolNames(cs)).withCause(err)
	}
	return classifyError(err)
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
