//! Saved source profiles: connection strings, project directories, and .pgdac
//! artifacts the user wants to keep between sessions, in profiles.json in the
//! app config dir.
//!
//! A connection string's password never goes into that file. When a database
//! source carries one, the full connection string is kept in the OS credential
//! store (Windows Credential Manager, the macOS Keychain, the Secret Service on
//! Linux) under the profile's id, and the file keeps the string without it
//! (`hasSecret: true`). The frontend only ever sees the file's version: it
//! refers to a saved source as `profile:<id>`, and `resolve` puts the full
//! string together here, in the Rust layer, just before a request goes to the
//! daemon. Files written by older versions, with passwords in them, are moved
//! to the credential store the first time they are read.

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Manager};

use crate::dsn::{self, Split};

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Profile {
    pub id: String,
    pub name: String,
    /// "db" | "project" | "artifact"
    pub kind: String,
    /// A connection string without its password (db), or a path.
    pub value: String,
    /// The full connection string, password included, is in the credential
    /// store under this profile's id.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub has_secret: bool,
}

/// Where the full connection strings are kept. The app uses the OS credential
/// store; tests use memory.
pub trait Secrets {
    fn set(&self, id: &str, secret: &str) -> Result<(), String>;
    fn get(&self, id: &str) -> Result<String, String>;
    /// Removes the secret; a secret that is not there is not an error.
    fn delete(&self, id: &str) -> Result<(), String>;
}

/// The OS credential store, through the keyring crate.
pub struct Keyring;

/// The service name every entry is filed under: the app's identifier.
const SERVICE: &str = "dev.pgdac.app";

fn entry(id: &str) -> Result<keyring::Entry, String> {
    keyring::Entry::new(SERVICE, &format!("profile:{id}")).map_err(store_error)
}

fn store_error(e: keyring::Error) -> String {
    format!("the system credential store is not available: {e}")
}

impl Secrets for Keyring {
    fn set(&self, id: &str, secret: &str) -> Result<(), String> {
        entry(id)?.set_password(secret).map_err(store_error)
    }
    fn get(&self, id: &str) -> Result<String, String> {
        entry(id)?.get_password().map_err(|e| match e {
            keyring::Error::NoEntry => "it is not in the system credential store".to_string(),
            e => store_error(e),
        })
    }
    fn delete(&self, id: &str) -> Result<(), String> {
        match entry(id)?.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(store_error(e)),
        }
    }
}

/// Serializes every read-modify-write of profiles.json: the frontend may list
/// and save at the same time.
static LOCK: Mutex<()> = Mutex::new(());

fn profiles_path(app: &AppHandle) -> Result<PathBuf, String> {
    let dir = app.path().app_config_dir().map_err(|e| e.to_string())?;
    fs::create_dir_all(&dir).map_err(|e| format!("create config dir: {e}"))?;
    Ok(dir.join("profiles.json"))
}

fn with_file<T>(app: &AppHandle, f: impl FnOnce(&Path) -> Result<T, String>) -> Result<T, String> {
    let _guard = LOCK.lock().unwrap_or_else(|e| e.into_inner());
    f(&profiles_path(app)?)
}

/// Load the saved profiles (empty list if the file does not exist yet).
pub fn load(app: &AppHandle) -> Result<Vec<Profile>, String> {
    with_file(app, |path| load_from(path, &Keyring))
}

/// Add (or update) a profile and return the full updated list.
pub fn add(app: &AppHandle, profile: Profile) -> Result<Vec<Profile>, String> {
    with_file(app, |path| add_to(path, &Keyring, profile))
}

/// Remove a profile by id and return the full updated list.
pub fn remove(app: &AppHandle, id: &str) -> Result<Vec<Profile>, String> {
    with_file(app, |path| remove_from(path, &Keyring, id))
}

/// The value a request carries to the daemon: a `profile:<id>` reference
/// becomes the saved source's full value (its connection string with the
/// password); anything else is passed through.
pub fn resolve(app: &AppHandle, spec: &str) -> Result<String, String> {
    if !spec.starts_with(PROFILE_REF) {
        return Ok(spec.to_string());
    }
    with_file(app, |path| resolve_from(path, &Keyring, spec))
}

