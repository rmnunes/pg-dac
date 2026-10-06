//! Supervises the bundled `pg-dac serve` sidecar: spawns it, reads the loopback
//! address it prints on its first stdout line (the handshake), authenticates
//! every request with a per-launch bearer token passed via the PGDAC_TOKEN env
//! var, and proxies HTTP/SSE calls to it. pg-dac owns all PostgreSQL logic; this
//! is a thin client + process lifecycle.

use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::mpsc;
use std::sync::Mutex;
use std::time::Duration;

use rand::Rng;
use serde_json::Value;

pub struct Daemon {
    addr: String,
    token: String,
    bin: PathBuf,
    child: Mutex<Option<Child>>,
}

impl Daemon {
    /// Locate the sidecar, spawn `pg-dac serve`, and read its address handshake.
    pub fn start() -> Result<Daemon, String> {
        let bin = find_sidecar().ok_or_else(|| {
            "pg-dac sidecar binary not found next to the app or in src-tauri/binaries".to_string()
        })?;
        let token = random_token();

        let mut child = Command::new(&bin)
            .arg("serve")
            .env("PGDAC_TOKEN", &token)
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|e| format!("spawn pg-dac serve: {e}"))?;

        let stdout = child.stdout.take().ok_or("no stdout from sidecar")?;

        // Read the first line (the bound address) with a timeout, then keep
        // draining stdout so the child never blocks on a full pipe.
        let (tx, rx) = mpsc::channel::<String>();
        std::thread::spawn(move || {
            let mut reader = BufReader::new(stdout);
            let mut first = String::new();
            if reader.read_line(&mut first).is_ok() {
                let _ = tx.send(first.trim().to_string());
            }
            let mut sink = String::new();
            loop {
                sink.clear();
                if reader.read_line(&mut sink).unwrap_or(0) == 0 {
                    break;
                }
            }
        });

        let addr = match rx.recv_timeout(Duration::from_secs(30)) {
            Ok(a) if !a.is_empty() => a,
            _ => {
                let _ = child.kill();
                return Err("pg-dac serve did not report its address".to_string());
            }
        };

        Ok(Daemon {
            addr,
            token,
            bin,
            child: Mutex::new(Some(child)),
        })
    }

    fn url(&self, path: &str) -> String {
        format!("http://{}{}", self.addr, path)
    }

    fn auth(&self, req: ureq::Request) -> ureq::Request {
        req.set("Authorization", &format!("Bearer {}", self.token))
    }

    /// GET a JSON endpoint (e.g. /health).
    pub fn get(&self, path: &str) -> Result<Value, String> {
        let resp = self
            .auth(ureq::get(&self.url(path)))
            .call()
            .map_err(stringify)?;
        resp.into_json::<Value>().map_err(|e| e.to_string())
    }

    /// POST a JSON body and decode the JSON response (e.g. /plan, /script).
    pub fn post(&self, path: &str, body: Value) -> Result<Value, String> {
        let resp = self
            .auth(ureq::post(&self.url(path)))
            .send_json(body)
            .map_err(stringify)?;
        resp.into_json::<Value>().map_err(|e| e.to_string())
    }

    /// POST a JSON body to an SSE endpoint and invoke `on_event(event, data)` for
    /// each server-sent event. Used by both /plan (phase|result|canceled|error)
    /// and /publish (start|progress|done|error).
    pub fn stream<F: FnMut(&str, Value)>(&self, path: &str, body: Value, mut on_event: F) {
        let resp = match self.auth(ureq::post(&self.url(path))).send_json(body) {
            Ok(r) => r,
            Err(e) => {
                on_event("error", serde_json::json!({ "message": stringify(e) }));
                return;
            }
        };
        let reader = BufReader::new(resp.into_reader());
        let mut event = String::new();
        for line in reader.lines() {
            let line = match line {
                Ok(l) => l,
                Err(e) => {
                    on_event("error", serde_json::json!({ "message": e.to_string() }));
                    return;
                }
            };
            if let Some(rest) = line.strip_prefix("event:") {
                event = rest.trim().to_string();
            } else if let Some(rest) = line.strip_prefix("data:") {
                let data: Value = serde_json::from_str(rest.trim()).unwrap_or(Value::Null);
                let name = if event.is_empty() { "message" } else { &event };
                on_event(name, data);
            } else if line.is_empty() {
                event.clear();
            }
        }
    }

    /// Ask the daemon to cancel the in-flight operation with the given opId.
    pub fn cancel(&self, op_id: &str) -> Result<Value, String> {
        self.post("/cancel", serde_json::json!({ "opId": op_id }))
    }

    /// Run the sidecar one-shot (for stateless verbs like build/extract/inspect)
    /// and return combined stdout/stderr text.
    pub fn run_oneshot(&self, args: &[String], env: &[(String, String)]) -> Result<String, String> {
        let mut cmd = Command::new(&self.bin);
        cmd.args(args);
        for (k, v) in env {
            cmd.env(k, v);
        }
        let out = cmd.output().map_err(|e| format!("run pg-dac: {e}"))?;
        let mut text = String::new();
        text.push_str(&String::from_utf8_lossy(&out.stdout));
        text.push_str(&String::from_utf8_lossy(&out.stderr));
        if !out.status.success() {
            return Err(text);
        }
        Ok(text)
    }

    /// Best-effort graceful stop: ask the daemon to shut down (so it stops its
    /// embedded Postgres cleanly), then kill the child if it lingers.
    pub fn stop(&self) {
        let _ = self.auth(ureq::post(&self.url("/shutdown"))).call();
        if let Ok(mut guard) = self.child.lock() {
            if let Some(mut child) = guard.take() {
                std::thread::sleep(Duration::from_millis(500));
                let _ = child.kill();
                let _ = child.wait();
            }
        }
    }
}

fn stringify(e: ureq::Error) -> String {
    match e {
        ureq::Error::Status(code, resp) => {
            let body = resp.into_string().unwrap_or_default();
            format!("daemon returned {code}: {body}")
        }
        ureq::Error::Transport(t) => format!("daemon transport error: {t}"),
    }
}

fn random_token() -> String {
    let mut rng = rand::thread_rng();
    (0..32).map(|_| format!("{:x}", rng.gen_range(0..16u8))).collect()
}

/// find_sidecar looks for the pg-dac binary next to the running app (where Tauri
/// places externalBin) and, as a dev fallback, in src-tauri/binaries.
fn find_sidecar() -> Option<PathBuf> {
    let mut dirs: Vec<PathBuf> = Vec::new();
    if let Ok(exe) = std::env::current_exe() {
        if let Some(p) = exe.parent() {
            dirs.push(p.to_path_buf());
        }
    }
    dirs.push(PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("binaries"));

    for dir in dirs {
        if let Ok(entries) = std::fs::read_dir(&dir) {
            for entry in entries.flatten() {
                let name = entry.file_name().to_string_lossy().to_string();
                let stem = name.trim_end_matches(".exe");
                // Match "pg-dac" or "pg-dac-<triple>", but never the UI binary.
                if (stem == "pg-dac" || stem.starts_with("pg-dac-")) && !stem.contains("ui") {
                    return Some(entry.path());
                }
            }
        }
    }
    None
}
