package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"go.miloapis.com/ipam/internal/allocator"
)

// NewReclaimCommand builds `ipam reclaim-unprefixed`.
//
// It exists because the objects it removes cannot be removed through the API:
// an untenanted caller can see them and is refused the write, and a platform
// caller may write and cannot see them. See internal/allocator/unprefixed.go
// for why that pincer is two correct behaviours rather than a bug in either.
//
// Reporting is the default and deleting requires --confirm. That is deliberate
// and is the opposite of how the rest of this binary's subcommands behave:
// `migrate up` acts because applying a pending migration is the whole point of
// running it, while this deletes objects and the operator may be running it to
// find out whether it has anything to delete at all.
func NewReclaimCommand() *cobra.Command {
	var postgresDSN string
	var confirm bool

	cmd := &cobra.Command{
		Use:   "reclaim-unprefixed",
		Short: "Report, and with --confirm remove, objects stranded in the unprefixed keyspace",
		Long: `Report objects stranded in the unprefixed keyspace, and with --confirm remove them.

Every object IPAM stores belongs to a project, so keys look like
"project/<p>/ipam.miloapis.com/<resource>/<name>". Objects at
"/ipam.miloapis.com/<resource>/<name>" pre-date that cutover or were written by
a caller carrying no tenant before untenanted writes were closed. Nothing reads
them and no API call can remove them: an untenanted delete is refused by the
write gate, and a platform-scoped delete does not find them.

Without --confirm this only reports. With it, the allocation rows, the objects
and their pool-identity rows are removed in one transaction, and any surviving
pool that was holding a carve for one of them has its capacity status
recomputed — that space is genuinely returned, and the pool would otherwise go
on reporting it as allocated.

Objects at "/ippool/<name>" are NOT touched. That shape is residue from this
repository's own migration tests and no registry produces it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if postgresDSN == "" {
				postgresDSN = os.Getenv("POSTGRES_DSN")
			}
			if postgresDSN == "" {
				return fmt.Errorf("--postgres-dsn or POSTGRES_DSN is required")
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			db, err := pgxpool.New(ctx, postgresDSN)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}
			defer db.Close()

			tx, err := db.Begin(ctx)
			if err != nil {
				return fmt.Errorf("begin: %w", err)
			}
			// Rolled back on every path that does not explicitly commit, so a
			// run without --confirm cannot leave anything behind even though it
			// executes the same scan the destructive path does.
			committed := false
			defer func() {
				if !committed {
					_ = tx.Rollback(ctx)
				}
			}()

			var res *allocator.UnprefixedResidue
			if confirm {
				res, err = allocator.ReclaimUnprefixed(ctx, tx)
			} else {
				res, err = allocator.ScanUnprefixed(ctx, tx)
			}
			if err != nil {
				return err
			}

			report(cmd, res, confirm)

			if res.Empty() {
				return nil
			}
			if !confirm {
				fmt.Fprintln(cmd.OutOrStdout(),
					"\nNothing was changed. Re-run with --confirm to remove the above.")
				return nil
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			committed = true
			fmt.Fprintln(cmd.OutOrStdout(), "\nRemoved.")
			return nil
		},
	}

	cmd.Flags().StringVar(&postgresDSN, "postgres-dsn", "",
		"PostgreSQL connection string (required)")
	cmd.Flags().BoolVar(&confirm, "confirm", false,
		"actually delete; without this the command only reports")
	return cmd
}

// report prints what was found, sample rows included.
//
// The sample is not decoration. This whole task began with a count of 6,872
// that was attributed to the wrong cause, and a predicate selecting the wrong
// population returns real, plausible, moving numbers either way — so anything
// that reports a count here also shows the rows behind it.
func report(cmd *cobra.Command, res *allocator.UnprefixedResidue, confirm bool) {
	out := cmd.OutOrStdout()
	if res.Empty() {
		fmt.Fprintln(out, "The unprefixed keyspace is empty; nothing to reclaim.")
		return
	}

	verb := "Found"
	if confirm {
		verb = "Removed"
	}
	fmt.Fprintf(out, "%s in the unprefixed keyspace:\n", verb)
	for kind, n := range res.ObjectsByKind {
		fmt.Fprintf(out, "  %-14s %d objects\n", kind, n)
	}
	fmt.Fprintf(out, "  %-14s %d rows naming those objects\n", "allocations", res.Allocations)
	fmt.Fprintf(out, "  %-14s %d rows held inside those pools\n", "", res.AllocationsInside)
	fmt.Fprintf(out, "  %-14s %d rows\n", "pool identity", res.IdentityRows)

	fmt.Fprintln(out, "\nSample keys:")
	for _, key := range res.Sample {
		fmt.Fprintf(out, "  %s\n", key)
	}

	if len(res.SurvivingParents) > 0 {
		fmt.Fprintf(out,
			"\n%d surviving pool(s) are holding that space and get their capacity recomputed:\n",
			len(res.SurvivingParents))
		for _, p := range res.SurvivingParents {
			fmt.Fprintf(out, "  %s\n", p)
		}
	}
}
