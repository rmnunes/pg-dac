# Using the pg-dac CLI

The full command reference. For a first run, start with
[Trying it on a real database](#trying-it-on-a-real-database); for running it in a delivery
pipeline, see [ROLLOUT.md](../ROLLOUT.md).

## Commands

```bash
go build -o bin/pg-dac ./cmd/pg-dac   # or use a release binary

# A source is a connection string, a project directory, a .sql file, or a .pgdac artifact.
# For compare verbs, --to is the DESIRED state and --from is the schema to be changed.

# Compare and script (any source on either side)
pg-dac diff    --from "postgres://...target" --to ./schema
pg-dac script  --from "postgres://...target" --to ./schema --validate > migrate.sql
pg-dac report  --from app.pgdac              --to "postgres://...target" --format json
pg-dac plan    --from "postgres://...target" --to ./schema              # per-object JSON (GUI/tooling)

# Apply (publish) the desired state to a live database — including an empty one
pg-dac publish --from "postgres://...target" --to ./schema            # data loss is refused
pg-dac publish --from "postgres://...target" --to app.pgdac --dry-run # preview only

# Package a schema into a portable, signed artifact, and inspect it
pg-dac extract --from "postgres://...prod" --out prod.pgdac
pg-dac build   --from ./schema --out app.pgdac --version 2.1.0 --target-major 18
pg-dac inspect app.pgdac

# Schema AND data (the BACPAC analog): one consistent snapshot, verified and signed
pg-dac export --from "postgres://...prod" --out prod.pgbac --exclude-table 'audit.*'
pg-dac import --file prod.pgbac --target "postgres://...empty-db"   # empty target; keys/indexes/triggers after the rows

# Production publish controls
pg-dac publish --from "postgres://...prod" --to app.pgdac --single-transaction --statement-timeout 10m \
               --var deploy_environment=prod --exclude-object-type privilege

# Scaffold / reverse-engineer a declarative project
pg-dac init    --dir ./schema --name app                               # section folders for hand authoring
pg-dac extract --from "postgres://...prod" --to-project ./schema       # one file per object: schema/<schema>/<kind>/<name>.sql

# Update an existing project from a database (SSDT's "Update project from database"):
# the project is the target, only the files of changed objects are rewritten
pg-dac publish --from ./schema --to "postgres://...dev" --dry-run       # file-level diff
pg-dac publish --from ./schema --to "postgres://...dev"
```

Project files need no particular order: the build applies statements with deferred retry, so a
view may be defined before its table and a table's foreign keys live in the table's own file.
Updating a project rewrites an object's declarative statements in place (keeping the comments
above them), adds a file at the stable path for a new object, removes a dropped object's
statements under drop policy `all`, and adds new grantees to `roles:`. An object defined by
migration-style statements (`ALTER TABLE … ADD COLUMN`, `DO` blocks) cannot be rewritten by any
tool and is refused with directions.

