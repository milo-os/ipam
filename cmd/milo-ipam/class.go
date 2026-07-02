package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func setClassGVK(c *ipamv1alpha1.IPClass) {
	c.APIVersion = apiVersion
	c.Kind = "IPClass"
}

// classIsDefault reports whether a class is marked the platform default.
func classIsDefault(c *ipamv1alpha1.IPClass) bool {
	return c.Annotations[ipamv1alpha1.IsDefaultClassAnnotation] == "true"
}

func newClassCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "class",
		Short: "Browse the catalog of address-space policies (IPClass)",
		Long: `An IPClass names a kind of address space and the policy for handing it out —
which family, allowed prefix sizes, placement strategy, and reclaim behavior.
Claim from a class by name with "prefix claim --class <name>"; you never need to
know which pool backs it.`,
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
	var selector string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the address-space classes you can claim from",
		Args:    cobra.NoArgs,
		Example: `  datumctl ipam class list
  datumctl ipam class list -o wide`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, _, err := a.client()
			if err != nil {
				return err
			}
			list, err := cs.IpamV1alpha1().IPClasses().List(context.Background(), metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return classifyError(err)
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
				for i := range list.Items {
					_, _ = fmt.Fprintf(a.io.Out, "ipclass/%s\n", list.Items[i].Name)
				}
				return nil
			}
			return a.renderClassTable(list.Items)
		},
	}
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "Label selector to filter classes")
	return cmd
}

func (a *app) renderClassTable(classes []ipamv1alpha1.IPClass) error {
	if len(classes) == 0 {
		if !a.opts.quiet {
			_, _ = fmt.Fprintln(a.io.ErrOut, "No classes found.")
		}
		return nil
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })

	wide := a.opts.output == outputWide
	headers := []string{"NAME", "FAMILY", "PREFIXES", "RECLAIM", "DEFAULT"}
	if wide {
		headers = []string{"NAME", "FAMILY", "PREFIXES", "RECLAIM", "DEFAULT", "PROVISIONER", "VISIBILITY", "AGE"}
	}
	t := newTable(a.io.Out, headers)
	for i := range classes {
		c := &classes[i]
		def := ""
		if classIsDefault(c) {
			def = "*"
		}
		if wide {
			t.row(c.Name, orDash(string(c.Spec.IPFamily)), classPrefixRange(c),
				orDash(string(c.Spec.ReclaimPolicy)), def,
				orDash(c.Spec.Provisioner), orDash(c.Spec.Visibility),
				humanDuration(c.CreationTimestamp))
		} else {
			t.row(c.Name, orDash(string(c.Spec.IPFamily)), classPrefixRange(c),
				orDash(string(c.Spec.ReclaimPolicy)), def)
		}
	}
	return t.flush()
}

// classPrefixRange renders a class's allowed prefix bounds as "/24 – /28", or a
// single bound / dash when only one or neither is set.
func classPrefixRange(c *ipamv1alpha1.IPClass) string {
	lo := c.Spec.AllowedPrefixLengths.Min
	hi := c.Spec.AllowedPrefixLengths.Max
	switch {
	case lo > 0 && hi > 0:
		return fmt.Sprintf("/%d – /%d", lo, hi)
	case lo > 0:
		return fmt.Sprintf("≥ /%d", lo)
	case hi > 0:
		return fmt.Sprintf("≤ /%d", hi)
	default:
		return "—"
	}
}

// ---------------------------------------------------------------------------
// class show
// ---------------------------------------------------------------------------

func newClassShowCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "show <name>",
		Aliases: []string{"get", "describe"},
		Short:   "Show a class's policy in detail",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, _, err := a.client()
			if err != nil {
				return err
			}
			class, err := cs.IpamV1alpha1().IPClasses().Get(context.Background(), args[0], metav1.GetOptions{})
			if err != nil {
				return classGetError(err, args[0])
			}
			setClassGVK(class)
			if done, err := a.renderMachine(class, func() string { return "ipclass/" + class.Name }); done {
				return err
			}
			return a.renderClassDetail(class)
		},
	}
	return cmd
}

func (a *app) renderClassDetail(c *ipamv1alpha1.IPClass) error {
	t := newTable(a.io.Out, []string{"FIELD", "VALUE"})
	t.row("Name", c.Name)
	t.row("Family", orDash(string(c.Spec.IPFamily)))
	t.row("Provisioner", orDash(c.Spec.Provisioner))
	t.row("Strategy", orDash(string(c.Spec.Strategy)))
	t.row("Allowed prefixes", classPrefixRange(c))
	if c.Spec.DefaultPrefixLength > 0 {
		t.row("Default prefix", fmt.Sprintf("/%d", c.Spec.DefaultPrefixLength))
	}
	t.row("Reclaim policy", orDash(string(c.Spec.ReclaimPolicy)))
	t.row("Visibility", orDash(c.Spec.Visibility))
	if classIsDefault(c) {
		t.row("Default", "yes")
	}
	for k, v := range c.Spec.Parameters {
		t.row("Parameter "+k, v)
	}
	t.row("Age", humanDuration(c.CreationTimestamp))
	return t.flush()
}

// classGetError adds IPAM context to a failed class Get: a 404 becomes a clear
// "no such class" message with a pointer to the catalog.
func classGetError(err error, name string) error {
	if apierrors.IsNotFound(err) {
		return newCLIError(exitNotFound, fmt.Sprintf("class %q not found", name)).
			withFix("list available classes:\n       datumctl ipam class list").withCause(err)
	}
	return classifyError(err)
}
