# Changelog

All notable changes to pg-dac. The format follows Keep a Changelog; versions follow SemVer.
`0.9.0` is the release candidate for production rollout; `1.0.0` is cut after the first
production pilot completes without a correctness change.

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

### Changed
- Plan format 2, artifact format 4 (models carry grants). Older artifacts open with a warning.
- Privilege warnings are gone (privileges are managed); default privileges, view column
  privileges and database-level grants are reported as unmanaged.
- `extract --to-project` refuses an existing project directory: use `publish` with the project as
  the target to update it.

### Fixed
- identity → serial conversions order `CREATE SEQUENCE` after `DROP IDENTITY`.
- A `CREATE OR REPLACE` of a view or function no longer re-grants its privileges.
- Column-order warnings name only the misplaced columns.

## 0.1.0 — 2026-10-07

Roadmap WP0–WP4: compare/script/publish engine with dependency-aware migrations, bootstrap into an
empty database, deterministic output, signed `.pgdac` artifacts carrying deployment content,
refactorlog renames, drift detection with baselines, lint, the Tauri GUI with dependency-gated
include/exclude, CI and release workflows. See the audit notes for the audit and each work package's
acceptance on the real project.
