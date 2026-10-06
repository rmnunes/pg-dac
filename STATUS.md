# pg-dac — Project Status & Roadmap

_Last updated: 2026-10-06 (after WP0, WP1 and WP2; see the audit notes for the audit)_

A **DACPAC / SqlPackage-equivalent for PostgreSQL**: compare schemas, generate dependency-ordered
lock-safe migration scripts, package a schema into a portable **signed `.pgdac` artifact**, and
publish — via a CLI, a local daemon, a declarative project model, and a Tauri desktop GUI.

---

## Status at a glance

| Area | State |
|---|---|
| CLI engine: compare / script / report / plan | ✅ Verified on a ~290-object production schema |
| Publish into an **existing** database | ✅ Dependents (views, matviews, triggers) are dropped and recreated around blocking changes (WP2) |
| View column changes / function result-type changes | ✅ `DROP` + `CREATE` when `CREATE OR REPLACE` cannot apply (WP2) |
| `extract --to-project` | ✅ One numbered file per object in global dependency order (WP2); round-trips the ~290-object production schema |
| Publish into an **empty** database (bootstrap) | ✅ Fixed in WP1; regression-gated by `TestBootstrap_FromEmpty` |
| Deterministic output / stable artifact hash | ✅ Fixed in WP1 (differ walks model order) |
| Model independent of session `search_path` | ✅ Fixed in WP1 (introspection pins `search_path = ''`) |
| Signed `.pgdac` artifact (Merkle + Ed25519), format v2 | ✅ Signature covers content; manifest metadata not yet signed (WP3) |
| `pg-dac serve` daemon (warm Postgres, HTTP/SSE, cancel) | ✅ |
| Project model (`pgdac.yaml`, `init`) | ⚠️ `include`/`exclude`/`schemas`/`targetMajor` wired; `preDeploy`/`postDeploy`/`dropPolicy` parsed but not executed (WP3) |
| Tauri GUI | ✅ Builds and type-checks; previously confirmed working visually |
| Object coverage | ⚠️ See "Object coverage" — several attributes are silently dropped (WP3) |
| Version control / CI | ⚠️ Git initialized in WP0; no CI yet (WP4) |

---

## Architecture

```
            pg-dac-ui (Tauri v2 desktop)                      pg-dac (Go, single binary)
  React/Monaco/TanStack  ──invoke──▶  src-tauri (thin       ──spawn──▶  pg-dac serve (daemon)
   object tree + diff       supervisor: spawn sidecar,       ├─ warm embedded Postgres (per PG version)
   + publish modal)         port handshake, bearer token,    ├─ HTTP/SSE: /health /plan /script /publish /shutdown
        ▲   │ SSE→events     HTTP/SSE proxy)                  └─ engine: introspect → model → diff → plan → sqlgen
        └───┘                                                            → PlanDoc (structured per-object diff)
```

**Engine pipeline (pure Go):** `introspect (pg_catalog via pgx, search_path pinned)` → `typed model`
→ `diff (deterministic, typed ops)` → `plan (toposort over dependency edges + pg_dump-style
priority)` → `sqlgen (lock-safe SQL + typed hazards)`.

A "source" is resolved to the **same model** regardless of kind — a live DB, a project directory, a
`.sql` file, or a `.pgdac` — so DB↔DB, project↔DB, artifact↔DB, and artifact↔artifact all use one
code path. Non-DB sources are normalized through a real (embedded) PostgreSQL of the project's target
major.

---

## WP1 (2026-10-06) — what changed

| Audit finding | Fix |
|---|---|
| B1 non-deterministic plan order | `diff` walks collections in model order instead of ranging over maps (`TestDiff_Deterministic`) |
| B2 search_path-dependent definitions | `introspect` pins `search_path = ''` for the duration of a read and restores it; `.pgdac` format bumped to **2** (v1 artifacts open with a warning) |
| B6(i) SQL function bodies validated before their tables exist | `SET check_function_bodies = false` emitted at the top of scripts (`engine.SessionPreamble`) and set by the executor |
| B6(ii) functions emitted before the row types they return; views/triggers before the functions they call | `Function.DependsOn` (row types, enums, SQL-standard body refs), `Trigger.DependsOn`, view→function deps from `pg_depend`; planner edges for create and drop sides |
| B7 serial-backed sequences never created | Differ emits `CREATE SEQUENCE` before the table and `ALTER SEQUENCE … OWNED BY` after it; identity sequences stay implicit; drops cascade correctly |
| B8 `NOT VALID NOT VALID` | `NOT VALID` lives in `Constraint.Validated`; new `validateConstraint` op validates in place |
| `--target-major` missing on `build`/`extract` | Added; also threaded to project normalization on every verb and the daemon |
| Canonical text is not a parse fixed point (`c IN ('F','FE')` deparses to a form that re-parses differently), so a published target never converged on such expressions | `build.FromSQL` re-materializes the model from its own canonical SQL and re-introspects (fixed-point canonicalization; falls back with a warning if the schema uses unmanaged types) |

