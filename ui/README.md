# pg-dac-ui

A Tauri desktop **Schema Compare** GUI for PostgreSQL, driving the `pg-dac` engine.
It mirrors the SQL Server *Schema Compare* experience: pick two sources, review an
object-by-object diff with definition diffs and generated SQL, then generate a script
or publish — all backed by the same engine the CLI uses.

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
  TanStack Virtual (the object tree), and Zustand. IPC wrappers in `src/ipc/` mirror the
  daemon JSON (`PlanDoc`, `ObjectChange`, `Hazard`) 1:1.

## Run it (development)

Prereqs: Rust, Node + pnpm, and (Windows) the MSVC build tools. The pg-dac sidecar must be
built into `src-tauri/binaries/` with the Rust target-triple suffix:

```powershell
# from the repo root: build the sidecar for your target triple
$triple = (rustc -Vv | Select-String '^host:').ToString().Split(' ')[1]
go build -o "ui/src-tauri/binaries/pg-dac-$triple.exe" ./cmd/pg-dac

cd ui
pnpm install
pnpm tauri dev      # launches the app; it spawns `pg-dac serve` automatically
```

The first compare against a project/`.sql`/`.pgdac` source downloads the embedded
PostgreSQL binaries once (cached afterward), then stays warm for the session.

## Using the app

1. Set **Target (--from)** and **Desired (--to)** — each is a connection string, a project
   directory, a `.sql` file, or a `.pgdac` artifact (use 📁/📦 to browse). **Compare**.
2. Review the **object tree** (grouped, virtualized, severity-badged). Uncheck objects to
   exclude them; the plan re-computes with your selection.
3. The **detail pane** shows the Monaco definition diff (old vs new) and the generated SQL
   with hazard annotations.
4. **Generate Script** exports the SQL, or **Publish…** applies it — the publish dialog
   requires acknowledging each error-severity hazard code (and data loss) before *Apply*,
   then streams live per-statement progress.

## Build a release

```powershell
cd ui && pnpm tauri build
```

Bundles the signed `pg-dac` sidecar (`bundle.externalBin`) alongside the app.
