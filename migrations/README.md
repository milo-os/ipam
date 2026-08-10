# IPAM Service Migrations

SQL migrations for the PostgreSQL storage backend. They are [goose][goose]
migrations, embedded into the `ipam` binary by `embed.go` and applied by the
`ipam migrate` subcommand. Nothing else applies them.

[goose]: https://github.com/pressly/goose

## PostgreSQL floor: 13

Two independent things need 13, and `001_initial_schema.sql` is right to say so
in its header:

- 001 uses `pg_current_xact_id()` and `pg_snapshot_xmin()` for the watch
  cursor's xmin horizon. Both were added in 13.
- 002 needs the `btree_gist` extension, for the exclusion constraint that
  enforces address non-overlap. It is a *trusted* extension from 13 on, which is
  what lets the database owner install it without superuser — and the deployed
  role is an ordinary owner, not a superuser.

Nothing in either file uses 14+ syntax. Verified: the whole schema applies on
13.23 as a non-superuser owning its database, exclusion constraint included, and
the herd test passes there. Below 13 it fails at 001 with `pg_current_xact_id()
does not exist` (confirmed on 12.22). Deployed clusters run 17.10.

A guard at the top of 002 asserts the floor so it executes rather than rots,
though 001 failing first is the real enforcement.

> A previous revision of this file and of 002 claimed a floor of 14, justified
> by a recursive CTE with `CYCLE`. There is no such query anywhere in the repo —
> chain depth and cycle rejection happen in Go. The claim came from a task brief
> and was repeated without checking. If you raise this floor, name the feature
> that needs it and confirm the repo actually uses it.

## Running migrations

```bash
ipam migrate up      --postgres-dsn "$POSTGRES_DSN"   # apply, then sync field indexes
ipam migrate down    --postgres-dsn "$POSTGRES_DSN"   # roll back one migration
ipam migrate status  --postgres-dsn "$POSTGRES_DSN"
```

In Kubernetes this runs as the `migrate` init container on the apiserver
Deployment (`config/base/deployment.yaml`), before the apiserver starts.

`migrate up` does two things: `goose.Up`, and then `fieldindex.SyncIndexes`
over the `FieldIndex` values each resource declares in its `strategy.go`. Both
are idempotent.

**Expression indexes are declared in two places, and they must agree.** A
migration that drops an index whose Go declaration still exists will see it
recreated by `SyncIndexes` seconds later, in the same command. When changing a
field-selector index, change the SQL and the `strategy.go` declaration
together.

## Adding a migration

1. Create `migrations/{NNN}_{description}.sql` with `-- +goose Up` and
   `-- +goose Down` sections. Both are required; a migration without a working
   Down is a one-way door.
2. Wrap any statement containing a semicolon inside a body — a `DO $$…$$`
   block, a function definition — in `-- +goose StatementBegin` /
   `-- +goose StatementEnd`, or goose will split it at the wrong semicolon.
3. goose runs each migration in a transaction, so a failure rolls the whole
   file back. Do not add `CREATE INDEX CONCURRENTLY`, which cannot run in one.
4. Test the round trip against a real PostgreSQL before merging:

   ```bash
   docker run -d --name ipam-mig-test -e POSTGRES_PASSWORD=postgres -p 55432:5432 postgres:17.10
   DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
   ipam migrate up   --postgres-dsn "$DSN"
   ipam migrate down --postgres-dsn "$DSN"
   ipam migrate up   --postgres-dsn "$DSN"
   ```

   Compare `pg_dump -s` before the down and after the second up; they should be
   identical. Run it as a non-superuser role that owns the database, since that
   is what the deployment does.

## Never apply a migration with `psql -f`

`migrations/migrate.sh` used to do this and was deleted. The reasoning is kept
because the mistake is easy to repeat and its failure mode is silent.

goose's `-- +goose Down` marker is an ordinary SQL comment. `psql` does not know
it means anything, so `psql -f 001_initial_schema.sql` creates the entire schema
and then executes the Down section, dropping every table it just made — and
exits 0. Demonstrated on a scratch database: 0 tables remaining, success
reported. The script compounded it by querying a `schema_migrations` table it
never created (so every migration always looked unapplied) and defaulting
`PGUSER`/`PGDATABASE` to `quota`.

Deleted alongside it: `config/components/postgres-migrations/`, a Job and
ConfigMap carrying an inline copy of a much older schema — `ipam_prefix_allocations`,
no `labels` column, no `commit_xid`, none of the expression indexes. It was
referenced by no overlay. The real path has always been the `migrate` init
container in `config/base/deployment.yaml`.

Apply migrations with `ipam migrate up`, which uses goose and understands the
markers.

## Rebuilding `ipam_pool_class_offer`

The offer table is a cache of `IPPool.spec.classNames`, maintained by the
application on pool **spec** writes only — never on status writes, which happen
on every allocation and would turn independent claims into contention on a
shared row. Because it is a cache, it can always be rebuilt from the objects:

```sql
BEGIN;
TRUNCATE ipam_pool_class_offer;
INSERT INTO ipam_pool_class_offer (pool_key, class_name)
SELECT o.key, c.value #>> '{}'
  FROM ipam_objects o,
       LATERAL jsonb_array_elements(
           COALESCE(ipam_data_to_jsonb(o.data) -> 'spec' -> 'classNames', '[]'::jsonb)) AS c(value)
 WHERE o.kind = 'IPPool'
    ON CONFLICT DO NOTHING;
COMMIT;
```

To find drift without changing anything, run the `SELECT` into a temp table and
`EXCEPT` it against the live table in both directions.

Application writers should call the helper rather than issuing DML directly; it
writes nothing and takes no row locks when the offered set is unchanged:

```sql
SELECT ipam_sync_pool_class_offers($1::text, $2::text[]);
```
