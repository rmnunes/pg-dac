# How pg-dac works

## Why it exists

PostgreSQL already has good schema-diff engines (Atlas, Stripe's pg-schema-diff, pgschema),
but none ship the part of DACPAC that makes it portable and CI-friendly: a sealed,
version-targeted, **signed artifact**, a machine-readable change manifest, a refactorlog for
data-preserving renames, and artifact↔database / artifact↔artifact comparison. pg-dac fills
that gap, with first-class online/lock-safe DDL built in.

## The pipeline

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
  afterwards, each statement annotated with why. That includes the parts of a table that call such
  a function: column defaults, CHECK constraints, index expressions and predicates, and policies.
- **SQL generation + safety** — renders lock-safe DDL: `CREATE INDEX CONCURRENTLY`, gap-free index rebuilds (a changed index is built concurrently under a temporary name, then swapped in), constraints
  added `NOT VALID` then `VALIDATE`d, scan-free `SET NOT NULL` on PG12+, etc. Each statement is
  tagged with typed hazards; data-loss operations are blocked unless `--allow-hazards` is given.
  Scripts and the executor run with `check_function_bodies = false` (as pg_dump does), so a
  SQL-language function may reference objects created later in the same migration.

## What is managed

**Managed objects:** schemas, extensions, enums, domains, composite types, collations, sequences
(standalone, serial, and identity), functions and procedures, tables (columns incl.
identity/generated/defaults/not-null/storage/compression/statistics, `UNLOGGED`, storage parameters,
partitioning), primary/foreign/unique/check/exclude constraints (incl. `NOT VALID` state), indexes on
tables and materialized views, triggers on tables and views (incl. enabled state), row-level-security
policies and table RLS enable/force, views (incl. check option and security options), materialized
views, comments on all of them, and privileges (GRANT/REVOKE incl. grant options and column
privileges) on schemas, tables, views, sequences, routines and types.

**Out of scope** (details in [STATUS.md](../STATUS.md)): ownership and default privileges are warned
about, not modeled; a generated column whose expression calls a function being recreated is not
recreated around it; changes PostgreSQL cannot make in place (removing enum values, altering a
domain or composite type, re-partitioning) are reported as an `UNSUPPORTED_CHANGE` hazard rather
than applied; roles are required to exist, never created.

## How it is verified

Every gate below runs in `go test ./...` (and in CI on every push):

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
