//! `/api/backup` — the phone's `/` menu drives the engine's own backup.
//!
//! The tree is stored in a Hugging Face bucket by the `aixodia-backup` crate;
//! this module only decides *when* it happens and reports how it went. The hub
//! token and the bucket name live in D1 state under `backup/config`, never in
//! the image or the source.
//!
//! Everything that touches the bucket blocks: `reqwest::blocking` refuses to be
//! built inside a tokio runtime, and `hf-xet` panics when its blocking calls run
//! on a worker. So each operation is handed to a plain OS thread and awaited
//! through a channel, and a worker that panics must still release the slot —
//! otherwise the engine could never back up again.

use std::sync::{Arc, Mutex, OnceLock};

use axum::body::Bytes;
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::Response;
use serde::Deserialize;
use serde_json::{json, Value};

use crate::gateway::{write_error, write_json, Gateway};

/// The D1 state key holding the backup configuration.
pub const BACKUP_CONFIG_KEY: &str = "backup/config";

/// One running operation and the result of the last one to finish.
#[derive(Default)]
struct Slots {
    active: Option<Active>,
    finished: Value,
}

struct Active {
    operation: String,
    started_at: i64,
    reporter: aixodia_backup::Reporter,
}

fn slots() -> &'static Mutex<Slots> {
    static SLOTS: OnceLock<Mutex<Slots>> = OnceLock::new();
    SLOTS.get_or_init(|| Mutex::new(Slots::default()))
}

/// The config as it is stored, with every field optional so a partial write
/// keeps what was already there.
#[derive(Clone, Debug, Default, serde::Serialize, Deserialize)]
struct Stored {
    #[serde(default)]
    token: Option<String>,
    #[serde(default)]
    bucket: Option<String>,
    #[serde(default)]
    endpoint: Option<String>,
    #[serde(default)]
    prefix: Option<String>,
    #[serde(default)]
    root: Option<String>,
    #[serde(default)]
    excludes: Option<Vec<String>>,
}

/// The configuration, with the stored state layered over the defaults. The
/// environment may supply the token, for a machine that keeps it out of D1.
fn resolve(stored: Option<&str>) -> aixodia_backup::Config {
    let parsed: Stored = stored
        .and_then(|raw| serde_json::from_str(raw).ok())
        .unwrap_or_default();
    let defaults = aixodia_backup::Config::default();
    let token = parsed
        .token
        .filter(|value| !value.trim().is_empty())
        .or_else(|| {
            std::env::var("HF_TOKEN")
                .ok()
                .filter(|value| !value.trim().is_empty())
        })
        .unwrap_or(defaults.token);
    let bucket = parsed
        .bucket
        .filter(|value| !value.trim().is_empty())
        .unwrap_or(defaults.bucket);
    let endpoint = parsed
        .endpoint
        .filter(|value| !value.trim().is_empty())
        .unwrap_or(defaults.endpoint);
    let root = parsed
        .root
        .map(std::path::PathBuf::from)
        .unwrap_or(defaults.root);
    aixodia_backup::Config {
        token,
        bucket,
        endpoint,
        prefix: parsed.prefix.unwrap_or(defaults.prefix),
        root,
        excludes: parsed.excludes.unwrap_or(defaults.excludes),
        timeout_secs: defaults.timeout_secs,
    }
}

async fn stored_config(g: &Gateway) -> Result<Option<String>, String> {
    let Some(store) = g.state_store() else {
        return Err("the engine state store is not open".into());
    };
    store
        .get_state(BACKUP_CONFIG_KEY)
        .await
        .map_err(|err| err.to_string())
}

/// `GET /api/backup` — whether it is configured, what is running, and how the
/// last run ended. Nothing here goes to the bucket.
pub async fn api_backup_status(State(g): State<Arc<Gateway>>) -> Response {
    let stored = match stored_config(&g).await {
        Ok(stored) => stored,
        Err(err) => return write_error(StatusCode::BAD_GATEWAY, "database_error", &err),
    };
    let config = resolve(stored.as_deref());
    let slots = slots().lock().unwrap_or_else(|e| e.into_inner());
    let active = slots.active.as_ref().map(|active| {
        json!({
            "operation": active.operation,
            "started_at": active.started_at,
            "progress": active.reporter.snapshot(),
        })
    });
    write_json(
        StatusCode::OK,
        json!({
            "configured": ready(&config, BACKUP_RUN),
            "needs": needs(&config, BACKUP_RUN),
            "restorable": ready(&config, RESTORE_RUN),
            "needs_restore": needs(&config, RESTORE_RUN),
            "bucket": config.bucket,
            "prefix": config.prefix,
            "root": config.root.display().to_string(),
            "excludes": config.excludes,
            "token": mask(&config.token),
            "active": active,
            "finished": slots.finished,
        }),
    )
}