---

## WP2 (2026-10-06) — what changed

| Audit finding | Fix |
|---|---|
| B3 `CREATE OR REPLACE VIEW` cannot drop/rename/retype columns | `View.Columns` is introspected; a changed view whose column list is not a pure append is `DROP`ped and `CREATE`d, and its dependents are recreated (`viewReplaceCompatible`) |
| B4 `CREATE OR REPLACE FUNCTION` cannot change the result type | `Function.ReturnType` / `Arguments` are introspected; a changed result type, OUT parameter, or function↔procedure switch is `DROP`ped and `CREATE`d, and dependents are recreated (`functionRecreateReason`) |
| B5 a view blocks `ALTER COLUMN TYPE`, `DROP COLUMN`, `DROP TABLE`; a view/trigger blocks `DROP FUNCTION` | `View.ColumnDeps` (pg_depend `refobjsubid`) is introspected; `engine.expandDependents` finds every dependent transitively, drops it before the blocking op and recreates it after (desired form if the source has it, current form if it can still exist, drop-only if it references something that disappears). Each generated op carries a `Reason` rendered as a SQL comment |
| B9 `ALTER COLUMN TYPE` fails without `USING` | `USING "col"::type` is always emitted (a binary-coercible change still skips the rewrite); the note tells the user to edit it when values need transforming |
| Extract ordering (STATUS limitation 4) | `engine.ObjectScripts` emits one numbered file per object in the plan's global dependency order into `schema/`; `extract --to-project` writes that layout with a matching `pgdac.yaml`. `init` keeps the section folders for hand authoring |
| Silent cycle fallback | `plan.Plan.Cyclic` + `engine.Result.Warnings`; the CLI prints `warning (plan): …`, the daemon includes it in `warnings`, and the script header carries it |

Regression gates added: `TestExpandDependents_*` (unit: alter type under chained views, incompatible
view recreating its dependents, function result-type change under a view, drop table under a
rewritten view), `TestViewReplaceCompatible`, `TestFunctionRecreateReason`,
`TestExtractProjectRoundTrip` (extract → build → compare empty, both directions), and 15 scenario
entries flipped to `ok` plus 7 new ones (matview under a type change, OUT parameter change,
function→procedure, append-only view replace, …).

---

## Object coverage

**Managed (introspected, diffed, generated, published):** schemas, extensions, enums, collations,
sequences (standalone, serial-owned, identity-implicit), functions & procedures, tables (columns incl.
identity/generated/defaults/not-null, comments on create), PK/FK/unique/check/exclude constraints
(incl. `NOT VALID` state), indexes, triggers, RLS policies (+ table RLS enable/force), views,
materialized views.

**Not modeled — silently dropped on publish, `--validate` still reports convergence (WP3):**
partitioning (parent and children are created as plain tables), indexes on materialized views,
triggers on views (`INSTEAD OF`), `WITH CHECK OPTION`, trigger enabled/disabled state, `UNLOGGED`,
storage parameters (`fillfactor`, autovacuum…), column `STORAGE`/`COMPRESSION`/statistics target,
comment changes on existing objects, enum value removal/reorder, generated-expression changes,
grants/ACLs/ownership.

**Warned about, not managed:** domains, composite/range types, aggregate/window functions, foreign
tables, non-view rules. A table that uses an unmanaged type fails to publish.

---

## Known limitations

1. **Dependents the engine does not yet recreate.** Views, materialized views and triggers are
   handled. Policies, column defaults, CHECK constraints and index expressions that call a
   function being dropped/recreated, and generated columns that read an altered column, are not —
   PostgreSQL's own error surfaces at publish time. A recreated materialized view is rebuilt
   (`WITH DATA`), which recomputes its contents.
2. **Silently dropped attributes** — see "Object coverage". Rule to adopt: anything introspectable
   but unmanaged must warn; anything that would be created *wrong* (partitions) must error.
3. **Artifact trust (WP3).** The Ed25519 signature covers the Merkle root of the parts, not the
   manifest's name/version/targetMajor; `--require-key` exists only on `inspect`; the artifact does
   not carry pre/post-deploy scripts or the project manifest.
