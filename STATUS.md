# pg-dac — Project Status & Roadmap

_Last updated: 2026-10-07 — **roadmap complete** (WP0–WP4; see the audit notes for the audit and per-WP addenda)_

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
| Signed `.pgdac` artifact (Merkle + Ed25519), format v3 | ✅ Signature covers the whole manifest; `--require-key` on every verb and the daemon; optional `trusted_keys` file (WP3) |
| Artifact as a deployment unit | ✅ Carries `pgdac.yaml`, pre/post-deploy scripts and the drop policy; `publish` runs them (WP3) |
| `pg-dac serve` daemon (warm Postgres, HTTP/SSE, cancel) | ✅ |
| Project model (`pgdac.yaml`, `init`) | ✅ `include`/`exclude`/`schemas`/`targetMajor`/`preDeploy`/`postDeploy`/`dropPolicy` all honored |
| Tauri GUI | ✅ Builds and type-checks; previously confirmed working visually |
| Object coverage | ✅ Partitioning, domains, composite types, matview indexes, view triggers/options, trigger state, UNLOGGED, storage parameters, column storage/compression/statistics, comments (WP3); grants are warned about |
| Refactorlog (renames without drop + create) | ✅ `refactorlog.json` + `pg-dac refactor rename`; applied as `ALTER … RENAME`, recorded per target in `pgdac.refactorlog` (WP4) |
| Drift detection / `BlockWhenDriftDetected` | ✅ Every publish records a baseline in `pgdac.baseline`; `pg-dac drift` (exit 2 on drift), `pg-dac baseline`; publish refuses a drifted target unless `--allow-drift` (WP4) |
| Lint | ✅ `pg-dac lint` — hazards by code/severity/object, exit 2 at or above `--fail-on` (WP4) |
| GUI dependency-gated include/exclude | ✅ Excluding an object excludes what depends on it; including brings dependencies back; new options (signer key, deploy scripts, drift); engine format/version handshake (WP4) |
| CLI + GUI coexistence (embedded PostgreSQL) | ✅ Complete binaries cache is reused, not re-extracted (the library's `pg_ctl` check missed `.exe` on Windows) (WP4) |
| Version control / CI / release | ✅ Git; GitHub Actions CI (Go matrix, embedded-PG integration, GUI type-check/build, cargo check) and tag-driven release (CLI per triple + Tauri installers); versions pinned and test-enforced (WP4) |

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

## WP3 (2026-10-06) — what changed

| Audit finding | Fix |
|---|---|
| §4.2 partitioning created as plain tables | `Table.PartitionKey` (PARTITION BY) and `Table.PartitionOf` (PARTITION OF … bound) introspected and rendered; partitions keep only their local objects; parent-before-child / child-before-parent edges; a re-bound partition is recreated (data-loss gated) |
| §4.2 matview indexes, view triggers, `WITH CHECK OPTION` dropped | `MaterializedView.Indexes`, `View.Triggers`, `View.CheckOption`/`SecurityBarrier`/`SecurityInvoker` introspected, diffed and rendered; recreated views/matviews get their triggers/indexes back |
| §4.2 trigger state, `UNLOGGED`, storage parameters, column storage/compression/statistics | `Trigger.Enabled`, `Table.Persistence`, `Table.RelOptions` (incl. `toast.*`), `Column.Storage`/`Compression`/`StatsTarget` diffed with dedicated ops |
| §4.2 comments on existing objects | `setComment` op for every managed object and sub-object (ROUTINE/COLUMN/CONSTRAINT/INDEX/TRIGGER/POLICY/…); comments on constraints, indexes and policies are now emitted on create too |
| §4.2 enum value removal/reorder, generated-expression change | New labels are inserted in position (`ADD VALUE … BEFORE/AFTER`); removal/reorder, domain/composite changes and re-partitioning are emitted as an **`UNSUPPORTED_CHANGE`** hazard (error severity, `RAISE WARNING` statement) so the script says it will not converge; generated columns use `SET EXPRESSION` (PG17+), `DROP EXPRESSION` (PG13+) or drop + re-add |
| §4.2 domains and composite types caused hard failures | Both are managed (create/drop; a changed definition is an unsupported-change hazard) |
| §4.2 grants | Counted and warned (`objects with explicit privileges … privileges are not published`); still not managed |
| §4.3 signature covered only the Merkle root | Format **3**: the signature covers the canonical JSON of the whole manifest (older artifacts verify with their scheme) |
| §4.3 no authenticity on compare/publish | `--require-key` on diff/script/report/plan/publish/build/extract and the daemon's `requireKey`; a `trusted_keys` file (`<UserConfigDir>/pg-dac/trusted_keys`) gates every artifact source when present; otherwise the signer fingerprint (or "unsigned") is reported as a warning |
| §4.3 artifact not a deployment unit; `preDeploy`/`postDeploy`/`dropPolicy` dead | `build` embeds `project/pgdac.yaml` and `deploy/pre|post/NNNN_*.sql`; `publish` runs pre-deploy scripts before and post-deploy scripts after the migration (`--no-deploy-scripts` to skip); `dropPolicy` (`none`/`safe`/`all`) is the default drop behavior for compare and publish unless `--drop-objects-not-in-source` is given |
| Publish gate | `HasBlocking` (any error-severity hazard: data loss or unsupported change) replaces the data-loss-only gate |

Regression gates added: `TestDiff_*` for every new op, `TestSignatureCoversManifestMetadata`,
`TestResolveArtifactTrust`, deploy package tests (plan from project, artifact parts, drop policy,
script execution), introspection invariants for the new attributes, the bootstrap fixture extended
with every formerly-lossy attribute (partitions, domain, composite, matview index, view trigger,
check option, security barrier, disabled trigger, UNLOGGED + fillfactor, storage/statistics,
comments), and ~30 scenario entries (lossy ones now verified, misses flipped to `ok`, two `noconv`).

---

## WP4 (2026-10-07) — what changed

| Roadmap item | Delivered |
|---|---|
| N4 refactorlog | `internal/refactor`: `refactorlog.json` (`operations[]` with stable ids: table, column, view, matview, sequence, function, index, constraint, trigger, policy, enum, domain, compositeType, schema; optional `toSchema`). `pg-dac refactor rename/list`. Compare verbs apply pending renames to the target model and emit the `ALTER … RENAME` statements as a prelude; `publish` runs them, records the ids in the target's `pgdac.refactorlog`, re-reads the target and then migrates — data and dependents survive. Carried inside the artifact (`project/refactorlog.json`) |
| N5 drift | `internal/drift`: `pgdac.baseline` (gzipped canonical model + SHA-256, source, time). `publish` checks the target against its latest baseline before touching it and refuses on drift (`--allow-drift` overrides; `--no-baseline` skips recording); every successful publish records a fresh baseline. `pg-dac drift` explains the drift as a plan (exit 2), `pg-dac baseline` records one explicitly; daemon `/drift` and `allowDrift`/`recordBaseline` on publish |
| N8 lint | `pg-dac lint --from --to [--fail-on error|warning|none] [--format json]` — hazards grouped by code with severity, count and objects; exit 2 when the threshold is hit |
| N2 GUI gating | Excluding a change auto-excludes every plan change that depends on it (transitively, non-drop), shown disabled with the cause; including a change re-includes its plan dependencies. Options panel gained signer key, skip deploy scripts, allow drift. On start the UI checks the engine's plan format and pinned version |
| N9 embedded-PG coexistence | Root cause found: the library decides to extract by stat'ing `bin/pg_ctl` without `.exe`, so on Windows every start re-extracted over a running instance's files. The cache now gets an executable `pg_ctl` copy plus a completion marker; incomplete caches are wiped and rebuilt. Test: a second server starts while the first runs |
| N3 CI / release | `.github/workflows/ci.yml` (Go build/vet/unit on Linux/Windows/macOS, full embedded-PG suite, GUI `tsc` + `vite build`, `cargo check`) and `release.yml` on `v*` tags (tag must equal `ui/PGDAC_VERSION`; CLI for 5 triples with the version stamped; `tauri-action` installers with the matching sidecar; checksummed release assets). `TestVersionsAgree` keeps `ui/PGDAC_VERSION`, `package.json`, `tauri.conf.json`, `Cargo.toml` and the Go default in step |
| Reserved schema | pg-dac's bookkeeping lives in schema `pgdac` (PostgreSQL reserves `pg_*`), which introspection never includes in a model |

---

## Object coverage

**Managed (introspected, diffed, generated, published):** schemas, extensions, enums (labels added
in position), domains, composite types, collations, sequences (standalone, serial-owned,
identity-implicit), functions & procedures (incl. result-type changes), tables (columns incl.
identity/generated/defaults/not-null/storage/compression/statistics; `UNLOGGED`; storage parameters;
partitioning — `PARTITION BY` parents and `PARTITION OF` children), PK/FK/unique/check/exclude
constraints (incl. `NOT VALID` state), indexes (on tables and materialized views), triggers (on
tables and views, incl. enabled state), RLS policies (+ table RLS enable/force), views (incl. column
list, `CHECK OPTION`, `security_barrier`/`security_invoker`), materialized views, and comments on
all of the above.

**Reported as `UNSUPPORTED_CHANGE` (error-severity hazard; the script will not converge until done by
hand):** removing or reordering enum values, changing a domain or composite type definition,
changing a table's partitioning.

**Warned about, not managed:** grants/ACLs/ownership, aggregate/window functions, range/multirange
types, foreign tables, non-view rules, `INHERITS` (non-partition) inheritance.

---

## Known limitations

1. **Dependents the engine does not yet recreate.** Views, materialized views and triggers are
   handled. Policies, column defaults, CHECK constraints and index expressions that call a
   function being dropped/recreated, and generated columns that read an altered column, are not —
   PostgreSQL's own error surfaces at publish time. A recreated materialized view is rebuilt
   (`WITH DATA`), which recomputes its contents.
2. **Grants are not published.** Explicit privileges are counted and warned about; ownership is
   not modeled either. Partitions can be created and dropped but existing tables are not
   attached/detached, and `INHERITS` inheritance is not modeled.
3. **Deploy scripts are plain SQL.** psql meta-commands (`\i`, `\echo`) cannot run server-side;
   pre/post-deploy scripts run as one implicit transaction each and are re-run on every publish,
   so they must be idempotent.
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

**All planned work packages (WP0–WP4) and the audit's N1–N9 items are complete.** What remains
is explicitly out of scope for v1 or deferred:

- **Not managed (warned):** grants/ACLs/ownership, attaching/detaching *existing* partitions,
  `INHERITS` inheritance, aggregate/window functions, range/multirange types, foreign tables and
  servers, rules, event triggers, publications/subscriptions.
- **Reported as `UNSUPPORTED_CHANGE` rather than applied:** removing/reordering enum values,
  altering a domain or composite type definition, re-partitioning a table.
- **Deferred:** a `.pgbac` data artifact (schema + table data, the BACPAC analog); a GUI for
  authoring the refactorlog (today: `pg-dac refactor rename`); dependents other than views,
  materialized views and triggers when a function is recreated (policies, defaults, CHECKs,
  index expressions — PostgreSQL's own error surfaces).

---

## Repo layout

```
pg-dac/
  cmd/pg-dac/            CLI + verbs: diff script report plan lint publish build extract inspect serve init refactor drift baseline
  .github/workflows/     CI (Go matrix + embedded-PG integration + GUI + cargo) and tag-driven release
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
    artifact/           .pgdac read/write (format 3: signed manifest, deploy parts), Merkle, Ed25519 signing, trusted_keys
    deploy/             pre/post-deploy scripts, dropPolicy and refactorlog from a project or artifact; publish runs them
    refactor/           refactorlog.json: renames applied as ALTER … RENAME, recorded in pgdac.refactorlog
    drift/              baselines in pgdac.baseline; drift detection and explanation
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
pg-dac publish --from "postgres://…/db" --to ./project            # runs pre-deploy, migration, post-deploy; blocking hazards refused w/o --allow-hazards
pg-dac publish --from "postgres://…/db" --to app.pgdac --require-key <fingerprint>
pg-dac extract --from "postgres://…/db" --out app.pgdac           # or --to-project ./dir (schema/NNNN_*.sql in dependency order)
pg-dac build   --from ./project --out app.pgdac --target-major 18
pg-dac init    --dir ./project --name myapp
pg-dac inspect app.pgdac
pg-dac refactor rename --dir ./project --kind table --schema app --from customers --to clients
pg-dac drift   --from "postgres://…/db"                           # exit 2 when the target changed outside pg-dac
pg-dac lint    --from "postgres://…/db" --to ./project --fail-on error
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
