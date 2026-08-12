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

func setClassGVK(c *ipamv1alpha1.IPClass) {
	c.APIVersion = apiVersion
	c.Kind = "IPClass"
}

func newClassCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "class",
		Aliases: []string{"classes", "ipclass"},
		Short:   "Inspect the kinds of address space you can claim (IPClass)",
		Long: `A class is the policy object a claim names to get an address. It is what a
consumer picks, and the only addressing choice a consumer makes: the pool, the
CIDR, the size, and the location all follow from the class and the claim's
scope.

Classes are operator-authored and cluster-scoped, so this group is read-only.
The column that matters most is POOLS — a class no pool offers itself to cannot
satisfy any claim, and reading that here is much cheaper than discovering it
from a failed claim.`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return c.Help()
			}
			return unknownSubcommandError(c, args[0])
		},
	}
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(
		newClassListCommand(a),
		newClassShowCommand(a),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// class list
// ---------------------------------------------------------------------------

func newClassListCommand(a *app) *cobra.Command {
	var (
		family   string
		selector string
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the classes available to claim from",
		Args:    cobra.NoArgs,
		Example: `  datumctl ipam class list
  datumctl ipam class list --family ipv6 -o wide`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, _, err := a.client()
			if err != nil {
				return err
			}
			list, err := cs.IpamV1alpha1().IPClasses().List(context.Background(), metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return classifyError(err)
			}
			items := list.Items
			if family != "" {
				fam, pErr := parseFamily(family)
				if pErr != nil {
					return pErr
				}
				filtered := items[:0:0]
				for i := range items {
					if items[i].Spec.IPFamily == fam {
						filtered = append(filtered, items[i])
					}
				}
				items = filtered
				list.Items = items
			}
			for i := range list.Items {
				setClassGVK(&list.Items[i])
			}
			list.APIVersion = apiVersion
			list.Kind = "IPClassList"

			switch a.opts.output {
			case outputJSON:
				return encodeJSON(a.io.Out, list)
			case outputYAML:
				return encodeYAML(a.io.Out, list)
			case outputName:
				for i := range items {
					_, _ = fmt.Fprintf(a.io.Out, "ipclass/%s\n", items[i].Name)
				}
				return nil
			}
			return a.renderClassTable(items)
		},
	}
	cmd.Flags().StringVar(&family, "family", "", "Only show classes of this address family: ipv4|ipv6")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "Label selector to filter classes")
	return cmd
}

func (a *app) renderClassTable(classes []ipamv1alpha1.IPClass) error {
	if len(classes) == 0 {
		if !a.opts.quiet {
			_, _ = fmt.Fprintln(a.io.ErrOut, "No classes found. Without a class there is nothing to claim;")
			_, _ = fmt.Fprintln(a.io.ErrOut, "classes are operator-authored, so ask whoever runs the platform.")
		}
		return nil
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })

	wide := a.opts.output == outputWide
	headers := []string{"NAME", "FAMILY", "PARENT", "UNIT", "POOLS", "PHASE", "AGE"}
	if wide {
		headers = []string{"NAME", "FAMILY", "PARENT", "UNIT", "POOLS", "PROVISIONED", "REQUIRED SCOPE", "UNIQUE WITHIN", "SOURCE", "PHASE", "AGE"}
	}
	t := newTable(a.io.Out, headers)

	var starved []string
	for i := range classes {
		c := &classes[i]
		if c.Status.OfferingPools == 0 {
			starved = append(starved, c.Name)
		}
		name := c.Name
		if isDefaultClass(c) {
			name += " (default)"
		}
		pools := classPoolsCell(c.Status.OfferingPools, a.color.enabled)
		if wide {
			t.row(name, orDash(string(c.Spec.IPFamily)), orDash(c.Spec.ParentClassName),
				classUnitCell(c), pools, itoa(int(c.Status.ProvisionedPools)),
				orDashList(c.Status.RequiredScopeRoles), orDashList(c.Spec.UniqueWithin),
				classSourceCell(c), phaseText(string(c.Status.Phase)),
				humanDuration(c.CreationTimestamp))
		} else {
			t.row(name, orDash(string(c.Spec.IPFamily)), orDash(c.Spec.ParentClassName),
				classUnitCell(c), pools, phaseText(string(c.Status.Phase)),
				humanDuration(c.CreationTimestamp))
		}
	}
	if err := t.flush(); err != nil {
		return err
	}

	// A class with no offering pool is a live outage for every claim that names
	// it, and the table cell alone is easy to skim past. Say it in words.
	if len(starved) > 0 && !a.opts.quiet {
		_, _ = fmt.Fprintf(a.io.ErrOut,
			"\nNo pool offers %s. A claim naming a class with no pool fails; an\n"+
				"operator fixes it by adding the class to a pool's classNames.\n",
			strings.Join(quoteAll(starved), ", "))
	}
	return nil
}

