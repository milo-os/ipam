package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"text/tabwriter"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"

	"go.miloapis.com/ipam/internal/fieldindex"
	ipamregistry "go.miloapis.com/ipam/internal/registry/ipam"
	"go.miloapis.com/ipam/migrations"
)

func NewMigrateCommand() *cobra.Command {
	var postgresDSN string

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage database schema migrations",
	}

	cmd.PersistentFlags().StringVar(&postgresDSN, "postgres-dsn", "",
		"PostgreSQL connection string (required)")

	openDB := func() (*sql.DB, error) {
		if postgresDSN == "" {
			postgresDSN = os.Getenv("POSTGRES_DSN")
		}
		if postgresDSN == "" {
			return nil, fmt.Errorf("--postgres-dsn or POSTGRES_DSN is required")
		}
		db, err := sql.Open("pgx", postgresDSN)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		return db, nil
	}

	setupGoose := func(_ *sql.DB) error {
		goose.SetBaseFS(migrations.FS)
		if err := goose.SetDialect("postgres"); err != nil {
			return fmt.Errorf("set dialect: %w", err)
		}
		return nil
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations then sync field-selector indexes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := setupGoose(db); err != nil {
				return err
			}
			if err := goose.Up(db, "."); err != nil {
				return fmt.Errorf("goose up: %w", err)
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := fieldindex.SyncIndexes(ctx, db, ipamregistry.AllFieldIndexes()); err != nil {
				return fmt.Errorf("sync field indexes: %w", err)
			}
			fmt.Println("migrations applied and field indexes synced")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Roll back the most recent migration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := setupGoose(db); err != nil {
				return err
			}
			if err := goose.Down(db, "."); err != nil {
				return fmt.Errorf("goose down: %w", err)
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show migration status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := setupGoose(db); err != nil {
				return err
			}
			migrations, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
			if err != nil {
				return fmt.Errorf("collect migrations: %w", err)
			}
			current, err := goose.GetDBVersion(db)
			if err != nil {
				return fmt.Errorf("get db version: %w", err)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			_, _ = fmt.Fprintln(w, "VERSION\tSTATUS\tFILE")
			for _, m := range migrations {
				status := "pending"
				if m.Version <= current {
					status = "applied"
				}
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\n", m.Version, status, m.Source)
			}
			return w.Flush()
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "sync-indexes",
		Short: "Create or update field-selector expression indexes without running migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			indexes := ipamregistry.AllFieldIndexes()
			if err := fieldindex.SyncIndexes(ctx, db, indexes); err != nil {
				return fmt.Errorf("sync field indexes: %w", err)
			}
			fmt.Printf("synced %d field indexes\n", len(indexes))
			return nil
		},
	})

	return cmd
}