const PROFILE_REF: &str = "profile:";

fn read(path: &Path) -> Result<Vec<Profile>, String> {
    match fs::read(path) {
        Ok(bytes) => {
            serde_json::from_slice(&bytes).map_err(|e| format!("parse profiles.json: {e}"))
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Vec::new()),
        Err(e) => Err(format!("read profiles.json: {e}")),
    }
}

/// Writes the file through a temporary file and a rename, so a crash never
/// leaves it half written.
fn write(path: &Path, profiles: &[Profile]) -> Result<(), String> {
    let data = serde_json::to_vec_pretty(profiles).map_err(|e| e.to_string())?;
    let tmp = path.with_extension("json.tmp");
    fs::write(&tmp, data).map_err(|e| format!("write profiles.json: {e}"))?;
    fs::rename(&tmp, path).map_err(|e| format!("write profiles.json: {e}"))
}

/// Moves the password of a database profile into the credential store,
/// leaving the profile with the rest of its connection string. Returns false
/// when there was no password to move.
fn seal(secrets: &dyn Secrets, p: &mut Profile) -> Result<bool, String> {
    if p.kind != "db" || p.has_secret {
        return Ok(false);
    }
    match dsn::split(&p.value)? {
        Split::NoPassword => Ok(false),
        Split::Stripped(rest) => {
            secrets.set(&p.id, &p.value)?;
            // Read it back before the password leaves the file.
            if secrets.get(&p.id)? != p.value {
                return Err(
                    "the system credential store returned a different value than was saved"
                        .to_string(),
                );
            }
            p.value = rest;
            p.has_secret = true;
            Ok(true)
        }
    }
}

pub fn load_from(path: &Path, secrets: &dyn Secrets) -> Result<Vec<Profile>, String> {
    let mut list = read(path)?;
    let mut moved = false;
    for p in &mut list {
        match seal(secrets, p) {
            Ok(m) => moved |= m,
            // The file stays as it was; the source keeps working, and the next
            // read tries again.
            Err(e) => eprintln!("pg-dac: could not move the password of saved source {:?} to the credential store: {e}", p.name),
        }
    }
    if moved {
        write(path, &list)?;
    }
    Ok(list)
}

pub fn add_to(
    path: &Path,
    secrets: &dyn Secrets,
    mut profile: Profile,
) -> Result<Vec<Profile>, String> {
    profile.has_secret = false;
    seal(secrets, &mut profile).map_err(|e| format!("the source was not saved: {e}"))?;
    let mut list = load_from(path, secrets)?;
    // The same source saved again (same id, or the same value) replaces the
    // earlier entry, a new password included.
    let (replaced, kept): (Vec<Profile>, Vec<Profile>) = list
        .drain(..)
        .partition(|p| p.id == profile.id || (p.kind == profile.kind && p.value == profile.value));
    list = kept;
    list.push(profile.clone());
    if let Err(e) = write(path, &list) {
        if profile.has_secret {
            let _ = secrets.delete(&profile.id);
        }
        return Err(e);
    }
    for old in replaced
        .iter()
        .filter(|p| p.has_secret && p.id != profile.id)
    {
        let _ = secrets.delete(&old.id);
    }
    Ok(list)
}

pub fn remove_from(path: &Path, secrets: &dyn Secrets, id: &str) -> Result<Vec<Profile>, String> {
    let mut list = load_from(path, secrets)?;
    let had_secret = list.iter().any(|p| p.id == id && p.has_secret);
    list.retain(|p| p.id != id);
    write(path, &list)?;
    if had_secret {
        secrets.delete(id)?;
    }
    Ok(list)
}