// classPoolsCell renders the offering-pool count, calling out zero because that
// value means the class cannot satisfy a claim at all.
func classPoolsCell(n int32, useColor bool) string {
	if n > 0 {
		return itoa(int(n))
	}
	if useColor {
		return colorize("0 (none)", colorRed)
	}
	return "0 (none)"
}

// classSourceCell names the project a referencing class draws its policy from.
// A local definition has none.
func classSourceCell(c *ipamv1alpha1.IPClass) string {
	if c.Spec.Source == nil {
		return "—"
	}
	return c.Spec.Source.Project + "/" + c.Spec.Source.Name
}

// classUnitCell renders the size a claim of this class gets by default, with its
// allowed range when the class permits more than one size.
func classUnitCell(c *ipamv1alpha1.IPClass) string {
	if c.Spec.DefaultPrefixLength == 0 && c.Spec.AllowedPrefixLengths == nil {
		return "—"
	}
	unit := "—"
	if c.Spec.DefaultPrefixLength != 0 {
		unit = fmt.Sprintf("/%d", c.Spec.DefaultPrefixLength)
	}
	if r := c.Spec.AllowedPrefixLengths; r != nil && r.Min != r.Max {
		unit += fmt.Sprintf(" (/%d–/%d)", r.Min, r.Max)
	}
	return unit
}

func isDefaultClass(c *ipamv1alpha1.IPClass) bool {
	return c.Annotations[ipamv1alpha1.IsDefaultClassAnnotation] == "true"
}

// ---------------------------------------------------------------------------
// class show
// ---------------------------------------------------------------------------

func newClassShowCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "show <name>",
		Aliases: []string{"get", "describe"},
		Short:   "Show a class, its policy, and the pools that back it",
		Args:    cobra.ExactArgs(1),
		Example: `  datumctl ipam class show tenant-endpoint-ipv4`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, _, err := a.client()
			if err != nil {
				return err
			}
			class, err := cs.IpamV1alpha1().IPClasses().Get(context.Background(), args[0], metav1.GetOptions{})
			if err != nil {
				return classGetError(err, args[0], cs)
			}
			setClassGVK(class)
			if done, mErr := a.renderMachine(class, func() string { return "ipclass/" + class.Name }); done {
				return mErr
			}
			return a.renderClassDetail(class, cs)
		},
	}
}