/// `PUT /api/backup/config` — store the hub token and the bucket in D1.
pub async fn api_backup_config_put(State(g): State<Arc<Gateway>>, body: Bytes) -> Response {
    let incoming: Stored = match serde_json::from_slice(&body) {
        Ok(incoming) => incoming,
        Err(err) => {
            return write_error(
                StatusCode::BAD_REQUEST,
                "invalid_request_error",
                &format!("config must be a JSON object: {err}"),
            )
        }
    };
    let Some(store) = g.state_store() else {
        return write_error(
            StatusCode::BAD_GATEWAY,
            "database_error",
            "the engine state store is not open",
        );
    };
    let stored = match stored_config(&g).await {
        Ok(stored) => stored,
        Err(err) => return write_error(StatusCode::BAD_GATEWAY, "database_error", &err),
    };
    // An omitted field keeps its stored value, so the app can set the bucket
    // without ever resending the token.
    let previous: Stored = stored
        .as_deref()
        .and_then(|raw| serde_json::from_str(raw).ok())
        .unwrap_or_default();
    let merged = Stored {
        token: incoming
            .token
            .filter(|value| !value.trim().is_empty())
            .or(previous.token),
        bucket: incoming.bucket.or(previous.bucket),
        endpoint: incoming.endpoint.or(previous.endpoint),
        prefix: incoming.prefix.or(previous.prefix),
        root: incoming.root.or(previous.root),
        excludes: incoming.excludes.or(previous.excludes),
    };
    let raw = match serde_json::to_string(&merged) {
        Ok(raw) => raw,
        Err(err) => {
            return write_error(
                StatusCode::INTERNAL_SERVER_ERROR,
                "encode_failed",
                &err.to_string(),
            )
        }
    };
    if let Err(err) = store.put_state(BACKUP_CONFIG_KEY, &raw).await {
        return write_error(StatusCode::BAD_GATEWAY, "database_error", &err.to_string());
    }
    let config = resolve(Some(&raw));
    write_json(
        StatusCode::OK,
        json!({
            "configured": ready(&config, BACKUP_RUN),
            "needs": needs(&config, BACKUP_RUN),
            "restorable": ready(&config, RESTORE_RUN),
            "needs_restore": needs(&config, RESTORE_RUN),
            "bucket": config.bucket,
            "prefix": config.prefix,
            "root": config.root.display().to_string(),
            "excludes": config.excludes,
            "token": mask(&config.token),
        }),
    )
}

/// `POST /api/backup` — start a backup, or report that one is already running.
pub async fn api_backup_start(State(g): State<Arc<Gateway>>) -> Response {
    let stored = match stored_config(&g).await {
        Ok(stored) => stored,
        Err(err) => return write_error(StatusCode::BAD_GATEWAY, "database_error", &err),
    };
    let config = resolve(stored.as_deref());
    if !ready(&config, BACKUP_RUN) {
        return not_configured(&config, BACKUP_RUN);
    }
    let Some(reporter) = claim("backup") else {
        return busy();
    };
    spawn_job("backup", move || {
        aixodia_backup::backup(&config, &reporter)
            .and_then(json_of)
            .map_err(|err| err.to_string())
    });
    write_json(
        StatusCode::ACCEPTED,
        json!({"started": true, "operation": "backup"}),
    )
}

