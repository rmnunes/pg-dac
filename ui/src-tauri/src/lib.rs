//! pg-dac-ui Tauri backend: a thin supervisor + proxy over the bundled
//! `pg-dac serve` daemon. The frontend invokes these commands; they forward to
//! the warm daemon and (for compare and publish) stream SSE progress back as
//! Tauri events.

mod daemon;
mod profiles;

use std::sync::Arc;

use serde_json::Value;
use tauri::{AppHandle, Emitter, Manager, RunEvent, State};

use daemon::Daemon;
use profiles::Profile;

struct AppState {
    daemon: Arc<Daemon>,
}

/// The event channel publish progress is emitted on (data: {event, data}).
const PUBLISH_EVENT: &str = "pgdac://publish";

/// The event channel compare progress is emitted on (data: {event, data}); event
/// is one of "phase" | "result" | "canceled" | "error".
const COMPARE_EVENT: &str = "pgdac://compare";

// Every proxy command is `async` and runs its blocking ureq call off the main
// thread. This is deliberate: a *synchronous* Tauri command executes on the
// main thread, which also drives the OS window event loop — so a multi-second
// compare would freeze the window (no repaint, no minimize/move, "Not
// Responding"). `proxy` hands a request/response call to the blocking-thread
// pool; `stream` hands an SSE subscription to it and forwards each event to the
// frontend as it arrives. The window stays fully interactive while work runs.

/// POST `body` to a JSON endpoint of the daemon and return its response.
async fn proxy(
    state: State<'_, AppState>,
    path: &'static str,
    body: Value,
) -> Result<Value, String> {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || daemon.post(path, body))
        .await
        .map_err(|e| e.to_string())?
}

/// POST `body` to an SSE endpoint of the daemon and re-emit every server-sent
/// event on `channel` as {event, data}. Returns as soon as the subscription is
/// handed off; the frontend listens for the events.
fn stream(
    app: AppHandle,
    state: State<'_, AppState>,
    path: &'static str,
    channel: &'static str,
    body: Value,
) {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || {
        daemon.stream(path, body, |event, data| {
            let _ = app.emit(channel, serde_json::json!({ "event": event, "data": data }));
        });
    });
}

#[tauri::command]
async fn pgdac_health(state: State<'_, AppState>) -> Result<Value, String> {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || daemon.get("/health"))
        .await
        .map_err(|e| e.to_string())?
}

/// Starts a compare and streams progress over COMPARE_EVENT. The request
/// carries an `opId` the frontend can pass to `pgdac_cancel` to abort it.
#[tauri::command]
fn pgdac_plan(app: AppHandle, state: State<'_, AppState>, req: Value) {
    stream(app, state, "/plan", COMPARE_EVENT, req);
}

/// Cancel an in-flight compare (or any op) by the opId the frontend assigned it.
#[tauri::command]
async fn pgdac_cancel(state: State<'_, AppState>, op_id: String) -> Result<Value, String> {
    proxy(state, "/cancel", serde_json::json!({ "opId": op_id })).await
}

/// Re-plans the last comparison with a changed selection or options, without
/// reading the sources again (see the daemon's /replan).
#[tauri::command]
async fn pgdac_replan(state: State<'_, AppState>, req: Value) -> Result<Value, String> {
    proxy(state, "/replan", req).await
}

#[tauri::command]
async fn pgdac_script(state: State<'_, AppState>, req: Value) -> Result<Value, String> {
    proxy(state, "/script", req).await
}

/// Starts a publish and streams progress over PUBLISH_EVENT
/// (start | progress | done | error).
#[tauri::command]
fn pgdac_publish(app: AppHandle, state: State<'_, AppState>, req: Value) {
    stream(app, state, "/publish", PUBLISH_EVENT, req);
}

/// Saved source profiles (connection strings / projects / artifacts). The list
/// returned to the frontend includes the raw value; the UI is responsible for
/// only ever displaying a masked label, never the secret.
#[tauri::command]
async fn profiles_list(app: AppHandle) -> Result<Vec<Profile>, String> {
    profiles::load(&app)
}

#[tauri::command]
async fn profile_add(app: AppHandle, profile: Profile) -> Result<Vec<Profile>, String> {
    profiles::add(&app, profile)
}

#[tauri::command]
async fn profile_remove(app: AppHandle, id: String) -> Result<Vec<Profile>, String> {
    profiles::remove(&app, &id)
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_dialog::init())
        .setup(|app| {
            let daemon = Daemon::start().map_err(|e| -> Box<dyn std::error::Error> {
                Box::<dyn std::error::Error>::from(format!("failed to start pg-dac daemon: {e}"))
            })?;
            app.manage(AppState {
                daemon: Arc::new(daemon),
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            pgdac_health,
            pgdac_plan,
            pgdac_cancel,
            pgdac_script,
            pgdac_replan,
            pgdac_publish,
            profiles_list,
            profile_add,
            profile_remove,
        ])
        .build(tauri::generate_context!())
        .expect("error while building pg-dac-ui")
        .run(|app_handle, event| {
            if let RunEvent::ExitRequested { .. } = event {
                if let Some(state) = app_handle.try_state::<AppState>() {
                    state.daemon.stop();
                }
            }
        });
}
