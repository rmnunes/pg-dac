# pg-dac — Project Status & Roadmap

_Last updated: 2026-11-23 — **v0.9.4**, the release candidate on the way to 1.0. Work packages WP0–WP8 are complete; see [Roadmap](#roadmap) for what comes next._

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
| `extract --to-project` | ✅ One file per object at a stable path, `schema/<schema>/<kind>/<name>.sql` (WP6); round-trips the ~900-object production schema |
| Update a project from a database (project as Target) | ✅ `publish --from <project> --to <db>`; files of changed objects rewritten in place, new files at stable paths, drops per policy, `roles:` kept current; file-level `--dry-run`; imperative definitions refused with directions (WP6) |
| Order-free project files | ✅ The build applies statements with deferred retry, like an SSDT project (WP6) |
| Export / Import of schema and data (`.pgbac`) | ✅ One consistent snapshot, gzip CSV per table, sequence positions, Merkle root and signature over the data; import into an empty database pg_dump-style with baseline recorded (WP7) |
| Production publish controls | ✅ `--single-transaction` (CONCURRENTLY-aware batching), `--statement-timeout`, `--lock-timeout`, `--var` deploy variables, `--exclude-object-type`, `--validate`, `--dry-run` (WP7) |
| Publish into an **empty** database (bootstrap) | ✅ Fixed in WP1; regression-gated by `TestBootstrap_FromEmpty` |
| Deterministic output / stable artifact hash | ✅ Fixed in WP1 (differ walks model order) |
| Model independent of session `search_path` | ✅ Fixed in WP1 (introspection pins `search_path = ''`) |
| Signed `.pgdac` artifact (Merkle + Ed25519), format v3 | ✅ Signature covers the whole manifest; `--require-key` on every verb and the daemon; optional `trusted_keys` file (WP3) |
| Artifact as a deployment unit | ✅ Carries `pgdac.yaml`, pre/post-deploy scripts and the drop policy; `publish` runs them (WP3) |
| `pg-dac serve` daemon (warm Postgres, HTTP/SSE, cancel) | ✅ |
| Project model (`pgdac.yaml`, `init`) | ✅ `include`/`exclude`/`schemas`/`targetMajor`/`preDeploy`/`postDeploy`/`dropPolicy` all honored |
| Tauri GUI | ✅ SSDT-style layout after the UX evaluation (`ui/UX-REVIEW.md`): toolbar, source dropdowns, Options dialog, change list grouped by action with keyboard navigation, Risks tab, notes panel, native dialogs; axe-core clean; builds and type-checks (WP8) |
| Object coverage | ✅ Partitioning, domains, composite types, matview indexes, view triggers/options, trigger state, UNLOGGED, storage parameters, column storage/compression/statistics, comments (WP3); privileges — GRANT/REVOKE on schemas, tables, views, sequences, routines, types and columns (WP5) |
| Canonical comparison of live databases | ✅ Every side, a live database included, is brought to PostgreSQL's deparse fixed point through the scratch server before diffing (memoized), so an expression never differs only by how it was deparsed (WP5) |
| Comparison semantics (SSDT options) | ✅ Routine bodies compared modulo whitespace (`--compare-whitespace`), comments optional (`--ignore-comments`), privileges optional (`--ignore-privileges`; revokes follow the drop policy), column order `ignore`/`warn`/`rebuild` (`--column-order`); defaults per project in `pgdac.yaml` `compare:` (WP5) |
| Table rebuild (declared column order) | ✅ Opt-in: one atomic block creates the table in the declared order, copies rows, swaps; constraints, indexes, triggers, policies, privileges, owned sequences, foreign keys from other tables and dependent views are recreated; the drop policy decides what the target-only columns do (WP5) |
| Sequence seeding | ✅ A column that gains an identity or a default on a new sequence on a populated table has the sequence seeded past `max(col)` (WP5) |
| Connection resiliency | ✅ Live-database connects (`internal/dbconn`) retry with backoff (250 ms, 500 ms, 1 s) what heals by waiting — DNS, dial/read failures before the handshake, a server starting up (57P03), full (53300) or reporting a connection exception (08xxx); authentication and other server answers are not retried; errors name the server, never the password (WP8 follow-up) |
| Index rebuild without a gap | ✅ A changed index is built `CONCURRENTLY` under a temporary name while the current one serves, then swapped in (drop old, rename); an index left under its rebuild name by an interrupted run is removed whatever the drop policy; offline or partitioned: drop + create in one transaction, the drop `IF EXISTS` (WP8 follow-up) |
| Refactorlog (renames without drop + create) | ✅ `refactorlog.json` + `pg-dac refactor rename`; applied as `ALTER … RENAME`, recorded per target in `pgdac.refactorlog` (WP4) |
| Drift detection / `BlockWhenDriftDetected` | ✅ Every publish records a baseline in `pgdac.baseline`; `pg-dac drift` (exit 2 on drift), `pg-dac baseline`; publish refuses a drifted target unless `--allow-drift` (WP4) |
| Lint | ✅ `pg-dac lint` — hazards by code/severity/object, exit 2 at or above `--fail-on` (WP4) |
| GUI dependency-gated include/exclude | ✅ Excluding an object excludes what depends on it; including brings dependencies back; new options (signer key, deploy scripts, drift); engine format/version handshake (WP4) |
| CLI + GUI coexistence (embedded PostgreSQL) | ✅ Complete binaries cache is reused, not re-extracted (the library's `pg_ctl` check missed `.exe` on Windows) (WP4) |
| Desktop updates | ✅ Tauri updater driven from the Rust layer so the sidecar is stopped before the installer replaces it; signed `latest.json` from GitHub Releases; update strip, manual check, restart gated on running operations; release refuses to run without the updater key (`docs/UPDATES.md`). Signed releases since v0.9.2 |
| Version control / CI / release | ✅ Public at github.com/rmnunes/pg-dac; GitHub Actions CI (Go on Linux/Windows/macOS, embedded-PG integration, GUI type-check/lint/tests/build, cargo fmt + clippy) and tag-driven release (CLI for five targets + signed Tauri installers, published as a draft); versions pinned and test-enforced, Tauri JS packages checked against their Rust crates (WP4, 0.9.2) |
| Saved passwords | ✅ A saved connection string's password is kept in the OS credential store (Windows Credential Manager, macOS Keychain, Secret Service), never in `profiles.json` or the frontend; older files are migrated on first read (0.9.4) |
| Baselines without passwords | ✅ A publish whose desired state is a database records only the server it names in the target's `pgdac.baseline`; older rows are redacted when read (0.9.4) |
| Text encoding | ✅ The scratch cluster is UTF-8 with the C locale on every platform and every session is UTF-8, so non-ASCII definitions compare the same on Windows and Linux; `PGCLIENTENCODING` is ignored (0.9.3) |
| Target platform check | ✅ A desired state normalized for a newer PostgreSQL major than the target runs is refused, like SqlPackage's platform check (`--target-major`, `--allow-incompatible-platform`); version-gated statements follow the target's version (0.9.3) |

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
code path. Every source, a live database included, is brought to its canonical fixed point through
a real (embedded) PostgreSQL of the target major: the model is rendered to SQL, applied to a scratch
database and re-introspected until it stops changing, so the text PostgreSQL deparses for an
expression is the same on both sides whatever created the object. Drift detection keeps the raw
introspection (baseline and current are read the same way).

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

## WP5 (2026-10-07) — what changed

Found while comparing the first real project against its Azure DEV database (see the audit notes,
WP5 addendum): of seven reported changes, two were deparse-form artifacts, two were whitespace,
one was comments only, one would have broken inserts, and the view recreate would have dropped a
grant silently.

- **Live databases are canonicalized too** (`build.Canonicalize`, used by `source.Resolve` and
  `publish`): the deparse fixed point is now reached on every side. Memoized per model hash;
  owners and the exact server version are restored from the raw introspection; a failure (an
  extension the scratch server lacks) falls back to the raw model with an explicit warning.
- **Routine bodies compare modulo whitespace** (`internal/sqltext`: a lexer that collapses
  whitespace outside literals, one level into `$function$` bodies for `sql`/`plpgsql` only);
  `--compare-whitespace` restores exact comparison, `--ignore-comments` also ignores comments.
- **Privileges are modeled**: effective ACLs (built-in defaults included, owner entry excluded) on
  schemas, tables, views, materialized views, sequences, routines, enums, domains, composite types
  and table columns. GRANT for what the source adds; REVOKE only under drop policy `all`
  (SSDT's `DropPermissionsNotInSource`); an object pg-dac drops and recreates always gets its
  privileges back, even with `--ignore-privileges`. A GRANT to a role the live target lacks is a
  blocking `MISSING_ROLE` hazard; when it is the only blocker, `publish` runs the project's
  pre-deploy scripts (where roles are provisioned) and re-checks before refusing. Projects list
  environment roles in `pgdac.yaml` `roles:` (created in the scratch database so the project
  builds); `extract` writes them. A `CREATE OR REPLACE` keeps an object's ACL, so only a real
  drop + create re-grants.
- **Column order policy** (`compare.columnOrder` / `--column-order`): `warn` (default) reports a
  declared order PostgreSQL cannot reach by appending; `ignore` is silent; `rebuild` performs the
  SSDT table rebuild as one atomic `DO` block with the drop policy deciding what happens to
  target-only columns, constraints, indexes, triggers, policies and owned sequences.
- **Sequence seeding**: serial→identity, identity→serial and "new default on a new sequence" seed
  the sequence past `max(col)`; identity→serial now orders `CREATE SEQUENCE` after `DROP IDENTITY`.
- **Daemon/GUI**: warnings carry the side they concern (Target / Source / Plan); the options panel
  exposes privileges, column order, whitespace and comments (project default by default). Plan
  format 2, artifact format 4.

---

## WP6 (2026-10-07) — the project as a compare target

The principle: the tool, not the operator, closes the loop in both directions, as SSDT's Schema
Compare does when the project is the target.

- **Order-insensitive build** (`execute.ApplyDeferred`, `sqltext.Split`): statements are applied
  one by one; those that fail are retried after the rest until a pass makes no progress, and the
  survivors are reported together. A view before its table, a foreign key before the referenced
  table, a trigger before its function all build. Hand-authored projects need no numbering.
- **Stable layout** (`engine.ProjectFiles`): one file per object at `schema/<schema>/<kind>/<name>.sql`
  (and `extensions/<name>.sql`, `schema/<schema>/schema.sql`), holding the object's CREATE, its
  constraints, indexes, triggers, policies, comments and grants. Routine overloads get a hash
  suffix; names that collide on a case-insensitive file system too. `extract --to-project` writes
  it and refuses to overwrite an existing project (that is an update).
- **Project as a publish target** (`internal/projsync`, `publish --from <project> --to <db>`,
  daemon and GUI "Update Target project"): the project's files are indexed by the object each
  statement defines or annotates (`sqltext.ParseHead`); a changed object's declarative statements
  are replaced in place by the canonical rendering (leading comments kept), a new object gets a
  file at the stable path, a dropped object loses its statements under drop policy `all`, new
  grantees are appended to `roles:` without reformatting the manifest. `--dry-run` prints a
  unified file diff. An object defined imperatively is refused with directions; `--allow-hazards`
  updates the rest. Routine overloads are rewritten as one unit.
- **Directions in messages**: the column-order warning and the serial-to-identity note say both
  ways out — rebuild the target, or update the project from it.

---

## WP7 (2026-10-07) — SqlPackage completeness

- **Single-transaction publish** (`publish --single-transaction`, `script --single-transaction`,
  daemon/GUI): consecutive transactional statements run in one transaction, rolled back together
  on failure and retried as a whole on a lock timeout; `CONCURRENTLY` statements run on their own
  between transactions; with `--no-online` the whole migration is one transaction
  (`IncludeTransactionalScripts`). `--statement-timeout` sets `statement_timeout`.
- **Object-type exclusion** (`compare.excludeObjectTypes`, `--exclude-object-type`, daemon/GUI):
  table, view, matview, sequence, function, enum, domain, compositeType, collation, schema,
  extension, index, constraint, trigger, policy, privilege, comment (`ExcludeObjectTypes`).
- **Deploy variables** (`pgdac.yaml variables:`, `--var name=value`, daemon/GUI): `$(name)`
  placeholders in pre/post-deploy scripts; every placeholder must resolve before anything runs;
  schema files are never substituted (SQLCMD variables).
- **Export / Import** (`internal/pgbac`; `pg-dac export`, `pg-dac import`, `inspect`): the
  `.pgbac` archive carries the canonical model plus one gzip CSV per leaf table (generated columns
  excluded) and every sequence position, read in one REPEATABLE READ snapshot; parts are hashed
  into the Merkle root and the signature, data parts verified while streaming. Import requires an
  empty target, creates pre-data objects, COPYs the rows, restores sequences (identity sequences
  located through `pg_depend`), then adds keys, indexes, triggers, policies and fills materialized
  views, and records the drift baseline (Export/Import, the BACPAC analog).

---

## WP8 (2026-10-07) — GUI redesign, SSDT-faithful

A UX evaluation of the desktop GUI (`ui/UX-REVIEW.md`: heuristics with severity ratings, axe-core,
keyboard pass, layout measurements, cognitive walkthrough) found the review workspace starved
(42 % of the window; 22 % near the minimum size), a flat 12-control options row mixing comparison
semantics with deployment switches, permanent warnings in CLI vocabulary, and accessibility
failures (unnamed checkboxes, non-dialog modals, no Escape, a mouse-only list, failing contrast,
a light editor in a dark shell). Of three options offered, "SSDT faithful" was chosen:

- **Toolbar** (Compare `Ctrl+Enter`, Generate Script, Publish/Update, Options with a non-default
  count) and **Source/Target dropdowns** holding the saved sources plus browse / open artifact /
  type a connection string (masked).
- **Options dialog**, tabs Comparison and Deployment, every control labelled and described;
  the status bar flags options changed since the last comparison.
- **Change list grouped by action** (Drop / Change / Add) with tri-state group checkboxes,
  filter and "risks only", a badge naming each row's most serious risk; an ARIA tree with roving
  focus (arrows, Space, Enter, Left/Right), still virtualized.
- **Risks tab** rendering hazard codes as title + explanation + affected statements
  (`ui/src/vocab.ts`); the publish dialog lists the objects behind each acknowledgement and says
  why Apply is disabled.
- **Notes panel** for the engine's warnings: collapsible from the status bar, per-note dismiss.
- **Foundation**: native `<dialog>` modals (focus trap, Escape, focus return), contrast-checked
  tokens (≥ 4.5:1), 12 px minimum text, dark Monaco themes, Lucide icons, landmarks.
- **Gap-free index rebuilds** (`diff.OpReplaceIndex`, `sqlgen.renderReplaceIndex`): a changed
  index is built `CONCURRENTLY` under a deterministic temporary name, the old one is dropped and
  the replacement renamed — the `REINDEX CONCURRENTLY` sequence — so queries never lose the
  index and no `INDEX_DROPPED` risk is raised; a leftover from an interrupted run is cleared
  first; offline or on a partitioned table the rebuild is drop + create in one transaction.
- **Browser dev harness**: `pnpm dev` without Tauri routes IPC to an in-browser mock daemon
  replaying an anonymized real plan, for UX work and audits without a database.

Re-audit: axe-core zero violations (was 1 critical, 1 serious, 3 moderate); workspace 393 px of a
1024×680 window with the notes open (was 151 px).

---

## Object coverage

**Managed (introspected, diffed, generated, published):** schemas, extensions, enums (labels added
in position), domains, composite types, collations, sequences (standalone, serial-owned,
identity-implicit), functions & procedures (incl. result-type changes), tables (columns incl.
identity/generated/defaults/not-null/storage/compression/statistics; `UNLOGGED`; storage parameters;
partitioning — `PARTITION BY` parents and `PARTITION OF` children), PK/FK/unique/check/exclude
constraints (incl. `NOT VALID` state), indexes (on tables and materialized views), triggers (on
tables and views, incl. enabled state), RLS policies (+ table RLS enable/force), views (incl. column
list, `CHECK OPTION`, `security_barrier`/`security_invoker`), materialized views, comments on all
of the above, and privileges (GRANT/REVOKE, incl. `WITH GRANT OPTION` and column privileges) on
schemas, tables, views, materialized views, sequences, routines, enums, domains and composite types.

**Reported as `UNSUPPORTED_CHANGE` (error-severity hazard; the script will not converge until done by
hand):** removing or reordering enum values, changing a domain or composite type definition,
changing a table's partitioning, rebuilding a partitioned table or a partition for column order.

**Warned about, not managed:** ownership, default privileges (`ALTER DEFAULT PRIVILEGES`),
column-level privileges on views, database-level grants, aggregate/window functions,
range/multirange types, foreign tables, non-view rules, `INHERITS` (non-partition) inheritance.
Roles are cluster objects: never created or dropped, only required to exist.

---

## Known limitations

1. **Dependents the engine does not yet recreate.** Views, materialized views and triggers are
   handled. Policies, column defaults, CHECK constraints and index expressions that call a
   function being dropped/recreated, and generated columns that read an altered column, are not —
   PostgreSQL's own error surfaces at publish time. A recreated materialized view is rebuilt
   (`WITH DATA`), which recomputes its contents.
2. **Ownership and default privileges are not modeled.** Objects are created by the deploying
   role; `ALTER DEFAULT PRIVILEGES`, database-level grants and column privileges on views are
   counted and warned about. Partitions can be created and dropped but existing tables are not
   attached/detached, and `INHERITS` inheritance is not modeled.
3. **Deploy scripts are plain SQL.** psql meta-commands (`\i`, `\echo`) cannot run server-side;
   pre/post-deploy scripts run as one implicit transaction each and are re-run on every publish,
   so they must be idempotent.
4. **`USING` casts are plain conversions.** `ALTER COLUMN TYPE` emits `USING "col"::type`; when
   values need a real transformation (parsing, defaults for unconvertible rows) the script must be
   edited by hand. The hazard note says so.
5. **Embedded PostgreSQL** is downloaded once per major version on first use, so the first
   compare on a machine (or a CI runner without a cache) is slow. The full test suite starts many
   scratch servers; run it with `go test -p 1 -timeout 40m ./...`.
6. `.pgdac` is compressed, not encrypted.
7. A true dependency cycle (possible only for objects that were themselves created via ALTERs)
   is reported as a `plan` warning and the cyclic operations are appended in priority order; the
   migration may still fail to apply.
8. **Updating a project from a database rewrites declarative statements only.** An object defined
   by migration steps (`CREATE … IF NOT EXISTS` followed by `ALTER TABLE … ADD COLUMN`, `DO`
   blocks) is refused with directions; unqualified names in project files are taken to be in
   `public`; a rename in the database appears as a drop plus an add, as in SSDT — record it in the
   refactorlog instead.

---

## Roadmap

**Done:** all work packages (WP0–WP8) and the audit's N1–N9 items.

| Release | What it added |
|---|---|
| 0.9.0 (2026-10-07) | Release candidate: the complete engine, CLI, daemon, project model, artifacts and GUI |
| 0.9.1 (2026-11-20) | Desktop auto-update, new app icon |
| 0.9.2 (2026-11-21) | First public release from GitHub, signed with the updater key |
| 0.9.3 (2026-11-23) | UTF-8 scratch server and sessions; target platform check (both found by the first project using pg-dac) |
| 0.9.4 (2026-11-23) | Saved passwords in the OS credential store; no passwords in baselines |

**1.0** is cut when the first production pilot completes without a correctness change
(CHANGELOG.md). Problems the pilot finds are fixed with a regression test and a patch release.

**Next, in order:**

1. **PostgreSQL version matrix in CI.** 11 to 18 are supported targets, but the integration tests
   run almost entirely on 16, with a few on 18. Run the publish and convergence gates on every supported major.
2. **Dependents of a recreated function.** Policies, column defaults, CHECK constraints and index
   expressions that call a function being dropped and recreated are not recreated around it
   today; PostgreSQL's error surfaces at publish time.
3. **Ownership and default privileges** (`ALTER DEFAULT PRIVILEGES`), modeled and compared
   instead of warned about; they matter most on Azure.
4. **Code signing** for the Windows and macOS installers (needs a signing certificate and an Apple
   developer account), and **tauri-action v1** in the release workflow (it renames the macOS
   update bundle and changes the URLs in `latest.json`, so it is tested on its own).

**Still out of scope or deferred:**

- **Not managed (warned):** database-level grants, column privileges on views, attaching/detaching
  *existing* partitions, `INHERITS` inheritance, aggregate/window functions, range/multirange
  types, foreign tables and servers, rules, event triggers, publications/subscriptions. Roles are
  required to exist, never created.
- **Reported as `UNSUPPORTED_CHANGE` rather than applied:** removing/reordering enum values,
  altering a domain or composite type definition, re-partitioning a table, rebuilding a
  partitioned table or a partition for column order.
- **Deferred:** a GUI for authoring the refactorlog (today: `pg-dac refactor rename`) and for
  export/import (today: the CLI); in-place update of imperative project files (refused with
  directions).

---

## Repo layout

```
pg-dac/
  cmd/pg-dac/            CLI + verbs: diff script report plan lint publish build extract inspect serve init refactor drift baseline export import
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
    artifact/           .pgdac read/write (format 4: signed manifest, deploy parts, grants), Merkle, Ed25519 signing, trusted_keys
    deploy/             pre/post-deploy scripts, dropPolicy and refactorlog from a project or artifact; publish runs them
    refactor/           refactorlog.json: renames applied as ALTER … RENAME, recorded in pgdac.refactorlog
    drift/              baselines in pgdac.baseline; drift detection and explanation
    project/            pgdac.yaml manifest (+ doublestar globs)
    source/             resolve any source spec → model; OptionsFor (schemas, target major, signer trust)
    dbconn/             live-database connect with retry of transient failures
    publish/            the publish pipeline the CLI and the daemon share (drift gate, refactorlog, deploy scripts, baseline)
    projsync/           update a project's files from a desired state (publish with a project as target)
    pgbac/              .pgbac export/import of schema and data
    sqltext/            SQL statement splitting, head parsing and whitespace-insensitive normalization
    validate/           replay a migration in a scratch database and check convergence
  ui/                   Tauri GUI (src/ React, src-tauri/ Rust supervisor, binaries/ sidecar)
  testdata/             demo project + fixtures
```
Module: `github.com/rmnunes/pg-dac` (Go 1.25). License Apache-2.0.

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
pg-dac extract --from "postgres://…/db" --out app.pgdac           # or --to-project ./dir (schema/<schema>/<kind>/<name>.sql)
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
$version = "$((Get-Content ui/PGDAC_VERSION).Trim())-dev+$(git rev-parse --short HEAD)"
go build -ldflags "-X main.version=$version" -o "ui/src-tauri/binaries/pg-dac-$triple.exe" ./cmd/pg-dac
cd ui && pnpm install && pnpm tauri dev
```
Restart `pnpm tauri dev` after rebuilding the sidecar: the app runs the copy Tauri placed in
`src-tauri/target/debug/`, not the file in `binaries/`. The status bar shows the running engine as
`<version>-dev+<commit>` so a stale sidecar is obvious.

**Tests:** `go test -short ./...` (no DB) · `go test -p 1 ./...` (+ embedded-Postgres integration:
bootstrap gate, scenario matrix, introspection invariants, publish lifecycle) · in `ui/`:
`pnpm lint`, `pnpm test` (vitest), `pnpm exec tsc --noEmit` · in `ui/src-tauri/`:
`cargo clippy --all-targets -- -D warnings`.
