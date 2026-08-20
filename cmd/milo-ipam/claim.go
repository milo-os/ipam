package main

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
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

func newClaimCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use: "claim",
		// `prefix` stays as an alias so that habit still lands somewhere, but
		// the command's vocabulary follows the API.
		Aliases: []string{"claims", "prefix"},
		Short:   "Request, inspect, and release addresses (IPClaim)",
		Long: `A claim is a long-lived request for an address of a named class. It binds one
allocation when it is created and holds it for as long as the claim exists —
the same relationship a PersistentVolumeClaim has with its volume.

You name a class and the scope the address is for. You do not name a pool, a
CIDR, or a location: which pool serves a claim follows from its class and its
scope, and the server resolves it into status.poolRef.`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return c.Help()
			}
			return unknownSubcommandError(c, args[0])
		},
	}
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(
		newClaimCreateCommand(a),
		newClaimListCommand(a),
		newClaimShowCommand(a),
		newClaimReleaseCommand(a),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// claim create
// ---------------------------------------------------------------------------

type claimOptions struct {
	class         string
	family        string
	scope         []string
	address       string
	prefixLength  int
	name          string
	owner         string
	reclaimPolicy string
	dryRun        bool
}

func newClaimCreateCommand(a *app) *cobra.Command {
	o := &claimOptions{}
	cmd := &cobra.Command{
		Use:     "create",
		Aliases: []string{"request"},
		Short:   "Claim an address of a class and get it back synchronously",
		Long: `Claim an address. The allocated address is returned in the same call.

You name a class and the scope the address is for. The class decides the size,
the routing, and the space it comes from; the scope decides which pool inside
that space serves this claim and which other allocations it must not collide
with. A class names the scope roles it needs — see ` + "`class show`" + ` — and a claim
missing one of them is rejected rather than falling back to a wider comparison.

Allocation is not idempotent: each claim consumes space. Pass a stable --name to
make retries safe — a retried claim with the same name returns the existing
allocation instead of consuming a second address. That covers a retry, not the
name: reusing a name for a different class or scope is refused, because the
address it would hand back was allocated for a different question.

` + scopeGrammarHelp(),
		Example: `  # An address of a class, for a network in a location
  datumctl ipam claim create --class tenant-endpoint-ipv4 \
    --scope network=default --scope location=us-central-1

  # Idempotent (safe to retry)
  datumctl ipam claim create --class public-unicast-ipv4 --name web-vip

  # The default class for a family
  datumctl ipam claim create --family ipv6 --scope network=default

  # A role the CLI does not know needs a qualified reference
  datumctl ipam claim create --class fabric-link-ipv6 \
    --scope site=Site.infra.example.com/dc-1

  # Preview without consuming space
  datumctl ipam claim create --class tenant-endpoint-ipv4 \
    --scope network=default --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaimCreate(a, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.class, "class", "", "Class of address to claim; list them with: datumctl ipam class list")
	f.StringVar(&o.family, "family", "", "Address family: ipv4|ipv6. Selects the default class when --class is omitted")
	f.StringArrayVar(&o.scope, "scope", nil, scopeFlagUsage("References this claim is made for"))
	f.StringVar(&o.address, "address", "", "Bind this specific address instead of letting the allocator choose")
	f.IntVar(&o.prefixLength, "prefix-length", 0, "Block size in bits, within the class's allowed range. Omit to use the class default")
	f.StringVar(&o.name, "name", "", "Stable claim name; reusing it makes retries idempotent")
	f.StringVar(&o.owner, "owner", "", "Consumer object this address is held for, as Kind.apiGroup/name")
	f.StringVar(&o.reclaimPolicy, "reclaim-policy", "", "Override the class default: Delete|Retain")
	f.BoolVar(&o.dryRun, "dry-run", false, "Preview the allocation without consuming space")
	return cmd
}

func runClaimCreate(a *app, o *claimOptions) error {
	claim, err := buildClaim(o, "")
	if err != nil {
		return err
	}

	cs, ns, err := a.client()
	if err != nil {
		return err
	}
	claim.Namespace = ns

	// Idempotency first: reading back a claim that already exists must not
	// depend on its class still being readable, or a retry starts failing the
	// moment an operator retires the class the claim was made under.
	if o.name != "" {
		existing, gErr := cs.IpamV1alpha1().IPClaims(ns).Get(context.Background(), o.name, metav1.GetOptions{})
		if gErr == nil {
			// Idempotency is a promise about a *retry*, not about the name. A
			// reused name carrying a different request is a different question,
			// and returning the old answer to it is silent: the success output
			// renders the existing claim's class and scope, so it reads as
			// confirmation of what was asked for rather than as a substitution.
			if diffs := claimRequestDiff(claim, existing); len(diffs) > 0 {
				return claimNameReusedError(o.name, diffs)
			}
			a.vlogf("claim %q already exists; returning the existing allocation (idempotent)", o.name)
			return a.renderClaimResult(existing, cs, true)
		}
		if !apierrors.IsNotFound(gErr) {
			return classifyError(gErr)
		}
	}

	if o.dryRun {
		// Server-side dry-run: the apiserver resolves the pool and computes the
		// real next address inside the allocation transaction and rolls back,
		// persisting nothing. That makes the preview exact rather than a
		// client-side guess, down to which pool the scope resolved to.
		dryClaim, dErr := cs.IpamV1alpha1().IPClaims(ns).Create(context.Background(), claim,
			metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if dErr != nil {
			return a.claimCreateError(dErr, o, cs)
		}
		return a.renderClaimDryRun(dryClaim, cs)
	}

	created, cErr := cs.IpamV1alpha1().IPClaims(ns).Create(context.Background(), claim, metav1.CreateOptions{})
	if cErr != nil {
		return a.claimCreateError(cErr, o, cs)
	}
	return a.renderClaimResult(created, cs, false)
}

// buildClaim validates the flags and assembles the IPClaim to submit. It takes
// no client, so it is the unit-testable half of the create path.
func buildClaim(o *claimOptions, ns string) (*ipamv1alpha1.IPClaim, error) {
	if o.class == "" && o.family == "" {
		return nil, usageErrorf(
			"a claim needs a class: pass --class <name>, or --family ipv4|ipv6 to use the default class.\n" +
				"       see what you can claim:\n" +
				"       datumctl ipam class list")
	}

	var family ipamv1alpha1.IPFamily
	if o.family != "" {
		fam, err := parseFamily(o.family)
		if err != nil {
			return nil, err
		}
		family = fam
	}

	scope, err := buildScope(o.scope)
	if err != nil {
		return nil, err
	}

	owner, err := parseObjectRef(o.owner)
	if err != nil {
		return nil, err
	}

	policy, err := parseReclaimPolicy(o.reclaimPolicy)
	if err != nil {
		return nil, err
	}

	if o.address != "" {
		if _, aErr := netip.ParseAddr(o.address); aErr != nil {
			if _, pErr := netip.ParsePrefix(o.address); pErr != nil {
				return nil, usageErrorf("invalid --address %q: expected an IP address or CIDR", o.address)
			}
		}
	}

	claim := &ipamv1alpha1.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns},
		Spec: ipamv1alpha1.IPClaimSpec{
			ClassName:     o.class,
			IPFamily:      family,
			Address:       o.address,
			Scope:         scope,
			OwnerRef:      owner,
			ReclaimPolicy: policy,
		},
	}
	if o.prefixLength > 0 {
		claim.Spec.PrefixLength = int32Ptr(int32(o.prefixLength))
	}

	// A stable --name makes retries idempotent. When omitted, synthesize a unique
	// client-side name: the IPAM aggregated apiserver does not implement
	// server-side metadata.generateName.
	if o.name != "" {
		claim.Name = o.name
	} else {
		claim.Name = generateResourceName("claim")
	}
	setClaimGVK(claim)
	return claim, nil
}

// parseReclaimPolicy validates the --reclaim-policy value against the API enum,
// accepting any casing.
func parseReclaimPolicy(s string) (ipamv1alpha1.ReclaimPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "delete":
		return ipamv1alpha1.ReclaimDelete, nil
	case "retain":
		return ipamv1alpha1.ReclaimRetain, nil
	default:
		return "", usageErrorf("invalid --reclaim-policy %q: must be Delete or Retain", s)
	}
}

// claimRequestDiff reports the fields where a request disagrees with the claim
// already holding its name, as "field: requested X, existing Y" lines.
//
// Silence on either side is not a difference, and both directions matter.
//
// A request that omits a flag is a question not asked, so an omission can never
// mismatch. An existing claim silent on a field is not evidence either. Several
// of these fields are derivable: a claim that named a class carries no family
// in its spec, and one that named `--family` carries no class. Comparing a
// stated value against a derived one that spec never recorded invents a
// difference, on exactly the ordinary retry this path exists to serve.
//
// Scope is the exception, and the comparison covers it whenever the request
// states one. The server never derives a scope, so an empty scope on the
// existing claim means it really was made without one, in a different address
// space.
func claimRequestDiff(requested, existing *ipamv1alpha1.IPClaim) []string {
	var diffs []string
	add := func(field, want, got string) {
		diffs = append(diffs, fmt.Sprintf("%s: requested %s, existing claim has %s", field, want, got))
	}
	// stated compares two values only when both sides say something.
	stated := func(field, want, got string) {
		if want != "" && got != "" && want != got {
			add(field, want, got)
		}
	}

	stated("class", requested.Spec.ClassName, existing.Spec.ClassName)
	stated("family", string(requested.Spec.IPFamily), string(existing.Spec.IPFamily))
	stated("address", requested.Spec.Address, existing.Spec.Address)
	stated("reclaim policy", string(requested.Spec.ReclaimPolicy), string(existing.Spec.ReclaimPolicy))
	if r, e := requested.Spec.PrefixLength, existing.Spec.PrefixLength; r != nil && e != nil && *r != *e {
		add("prefix length", fmt.Sprintf("/%d", *r), fmt.Sprintf("/%d", *e))
	}
	// Scope decides which address space the claim lands in, so a difference here
	// matters most: the returned address is unique within a space the caller did
	// not ask about. Compare the scope whole rather than per role, because a
	// scope with an extra or missing role differs as much as one with a changed
	// value.
	if len(requested.Spec.Scope) > 0 && !reflect.DeepEqual(requested.Spec.Scope, existing.Spec.Scope) {
		add("scope", formatScope(requested.Spec.Scope), formatScope(existing.Spec.Scope))
	}
	return diffs
}

// claimNameReusedError refuses a create whose name is already held by a claim
// asking for something else.
//
// Conflict rather than usage: the flags are well-formed and the request is
// legitimate, it is the name that is taken. That matches the immutability
// refusals elsewhere in this API, and leaves exit 2 meaning "you typed something
// wrong" rather than "the cluster is in a state you didn't expect".
func claimNameReusedError(name string, diffs []string) *cliError {
	var fix strings.Builder
	fix.WriteString("a retry with the same --name returns the existing allocation; this request\n")
	fix.WriteString("       differs from it, so it would have been answered with an address for a\n")
	fix.WriteString("       different question:")
	for _, d := range diffs {
		fix.WriteString("\n       " + d)
	}
	fix.WriteString("\n\n       Claim it under a different --name, or release the existing claim first:")
	fix.WriteString("\n       datumctl ipam claim release " + name)
	return newCLIError(exitConflict, fmt.Sprintf(
		"claim %q already exists and was made for a different request", name)).withFix(fix.String())
}

// claimAddress is the address a claim holds: the single-address form when the
// class hands out hosts, the block otherwise.
func claimAddress(c *ipamv1alpha1.IPClaim) string {
	if c.Status.Address != "" {
		return c.Status.Address
	}
	return c.Status.AllocatedCIDR
}

// claimPoolName is the pool the allocator resolved for a claim. It lives in
// status because the consumer does not choose it.
func claimPoolName(c *ipamv1alpha1.IPClaim) string {
	if c.Status.PoolRef != nil {
		return c.Status.PoolRef.Name
	}
	return "—"
}

// renderClaimResult prints the success output for a created or pre-existing
// claim, re-fetching the resolved pool for the utilization line.
func (a *app) renderClaimResult(claim *ipamv1alpha1.IPClaim, cs clientset.Interface, idempotent bool) error {
	setClaimGVK(claim)
	if done, err := a.renderMachine(claim, func() string { return "ipclaim/" + claim.Name }); done {
		return err
	}
	addr := claimAddress(claim)
	if a.opts.quiet {
		// Script-friendly: just the address (the one fact the caller came for).
		_, _ = fmt.Fprintln(a.io.Out, addr)
		return nil
	}

	verb := "Claimed"
	if idempotent {
		verb = "Reused existing claim for"
	}
	_, _ = fmt.Fprintf(a.io.Out, "%s %s %s\n", successPrefix(a.color), verb, orDash(addr))
	_, _ = fmt.Fprintf(a.io.Out, "  claim:       %s\n", claim.Name)
	_, _ = fmt.Fprintf(a.io.Out, "  class:       %s\n", orDash(claim.Spec.ClassName))
	if len(claim.Spec.Scope) > 0 {
		_, _ = fmt.Fprintf(a.io.Out, "  scope:       %s\n", formatScope(claim.Spec.Scope))
	}
	if claim.Status.BoundAllocationRef != nil {
		_, _ = fmt.Fprintf(a.io.Out, "  allocation:  %s\n", claim.Status.BoundAllocationRef.Name)
	}
	// The pool is a result, not an input, so it is reported rather than echoed.
	if claim.Status.PoolRef != nil {
		line := claim.Status.PoolRef.Name
		if p, err := cs.IpamV1alpha1().IPPools().Get(context.Background(), claim.Status.PoolRef.Name, metav1.GetOptions{}); err == nil {
			cidr := p.Status.AllocatedCIDR
			if cidr == "" {
				cidr = p.Spec.CIDR
			}
			line = fmt.Sprintf("%s (%s, %.0f%% used)", p.Name, orDash(cidr), poolUtilization(p))
		}
		_, _ = fmt.Fprintf(a.io.Out, "  from pool:   %s\n", line)
	}
	_, _ = fmt.Fprintf(a.io.Out, "  org/project: %s\n", a.scopeLine(claim.Namespace))
	return nil
}

func (a *app) renderClaimDryRun(dryClaim *ipamv1alpha1.IPClaim, cs clientset.Interface) error {
	// Machine formats emit the would-be claim object, carrying the computed
	// address and the pool the scope resolved to.
	setClaimGVK(dryClaim)
	if done, err := a.renderMachine(dryClaim, func() string { return "ipclaim/" + dryClaim.Name }); done {
		return err
	}
	_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — no allocation was made.")
	if addr := claimAddress(dryClaim); addr != "" {
		_, _ = fmt.Fprintf(a.io.ErrOut, "Would claim:   %s of class %s\n", addr, orDash(dryClaim.Spec.ClassName))
	} else {
		_, _ = fmt.Fprintf(a.io.ErrOut, "Would claim:   an address of class %s\n", orDash(dryClaim.Spec.ClassName))
	}
	if len(dryClaim.Spec.Scope) > 0 {
		_, _ = fmt.Fprintf(a.io.ErrOut, "Scope:         %s\n", formatScope(dryClaim.Spec.Scope))
	}
	if ref := dryClaim.Status.PoolRef; ref != nil {
		line := ref.Name
		if p, err := cs.IpamV1alpha1().IPPools().Get(context.Background(), ref.Name, metav1.GetOptions{}); err == nil {
			line = fmt.Sprintf("%s (%.0f%% used)", p.Name, poolUtilization(p))
		}
		_, _ = fmt.Fprintf(a.io.ErrOut, "Resolved pool: %s\n", line)
	}
	return nil
}

// claimCreateError turns a failed claim Create into an IPAM-aware error. The
// signature case is 507 (exhaustion), which is re-decorated with the class, the
// scope, and the pools that back the class.
func (a *app) claimCreateError(err error, o *claimOptions, cs clientset.Interface) error {
	switch httpStatusCode(err) {
	case 507:
		return exhaustionError(o.class, formatScope(mustScope(o.scope)), classPoolLines(cs, o.class), err)
	case 404:
		if o.class != "" {
			return classGetError(apierrors.NewNotFound(
				ipamv1alpha1.SchemeGroupVersion.WithResource("ipclasses").GroupResource(), o.class), o.class, cs)
		}
		return classifyError(err)
	case 409:
		return newCLIError(exitConflict, fmt.Sprintf("conflict: %s", apiMessage(err))).
			withFix("a claim with that name already exists; pick a different --name.").withCause(err)
	}
	return classifyError(err)
}

// mustScope re-parses the scope flags for error decoration. The values already
// parsed once on the way in, so a failure here is not worth reporting again.
func mustScope(args []string) map[string]ipamv1alpha1.ScopeRef {
	scope, err := buildScope(args)
	if err != nil {
		return nil
	}
	return scope
}

// classPoolLines summarizes the pools backing a class, for exhaustion messages.
func classPoolLines(cs clientset.Interface, className string) []string {
	if className == "" {
		return nil
	}
	offering, provisioned := poolsForClass(cs, className)
	return append(offering, provisioned...)
}

// ---------------------------------------------------------------------------
// claim list
// ---------------------------------------------------------------------------

func newClaimListCommand(a *app) *cobra.Command {
	var (
		class     string
		pool      string
		scopeArgs []string
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List claims and the addresses they hold",
		Long: `List claims and the addresses they hold.

--pool filters on status.poolRef, which is the pool the allocator resolved. It
is a read-side question — "what is drawing on this pool" — not a way to pick one.

` + scopeGrammarHelp(),
		Args: cobra.NoArgs,
		Example: `  datumctl ipam claim list
  datumctl ipam claim list --class tenant-endpoint-ipv4 -o wide
  datumctl ipam claim list --scope network=default`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			wantScope, err := buildScope(scopeArgs)
			if err != nil {
				return err
			}
			list, err := cs.IpamV1alpha1().IPClaims(ns).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return classifyError(err)
			}
			items := list.Items[:0:0]
			for i := range list.Items {
				c := &list.Items[i]
				if class != "" && c.Spec.ClassName != class {
					continue
				}
				if pool != "" && claimPoolName(c) != pool {
					continue
				}
				if !scopeContains(c.Spec.Scope, wantScope) {
					continue
				}
				items = append(items, *c)
			}
			list.Items = items
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
			return a.renderClaimTable(items)
		},
	}
	f := cmd.Flags()
	f.StringVar(&class, "class", "", "Only show claims of this class")
	f.StringVar(&pool, "pool", "", "Only show claims the allocator drew from this pool (status.poolRef)")
	f.StringArrayVar(&scopeArgs, "scope", nil, scopeFlagUsage("Only show claims whose scope includes these references"))
	return cmd
}

func (a *app) renderClaimTable(claims []ipamv1alpha1.IPClaim) error {
	if len(claims) == 0 {
		if !a.opts.quiet {
			_, _ = fmt.Fprintln(a.io.ErrOut, "No claims found.")
		}
		return nil
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Name < claims[j].Name })

	wide := a.opts.output == outputWide
	headers := []string{"NAME", "CLASS", "ADDRESS", "POOL", "SCOPE", "PHASE", "AGE"}
	if wide {
		headers = []string{"NAME", "CLASS", "ADDRESS", "POOL", "SCOPE", "PHASE", "ALLOCATION", "OWNER", "RECLAIM", "AGE"}
	}
	t := newTable(a.io.Out, headers)
	for i := range claims {
		c := &claims[i]
		if wide {
			alloc := "—"
			if c.Status.BoundAllocationRef != nil {
				alloc = c.Status.BoundAllocationRef.Name
			}
			t.row(c.Name, orDash(c.Spec.ClassName), orDash(claimAddress(c)), claimPoolName(c),
				formatScope(c.Spec.Scope), phaseText(string(c.Status.Phase)),
				alloc, formatObjectRef(c.Spec.OwnerRef), orDash(string(c.Spec.ReclaimPolicy)),
				humanDuration(c.CreationTimestamp))
		} else {
			t.row(c.Name, orDash(c.Spec.ClassName), orDash(claimAddress(c)), claimPoolName(c),
				formatScope(c.Spec.Scope), phaseText(string(c.Status.Phase)),
				humanDuration(c.CreationTimestamp))
		}
	}
	return t.flush()
}

// phaseText renders a phase as plain, color-independent text.
func phaseText(p string) string {
	if p == "" {
		return "—"
	}
	return strings.ToUpper(p)
}

// ---------------------------------------------------------------------------
// claim show
// ---------------------------------------------------------------------------

func newClaimShowCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "show <name|address>",
		Aliases: []string{"get", "describe"},
		Short:   "Show a claim by name, or by the address it holds",
		Args:    cobra.ExactArgs(1),
		Example: `  datumctl ipam claim show web-vip
  datumctl ipam claim show 198.51.100.11`,
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
			if done, mErr := a.renderMachine(claim, func() string { return "ipclaim/" + claim.Name }); done {
				return mErr
			}
			return a.renderClaimDetail(claim)
		},
	}
}

// resolveClaim looks up a claim by name, or by the address or block it holds
// when the argument parses as an IP or CIDR.
func resolveClaim(cs clientset.Interface, ns, arg string) (*ipamv1alpha1.IPClaim, error) {
	if looksLikeAddress(arg) {
		list, err := cs.IpamV1alpha1().IPClaims(ns).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return nil, classifyError(err)
		}
		for i := range list.Items {
			c := &list.Items[i]
			if c.Status.Address == arg || c.Status.AllocatedCIDR == arg {
				return c, nil
			}
		}
		// This search comes up empty for one of two reasons. An address held
		// with no claim behind it, released under Retain, is invisible to a
		// claim search and is exactly what `address show` answers in this
		// namespace, so the message hands off to it. An address allocated in
		// another project has no command to hand off to, since `address show`
		// reads the same tenant keyspace, so the message states that instead.
		return nil, newCLIError(exitNotFound,
			fmt.Sprintf("no claim in namespace %q holds %q", ns, arg)).
			withFix("an address held without a claim — released under reclaim policy Retain —\n" +
				"       is found by the reverse lookup instead:\n" +
				"       datumctl ipam address show " + arg + "\n\n" +
				"       If the address belongs to another namespace or another project, it is\n" +
				"       not visible from here and no flag widens the search.")
	}
	claim, err := cs.IpamV1alpha1().IPClaims(ns).Get(context.Background(), arg, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, newCLIError(exitNotFound, fmt.Sprintf("claim %q not found in this project", arg)).
				withFix("list claims:\n       datumctl ipam claim list").withCause(err)
		}
		return nil, classifyError(err)
	}
	return claim, nil
}

// looksLikeAddress reports whether an argument is an IP address or a CIDR rather
// than a resource name.
func looksLikeAddress(arg string) bool {
	if _, err := netip.ParseAddr(arg); err == nil {
		return true
	}
	_, err := netip.ParsePrefix(arg)
	return err == nil
}

func (a *app) renderClaimDetail(c *ipamv1alpha1.IPClaim) error {
	t := newTable(a.io.Out, []string{"FIELD", "VALUE"})
	t.row("Name", c.Name)
	t.row("Class", orDash(c.Spec.ClassName))
	t.row("Address", orDash(claimAddress(c)))
	if c.Status.Address != "" && c.Status.AllocatedCIDR != "" && c.Status.Address != c.Status.AllocatedCIDR {
		t.row("Block", c.Status.AllocatedCIDR)
	}
	t.row("Phase", phaseText(string(c.Status.Phase)))
	t.row("Pool", claimPoolName(c))
	if c.Spec.IPFamily != "" {
		t.row("Family", string(c.Spec.IPFamily))
	}
	if c.Spec.PrefixLength != nil {
		t.row("Requested size", fmt.Sprintf("/%d", *c.Spec.PrefixLength))
	}
	for _, role := range sortedScopeRoles(c.Spec.Scope) {
		t.row("Scope "+role, formatScopeRef(c.Spec.Scope[role]))
	}
	if c.Status.BoundAllocationRef != nil {
		t.row("Allocation", c.Status.BoundAllocationRef.Name)
	}
	if c.Spec.ReclaimPolicy != "" {
		t.row("Reclaim policy", string(c.Spec.ReclaimPolicy))
	}
	if c.Spec.OwnerRef != nil {
		t.row("Held for", formatObjectRef(c.Spec.OwnerRef))
	}
	t.row("Age", humanDuration(c.CreationTimestamp))
	if err := t.flush(); err != nil {
		return err
	}
	return a.renderConditions(c.Status.Conditions)
}

// renderConditions prints any non-True conditions, which is where a claim that
// did not bind explains itself.
func (a *app) renderConditions(conditions []metav1.Condition) error {
	var notable []metav1.Condition
	for _, c := range conditions {
		if c.Status != metav1.ConditionTrue {
			notable = append(notable, c)
		}
	}
	if len(notable) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(a.io.Out)
	t := newTable(a.io.Out, []string{"CONDITION", "STATUS", "REASON", "MESSAGE"})
	for _, c := range notable {
		t.row(c.Type, string(c.Status), orDash(c.Reason), orDash(c.Message))
	}
	return t.flush()
}

// ---------------------------------------------------------------------------
// claim release
// ---------------------------------------------------------------------------

func newClaimReleaseCommand(a *app) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "release <name>",
		Aliases: []string{"rm", "delete"},
		Short:   "Release (delete) a claim",
		Long: `Release a claim. What happens to the address depends on the effective reclaim
policy: Delete frees it back to the pool, Retain leaves the allocation in place
with its claim reference cleared, still held and still counted against its
owner, until something releases it explicitly.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cs, ns, err := a.client()
			if err != nil {
				return err
			}
			claim, err := cs.IpamV1alpha1().IPClaims(ns).Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return newCLIError(exitNotFound, fmt.Sprintf("claim %q not found in this project", name)).
						withFix("list claims:\n       datumctl ipam claim list").withCause(err)
				}
				return classifyError(err)
			}

			addr := orDash(claimAddress(claim))
			retained := effectiveReclaimPolicy(cs, claim) == ipamv1alpha1.ReclaimRetain
			fate := "frees"
			if retained {
				fate = "retains (does not free)"
			}

			if dryRun {
				_, _ = fmt.Fprintln(a.io.ErrOut, "Dry run — nothing was released.")
				_, _ = fmt.Fprintf(a.io.ErrOut, "Would release claim %q, which %s %s.\n", name, fate, addr)
				if claim.Status.BoundAllocationRef != nil && retained {
					_, _ = fmt.Fprintf(a.io.ErrOut,
						"Allocation %q would remain, still held, with its claim reference cleared.\n",
						claim.Status.BoundAllocationRef.Name)
				}
				return nil
			}

			prompt := fmt.Sprintf("Release claim %q (%s)?", name, addr)
			if retained {
				prompt = fmt.Sprintf("Release claim %q? %s stays allocated under Retain.", name, addr)
			}
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
			_, _ = fmt.Fprintf(a.io.Out, "%s Released claim %q\n", successPrefix(a.color), name)
			if retained {
				_, _ = fmt.Fprintf(a.io.Out,
					"  %s is retained and still held; release it with `datumctl ipam allocation release`.\n", addr)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be released without releasing it")
	return cmd
}

// effectiveReclaimPolicy resolves what will actually happen to a claim's address
// when it is released: the claim's own override, else the class default.
//
// The claim's spec is not enough on its own. A claim that states no policy
// inherits Retain from its class, and this prompt must never tell the user
// their address will be freed when it will not. The class lookup is
// best-effort; if it fails, the answer falls back to the API default.
func effectiveReclaimPolicy(cs clientset.Interface, c *ipamv1alpha1.IPClaim) ipamv1alpha1.ReclaimPolicy {
	if c.Spec.ReclaimPolicy != "" {
		return c.Spec.ReclaimPolicy
	}
	if c.Spec.ClassName == "" {
		return ipamv1alpha1.ReclaimDelete
	}
	class, err := cs.IpamV1alpha1().IPClasses().Get(context.Background(), c.Spec.ClassName, metav1.GetOptions{})
	if err != nil || class.Spec.ReclaimPolicy == "" {
		return ipamv1alpha1.ReclaimDelete
	}
	return class.Spec.ReclaimPolicy
}
