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
on 2026-10-06 (the audit notes) found the from-empty bootstrap broken; the first
work package since then fixed it. The current gates, all in `go test ./...`:

- **From-empty bootstrap** (`internal/validate/bootstrap_test.go`): a comprehensive schema is
  built, published into an empty scratch database, re-introspected, and must show no residual;
  the script must be byte-identical across repeated compares; the model must survive a trip
  through a signed `.pgdac`.
- **Scenario matrix** (`internal/engine/scenarios_test.go`): the migrations from the audit, each
  tagged `ok`, `fail` (known gap, with the work package that fixes it) or `miss` (attribute not yet
  modeled). A gap that starts passing makes the test tell you to flip its tag.
- **Introspection invariants** (`internal/introspect`): the model is independent of the session
  `search_path`; `NOT VALID` is a flag, not definition text; function, trigger and view dependency
  edges are recorded.

**Managed objects:** schemas, extensions, enums, collations, sequences (standalone, serial, and
identity), functions and procedures, tables (columns incl. identity/generated/defaults/not-null,
comments on create), primary/foreign/unique/check/exclude constraints (incl. `NOT VALID` state),
indexes, triggers, row-level-security policies and table RLS enable/force, views, materialized views.

**Known gaps** (details and plan in [STATUS.md](STATUS.md)): partitioning, materialized-view
indexes, view triggers, check options, trigger enable state, storage parameters, comment changes,
enum value removal and grants are not modeled yet and are **silently dropped** on publish; policies,
defaults and constraints that call a function being recreated are not yet recreated around it; the
artifact's signature covers the content but not yet the manifest metadata; domains/composite/range
types are reported as warnings only.

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
pg-dac extract --from "postgres://...prod" --to-project ./schema       # schema/NNNN_<kind>_<name>.sql in dependency order
```

Useful flags: `--schema <name>` (repeatable), `--target-major <n>` (version-gated strategies, and
the PostgreSQL major used to normalize a project source; a project's `pgdac.yaml` `targetMajor`
is the default), `--no-online`, `--allow-hazards`, `--drop-objects-not-in-source`, `--validate`,
`--dry-run` and `--lock-timeout` (publish), `--no-sign` (build/extract), `--require-key` (inspect).

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
go test -p 1 ./...         # + embedded-PostgreSQL integration tests (downloads binaries once);
                           #   -p 1 keeps packages from racing on the embedded binaries
```

Set `PG_DAC_TEST_DSN` to also run the self-idempotence check against a real database.

## License

Apache-2.0. The dependency-ordering algorithm is reimplemented from PostgreSQL's
`pg_dump_sort.c` (BSD) and the hazard rules from the PostgreSQL docs and Squawk (concepts only);
no GPL code is copied.
