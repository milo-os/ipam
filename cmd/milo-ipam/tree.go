package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func newPoolTreeCommand(a *app) *cobra.Command {
	var withAllocations bool
	cmd := &cobra.Command{
		Use:   "tree [name]",
		Short: "Render the pool hierarchy as a tree",
		Args:  cobra.MaximumNArgs(1),
		Long: `Render the parentPoolRef hierarchy as an indented tree, with utilization at
every level. Pass a pool name to root the tree there; omit it to show all roots.
The layout is computed client-side from data the API already returns.`,
		Example: `  datumctl ipam pool tree
  datumctl ipam pool tree prod-backbone --allocations`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			list, err := cs.IpamV1alpha1().IPPools().List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return classifyError(err)
			}
			pools := list.Items

			// Optionally gather allocations as leaves under their pool. Allocations
			// rather than claims: they are what actually occupies space in a pool,
			// including reservations and retained addresses no claim points at.
			leaves := map[string][]ipamv1alpha1.IPAllocation{}
			if withAllocations {
				if allocs, aErr := cs.IpamV1alpha1().IPAllocations(ns).List(context.Background(), metav1.ListOptions{}); aErr == nil {
					for i := range allocs.Items {
						poolName := allocs.Items[i].Spec.PoolRef.Name
						leaves[poolName] = append(leaves[poolName], allocs.Items[i])
					}
				}
			}

			// JSON/YAML just dump the pool list — the tree is a presentation.
			switch a.opts.output {
			case outputJSON:
				for i := range pools {
					setPoolGVK(&pools[i])
				}
				return encodeJSON(a.io.Out, list)
			case outputYAML:
				for i := range pools {
					setPoolGVK(&pools[i])
				}
				return encodeYAML(a.io.Out, list)
			}

			roots := buildRoots(pools, argOrEmpty(args))
			if len(roots) == 0 {
				if name := argOrEmpty(args); name != "" {
					return newCLIError(exitNotFound, fmt.Sprintf("pool %q is not visible in the active project", name)).
						withFix("list visible pools:\n       datumctl ipam pool list")
				}
				_, _ = fmt.Fprintln(a.io.ErrOut, "No pools found.")
				return nil
			}

			byParent := indexByParent(pools)
			tw := tabwriter.NewWriter(a.io.Out, 0, 2, 2, ' ', 0)
			for _, root := range roots {
				a.printTreeNode(tw, root, byParent, leaves, "", true, true)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&withAllocations, "allocations", false, "Include the allocations held under each pool")
	return cmd
}

func argOrEmpty(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

// buildRoots returns the top-level pools to print. With a name, the single named
// pool is the root; otherwise every pool with no (or an unresolved) parent.
func buildRoots(pools []ipamv1alpha1.IPPool, name string) []ipamv1alpha1.IPPool {
	if name != "" {
		for i := range pools {
			if pools[i].Name == name {
				return []ipamv1alpha1.IPPool{pools[i]}
			}
		}
		return nil
	}
	known := map[string]bool{}
	for i := range pools {
		known[pools[i].Name] = true
	}
	var roots []ipamv1alpha1.IPPool
	for i := range pools {
		p := pools[i].Spec.ParentPoolRef
		if p == nil || !known[p.Name] {
			roots = append(roots, pools[i])
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Name < roots[j].Name })
	return roots
}

func indexByParent(pools []ipamv1alpha1.IPPool) map[string][]ipamv1alpha1.IPPool {
	idx := map[string][]ipamv1alpha1.IPPool{}
	for i := range pools {
		if p := pools[i].Spec.ParentPoolRef; p != nil {
			idx[p.Name] = append(idx[p.Name], pools[i])
		}
	}
	for k := range idx {
		sort.Slice(idx[k], func(i, j int) bool { return idx[k][i].Name < idx[k][j].Name })
	}
	return idx
}

// printTreeNode renders one pool and recurses into its children and (optional)
// leaf allocations. indent is the accumulated indentation; isLast controls the
// connector glyph; isRoot suppresses a connector for top-level nodes.
func (a *app) printTreeNode(
	w io.Writer,
	pool ipamv1alpha1.IPPool,
	byParent map[string][]ipamv1alpha1.IPPool,
	leaves map[string][]ipamv1alpha1.IPAllocation,
	indent string,
	isLast bool,
	isRoot bool,
) {
	connector := ""
	childIndent := indent
	if !isRoot {
		if isLast {
			connector = "└─ "
			childIndent = indent + "   "
		} else {
			connector = "├─ "
			childIndent = indent + "│  "
		}
	}

	cidr := pool.Status.AllocatedCIDR
	if cidr == "" {
		cidr = pool.Spec.CIDR
	}
	pct := poolUtilization(&pool)
	kind := "child pool"
	if pool.Spec.ParentPoolRef == nil {
		kind = "root pool"
	}
	line := fmt.Sprintf("%s%s\t%s\t%s\t%.0f%% used\t(%s)",
		indent+connector, pool.Name, orDash(cidr), orDash(string(poolFamily(&pool))), pct, kind)
	_, _ = fmt.Fprintln(w, line)

	children := byParent[pool.Name]
	allocs := leaves[pool.Name]

	for i := range children {
		last := i == len(children)-1 && len(allocs) == 0
		a.printTreeNode(w, children[i], byParent, leaves, childIndent, last, false)
	}
	for i := range allocs {
		last := i == len(allocs)-1
		a.printAllocationLeaf(w, &allocs[i], childIndent, last)
	}
}

func (a *app) printAllocationLeaf(w io.Writer, al *ipamv1alpha1.IPAllocation, indent string, isLast bool) {
	connector := "├─ "
	if isLast {
		connector = "└─ "
	}
	holder := formatObjectRef(al.Spec.OwnerRef)
	if holder == "—" {
		holder = allocationClaimName(al)
	}
	_, _ = fmt.Fprintf(w, "%s%s\t%s\t%s\t─\t(%s · %s)\n",
		indent+connector, al.Name, orDash(allocationAddress(al)),
		orDash(string(al.Spec.IPFamily)), orDash(al.Spec.ClassName), holder)
}
