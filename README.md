# pg-dac

A **DACPAC / SqlPackage-equivalent for PostgreSQL**. It compares schemas, produces
dependency-ordered, lock-safe migration scripts, and (in later milestones) packages a
schema into a portable, signed `.pgdac` artifact you can compare against a target database.

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

- **Model** — built from live `pg_catalog` introspection (the authoritative source). A
  declarative project or a `.pgdac` artifact is reduced to the same model by materializing it
  in a throwaway PostgreSQL and re-introspecting, so cosmetic noise (`varchar` vs
  `character varying`, reparenthesized checks, whitespace) never shows up as a false diff.
- **Diff** — purely name-keyed; emits typed operations (no SQL).
- **Plan** — orders operations with a topological sort over derived dependency edges plus a
  pg_dump-style object-type priority as a tie-breaker (drops in dependents-first order, creates
  in dependencies-first order; foreign keys land after the tables they reference).
- **SQL generation + safety** — renders lock-safe DDL: `CREATE INDEX CONCURRENTLY`, constraints
  added `NOT VALID` then `VALIDATE`d, scan-free `SET NOT NULL` on PG12+, etc. Each statement is
  tagged with typed hazards; data-loss operations are blocked unless `--allow-hazards` is given.

## Status

**Complete and verified end-to-end against real PostgreSQL (milestones 1–5).**

- **M1 — compare + script.** Object coverage: schemas, extensions, enums, sequences, **functions
  and procedures**, tables (columns incl. identity/generated/defaults/not-null, comments),
  primary/foreign/unique/check/exclude constraints, indexes, **triggers**, **row-level-security
  policies** (and table RLS enable/force), views, and materialized views. Object types pg-dac does
  not yet manage (domains, composite/range types, aggregates, foreign tables, collations,
  non-view rules, partition attachments) are **reported as warnings** so a "no differences" result
  is never silently misleading.
- **M2 — scratch database + normalization + plan validation.** A bundled, auto-managed embedded
  PostgreSQL builds a model from a declarative `.sql` project/file (apply + re-introspect) and
  `--validate`s a migration by replaying it against a scratch copy of the source and confirming
  it converges.
- **M3 — `publish`.** Applies the migration with lock-safety: a session `lock_timeout` plus
  jittered retries on lock-timeout errors, per-statement autocommit (so `CONCURRENTLY` runs and a
  failed publish is resumable), and a data-loss block gate.
- **M4 — signed `.pgdac` artifact.** `build`/`extract` produce a portable archive (manifest +
  model + canonical SQL) with a SHA-256 Merkle root and an Ed25519 signature; `inspect` verifies
  integrity and the signature.
- **M5 — artifact comparison.** Any source — connection string, project directory, `.sql` file,
  or `.pgdac` — can be either side of a comparison, so artifact↔database and artifact↔artifact
  comparisons (and publishing *from* an artifact) all work through the same verbs.

Sources sharing one CLI invocation reuse a single scratch server (ephemeral databases), and a
cross-process lock makes concurrent server starts safe.

## Usage

```bash
go build -o bin/pg-dac ./cmd/pg-dac

# A source is a connection string, a project directory, a .sql file, or a .pgdac artifact.
# For compare verbs, --to is the DESIRED state and --from is the schema to be changed.

# Compare and script (any source on either side)
pg-dac diff    --from "postgres://...target" --to ./schema
pg-dac script  --from "postgres://...target" --to ./schema --validate > migrate.sql
pg-dac report  --from app.pgdac              --to "postgres://...target" --format json

# Apply (publish) the desired state to a live database
pg-dac publish --from "postgres://...target" --to ./schema            # data loss is refused
pg-dac publish --from "postgres://...target" --to ./schema --dry-run  # preview only

# Package a schema into a portable, signed artifact, and inspect it
pg-dac extract --from "postgres://...prod" --out prod.pgdac
pg-dac build   --from ./schema             --out app.pgdac --version 2.1.0
pg-dac inspect app.pgdac
```

Useful flags: `--schema <name>` (repeatable), `--target-major <n>`, `--no-online`,
`--allow-hazards`, `--drop-objects-not-in-source`, `--validate`, `--dry-run` (publish),
`--lock-timeout` (publish), `--no-sign` (build/extract), `--require-key` (inspect).

> The first run downloads the embedded PostgreSQL binaries (cached afterward), so any command
> that uses a project/`.sql`/artifact source or `--validate` takes longer the first time.

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

## Testing

```bash
go test -short ./...   # fast unit tests, no database required
go test ./...          # + embedded-PostgreSQL integration tests (downloads binaries once)
```

Set `PG_DAC_TEST_DSN` to also run the self-idempotence check against a real database.

## Development

```bash
go test ./...                                   # unit tests (no database required)
PG_DAC_TEST_DSN="postgres://..." go test ./internal/engine   # + self-idempotence integration test
```

## License

Apache-2.0. The dependency-ordering algorithm is reimplemented from PostgreSQL's
`pg_dump_sort.c` (BSD) and the hazard rules from the PostgreSQL docs and Squawk (concepts only);
no GPL code is copied.
