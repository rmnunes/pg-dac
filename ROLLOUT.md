# Production rollout guide

How to run pg-dac the way SqlPackage is run in a delivery pipeline: build an artifact once,
verify it, publish it per environment with the safety gates on, and keep every environment honest
with drift checks. Commands below assume `pg-dac` on the PATH and connection strings in
environment variables; never put a password in a command line (pg-dac reads `PGPASSWORD`, and
`postgres://user@host/db` without a password works with it).

## 1. Prerequisites

| Item | Requirement |
|---|---|
| PostgreSQL | 11 to 18 as targets; the scratch server matches the target major (downloaded once per major into the user cache; pre-seed CI runners by running any compare once, or cache `~/.cache/pg-dac` / `%LOCALAPPDATA%\pg-dac`) |
| Deploy role | Owner of the schemas it publishes (or superuser-equivalent on Azure); `CREATE` on the database for the `pgdac` bookkeeping schema. With `owner:` in `pgdac.yaml`: able to `SET ROLE` to the owner (`GRANT app_owner TO deployer`); `publish` checks this and the other rights ownership needs before it starts |
| Roles | Cluster objects pg-dac never creates. List environment roles in `pgdac.yaml roles:`; provision them in a pre-deploy script (Azure: `pgaadauth_create_principal`); a grant to a missing role blocks the publish |
| Project | Declarative files (one `CREATE` per object; `pg-dac extract --to-project` produces the layout). Migration-style scripts belong in `preDeploy` / `postDeploy` |
| Signing | A signing key per build agent (`pg-dac build` creates it under the pg-dac config dir); consumers pin it with `--require-key <fingerprint>` or a `trusted_keys` file |

## 2. The pipeline

```bash
# Build once per commit: the artifact is the unit that moves between environments.
pg-dac lint   --from "$DEV_DSN" --to ./schema --fail-on error            # hazards as a gate
pg-dac build  --from ./schema --out "app-$GIT_SHA.pgdac" --version "$VERSION"
pg-dac inspect "app-$GIT_SHA.pgdac"                                       # integrity + signer

# Per environment: preview, validate, publish, record.
pg-dac drift   --from "$DSN"                                              # exit 2 = someone changed it by hand
pg-dac script  --from "$DSN" --to "app-$GIT_SHA.pgdac" --require-key "$KEY" --single-transaction > "migrate-$ENV.sql"
pg-dac publish --from "$DSN" --to "app-$GIT_SHA.pgdac" --require-key "$KEY" --validate \
               --single-transaction --lock-timeout 5s --statement-timeout 30m \
               --var deploy_environment="$ENV"
```

- `lint` and `script` never touch the target; `publish --dry-run` prints the exact migration and
  the substituted deploy scripts.
- `--validate` replays the migration in a scratch database and proves it converges before the
  real one starts.
- `publish` refuses data loss and unsupported changes unless `--allow-hazards`, refuses a drifted
  target unless `--allow-drift`, and refuses a grant to a role the target lacks unless a pre-deploy
  script provisions it. It records a baseline after every successful run.
- `--single-transaction` makes the migration atomic where PostgreSQL allows: `CONCURRENTLY`
  statements (online index builds) cannot be inside a transaction and run between transactional
  batches. For a strictly atomic migration use `--no-online` as well and accept the locks.
- A failed publish leaves the target partially migrated; re-running `publish` resumes from the new
  current state. With `--single-transaction` the open batch is rolled back first.

## 3. Policies to decide once per project

| Decision | Where | Production recommendation |
|---|---|---|
| What is dropped | `dropPolicy: none | safe | all` | `safe` (constraints and indexes only); `all` only when the project is the complete truth, including privileges |
| Column order | `compare.columnOrder` | `warn`; `rebuild` only for small tables, in a maintenance window |
| Privileges | `compare.privileges` | `true`; keep service-owned objects (triggers a service installs at start-up) out of the project and the drop policy at `none`/`safe` |
| Object types | `compare.excludeObjectTypes` | empty; use to leave e.g. `policy` to another owner |
| Variables | `variables:` + `--var` | environment name, retention windows, role names that differ per environment |

## 4. Environments without a shared project history

- **A new environment from production data**: `pg-dac export --from "$PROD_DSN" --out prod.pgbac
  --exclude-table 'audit.*'` then `pg-dac import --file prod.pgbac --target "$NEW_DSN"` (empty
  database; roles must exist). The import is a consistent snapshot, verified against the signed
  manifest, loaded without triggers firing, keys and indexes added afterwards.
- **Adopting an existing database as the project**: `pg-dac extract --from "$DSN" --to-project
  ./schema`, commit, then compare in both directions. When the database moves on, `pg-dac publish
  --from ./schema --to "$DSN"` updates the project files (SSDT's Update project from database).
- **Rollback**: pg-dac generates forward migrations. To roll back, publish the previous artifact:
  `pg-dac publish --from "$DSN" --to "app-$PREVIOUS_SHA.pgdac"` (data added by the new version is
  kept unless the drop policy says otherwise; preview with `--dry-run`).

## 5. Observability and audit

- `pg-dac report --format json` and `pg-dac plan` give machine-readable plans for review systems.
- `pgdac.baseline` holds every published model with its SHA-256 and source; `pgdac.refactorlog`
  the renames applied. `pg-dac drift` compares the live catalog with the last baseline.
- Every artifact and archive is Merkle-hashed and Ed25519-signed; `inspect` verifies both.

## 6. Known limits to plan around

See STATUS.md "Known limitations": ownership is compared only for a project with `owner:`; partitions
are created and dropped but not attached/detached; renames in a database → project update appear
as drop + add (record them in the refactorlog); imperative project files cannot be updated in
place; `.pgdac`/`.pgbac` are compressed, not encrypted.
