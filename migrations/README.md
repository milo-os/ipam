# IPAM Service Migrations

SQL migrations for the Postgres storage backend.

## Naming Convention

Files follow the pattern `{NNN}_{description}.sql` for the forward (up)
migration. A matching `{NNN}_{description}.down.sql` may exist alongside it
to manually reverse the corresponding up migration. `migrate.sh` only
applies up migrations; rollback is run by hand with `psql` against the
relevant `.down.sql` file. Down files are written as best-effort destructive
reversals — they drop columns, tables, or constraints — and should be used
only on test clusters or during a deliberate schema-reversal exercise.

## Running Migrations

### Locally

```bash
export PGHOST=localhost
export PGPORT=5432
export PGUSER=quota
export PGPASSWORD=quota
export PGDATABASE=quota
./migrations/migrate.sh
```

### In Kubernetes

The `migrate` init container in `config/base/deployment.yaml` runs
`ipam migrate up` before the apiserver starts. The binary embeds the SQL files
(`migrations/embed.go`) and applies them with goose, so a new migration ships
with the image and needs no manifest change.

## Adding New Migrations

1. Create a new file: `migrations/{NNN}_{description}.sql`
2. Use `IF NOT EXISTS` / `IF EXISTS` guards where possible so migrations are
   idempotent.
3. Add a matching `{NNN}_{description}.down.sql` that reverses the change.
   Use `IF EXISTS` guards there too — a partial down apply must not fail.
