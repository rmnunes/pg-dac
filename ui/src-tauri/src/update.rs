//! In-app updates. Released builds check the signed `latest.json` the release
//! workflow publishes next to the installers (see docs/UPDATES.md); the
//! frontend shows the result and the user decides when to download and when to
//! restart.
//!
//! The update runs here rather than through the updater plugin's JavaScript
//! API because of the bundled `pg-dac serve` sidecar. Installing replaces that
//! binary, and on Windows the installer is launched and the app exits at once
//! (`std::process::exit`), skipping the normal exit path that stops the daemon.
//! A daemon still running holds its executable open, so the installer could not
//! replace it. Driving the update from Rust lets the install stop the daemon
//! first, on every platform, and keeps updater permissions out of the webview.

use std::sync::{Mutex, MutexGuard};

use serde::Serialize;
use tauri::{AppHandle, Emitter, Manager, State};
use tauri_plugin_updater::{Update, UpdaterExt};

use crate::AppState;

/// Download progress is emitted on this channel as {received, total}.
const UPDATE_EVENT: &str = "pgdac://update";

/// Progress is emitted at most once per this many bytes (and on the last
/// chunk), so a download is a few hundred events, not tens of thousands.
const PROGRESS_STEP: u64 = 256 * 1024;

/// The update found by the last check and, once downloaded and verified, its
/// installer bytes.
#[derive(Default)]
pub struct Pending {
    update: Mutex<Option<Update>>,
    bytes: Mutex<Option<Vec<u8>>>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateInfo {
    version: String,
    current_version: String,
    notes: Option<String>,
}

fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// A build without the release public key cannot verify an update, so it does
/// not look for one (local builds, and every build until the signing key is
/// set up).
fn configured(app: &AppHandle) -> bool {
    app.config()
        .plugins
        .0
        .get("updater")
        .and_then(|u| u.get("pubkey"))
        .and_then(|k| k.as_str())
        .is_some_and(|k| !k.trim().is_empty())
}

/// Asks the release endpoint whether a newer version exists. `None` means this
/// is the latest version.
#[tauri::command]
pub async fn update_check(
    app: AppHandle,
    pending: State<'_, Pending>,
) -> Result<Option<UpdateInfo>, String> {
    if !configured(&app) {
        return Err("this build is not set up for updates (no release signing key)".into());
    }
    let handle = app.clone();
    let updater = app
        .updater_builder()
        // Windows only: runs once the installer has been extracted, just before
        // it is launched and this process exits.
        .on_before_exit(move || {
            if let Some(state) = handle.try_state::<AppState>() {
                state.daemon().stop();
            }
            handle.cleanup_before_exit();
        })
        .build()
        .map_err(|e| e.to_string())?;
    let update = updater.check().await.map_err(|e| e.to_string())?;
    let info = update.as_ref().map(|u| UpdateInfo {
        version: u.version.clone(),
        current_version: u.current_version.clone(),
        notes: u.body.clone(),
    });
    *lock(&pending.update) = update;
    *lock(&pending.bytes) = None;
    Ok(info)
}

/// Downloads the update found by the last check and verifies its signature
/// against the public key built into the app. Progress goes to UPDATE_EVENT.
/// The daemon keeps running: nothing changes until `update_install`.
#[tauri::command]
pub async fn update_download(app: AppHandle, pending: State<'_, Pending>) -> Result<(), String> {
    let update = lock(&pending.update)
        .clone()
        .ok_or("there is no update to download; check for updates first")?;
    let mut received: u64 = 0;
    let mut reported: u64 = 0;
    let bytes = update
        .download(
            |chunk, total| {
                received += chunk as u64;
                if received - reported >= PROGRESS_STEP || total.is_some_and(|t| received >= t) {
                    reported = received;
                    let _ = app.emit(
                        UPDATE_EVENT,
                        serde_json::json!({ "received": received, "total": total }),
                    );
                }
            },
            || {},
        )
        .await
        .map_err(|e| e.to_string())?;
    *lock(&pending.bytes) = Some(bytes);
    Ok(())
}

/// Installs the downloaded update and restarts into it. On Windows the
/// installer takes over and this process exits inside `install` (the exit hook
/// set at check time stops the daemon). Elsewhere the files are replaced in
/// place, then the daemon is stopped and the app restarts.
#[tauri::command]
pub async fn update_install(
    app: AppHandle,
    pending: State<'_, Pending>,
    state: State<'_, AppState>,
) -> Result<(), String> {
    let update = lock(&pending.update)
        .clone()
        .ok_or("there is no update to install; check for updates first")?;
    let bytes = lock(&pending.bytes)
        .take()
        .ok_or("the update has not been downloaded yet")?;
    if let Err(e) = update.install(&bytes) {
        // The Windows exit hook may already have stopped the daemon (the
        // installer then failed to launch, e.g. elevation was declined): bring
        // it back so the app keeps working, and keep the download for a retry.
        state.ensure_daemon()?;
        *lock(&pending.bytes) = Some(bytes);
        return Err(format!("the update could not be installed: {e}"));
    }
    state.daemon().stop();
    app.restart();
}