pub fn resolve_from(path: &Path, secrets: &dyn Secrets, spec: &str) -> Result<String, String> {
    let Some(id) = spec.strip_prefix(PROFILE_REF) else {
        return Ok(spec.to_string());
    };
    let list = load_from(path, secrets)?;
    let p = list
        .iter()
        .find(|p| p.id == id)
        .ok_or("the saved source no longer exists; choose it again")?;
    if !p.has_secret {
        return Ok(p.value.clone());
    }
    secrets.get(id).map_err(|e| {
        format!(
            "the password of the saved source {:?} could not be read ({e}); delete the source and save it again",
            p.name
        )
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::collections::HashMap;

    #[derive(Default)]
    struct Memory(RefCell<HashMap<String, String>>);

    impl Secrets for Memory {
        fn set(&self, id: &str, secret: &str) -> Result<(), String> {
            self.0.borrow_mut().insert(id.into(), secret.into());
            Ok(())
        }
        fn get(&self, id: &str) -> Result<String, String> {
            self.0
                .borrow()
                .get(id)
                .cloned()
                .ok_or_else(|| "missing".into())
        }
        fn delete(&self, id: &str) -> Result<(), String> {
            self.0.borrow_mut().remove(id);
            Ok(())
        }
    }

    /// A credential store that refuses everything, like a Linux session
    /// without a Secret Service.
    struct Unavailable;

    impl Secrets for Unavailable {
        fn set(&self, _: &str, _: &str) -> Result<(), String> {
            Err("no credential store".into())
        }
        fn get(&self, _: &str) -> Result<String, String> {
            Err("no credential store".into())
        }
        fn delete(&self, _: &str) -> Result<(), String> {
            Err("no credential store".into())
        }
    }

    fn db(id: &str, value: &str) -> Profile {
        Profile {
            id: id.into(),
            name: format!("[{id}]"),
            kind: "db".into(),
            value: value.into(),
            has_secret: false,
        }
    }

    fn file() -> (tempdir::Dir, PathBuf) {
        let dir = tempdir::Dir::new();
        let path = dir.0.join("profiles.json");
        (dir, path)
    }

    /// The password never reaches the file or the list the frontend gets, and
    /// a request resolves to the full connection string.
    #[test]
    fn saving_keeps_the_password_out_of_the_file() {
        let (_dir, path) = file();
        let secrets = Memory::default();
        let full = "postgres://app:s3cret@db.example/app?sslmode=require";

        let list = add_to(&path, &secrets, db("dev", full)).unwrap();
        assert_eq!(
            list[0].value,
            "postgres://app@db.example/app?sslmode=require"
        );
        assert!(list[0].has_secret);
        assert!(!fs::read_to_string(&path).unwrap().contains("s3cret"));
        assert_eq!(secrets.get("dev").unwrap(), full);
        assert_eq!(resolve_from(&path, &secrets, "profile:dev").unwrap(), full);
        assert_eq!(
            resolve_from(&path, &secrets, "./schema").unwrap(),
            "./schema"
        );
    }

    /// A file from an older version, with passwords in it, is migrated on the
    /// first read; sources without a password are left alone.
    #[test]
    fn migrates_an_old_file() {
        let (_dir, path) = file();
        let old = vec![
            db("dev", "host=dev user=app password=s3cret dbname=app"),
            db("local", "postgres://app@localhost/app"),
            Profile {
                id: "proj".into(),
                name: "p".into(),
                kind: "project".into(),
                value: r"C:\dev\app\schema".into(),
                has_secret: false,
            },
        ];
        fs::write(&path, serde_json::to_vec(&old).unwrap()).unwrap();
        let secrets = Memory::default();

        let list = load_from(&path, &secrets).unwrap();
        assert_eq!(list[0].value, "host=dev user=app dbname=app");
        assert!(list[0].has_secret && !list[1].has_secret && !list[2].has_secret);
        assert!(!fs::read_to_string(&path).unwrap().contains("s3cret"));
        assert_eq!(
            resolve_from(&path, &secrets, "profile:dev").unwrap(),
            "host=dev user=app password=s3cret dbname=app"
        );
        assert_eq!(
            resolve_from(&path, &secrets, "profile:local").unwrap(),
            "postgres://app@localhost/app"
        );
    }

    /// Without a credential store an old file is not touched (nothing is
    /// lost) and a new password is refused instead of being written to disk.
    #[test]
    fn without_a_credential_store() {
        let (_dir, path) = file();
        let old = vec![db("dev", "postgres://app:s3cret@dev/app")];
        let original = serde_json::to_vec(&old).unwrap();
        fs::write(&path, &original).unwrap();

        let list = load_from(&path, &Unavailable).unwrap();
        assert_eq!(list[0].value, "postgres://app:s3cret@dev/app");
        assert_eq!(fs::read(&path).unwrap(), original);

        let err = add_to(
            &path,
            &Unavailable,
            db("acc", "postgres://app:other@acc/app"),
        )
        .unwrap_err();
        assert!(err.contains("not saved"), "{err}");
        assert!(!fs::read_to_string(&path).unwrap().contains("other"));
        // A source without a password needs no credential store.
        add_to(
            &path,
            &Unavailable,
            db("local", "postgres://app@localhost/app"),
        )
        .unwrap();
    }

    /// Saving the same source again replaces it, password included; removing
    /// it removes its password too.
    #[test]
    fn replace_and_remove() {
        let (_dir, path) = file();
        let secrets = Memory::default();
        add_to(&path, &secrets, db("a", "postgres://app:one@h/db")).unwrap();
        let list = add_to(&path, &secrets, db("b", "postgres://app:two@h/db")).unwrap();
        assert_eq!(list.len(), 1);
        assert!(
            secrets.get("a").is_err(),
            "the replaced source's password is left behind"
        );
        assert_eq!(
            resolve_from(&path, &secrets, "profile:b").unwrap(),
            "postgres://app:two@h/db"
        );

        let list = remove_from(&path, &secrets, "b").unwrap();
        assert!(list.is_empty());
        assert!(secrets.get("b").is_err());
        assert!(resolve_from(&path, &secrets, "profile:b").is_err());
    }

    /// A password removed from the credential store outside the app is
    /// reported by name, not silently dropped from the connection.
    #[test]
    fn missing_password_is_reported() {
        let (_dir, path) = file();
        let secrets = Memory::default();
        add_to(&path, &secrets, db("dev", "postgres://app:s3cret@dev/app")).unwrap();
        secrets.delete("dev").unwrap();
        let err = resolve_from(&path, &secrets, "profile:dev").unwrap_err();
        assert!(
            err.contains("[dev]") && err.contains("save it again"),
            "{err}"
        );
    }

    /// A string whose password cannot be located is refused, never stored.
    #[test]
    fn refuses_an_unparseable_password() {
        let (_dir, path) = file();
        let err = add_to(
            &path,
            &Memory::default(),
            db("x", "Host=h;Password=s3cret;Database=d"),
        )
        .unwrap_err();
        assert!(err.contains("not saved"), "{err}");
        assert!(!path.exists() || !fs::read_to_string(&path).unwrap().contains("s3cret"));
    }

    /// The real OS credential store, round trip. Ignored by default because it
    /// writes to the user's credential store; run with
    /// `cargo test -- --ignored os_credential_store`.
    #[test]
    #[ignore]
    fn os_credential_store() {
        let id = format!("test-{}", std::process::id());
        Keyring.set(&id, "postgres://app:s3cret@h/db").unwrap();
        assert_eq!(Keyring.get(&id).unwrap(), "postgres://app:s3cret@h/db");
        Keyring.delete(&id).unwrap();
        assert!(Keyring.get(&id).is_err());
        Keyring.delete(&id).unwrap();
    }

    mod tempdir {
        use std::path::PathBuf;

        /// A temporary directory removed when dropped.
        pub struct Dir(pub PathBuf);

        impl Dir {
            pub fn new() -> Self {
                use std::sync::atomic::{AtomicU32, Ordering};
                static N: AtomicU32 = AtomicU32::new(0);
                let p = std::env::temp_dir().join(format!(
                    "pgdac-profiles-{}-{}",
                    std::process::id(),
                    N.fetch_add(1, Ordering::Relaxed)
                ));
                std::fs::create_dir_all(&p).unwrap();
                Dir(p)
            }
        }

        impl Drop for Dir {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
    }
}