4. **`USING` casts are plain conversions.** `ALTER COLUMN TYPE` emits `USING "col"::type`; when
   values need a real transformation (parsing, defaults for unconvertible rows) the script must be
   edited by hand. The hazard note says so.
5. **Embedded PostgreSQL** re-extracts its binaries on start, so running the CLI while the GUI
   daemon is up on the same major can collide on the binaries directory. Run `go test -p 1 ./...`.
6. `.pgdac` is compressed, not encrypted.
7. A true dependency cycle (possible only for objects that were themselves created via ALTERs)
   is reported as a `plan` warning and the cyclic operations are appended in priority order; the
   migration may still fail to apply.

---

## Roadmap

- **WP3 — Honesty & trust.** Model partitioning, matview indexes, view triggers, check option,
  trigger state, comments, enum-removal hazard, unlogged, reloptions/storage; warn on grants; error
  on unmanaged types in use. Sign the canonical manifest; trusted keys + `--require-key` on every
  verb and the daemon; carry `pgdac.yaml` and pre/post-deploy scripts in the artifact and run them
  on publish; wire `dropPolicy`.
- **WP4 — DACPAC parity.** Refactorlog (renames without data loss), drift baseline +
  `BlockWhenDriftDetected`, GUI dependency-gated include/exclude, CI + release pipeline
  (Go sidecar matrix → `tauri-action` installers; enforce `ui/PGDAC_VERSION`).

---

## Repo layout

```
pg-dac/
  cmd/pg-dac/            CLI + verbs: diff script report plan publish build extract inspect serve init
  internal/
    model/              canonical schema model (+ JSON); lookups, identity-sequence predicate
    introspect/         live DB → model (pg_catalog via pgx); search_path pinned; dependency edges
    build/              .sql project/file → model (apply to scratch + introspect); manifest-aware
    devdb/              embedded Postgres lifecycle; per-version warm servers (WithShared/Acquire)
    diff/               deterministic name-keyed diff → typed ops
    plan/               pg_dump-style dependency ordering (toposort + priority + derived edges)
    sqlgen/             ops → lock-safe SQL + hazards
    safety/             hazard taxonomy + severity + version gating
    execute/            apply (session preamble, lock_timeout + retry; ApplyStream for SSE)
    engine/             Compare(), Result (+Warnings), dependents.go (drop/recreate dependents), ObjectChanges()/PlanDoc(), ObjectScripts (plan-ordered extract)
    artifact/           .pgdac read/write (format 2), Merkle, Ed25519 signing
    project/            pgdac.yaml manifest (+ doublestar globs)
    source/             resolve any source spec → model
  ui/                   Tauri GUI (src/ React, src-tauri/ Rust supervisor, binaries/ sidecar)
  testdata/             demo project + fixtures
```
Module: `github.com/rodrigonunes/pg-dac` (Go 1.25). License Apache-2.0.

---

## How to run

**CLI** (sources: connection string · project dir · `.sql` file · `.pgdac`; `--to` is desired,
`--from` is the target being changed):
```bash
go build -o bin/pg-dac ./cmd/pg-dac
pg-dac diff    --from "postgres://…/db" --to ./project
pg-dac script  --from "postgres://…/db" --to ./project --validate > migrate.sql
pg-dac publish --from "postgres://…/db" --to ./project            # data loss refused w/o --allow-hazards
pg-dac extract --from "postgres://…/db" --out app.pgdac           # or --to-project ./dir (schema/NNNN_*.sql in dependency order)
pg-dac build   --from ./project --out app.pgdac --target-major 18
pg-dac init    --dir ./project --name myapp
pg-dac inspect app.pgdac
```
> Connection strings: `.NET` form (`Host=…;Database=…;SSL Mode=Require`) → libpq form
> `host=… dbname=… sslmode=require` (or `postgres://…?sslmode=require`); pass the password via
> `PGPASSWORD` to keep it out of argv. Point a project source at the directory holding only schema
> DDL (not psql scripts), or add a `pgdac.yaml`.

**GUI:**
```bash
# build the sidecar for your Rust target triple, then run the app
$triple = (rustc -Vv | Select-String '^host:').ToString().Split(' ')[1]
go build -o "ui/src-tauri/binaries/pg-dac-$triple.exe" ./cmd/pg-dac
cd ui && pnpm install && pnpm tauri dev
```
Restart `pnpm tauri dev` after rebuilding the sidecar so it picks up engine changes.

**Tests:** `go test -short ./...` (no DB) · `go test -p 1 ./...` (+ embedded-Postgres integration:
bootstrap gate, scenario matrix, introspection invariants).
