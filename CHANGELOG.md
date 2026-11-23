# Changelog

All notable changes to pg-dac. The format follows Keep a Changelog; versions follow SemVer.
`0.9.0` is the release candidate for production rollout; `1.0.0` is cut after the first
production pilot completes without a correctness change.

## 0.9.3 — 2026-11-23

### Fixed
- **Non-ASCII text on Windows.** The scratch PostgreSQL pg-dac normalizes and validates in took
  its encoding from the OS (WIN1252 on Windows) and pg-dac's sessions took theirs from the
  environment, so with `PGCLIENTENCODING=UTF8` a project with an arrow or other character outside
  WIN1252 failed to build, and with any other non-UTF-8 value the scratch server never came up
  (reported as a start timeout). The scratch cluster is now UTF-8 with the C locale on every
  platform, every pg-dac session is UTF-8, and `PGCLIENTENCODING` (meant for `psql`) is ignored.
- **A project newer than the database.** Comparing a project normalized for a newer PostgreSQL
  major than the target runs planned what the newer server makes of the project, such as
  `ALTER EXTENSION btree_gist UPDATE TO '1.8'` against PostgreSQL 16. Like SqlPackage's platform
  check, that comparison is now refused with the fix (`--target-major <target's major>`), and
  `--allow-incompatible-platform` compares anyway. Version-gated statements now follow the target
  database's version instead of the project's, since they run on the target.

### Changed
- The README is now a short introduction with screenshots of the desktop app, install steps and a
  quick start; the full command reference moved to `docs/cli.md` and the internals to
  `docs/how-it-works.md`.
- Contributor guide, security policy, code of conduct, and issue and pull request templates.
- `pnpm screenshots` in `ui/` regenerates the README screenshots from the browser harness.

## 0.9.2 — 2026-11-21

### Added
- First release published from GitHub. Installers are signed with the pg-dac updater key, so
  from this version on the desktop app finds and verifies new releases on its own.

### Fixed
- The mock engine used by the browser harness reported a hard-coded version; it now follows
  `ui/PGDAC_VERSION` like a local sidecar build.

## 0.9.1 — 2026-11-20

### Added
- **The desktop app updates itself** (docs/UPDATES.md): it checks the signed release manifest
  after launch and every six hours, a strip under the toolbar offers a newer version, the user
  downloads it (signature verified against the key built into the app) and restarts when ready.
  Restarting stops the engine first and waits while a compare, script or publish runs. A manual
  check sits in the status bar. The release workflow signs the installers, uploads `latest.json`,
  and refuses to run until the updater key is configured; `pnpm release <version>` in `ui/` sets
  every version field.

### Changed
- New app icon: a stack of schema layers with the top one sliding into place. A simplified
  drawing is used at 16-32 px so the taskbar icon stays legible; `pnpm icons` in `ui/` rebuilds
  the set from `ui/src-tauri/icons/source/`.
- The toolbar no longer repeats the app name the window title already shows, and starts at the
  left edge like SSDT's.

## 0.9.0 — 2026-10-07 — release candidate

### Added
- **Privileges are part of the model** (WP5): GRANT/REVOKE on schemas, tables, views,
  materialized views, sequences, routines, enums, domains, composite types and table columns,
  compared with SSDT semantics (`--ignore-privileges`, revokes under drop policy `all`); an
  object pg-dac drops and recreates always gets its privileges back; `MISSING_ROLE` blocks a grant
  to a role the target lacks, and publish runs pre-deploy scripts first when that is the only
  blocker; `pgdac.yaml roles:`.
- **Column-order policy** (WP5): `compare.columnOrder` / `--column-order` = `ignore` | `warn`
  (default) | `rebuild` (the SSDT table rebuild as one atomic block, drop policy respected).
- **Sequence seeding** (WP5): identity/serial changes on populated tables seed the sequence past
  `max(col)`.
- **Live databases are canonicalized** (WP5) through the scratch PostgreSQL like every other
  source, so an expression never differs only by how it was deparsed.
- **Routine bodies compare modulo whitespace** (WP5); `--compare-whitespace`, `--ignore-comments`.
- **Update a project from a database** (WP6): `publish --from <project> --to <db>`, file-level
  `--dry-run`, GUI "Update Target project"; declarative statements rewritten in place, new files
  at stable paths, drops per policy, `roles:` maintained; imperative definitions refused with
  directions.
- **Order-insensitive build** (WP6): project files need no numbering; statements that fail are
  retried after the rest.
- **Stable project layout** (WP6): `schema/<schema>/<kind>/<name>.sql`, one file per object with
  its sub-objects, comments and grants; `extract --to-project` writes it.
- **Single-transaction publish** (WP7): `--single-transaction`, `--statement-timeout`.
- **Object-type exclusion** (WP7): `compare.excludeObjectTypes`, `--exclude-object-type`.
- **Deploy variables** (WP7): `pgdac.yaml variables:`, `--var name=value`, `$(name)` in pre/post
  scripts.
- **Export / Import of schema and data** (WP7): `pg-dac export` writes a `.pgbac` (one consistent
  snapshot, gzip CSV per table, sequence positions, Merkle root and signature over the data);
  `pg-dac import` fills an empty database pg_dump-style and records the baseline; `inspect` reads
  both archive kinds.
- Daemon warnings carry the side they concern (Target / Source / Plan); GUI options for every
  comparison setting and publish control.
