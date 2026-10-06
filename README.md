# pg-dac

A **DACPAC / SqlPackage-equivalent for PostgreSQL**. It compares schemas, produces
dependency-ordered, lock-safe migration scripts, and packages a schema into a portable, signed
`.pgdac` artifact you can compare against — and publish into — any target database, including an
empty one.

If you've used SQL Server's SqlPackage / SQL Database Projects and wished for the same
declarative, compare-and-script workflow on PostgreSQL, that's what pg-dac is.

It ships as a **CLI** (`pg-dac`), a local **daemon** (`pg-dac serve`, a warm-Postgres HTTP/SSE
API), a **project model** (`pgdac.yaml` + `pg-dac init` / `extract --to-project`), and a
**Tauri desktop GUI** for visual Schema Compare (see [`ui/`](ui/README.md)).

## Why it exists

PostgreSQL already has good schema-diff engines (Atlas, Stripe's pg-schema-diff, pgschema),
but none ship the part of DACPAC that makes it portable and CI-friendly: a sealed,
version-targeted, **signed artifact**, a machine-readable change manifest, a refactorlog for
data-preserving renames, and artifact↔database / artifact↔artifact comparison. pg-dac fills
that gap, with first-class online/lock-safe DDL built in.

## How it works

Every comparison reduces both sides to one canonical model, then diffs the models — never
raw SQL text:

```
source ─┐
        ├─► model ─► diff ─► dependency-ordered plan ─► lock-safe SQL ─► script / report / apply
target ─┘
```

- **Model** — built from live `pg_catalog` introspection (the authoritative source), with the
  session `search_path` pinned empty so every definition is fully schema-qualified and the model
  does not depend on who is reading it. A declarative project or a `.pgdac` artifact is reduced to
  the same model by materializing it in a throwaway PostgreSQL and re-introspecting — then
  re-materialized once more from its own canonical SQL, because PostgreSQL's deparsed text is not
  always a parse fixed point — so cosmetic noise (`varchar` vs `character varying`, reparenthesized
  checks, whitespace, `IN (...)` lists) never shows up as a false diff and a published target
  converges.
- **Diff** — purely name-keyed, deterministic; emits typed operations (no SQL).
- **Plan** — orders operations with a topological sort over dependency edges (foreign keys, view
  and function references, row types used in function signatures, trigger functions, sequence
  ownership, the columns a view reads) plus a pg_dump-style object-type priority as the tie-breaker.
  Changes PostgreSQL refuses while dependents exist — retyping or dropping a column a view reads,
  dropping a table or function a view or trigger uses, replacing a view whose columns changed or a
  function whose result type changed — automatically drop the dependents first and recreate them
  afterwards, each statement annotated with why.
- **SQL generation + safety** — renders lock-safe DDL: `CREATE INDEX CONCURRENTLY`, constraints
  added `NOT VALID` then `VALIDATE`d, scan-free `SET NOT NULL` on PG12+, etc. Each statement is
  tagged with typed hazards; data-loss operations are blocked unless `--allow-hazards` is given.
  Scripts and the executor run with `check_function_bodies = false` (as pg_dump does), so a
  SQL-language function may reference objects created later in the same migration.

## Status

The engine, daemon, project model, artifact, and GUI are built and exercised against real
PostgreSQL (embedded 16, Docker 16/18, and a ~290-object production schema). An independent audit
on 2026-10-06 (the audit notes) drove four work packages; the roadmap in
[STATUS.md](STATUS.md) is complete, including the refactorlog, drift detection, lint, and a CI /
release pipeline. The current gates, all in `go test ./...`:

- **From-empty bootstrap** (`internal/validate/bootstrap_test.go`): a comprehensive schema is
  built, published into an empty scratch database, re-introspected, and must show no residual;
  the script must be byte-identical across repeated compares; the model must survive a trip
  through a signed `.pgdac`.
- **Scenario matrix** (`internal/engine/scenarios_test.go`): the migrations from the audit, each
  tagged `ok`, `fail` (known gap, with the work package that fixes it) or `miss` (attribute not yet
  modeled). A gap that starts passing makes the test tell you to flip its tag.
- **Introspection invariants** (`internal/introspect`): the model is independent of the session
  `search_path`; `NOT VALID` is a flag, not definition text; function, trigger and view dependency
  edges are recorded; partitioning, view options, trigger state, storage attributes, domains and
  composite types are read.
- **Artifact trust** (`internal/artifact`, `internal/source`): the signature covers the whole
  manifest, and a required or trusted signer is enforced on every artifact source.

**Managed objects:** schemas, extensions, enums, domains, composite types, collations, sequences
(standalone, serial, and identity), functions and procedures, tables (columns incl.
identity/generated/defaults/not-null/storage/compression/statistics, `UNLOGGED`, storage parameters,
partitioning), primary/foreign/unique/check/exclude constraints (incl. `NOT VALID` state), indexes on
tables and materialized views, triggers on tables and views (incl. enabled state), row-level-security
policies and table RLS enable/force, views (incl. check option and security options), materialized
views, and comments on all of them.

**Out of scope for v1** (details in [STATUS.md](STATUS.md)): grants are warned about but not
published; policies, defaults and constraints that call a function being recreated are not
recreated around it; changes PostgreSQL cannot make in place (removing enum values, altering a
domain or composite type, re-partitioning) are reported as an `UNSUPPORTED_CHANGE` hazard rather
than applied; a `.pgbac` data artifact is deferred.

## Usage

```bash
go build -o bin/pg-dac ./cmd/pg-dac

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

Useful flags: `--schema <name>` (repeatable), `--target-major <n>` (version-gated strategies, and
the PostgreSQL major used to normalize a project source; a project's `pgdac.yaml` `targetMajor`
is the default), `--no-online`, `--allow-hazards`, `--drop-objects-not-in-source` (overrides the
project's `dropPolicy`), `--validate`, `--dry-run`, `--lock-timeout` and `--no-deploy-scripts`
(publish), `--no-sign` (build/extract), `--require-key <fingerprint>` (every verb that reads a
`.pgdac`; a `trusted_keys` file under the pg-dac config directory does the same for all runs).

**What counts as a difference** (SSDT's comparison options, with the same defaults): routine
bodies are compared ignoring whitespace and line endings (`--compare-whitespace` makes them
exact; `--ignore-comments` also ignores comments); privileges (`GRANT`/`REVOKE` on schemas,
tables, views, sequences, routines, types and columns) are part of the model and compared
(`--ignore-privileges`; revokes of privileges the source does not hold follow the drop policy,
like SSDT's `DropPermissionsNotInSource`), and an object pg-dac drops and recreates always gets
its privileges back; a table whose declared column order differs from the target's is reported
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

## Trying it on a real project or database

You need a PostgreSQL connection string for at least one side. **Always test against a copy or a
non-production database first.** The recommended order:

```bash
go build -o bin/pg-dac ./cmd/pg-dac
DB="postgres://user:pass@host:5432/dbname?sslmode=disable"

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

## Testing

```bash
go test -short ./...       # fast unit tests, no database required
go test -p 1 ./...         # + embedded-PostgreSQL integration tests (downloads binaries once)
```

CI runs the same on every push (`.github/workflows/ci.yml`); a `v*` tag matching
`ui/PGDAC_VERSION` builds the CLI for every platform and the desktop installers
(`.github/workflows/release.yml`).

Set `PG_DAC_TEST_DSN` to also run the self-idempotence check against a real database.

## License

Apache-2.0. The dependency-ordering algorithm is reimplemented from PostgreSQL's
`pg_dump_sort.c` (BSD) and the hazard rules from the PostgreSQL docs and Squawk (concepts only);
no GPL code is copied.