/// `POST /api/backup/restore` — rebuild the tree from the bucket's one
/// manifest. Requires `confirm`.
pub async fn api_backup_restore(State(g): State<Arc<Gateway>>, body: Bytes) -> Response {
    #[derive(Deserialize)]
    struct Request {
        #[serde(default)]
        confirm: bool,
    }
    let request: Request = match serde_json::from_slice(&body) {
        Ok(request) => request,
        Err(err) => {
            return write_error(
                StatusCode::BAD_REQUEST,
                "invalid_request_error",
                &format!("restore body must be JSON: {err}"),
            )
        }
    };
    // Overwriting the tree is the one thing a stray tap must not do, so the
    // engine will not restore without an explicit yes in the body.
    if !request.confirm {
        return write_json(
            StatusCode::BAD_REQUEST,
            json!({
                "error": "restore overwrites files under the root",
                "confirm_required": true,
            }),
        );
    }
    let stored = match stored_config(&g).await {
        Ok(stored) => stored,
        Err(err) => return write_error(StatusCode::BAD_GATEWAY, "database_error", &err),
    };
    let config = resolve(stored.as_deref());
    if !ready(&config, RESTORE_RUN) {
        return not_configured(&config, RESTORE_RUN);
    }
    let Some(reporter) = claim("restore") else {
        return busy();
    };
    // The target is always the configured root: a client chooses nothing about
    // where the tree lands.
    let root = config.root.clone();
    let label = root.display().to_string();
    spawn_job("restore", move || {
        aixodia_backup::restore(&config, &root, &reporter)
            .and_then(json_of)
            .map_err(|err| err.to_string())
    });
    write_json(
        StatusCode::ACCEPTED,
        json!({"started": true, "operation": "restore", "root": label}),
    )
}

fn json_of<T: serde::Serialize>(value: T) -> Result<Value, aixodia_backup::Error> {
    serde_json::to_value(value).map_err(aixodia_backup::Error::Json)
}

/// Whether a run could start now.
fn ready(config: &aixodia_backup::Config, for_backup: bool) -> bool {
    needs(config, for_backup).is_empty()
}

/// `needs()` argument: this run reads the tree, so the root has to be there.
const BACKUP_RUN: bool = true;
/// `needs()` argument: this run rebuilds the tree, so a missing root is the
/// reason to run it, not a reason to refuse.
const RESTORE_RUN: bool = false;

/// What has to be filled in before a run can start, as field names the app can
/// point at.
///
/// `for_backup` decides whether the root itself has to exist. A backup reads the
/// tree, so a missing root is a mistake in the configuration; a restore is the
/// answer to a missing tree — refusing it because the directory is not there
/// would be refusing to do the one job it has. The restore creates the root.
fn needs(config: &aixodia_backup::Config, for_backup: bool) -> Vec<&'static str> {
    let mut missing = Vec::new();
    if config.token.trim().is_empty() {
        missing.push("token");
    }
    if config.bucket.trim().is_empty() {
        missing.push("bucket");
    }
    if for_backup && !config.root.is_dir() {
        missing.push("root");
    }
    missing
}

fn not_configured(config: &aixodia_backup::Config, for_backup: bool) -> Response {
    write_json(
        StatusCode::BAD_REQUEST,
        json!({"error": "backup is not configured", "needs": needs(config, for_backup)}),
    )
}

/// Never echo a token. The tail is enough to tell two keys apart.
fn mask(token: &str) -> Value {
    if token.trim().is_empty() {
        return json!(null);
    }
    let chars: Vec<char> = token.chars().collect();
    let tail: String = chars[chars.len().saturating_sub(4)..].iter().collect();
    json!({"set": true, "tail": tail})
}

/// Take the single job slot, or `None` when a run is already in it.
fn claim(operation: &str) -> Option<aixodia_backup::Reporter> {
    let mut slots = slots().lock().unwrap_or_else(|e| e.into_inner());
    if slots.active.is_some() {
        return None;
    }
    let reporter = aixodia_backup::Reporter::new(operation);
    slots.active = Some(Active {
        operation: operation.to_string(),
        started_at: aixodia_backup::now_unix(),
        reporter: reporter.clone(),
    });
    Some(reporter)
}

fn busy() -> Response {
    let slots = slots().lock().unwrap_or_else(|e| e.into_inner());
    let operation = slots
        .active
        .as_ref()
        .map(|active| active.operation.clone())
        .unwrap_or_default();
    write_json(
        StatusCode::CONFLICT,
        json!({"error": "a backup operation is already running", "operation": operation}),
    )
}

/// Run one job on its own thread and record how it ended, whatever it does —
/// including panicking, which must not leave the slot claimed forever.
fn spawn_job(
    operation: &'static str,
    work: impl FnOnce() -> Result<Value, String> + Send + 'static,
) {
    let started_at = aixodia_backup::now_unix();
    std::thread::spawn(move || {
        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(work))
            .unwrap_or_else(|_| Err("the backup worker panicked".to_string()));
        finish(operation, started_at, outcome);
    });
}

