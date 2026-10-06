# pg-dac — Project Status & Roadmap

_Last updated: 2026-06-04_

A **DACPAC / SqlPackage-equivalent for PostgreSQL**: compare schemas, generate dependency-ordered
lock-safe migration scripts, package a schema into a portable **signed `.pgdac` artifact**, and
publish — via a CLI, a local daemon, a declarative project model, and a Tauri desktop GUI.

---

## Status at a glance

| Area | State |
|---|---|
| **Phase 1 — CLI engine (M1–M6)** | ✅ Done & verified vs real PostgreSQL |
| **P0 — Structured diff API (`PlanDoc`, `plan --json`)** | ✅ Done & verified |
| **P0b — `pg-dac serve` daemon (warm Postgres + HTTP/SSE)** | ✅ Done & verified |
| **P1 — Project model (`pgdac.yaml`, `init`, `extract --to-project`)** | ✅ Done (round-trip verified on a typical schema) |
| **GUI — Tauri Schema-Compare app (`ui/`)** | ✅ Built, compiles, **visually confirmed working** |
| **Coverage: functions, triggers, RLS, collations, extensions** | ✅ Managed |
| **Next steps (N1–N9 below)** | ⏳ Planned |

Verified end-to-end against a real **Azure PostgreSQL 18** database (the `acme` schema, ~150 objects)
and throwaway Docker Postgres.

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

**Engine pipeline (pure Go, no external Postgres logic anywhere else):**
`introspect (pg_catalog via pgx)` → `typed model` → `diff (typed ops)` → `plan (pg_dump-style
dependency ordering)` → `sqlgen (lock-safe SQL + typed hazards)`.

A "source" is resolved to the **same model** regardless of kind — a live DB (connection string), a
declarative project directory, a `.sql` file, or a `.pgdac` artifact — so DB↔DB, project↔DB,
artifact↔DB, and artifact↔artifact all use one code path. Non-DB sources are **normalized via a real
database**: applied to a throwaway "scratch" Postgres and re-introspected, so both sides canonicalize
identically (no cosmetic false diffs).

---

## What's built & verified

### Phase 1 — CLI engine (M1–M6)
- **M1 — compare + script:** dependency-ordered, hazard-annotated migration SQL.
- **M2 — scratch DB + plan validation:** bundled embedded PostgreSQL (`fergusstrange/embedded-postgres`,
  per-version cache) normalizes projects and `--validate`s a plan by replaying it.
- **M3 — `publish`:** applies with lock-safety (session `lock_timeout` + jittered retry on 55P03,
  per-statement autocommit so `CONCURRENTLY` works and a failed publish is resumable), and a
  data-loss block gate (`--allow-hazards`, `--dry-run`).
- **M4 — signed `.pgdac` artifact:** `build`/`extract`/`inspect`; zip of `manifest.json` + `model.json`
  + canonical SQL, SHA-256 **Merkle root + Ed25519 signature** (tamper-resistant, beyond DACPAC's
  checksum-only seal).
- **M5 — artifact comparison:** any source on either side; publish *from* an artifact.
- **M6 — coverage + honesty:** functions/procedures, triggers, RLS policies; warns about object types
  it doesn't manage so a "no differences" is never silently wrong.

### Phase 2 — structured API, daemon, project model, GUI
- **P0 — `PlanDoc` / `plan --json`:** per-object change records with before/after definitions,
  per-object generated SQL, dependency edges, and severity-tagged hazards; object-level
  include/exclude that re-plans (`engine.Options.IncludeIDs/ExcludeIDs`).
- **SSDT Schema Compare layout** (`ui/src/components/DetailPane.tsx`, `monaco-setup.ts`): **Source (of
  truth) on the LEFT, Target (gets updated) on the RIGHT**; Update pushes Source → Target. The diff
  colors what happens *to the Target*: Source-only lines are **green (added to Target)**, Target-only
  lines are **red (removed)**. The `−`/`+` gutter glyphs are hidden (`renderIndicators:false`) so
  nothing contradicts the colors (SSDT doesn't show them either). A **"Show result (with changes
  applied)"** checkbox swaps the diff for a single clean pane of the object's definition after the
  change. Source bar, diff, and progress dialog all use Source-left / Target-right.
- **P0b — `pg-dac serve`:** loopback HTTP/SSE daemon keeping the embedded Postgres **warm** (first
  `/plan` ~seconds cold, then sub-second). Stdout **port handshake** + `PGDAC_TOKEN` bearer auth.
- **P1 — project model:** `pgdac.yaml` (name, version, `targetMajor`, schema scope, ordered `include`
  globs, pre/post-deploy, `dropPolicy`); `pg-dac init` scaffolds it; `build` honors manifest order;
  `extract --to-project` reverse-engineers a DB into a dependency-ordered project tree.
- **GUI (`ui/`):** Tauri v2 + React 19 / Vite 6 / TS, Monaco `DiffEditor`, TanStack Virtual, Zustand —
  source pickers, object tree with include/exclude, definition-diff + generated-SQL panes, publish
  modal (per-hazard-code acknowledgement gating + live SSE progress), build/inspect. Mirrors the
  `rmnunes/pg-shell` stack.