func (a *app) renderClassDetail(c *ipamv1alpha1.IPClass, cs clientset.Interface) error {
	t := newTable(a.io.Out, []string{"FIELD", "VALUE"})
	name := c.Name
	if isDefaultClass(c) {
		name += fmt.Sprintf("  (default for %s)", c.Spec.IPFamily)
	}
	t.row("Name", name)
	// A referencing class holds no policy of its own: it says only that this
	// project may consume the referenced class, and claims allocate against that
	// one. Lead with it, since everything below is empty by construction.
	if s := c.Spec.Source; s != nil {
		t.row("References", fmt.Sprintf("class %s in project %s", s.Name, s.Project))
	}
	t.row("Family", orDash(string(c.Spec.IPFamily)))
	t.row("Phase", phaseText(string(c.Status.Phase)))
	if c.Spec.ParentClassName != "" {
		t.row("Carved from", c.Spec.ParentClassName)
	} else if c.Spec.Source == nil {
		t.row("Carved from", "— (drawn directly from pools that offer this class)")
	}
	t.row("Default size", classUnitCell(c))
	if r := c.Spec.AllowedPrefixLengths; r != nil {
		t.row("Allowed sizes", fmt.Sprintf("/%d–/%d", r.Min, r.Max))
	}
	// The resolved requirement is what a claim must supply, so it leads, with
	// the inputs it was derived from underneath.
	t.row("Claims must scope by", scopeRolesCell(c.Status.RequiredScopeRoles,
		"— (no scope required)"))
	t.row("Unique within", scopeRolesCell(c.Spec.UniqueWithin,
		"— (one address space platform-wide)"))
	if len(c.Spec.PoolPer) > 0 {
		t.row("Provisions a pool per", strings.Join(c.Spec.PoolPer, ", "))
	}
	if c.Spec.Strategy != "" {
		t.row("Strategy", string(c.Spec.Strategy))
	}
	if c.Spec.ReclaimPolicy != "" {
		t.row("Reclaim policy", string(c.Spec.ReclaimPolicy))
	}
	if c.Spec.RetentionLease != nil {
		t.row("Retention lease", c.Spec.RetentionLease.Duration.String())
	}
	if r := c.Spec.Routing; r.Internal != "" || r.External != "" {
		t.row("Routing", fmt.Sprintf("internal=%s external=%s",
			orDash(string(r.Internal)), orDash(string(r.External))))
	}
	if c.Spec.Provisioner != "" {
		t.row("Provisioner", c.Spec.Provisioner)
	}
	for _, k := range sortedKeys(c.Spec.Parameters) {
		t.row("Parameter "+k, c.Spec.Parameters[k])
	}
	t.row("Offering pools", classPoolsCell(c.Status.OfferingPools, a.color.enabled))
	if len(c.Spec.PoolPer) > 0 {
		t.row("Provisioned pools", itoa(int(c.Status.ProvisionedPools)))
	}
	t.row("Age", humanDuration(c.CreationTimestamp))
	if err := t.flush(); err != nil {
		return err
	}

	// Naming the pools makes "POOLS: 0" actionable: an operator sees which
	// pools do offer the class, and which one is missing the classNames entry.
	offering, provisioned := poolsForClass(cs, c.Name)
	if len(offering) > 0 {
		_, _ = fmt.Fprintf(a.io.Out, "\nPools offering this class:\n")
		for _, p := range offering {
			_, _ = fmt.Fprintf(a.io.Out, "  %s\n", p)
		}
	} else if !a.opts.quiet && c.Spec.Source == nil {
		// A referencing class is backed through the class it names, in another
		// project, so a local pool listing says nothing about whether it works.
		_, _ = fmt.Fprintf(a.io.ErrOut,
			"\nNo pool offers %q, so every claim naming it fails. An operator adds the\n"+
				"class to a pool with:\n       datumctl ipam pool create <name> --cidr <cidr> --class %s\n",
			c.Name, c.Name)
	}
	if len(provisioned) > 0 {
		_, _ = fmt.Fprintf(a.io.Out, "\nPools this class has provisioned:\n")
		for _, p := range provisioned {
			_, _ = fmt.Fprintf(a.io.Out, "  %s\n", p)
		}
	}
	return nil
}

// poolsForClass splits the visible pools into those that offer the class
// (spec.classNames) and those the class provisioned (spec.classRef). Best-effort:
// a listing failure yields empty slices rather than an error, because this is
// context on top of a detail view that already succeeded.
func poolsForClass(cs clientset.Interface, className string) (offering, provisioned []string) {
	list, err := cs.IpamV1alpha1().IPPools().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, nil
	}
	for i := range list.Items {
		p := &list.Items[i]
		line := poolSummaryLine(p)
		for _, n := range p.Spec.ClassNames {
			if n == className {
				offering = append(offering, line)
				break
			}
		}
		if p.Spec.ClassRef != nil && p.Spec.ClassRef.Name == className {
			provisioned = append(provisioned, line)
		}
	}
	sort.Strings(offering)
	sort.Strings(provisioned)
	return offering, provisioned
}

// poolSummaryLine is the one-line pool descriptor used wherever pools appear as
// context under another resource.
func poolSummaryLine(p *ipamv1alpha1.IPPool) string {
	cidr := p.Status.AllocatedCIDR
	if cidr == "" {
		cidr = p.Spec.CIDR
	}
	line := fmt.Sprintf("%s  %s  %.0f%% used", p.Name, orDash(cidr), poolUtilization(p))
	if len(p.Spec.Scope) > 0 {
		line += "  [" + formatScope(p.Spec.Scope) + "]"
	}
	return line
}

// classGetError turns a 404 on a class into a message that lists the classes
// that do exist, since a mistyped class name is the likeliest cause.
func classGetError(err error, name string, cs clientset.Interface) error {
	if !apierrors.IsNotFound(err) {
		return classifyError(err)
	}
	ce := newCLIError(exitNotFound, fmt.Sprintf("no class named %q", name))
	if names := visibleClassNames(cs); len(names) > 0 {
		return ce.withFix("classes that exist:\n       " + strings.Join(names, "\n       ")).withCause(err)
	}
	return ce.withFix("list classes:\n       datumctl ipam class list").withCause(err)
}

// visibleClassNames returns the names of classes visible to the caller, for
// "did you mean" context. Best-effort; nil on failure.
func visibleClassNames(cs clientset.Interface) []string {
	list, err := cs.IpamV1alpha1().IPClasses().List(context.Background(), metav1.ListOptions{})
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
