# Desktop app updates

The pg-dac desktop app updates itself from GitHub Releases with the Tauri v2
updater, the same mechanism pg-shell uses. The CLI does not: it runs in
pipelines, where the version is pinned by whoever installs it.

## What users see

- Shortly after launch, and every six hours while the app is open, it checks
  `https://github.com/rmnunes/pg-dac/releases/latest/download/latest.json`.
  A failed automatic check (offline, endpoint unreachable) shows nothing.
- When a newer version exists, a strip under the toolbar says so. **Download**
  fetches the installer and verifies its signature against the public key built
  into the app. The engine keeps running while it downloads.
- **Restart now** stops the engine, installs and restarts into the new version.
  It waits while a compare, script or publish is running, so an update never
  interrupts a deployment. On Windows the installer runs in passive mode (a
  progress bar, no questions).
- **Later** (the ×) hides that version until the user checks manually.
- **Check for updates** in the status bar runs a check on demand and reports
  "up to date" or why the check failed.

On Linux, the AppImage updates itself; `.deb` and `.rpm` installs are updated
through the package manager.

## Why the update runs in the Rust layer

The app bundles the `pg-dac serve` sidecar, and an update replaces it. On
Windows the updater launches the installer and exits the process immediately,
which skips the exit path that stops the sidecar; a sidecar still running keeps
its executable locked and the installer cannot replace it. So
`ui/src-tauri/src/update.rs` drives the check, download and install itself and
stops the sidecar first (through the updater's exit hook on Windows, before the
restart elsewhere). If the installer fails to launch, the sidecar is started
again. The webview gets no updater or process permissions.

## One-time setup (before the first release)

The signing key pair is not in the repository. Until it is set up, builds do not
look for updates, and the release workflow refuses to run (an app shipped
without the key could never update itself).

1. Generate the key pair on your machine. It asks for a password; keep the key
   file and the password safe. **If they are lost, existing installs can no
   longer be updated**, only reinstalled.

   ```powershell
   cd ui
   pnpm tauri signer generate -w "$HOME/.tauri/pg-dac.key"
   ```

2. Put the public key it prints into `ui/src-tauri/tauri.conf.json` at
   `plugins.updater.pubkey` and commit it. The public key is not a secret.

3. In the GitHub repository settings, add two Actions secrets:
   - `TAURI_SIGNING_PRIVATE_KEY`: the contents of `pg-dac.key`.
   - `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`: the password from step 1.

Use a key for pg-dac only, not pg-shell's.

## Cutting a release

```powershell
cd ui
pnpm release patch        # or minor, major, or an explicit 1.2.3
```

The script sets the version in `ui/PGDAC_VERSION`, `ui/package.json`,
`ui/src-tauri/tauri.conf.json`, `ui/src-tauri/Cargo.toml` (and Cargo.lock) and
the CLI's default in `cmd/pg-dac/main.go`; `TestVersionsAgree` fails if any of
them disagree.
Add the CHANGELOG section, commit, then tag and push the tag by name
(`git push --follow-tags` skips lightweight tags):

```powershell
git tag v1.2.3
git push origin main v1.2.3
```

The release workflow checks that the tag and every version field agree and that
the public key is set. It then builds the CLI binaries and the installers, signs
the installers, and uploads them with `latest.json` to a **draft** release.
Running apps only see published releases, so **publishing the draft is what
ships the update**. Check the draft's assets first.

Local `pnpm tauri build` does not produce updater artifacts and needs no key;
the workflow adds them with `--config src-tauri/tauri.release.conf.json`.

## Trying the update UI without a release

In the browser harness (`pnpm dev`, opened in a browser), add
`?update=0.9.1` to the URL: the mock offers that version, simulates the
download and "restarts" by reloading the page.

## Rotating the key

- **Lost, not leaked:** generate a new pair, replace the public key and the
  secrets, release. Existing installs cannot verify the new signatures and must
  be reinstalled once.
- **Leaked:** the same, and delete the old secret immediately. There is no
  revocation: rotate, release, and tell users to reinstall.