### Responsiveness & cancellation (GUI)
- Every Tauri command runs **off the main thread** (async + `spawn_blocking`), so the window stays
  interactive (minimize/move/repaint) during a compare — a synchronous command had been blocking the
  OS event loop, freezing the window as "Not Responding."
- **Compare is now a streamed operation.** The daemon's `/plan` is SSE: `phase` events (reading
  target → reading desired → diffing/planning, with object counts) then a `result`. A
  **CompareProgress** modal narrates this live.
- **Cancel** — `POST /cancel` + a per-request cancelable context registered by client-supplied `opId`;
  cancellation propagates through `source.Resolve` → introspect/scratch-build (pgx is context-aware),
  so it aborts the work and frees the warm Postgres, not just abandons the result. Verified live:
  canceling a hung compare yields `phase → canceled`. The store ignores late events after a cancel.

### GUI orientation — SSDT Schema Compare (Source left → Target right)
- The whole GUI mimics SSDT: left = **Source** (of truth), right = **Target** (gets updated); *Update*
  pushes **Source → Target**. The source bar, definition diff, and progress dialog all use this order.
  Mapping to the engine: Source = `--to` (desired), Target = `--from` (the side a publish applies to).
  A direction banner states "Update Target to match Source — N add · M change". Publish is disabled
  unless the Target is a DB connection (you can't apply to a folder; use Generate Script to back-port).
  (History: tried SSDT → inverted-colors → GitHub-standard before→after → back to SSDT. The glyph
  inconsistency that drove the detours is solved by hiding the `−`/`+` indicators, not by flipping
  sides. To push dev→prod: Source = DEV, Target = PROD.)
- Note the semantics never changed — only presentation. `add` = present in Source, missing from
  Target; conservative drop-hiding still applies, which is why swapping Source/Target changes the
  count (a project-vs-DB compare surfaced that the dev DB was 31 functions + 2 views ahead of the
  declarative `deploy/` project — real drift, correctly reported).

### Saved source profiles (GUI)
- Connection strings, project dirs, and `.pgdac` artifacts can be **saved and reused** across
  sessions. Persisted as JSON in the Tauri app config dir (`profiles.json`) via Rust commands
  (`profiles_list`/`profile_add`/`profile_remove` in `ui/src-tauri/src/profiles.rs`).
- **Secrets are never displayed.** A DB connection shows only a masked `database @ host` label
  (`ui/src/state/profiles.ts` derives it from both URL and key/value DSN forms); the password is held
  only in the file and resolved to the real value at compare time, never bound to a visible input.
  (Per the user's choice, the DSN is stored verbatim in the local file — no OS keychain.)
- A `SourcePicker` per side: typed value, a saved-list dropdown (pick/delete), and a 💾 save flow.
  Each side's store value is either a raw value or a `profile:<id>` ref; `resolveSpec` swaps refs for
  the real value when building the compare request.
- **Resizable change list:** a draggable `.splitter` between the change list and the results; the
  width persists in `localStorage` (`pgdac.treeWidth`) so it survives restarts.

### Hardening & bug fixes done this effort
- **Identity columns** diffed (serial→identity conversion handled).
- **Serial-backed sequences** emitted on extract (+ `OWNED BY`).
- **Line-ending normalization** (CRLF/CR→LF) on all introspected definitions — fixes phantom
  function diffs when a DB body was deployed with CRLF but the source files are LF.
- **Collations** fully managed (ICU/libc/builtin; version-aware catalog columns).
- **Version-matched scratch** Postgres (matches the project's `targetMajor`; **per-version** binary
  cache so PG16/PG18 don't clobber each other).
- **`os.Exit` cleanup bug** fixed (was orphaning embedded Postgres on command error).
- **GUI blank-page** fixed (React Rules-of-Hooks violation) + error boundary + **Monaco bundled
  locally** (CSP-safe, no CDN) + `worker-src` CSP.
- Clearer error when a source contains psql client-side meta-commands (`\i`, `\echo`).

---

## Object coverage

**Managed (introspected, diffed, generated, published):** schemas, extensions, enums, **collations**,
sequences (incl. identity/serial), functions & procedures, tables (columns incl.
identity/generated/defaults/not-null/comments), PK/FK/unique/check/exclude constraints, indexes,
triggers, RLS policies (+ table RLS enable/force), views, materialized views.

**Not yet managed (reported as warnings, never silently dropped):** domains, composite/range types,
aggregate/window functions, foreign tables, non-view rules, partition attachments, ACL/privileges,
comment-changes on existing objects, and column attribute changes beyond type/default/not-null/identity
(generated-expression, storage/compression).

---

## Repo layout

```
pg-dac/
  cmd/pg-dac/            CLI + verbs: diff script report plan publish build extract inspect serve init
  internal/
    model/              canonical schema model (+ JSON)
    introspect/         live DB → model (pg_catalog via pgx); line-ending + version-aware
    build/              .sql project/file → model (apply to scratch + introspect); manifest-aware
    devdb/              embedded Postgres lifecycle; per-version warm servers (WithShared/Acquire)
    diff/               name-keyed diff → typed ops
    plan/               pg_dump-style dependency ordering (toposort + priority)
    sqlgen/             ops → lock-safe SQL + hazards
    safety/             hazard taxonomy + severity + version gating
    execute/            apply (lock_timeout + retry; ApplyStream for SSE)
    engine/             Compare(), Result, ObjectChanges()/PlanDoc(), per-object canonical SQL, ObjectScripts
    artifact/           .pgdac read/write, Merkle, Ed25519 signing
    project/            pgdac.yaml manifest (+ doublestar globs)
    source/             resolve any source spec → model
  ui/                   Tauri GUI (src/ React, src-tauri/ Rust supervisor, binaries/ sidecar)
  testdata/             demo project + fixtures
  README.md  STATUS.md  (this file)
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
pg-dac extract --from "postgres://…/db" --out app.pgdac           # or --to-project ./dir
pg-dac init    --dir ./project --name myapp
pg-dac inspect app.pgdac
```
> Connection strings: `.NET` form (`Host=…;Database=…;SSL Mode=Require`) → libpq form
> `host=… dbname=… sslmode=require` (or `postgres://…?sslmode=require`); pass the password via
> `PGPASSWORD` to keep it out of argv.

**GUI:**
```bash
# build the sidecar for your Rust target triple, then run the app
$triple = (rustc -Vv | Select-String '^host:').ToString().Split(' ')[1]
go build -o "ui/src-tauri/binaries/pg-dac-$triple.exe" ./cmd/pg-dac
cd ui && pnpm install && pnpm tauri dev
```
Restart `pnpm tauri dev` after rebuilding the sidecar so it picks up engine changes.

**Tests:** `go test -short ./...` (no DB) · `go test ./...` (+ embedded-Postgres integration).

---

## Known limitations / caveats

1. **`extract --to-project` / from-empty build of the largest schemas** — one-object-per-file +
   coarse section ordering can't satisfy every cross-kind dependency (e.g. a function signature that
   references a view's row-type). DB↔project and DB↔DB compares/publish are solid; the absolute
   from-empty bootstrap of a ~150-object schema is the path with remaining edge cases. **Proper fix:
   drive extract file numbering from a true `pg_depend` global topological sort (N1).**
2. **Embedded-Postgres re-extracts on every start** → running the CLI *while the GUI is open* on the
   same PG version conflicts on the binaries dir ("Access is denied"). Fine for normal single-app use;
   artifact-vs-DB compares need no scratch and are immune.
3. **First run downloads the embedded Postgres binaries** (per version, ~tens of MB) — the first
   project/`--validate` compare is slow, then warm.
4. **`pgdac.yaml` `dropPolicy`** is documented but not yet wired to the engine's drop options (the
   CLI/GUI flags control drops today).
5. **GUI include/exclude** is plain (exclude + re-plan); not yet SSDT-style dependency-gated (N2).
6. `.pgdac` is **compressed, not encrypted** (signed for integrity, not confidentiality).

---

## Next steps & milestones (prioritized)

- **N1 — Bulletproof extraction / from-empty build.** Order extracted files by a real `pg_depend`
  topological sort across all object kinds (incl. function-signature→type, function→view-rowtype), so
  any schema round-trips and a fresh-server bootstrap of the whole schema applies cleanly. *(Biggest
  correctness win for the gnarliest schemas.)*
- **N2 — GUI dependency-gated include/exclude.** Use `ObjectChange.dependsOn` to disable a row whose
  dependents are still included (SCC-aware for cycles), matching SSDT.
- **N3 — Release / CI pipeline.** GitHub Actions: Go sidecar matrix (triple-named, `CGO_ENABLED=0`) →
  frontend dist → `tauri-action` 3-OS signed installers; version-pin the bundled sidecar via
  `ui/PGDAC_VERSION` + the daemon's `formatVersion` handshake.
- **N4 — Refactorlog (renames).** `refactorlog.json` + a `pg_dac_refactorlog` applied-ops table so
  `ALTER … RENAME` is emitted instead of drop+create (preserves data; DACPAC parity).
- **N5 — Drift governance.** Register a baseline model hash in a `pg_dac` schema in the target;
  `drift` verb + `BlockWhenDriftDetected` on publish.
- **N6 — Broader object coverage.** Domains, composite/range types, partition attachments,
  ACL/ownership, comment-change diffing, generated-expression/storage column attributes.
- **N7 — `.pgbac` data artifact.** Schema + table data (BACPAC analog), deferred.
- **N8 — Standalone lint.** Squawk-style migration linting surfaced in CLI + GUI.
- **N9 — Embedded-PG concurrency hardening.** Skip re-extraction when the version cache is complete;
  detect/reuse a running instance so CLI + GUI coexist.

---

## Pointers
- Approved design plan: `~/.claude/plans/this-is-an-empty-spicy-moon.md`.
- GUI specifics: `ui/README.md`.
- Reference project this was validated against: `<reference project>`
  (its `pgdac.yaml` scopes the schema to `deploy/`).
