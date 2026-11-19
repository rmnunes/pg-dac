//! pg-dac-ui Tauri backend: a thin supervisor + proxy over the bundled
//! `pg-dac serve` daemon. The frontend invokes these commands; they forward to
//! the warm daemon and (for publish) stream SSE progress back as Tauri events.

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

// The proxy commands below are `async` and run their blocking ureq call inside
// `spawn_blocking`. This is deliberate: a *synchronous* Tauri command executes on
// the main thread, which also drives the OS window event loop — so a multi-second
// compare would freeze the window (no repaint, no minimize/move, "Not Responding").
// Marking them async hands them to the async runtime, and spawn_blocking moves the
// blocking HTTP I/O onto a dedicated blocking-thread pool so it never starves an
// async worker either. The window stays fully interactive while work runs.

#[tauri::command]
async fn pgdac_health(state: State<'_, AppState>) -> Result<Value, String> {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || daemon.get("/health"))
        .await
        .map_err(|e| e.to_string())?
}

/// Starts a compare and streams progress over COMPARE_EVENT. Returns immediately;
/// the frontend listens for phase/result/canceled/error events. The request
/// carries an `opId` the frontend can pass to `pgdac_cancel` to abort it.
#[tauri::command]
fn pgdac_plan(app: AppHandle, state: State<AppState>, req: Value) -> Result<(), String> {
    let daemon = state.daemon.clone();
    std::thread::spawn(move || {
        daemon.stream("/plan", req, |event, data| {
            let _ = app.emit(COMPARE_EVENT, serde_json::json!({ "event": event, "data": data }));
        });
    });
    Ok(())
}

/// Cancel an in-flight compare (or any op) by the opId the frontend assigned it.
#[tauri::command]
async fn pgdac_cancel(state: State<'_, AppState>, op_id: String) -> Result<Value, String> {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || daemon.cancel(&op_id))
        .await
        .map_err(|e| e.to_string())?
}

/// Re-plans the last comparison with a changed selection or options, without
/// reading the sources again (see the daemon's /replan).
#[tauri::command]
async fn pgdac_replan(state: State<'_, AppState>, req: Value) -> Result<Value, String> {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || daemon.post("/replan", req))
        .await
        .map_err(|e| e.to_string())?
}

#[tauri::command]
async fn pgdac_script(state: State<'_, AppState>, req: Value) -> Result<Value, String> {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || daemon.post("/script", req))
        .await
        .map_err(|e| e.to_string())?
}

/// Starts a publish and streams progress over the PUBLISH_EVENT channel. Returns
/// immediately; the frontend listens for start/progress/done/error events.
#[tauri::command]
fn pgdac_publish(app: AppHandle, state: State<AppState>, req: Value) -> Result<(), String> {
    let daemon = state.daemon.clone();
    std::thread::spawn(move || {
        daemon.stream("/publish", req, |event, data| {
            let _ = app.emit(PUBLISH_EVENT, serde_json::json!({ "event": event, "data": data }));
        });
    });
    Ok(())
}

/// Build a .pgdac (one-shot). args mirror the CLI: name/version/no-sign/schema.
#[tauri::command]
async fn pgdac_build(
    state: State<'_, AppState>,
    from: String,
    out: String,
    name: Option<String>,
    version: Option<String>,
    no_sign: bool,
) -> Result<String, String> {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || {
        let mut args = vec!["build".into(), "--from".into(), from, "--out".into(), out];
        if let Some(n) = name {
            args.push("--name".into());
            args.push(n);
        }
        if let Some(v) = version {
            args.push("--version".into());
            args.push(v);
        }
        if no_sign {
            args.push("--no-sign".into());
        }
        daemon.run_oneshot(&args, &[])
    })
    .await
    .map_err(|e| e.to_string())?
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

/// Inspect/verify a .pgdac artifact (one-shot).
#[tauri::command]
async fn pgdac_inspect(state: State<'_, AppState>, path: String) -> Result<String, String> {
    let daemon = state.daemon.clone();
    tauri::async_runtime::spawn_blocking(move || daemon.run_oneshot(&["inspect".into(), path], &[]))
        .await
        .map_err(|e| e.to_string())?
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
            pgdac_build,
            pgdac_inspect,
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