Useful flags: `--schema <name>` (repeatable), `--target-major <n>` (the PostgreSQL major used to
normalize a project source, default the project's `pgdac.yaml` `targetMajor`, and to choose
version-gated statements, default the target database's version), `--allow-incompatible-platform`
(see below), `--no-online`, `--allow-hazards`, `--drop-objects-not-in-source` (overrides the
project's `dropPolicy`), `--exclude-object-type <kind>` (repeatable; SSDT's ExcludeObjectTypes),
`--validate`, `--dry-run`, `--lock-timeout`, `--statement-timeout`, `--single-transaction`
(consecutive transactional statements run in one transaction; CONCURRENTLY statements split it,
`--no-online` makes the whole migration one transaction), `--var name=value` (repeatable; `$(name)`
placeholders in pre/post-deploy scripts, SQLCMD-style, defaults from `pgdac.yaml` `variables:`)
and `--no-deploy-scripts` (publish), `--no-sign` (build/extract/export), `--require-key
<fingerprint>` (every verb that reads a `.pgdac`/`.pgbac`; a `trusted_keys` file under the pg-dac
config directory does the same for all runs).

**What counts as a difference** (SSDT's comparison options, with the same defaults): routine
bodies are compared ignoring whitespace and line endings (`--compare-whitespace` makes them
exact; `--ignore-comments` also ignores comments); privileges (`GRANT`/`REVOKE` on schemas,
tables, views, sequences, routines, types and columns) are part of the model and compared
(`--ignore-privileges`; revokes of privileges the source does not hold follow the drop policy,
like SSDT's `DropPermissionsNotInSource`), and an object pg-dac drops and recreates always gets
its privileges back; default privileges (`ALTER DEFAULT PRIVILEGES FOR ROLE … [IN SCHEMA …]`) are
compared the same way: those scoped to the source's schemas, and the database-wide ones of roles
the source has database-wide entries for, so another application's entries in a shared database
are left alone (an entry the source declares is applied in full, including a `REVOKE … FROM
PUBLIC`; the role an entry is for must be in `roles:`); a table whose declared column order differs from the target's is reported
(`--column-order warn`), ignored (`ignore`), or rebuilt in the declared order with its rows
copied (`rebuild`, the SSDT table rebuild — heavy and explicit). A project sets its defaults in
`pgdac.yaml`:

```yaml
compare:
  privileges: true        # false = ignore GRANT/REVOKE
  columnOrder: warn       # ignore | warn | rebuild
  ignoreWhitespace: true
  ignoreComments: false
roles: [app_readers]      # roles the project's GRANTs name but does not create
```

**A newer project than the database.** A project normalized for a newer PostgreSQL major than the
target runs is refused, as SqlPackage refuses a project whose target platform is newer than the
server: the model would describe what the newer server makes of the project (an extension at the
version that server ships, defaults and syntax it introduced), and the migration would ask the
target for things it does not have. For a project that targets 18 and a local database on 16,
compare for 16 with `--target-major 16`, or pass `--allow-incompatible-platform` to compare anyway.

**Text encoding.** pg-dac's sessions are always UTF-8, and its scratch server is created as UTF-8
with the C locale on every platform, so non-ASCII text in definitions and comments compares the
same on Windows and Linux. `PGCLIENTENCODING` is meant for `psql` and is ignored by pg-dac.

Roles are cluster objects pg-dac never creates. A GRANT to a role the target lacks is a blocking
`MISSING_ROLE` hazard; when that is the only blocker and the project has pre-deploy scripts,
`publish` runs them first (that is where a project provisions its roles) and re-checks before
refusing.

Every side of a comparison — a live database included — is first brought to the same canonical
form through a scratch PostgreSQL, so an expression never compares as different only because of
how PostgreSQL deparsed it when the object was created.

A project's `pgdac.yaml` `preDeploy`/`postDeploy` scripts, `dropPolicy` and `refactorlog.json`
travel inside the artifact and are honored by `publish`: pending renames run first as
`ALTER … RENAME` (recorded per target so they run once), pre-deploy scripts before the migration,
post-deploy scripts after it, and the drop policy is the default drop behavior. Every publish
records the resulting schema as the target's baseline; a target that later changed outside pg-dac
is refused until `--allow-drift` (or `pg-dac baseline`) accepts the change — `pg-dac drift` shows
what changed.

```bash
pg-dac refactor rename --dir ./schema --kind column --schema app --table users --from name --to full_name
pg-dac drift --from "postgres://...target"            # exit 2 when drift is detected
pg-dac lint  --from "postgres://...target" --to ./schema --fail-on error
```

> The first run downloads the embedded PostgreSQL binaries (cached per major version), so any
> command that uses a project/`.sql` source or `--validate` takes longer the first time.

## Trying it on a real database

You need a PostgreSQL connection string for at least one side. **Always test against a copy or a
non-production database first.** The recommended order:

```bash
go build -o bin/pg-dac ./cmd/pg-dac
# Keep the password out of the command line and shell history: libpq's PGPASSWORD
# (or a ~/.pgpass file) supplies it to a connection string that has none.
read -rs PGPASSWORD && export PGPASSWORD
DB="postgres://user@host:5432/dbname?sslmode=require"

# 1. Sanity: comparing a database to itself must be empty (and surfaces any
#    unsupported-object warnings for your schema).
bin/pg-dac diff --from "$DB" --to "$DB"

# 2. Snapshot the current schema into a signed, portable artifact.
bin/pg-dac extract --from "$DB" --out current.pgdac
bin/pg-dac inspect current.pgdac

# 3. See what it would take to converge a target toward a desired source.
#    Desired (--to) can be another database, a .pgdac, or a .sql project directory.
bin/pg-dac diff   --from "$TARGET_DB" --to current.pgdac           # summary + hazards
bin/pg-dac script --from "$TARGET_DB" --to current.pgdac --validate > migrate.sql

# 4. Apply (only after reviewing). Data loss is refused unless --allow-hazards.
bin/pg-dac publish --from "$TARGET_DB" --to current.pgdac --dry-run   # preview
bin/pg-dac publish --from "$TARGET_DB" --to current.pgdac             # apply
```

Tips: restrict scope with `--schema app` (repeatable); `--validate` replays the migration in a
throwaway database first; read every `warning (...)` line — those object types are not yet managed.
Point a project source at the directory that holds only schema DDL (psql meta-commands such as
`\i` cannot run server-side), or add a `pgdac.yaml` whose `include` globs select it.