- **GUI redesign, SSDT-faithful** (WP8, after the UX evaluation in `ui/UX-REVIEW.md`): toolbar
  with `Ctrl+Enter` compare, Source/Target dropdowns holding the saved sources, an Options dialog
  (Comparison / Deployment tabs), the change list grouped by action with tri-state group
  checkboxes, filter and full keyboard navigation, a Risks tab and publish acknowledgements that
  explain each hazard code and name the affected objects, a collapsible notes panel for the
  engine's warnings, native dialogs with focus management, contrast-checked dark theme (editors
  included), Lucide icons; axe-core reports no violations. A browser-only dev harness
  (`pnpm dev` without Tauri) replays a real plan through a mock daemon.
- **Selection re-plans instantly** (daemon `/replan`): the GUI asks the engine what the migration
  is under the current include/exclude selection without re-reading the sources, so an excluded
  object that must still be dropped and recreated for a dependency shows up as "recreated" with
  the reason, an excluded object nothing runs for says so, and the statement count is the one a
  publish runs. Excluded objects stay listed so they can be re-included. A re-plan the daemon
  refuses is reported above the list, and Apply waits for the re-plan.
- **Dependency visibility**: a Dependencies tab lists what the selected object depends on and what
  depends on it within the plan, each with its include state and a jump; rows excluded by
  dependency carry a "needs X" badge; a toggle that pulls other rows out (or back in) says so above
  the list; the publish dialog lists excluded objects the engine still has to recreate.
- Each plan object carries `reason`, the engine's explanation when it touches an object beyond the
  plain diff (a view recreated because a column it reads changes type, a rebuild, an object kept).
  The GUI marks such objects "recreated" in the list and the Definition tab says why.
- Schemas and extensions have a definition in the plan like every other object, so the GUI's
  Definition diff shows `CREATE SCHEMA` (with comment and grants) or `CREATE EXTENSION` (with the
  installed version noted) instead of two empty panes.
- Frontend checks: ESLint (recommended JS/TS and React Hooks rules) and vitest unit tests for the
  selection logic, version parsing and mock parity; CI runs them, and rustfmt and clippy
  (`-D warnings`) on the Tauri layer.

### Changed
- **Gap-free index rebuilds.** An index whose definition changed is rebuilt the way
  `REINDEX CONCURRENTLY` works: built `CONCURRENTLY` under a temporary name while the current
  index keeps serving queries, then the old one is dropped and the replacement takes its name
  (SHARE UPDATE EXCLUSIVE only). The table is never without the index and no `INDEX_DROPPED`
  risk is raised. Offline, or on a partitioned table, it is dropped and recreated inside the
  transaction and the statements say they are a rebuild. The GUI shows further risk types on a
  change as "+N risks".
- **One publish pipeline.** `internal/publish` holds the publish the CLI and the GUI both run —
  drift gate, refactorlog renames, missing-roles retry, deploy scripts, migration, baseline — with
  typed refusals (drift, blocked hazards, partial apply, baseline); a project update shares
  `publish.Project`. The two copies had begun to differ.
- **Connections retry what heals by waiting**: DNS, dial/read failures before the handshake, and a
  server that is starting up (57P03), full (53300) or reports a connection exception (08xxx);
  authentication and other answers fail at once. Connection errors name the server, never the
  password.
- **Daemon hardening**: every endpoint answers errors as `{"error": …}`, checks its method, bounds
  the request body and the header read; binding a non-loopback address requires `PGDAC_TOKEN`;
  the re-plan cache is keyed by everything that shapes how the sources are read (target major and
  signer key included). Request JSON spells the selection `includeIds` / `excludeIds`, like the rest of the API.
- Statement failures carry PostgreSQL's DETAIL and HINT on every path — streaming, batched,
  deferred project builds and deploy scripts.
- Plan format 2, artifact format 4 (models carry grants). Older artifacts open with a warning.
- Privilege warnings are gone (privileges are managed); default privileges, view column
  privileges and database-level grants are reported as unmanaged.
- `extract --to-project` refuses an existing project directory: use `publish` with the project as
  the target to update it.
- The GUI's Tauri layer is a single proxy and stream helper; the unused one-shot build/inspect
  commands are gone.

### Fixed
- Functions that depend on a dropped object — a relation whose row type their signature uses, an
  enum in their signature, what a SQL-standard body references — are recreated around the drop,
  like dependent views and triggers: dropped before it, recreated after it, kept with their
  current definition when the source does not define them, and dropped only (saying so) when
  what they need disappears for good. Before, PostgreSQL refused the drop ("other objects depend
  on it") at publish time.
- An offline index rebuild on a table whose indexed column the same plan drops failed (the column
  drop had already taken the index); the rebuild's drop is now `IF EXISTS`.
- An index left under its rebuild name by an interrupted run was kept forever when the drop policy
  kept unknown indexes; it is pg-dac's own leftover, so it is removed, and the next rebuild takes
  the name back.
- A definition that failed to render showed as an empty pane; the error now reaches the caller.
- The GUI list no longer resets its filter, collapsed groups and keyboard position on every
  include/exclude toggle, and the notes panel no longer reopens after the user collapsed it.
- A jump from the Dependencies tab moves the list's keyboard position to that row and keeps focus
  on the tab; a backdrop click no longer cancels a running compare; a dialog Chromium closes on a
  repeated Escape is reopened while its operation runs.
- identity → serial conversions order `CREATE SEQUENCE` after `DROP IDENTITY`.
- A `CREATE OR REPLACE` of a view or function no longer re-grants its privileges.
- Column-order warnings name only the misplaced columns.

## 0.1.0 — 2026-10-07

Roadmap WP0–WP4: compare/script/publish engine with dependency-aware migrations, bootstrap into an
empty database, deterministic output, signed `.pgdac` artifacts carrying deployment content,
refactorlog renames, drift detection with baselines, lint, the Tauri GUI with dependency-gated
include/exclude, CI and release workflows. See the audit notes for the audit and each work package's
acceptance on the real project.
