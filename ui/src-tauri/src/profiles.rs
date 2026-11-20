//! Saved source profiles: connection strings, project directories, and .pgdac
//! artifacts the user wants to keep between sessions. Persisted as a JSON array
//! in the app config dir. Per the user's choice, DB connection strings are stored
//! verbatim in this file (the GUI only ever *displays* a masked "database @ host"
//! label — the secret is never rendered), so this file is user-scoped local state.

use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Manager};

#[derive(Serialize, Deserialize, Clone)]
pub struct Profile {
    pub id: String,
    pub name: String,
    /// "db" | "project" | "artifact"
    pub kind: String,
    /// The raw value: a connection string (db), or a path (project/artifact).
    pub value: String,
}

fn profiles_path(app: &AppHandle) -> Result<PathBuf, String> {
    let dir = app.path().app_config_dir().map_err(|e| e.to_string())?;
    fs::create_dir_all(&dir).map_err(|e| format!("create config dir: {e}"))?;
    Ok(dir.join("profiles.json"))
}

/// Load the saved profiles (empty list if the file does not exist yet).
pub fn load(app: &AppHandle) -> Result<Vec<Profile>, String> {
    let path = profiles_path(app)?;
    match fs::read(&path) {
        Ok(bytes) => {
            serde_json::from_slice(&bytes).map_err(|e| format!("parse profiles.json: {e}"))
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Vec::new()),
        Err(e) => Err(format!("read profiles.json: {e}")),
    }
}

fn store(app: &AppHandle, profiles: &[Profile]) -> Result<(), String> {
    let path = profiles_path(app)?;
    let data = serde_json::to_vec_pretty(profiles).map_err(|e| e.to_string())?;
    fs::write(&path, data).map_err(|e| format!("write profiles.json: {e}"))
}

/// Add (or update) a profile, de-duplicating by id and by (kind, value), then
/// return the full updated list.
pub fn add(app: &AppHandle, profile: Profile) -> Result<Vec<Profile>, String> {
    let mut list = load(app)?;
    list.retain(|p| p.id != profile.id && !(p.kind == profile.kind && p.value == profile.value));
    list.push(profile);
    store(app, &list)?;
    Ok(list)
}

/// Remove a profile by id and return the full updated list.
pub fn remove(app: &AppHandle, id: &str) -> Result<Vec<Profile>, String> {
    let mut list = load(app)?;
    list.retain(|p| p.id != id);
    store(app, &list)?;
    Ok(list)
}
