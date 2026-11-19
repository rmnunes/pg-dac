# pg-dac-ui

A Tauri desktop **Schema Compare** GUI for PostgreSQL, driving the `pg-dac` engine.
It mirrors the SQL Server *Schema Compare* experience: pick two sources, review an
object-by-object diff with definition diffs, generated SQL and the risks of each change, then
generate a script or publish — all backed by the same engine the CLI uses.

## Architecture

```
React UI ──invoke──▶ src-tauri (Rust supervisor) ──spawn──▶ pg-dac serve  (warm Postgres + HTTP/SSE)
   ▲  │ events                  • reads port handshake (stdout)
   │  └─ pgdac://publish (SSE)   • per-launch bearer token (PGDAC_TOKEN)
   └───────────────────────────  • proxies /plan /script /publish to the daemon
```

- **The Rust layer is a thin supervisor + proxy** (`src/daemon.rs`, `src/lib.rs`). It finds
  the bundled `pg-dac` sidecar, runs `pg-dac serve`, reads the loopback address it prints,
  and forwards commands. No PostgreSQL logic lives in Rust — `pg-dac` owns it all.
- **The frontend** (`src/`) is React 19 + Vite 6 + TypeScript, Monaco (`DiffEditor`),
  TanStack Virtual (the change list), Zustand and Lucide icons. IPC wrappers in `src/ipc/`
  mirror the daemon JSON (`PlanDoc`, `ObjectChange`, `Hazard`) 1:1; `src/vocab.ts` holds the
  human copy for the engine's hazard codes.

## Run it (development)

Prereqs: Rust, Node + pnpm, and (Windows) the MSVC build tools. The pg-dac sidecar must be
built into `src-tauri/binaries/` with the Rust target-triple suffix:

```powershell
# from the repo root: build the sidecar for your target triple
$triple = (rustc -Vv | Select-String '^host:').ToString().Split(' ')[1]
$sha = git rev-parse --short HEAD
go build -ldflags "-X main.version=0.9.0-dev+$sha" -o "ui/src-tauri/binaries/pg-dac-$triple.exe" ./cmd/pg-dac

cd ui
pnpm install
pnpm tauri dev      # launches the app; it spawns `pg-dac serve` automatically
```

The app runs the copy of the sidecar that Tauri places next to its own binary in
`src-tauri/target/debug/`, so a sidecar rebuilt while the app is running is picked up only after
`pnpm tauri dev` is restarted. The status bar shows which engine is running: a dev build reports
`0.9.0-dev+<commit>`, so a stale sidecar is visible at a glance.

The first compare against a project/`.sql`/`.pgdac` source downloads the embedded
PostgreSQL binaries once (cached afterward), then stays warm for the session.

### Frontend only, in a browser

`pnpm dev` and http://localhost:1420 run the frontend without Tauri: `src/ipc/client.ts` routes
every command to the in-browser mock daemon (`src/ipc/mock.ts`), which replays a real plan
(`src/dev/fixture-plan.json`: five changes, data loss, three engine notes) with the same event
choreography as the Rust supervisor and keeps saved sources in `localStorage`. Use it for UX
work, visual review and accessibility audits (axe-core from the console) — no database needed.
The desktop app never takes this path.

## Using the app

The window is laid out like SSDT's Schema Compare:

- **Toolbar** (top right): **Compare** (`Ctrl+Enter`), **Generate Script**, **Publish to
  Target…** (or **Update Target project…** when the Target is a project directory), **Options**.
- **Source** (left, the desired state — the engine's `--to`) and **Target** (right, gets
  updated — `--from`). Each is a dropdown: the saved sources, then *Browse for a project
  folder…*, *Open a .pgdac artifact…* and *Type a connection string or path…*. A typed
  connection string is shown masked and can be kept as a saved source. The swap button
  exchanges the two.
- **Options** opens a dialog with two tabs. *Comparison*: schemas, Target PostgreSQL version,
  privileges, column order, routine whitespace and comments (each defaulting to the Source
  project's `pgdac.yaml` `compare:` block), excluded object types, required signer key.
  *Deployment*: online (lock-safe), single transaction, drop objects not in the Source, allow
  drift, skip pre/post-deploy scripts, deploy variables. The toolbar button shows how many
  options differ from their defaults; the status bar says when options changed after the last
  comparison.
- **Change list** (left pane), grouped by action — Drop, Change, Add — with a tri-state
  include checkbox per group and one per change. Filter by name or type, or show risks only.
  Each row's badge names its most serious risk ("data loss", "rewrite", "exclusive lock", …).
  Excluding a change also excludes every change that depends on it (shown disabled, with the
  cause); including one brings its dependencies back — the SSDT behaviour. Every selection change
  re-plans the migration on the daemon (sub-second, no re-read of the sources), so the list,
  the Generated SQL and the statement count always show what a publish would run: an excluded
  object the engine must still recreate for a dependency is marked "recreated" with the reason,
  and one nothing runs for says so. The list is a
  keyboard tree: arrows move, `Space` includes/excludes, `Enter` selects, `Left`/`Right`
  collapse/expand a group.
- **Detail pane**: *Definition* (Monaco side-by-side diff, Source left / Target current right,
  green = added to the Target, red = removed; a toggle shows the Target after the change),
  *Generated SQL*, and *Risks* — each hazard with a title, the engine's explanation and the
  statements that carry it.
- **Notes**: the engine's warnings (labelled Target / Source / Plan) in a collapsible panel
  above the status bar, each dismissible; the status bar toggles it and shows the count.
- **Publish** summarises the deployment settings (with a link to change them), lists the risks
  to acknowledge — each with the objects it affects — and streams per-statement progress.
  `Apply` stays disabled until every acknowledgement is ticked.

Every dialog is a native `<dialog>`: `Escape` closes it (or cancels a running compare), focus is
trapped inside and returns to the opener. Every control has an accessible name; the colour
tokens in `src/styles.css` meet WCAG AA contrast. `UX-REVIEW.md` records the evaluation that
led to this layout and the re-audit after it.

## Build a release

```powershell
cd ui && pnpm tauri build
```

Bundles the signed `pg-dac` sidecar (`bundle.externalBin`) alongside the app.