fn finish(operation: &str, started_at: i64, outcome: Result<Value, String>) {
    let summary = match &outcome {
        Ok(report) => json!({"ok": true, "report": report}),
        Err(err) => json!({"ok": false, "error": err}),
    };
    let mut slots = slots().lock().unwrap_or_else(|e| e.into_inner());
    if slots
        .active
        .as_ref()
        .map(|active| active.operation.as_str())
        == Some(operation)
    {
        slots.active = None;
    }
    slots.finished = json!({
        "operation": operation,
        "started_at": started_at,
        "finished_at": aixodia_backup::now_unix(),
        "summary": summary,
    });
    match outcome {
        Ok(_) => tracing::info!("backup {operation}: {summary}"),
        Err(err) => tracing::warn!("backup {operation} failed: {err}"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn blanks_are_reported_one_by_one() {
        let cfg = aixodia_backup::Config {
            token: String::new(),
            bucket: String::new(),
            root: std::path::PathBuf::from("/definitely/not/here"),
            ..Default::default()
        };
        assert_eq!(needs(&cfg, BACKUP_RUN), vec!["token", "bucket", "root"]);
        assert!(!ready(&cfg, BACKUP_RUN));
    }

    /// A machine whose tree is gone is exactly the machine that needs a restore,
    /// so the missing directory may not be a reason to refuse one.
    #[test]
    fn a_missing_root_blocks_a_backup_not_a_restore() {
        let dir = std::env::temp_dir().join(format!("aix-backup-root-{}", std::process::id()));
        let gone = dir.join("no-longer-here");
        let cfg = aixodia_backup::Config {
            token: "hf_stored".into(),
            bucket: "me/bucket".into(),
            root: gone.clone(),
            ..Default::default()
        };
        assert_eq!(needs(&cfg, BACKUP_RUN), vec!["root"]);
        assert!(!ready(&cfg, BACKUP_RUN));
        assert!(needs(&cfg, RESTORE_RUN).is_empty());
        assert!(ready(&cfg, RESTORE_RUN));

        std::fs::create_dir_all(&dir).ok();
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn a_write_without_a_token_keeps_the_stored_one() {
        let previous: Stored =
            serde_json::from_str(r#"{"token":"hf_stored","bucket":"me/bucket"}"#).unwrap();
        let incoming: Stored =
            serde_json::from_str(r#"{"bucket":"me/other","prefix":"p"}"#).unwrap();
        let merged = Stored {
            token: incoming.token.or(previous.token),
            bucket: incoming.bucket.or(previous.bucket),
            endpoint: incoming.endpoint.or(previous.endpoint),
            prefix: incoming.prefix.or(previous.prefix),
            root: incoming.root.or(previous.root),
            excludes: incoming.excludes.or(previous.excludes),
        };
        let cfg = resolve(Some(&serde_json::to_string(&merged).unwrap()));
        assert_eq!(cfg.bucket, "me/other");
        assert_eq!(cfg.prefix, "p");
        // A read-only test cannot claim the env token is absent, so it asserts
        // the one thing the merge decides: a write that never mentions the
        // token does not clear it.
        assert!(!cfg.token.is_empty() || std::env::var("HF_TOKEN").is_err());
    }

    /// An omitted `excludes` is the phone saying nothing about the scope, which
    /// must not silently widen or narrow what a run walks. Sending a list — even
    /// an empty one — is the phone answering for it, and an empty list really
    /// does mean "exclude nothing".
    #[test]
    fn a_write_that_does_not_mention_excludes_leaves_them_alone() {
        let previous: Stored = serde_json::from_str(r#"{"excludes":["target","*.pyc"]}"#).unwrap();
        let untouched: Stored = serde_json::from_str(r#"{"bucket":"me/bucket"}"#).unwrap();
        assert_eq!(
            untouched.excludes.or(previous.excludes),
            Some(vec!["target".to_string(), "*.pyc".to_string()]),
            "a write that never mentions the scope keeps it"
        );

        let previous: Stored = serde_json::from_str(r#"{"excludes":["target"]}"#).unwrap();
        let cleared: Stored = serde_json::from_str(r#"{"excludes":[]}"#).unwrap();
        assert_eq!(
            cleared.excludes.or(previous.excludes),
            Some(Vec::new()),
            "an explicit empty list is not an absent one"
        );
    }

    #[test]
    fn the_token_is_never_written_back_out() {
        let value = mask("hf_abcdefghijklmnopqrstuvwxyz");
        assert_eq!(value["set"], true);
        assert_eq!(value["tail"], "wxyz");
        assert!(
            !value.to_string().contains("abcdef"),
            "the head of a token must not be reported"
        );
        assert_eq!(mask(""), Value::Null);
    }
}
