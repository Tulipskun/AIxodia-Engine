//! aixodia-backup — the engine's own backup of a filesystem tree onto a
//! Hugging Face storage bucket.
//!
//! The layout is content addressed:
//!
//! ```text
//! <prefix>/objects/<aa>/<sha256>      one object per distinct file content
//! <prefix>/manifests/<utc>.json       one manifest per backup run
//! <prefix>/latest.json                pointer at the newest manifest
//! ```
//!
//! Two properties make this fast. A file whose size and mtime still match the
//! previous manifest is never read or hashed, so an unchanged tree costs one
//! `stat` per file and no bytes on the wire. And because an object key is the
//! file's sha256, content that already exists in the bucket needs no upload at
//! all — the new manifest simply points at the key that is already there. What
//! does move goes through Xet, which dedups at chunk level, so a file that
//! only grew uploads the growth.
//!
//! Nothing here is `async`, so a caller cannot hold a tokio worker: start
//! [`backup`] and [`restore`] with `std::thread::spawn` and read progress from
//! the [`Reporter`] they share. Within them, files are copied, hashed and
//! transferred concurrently with plain threads.

mod hub;
pub mod ops;
pub mod scan;
mod xet;

pub use hub::{FileItem, Hub};
pub use ops::{
    backup, fetch_manifest, latest_manifest, list, restore, BackupReport, RestoreReport,
};
pub use scan::{plan, DEFAULT_EXCLUDES};

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// The bucket's public hub endpoint. The S3 gateway is not usable with a plain
/// hub token — it needs separate `HFAK` credentials — so all transfers go
/// through the hub API.
pub const DEFAULT_ENDPOINT: &str = "https://huggingface.co";
pub const DEFAULT_PREFIX: &str = "aixodia/root";
pub const DEFAULT_RETENTION: usize = 14;

/// Concurrency for restore. Measured on this host: one stream from the bucket
/// saturates the link, but a tree of small files is round-trip bound, so the
/// win comes from having several requests in flight, not from splitting one
/// large file.
pub const RESTORE_CONCURRENCY: usize = 12;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("backup: {0}")]
    Config(String),
    #[error("backup: {operation} on {key} failed: http {status}: {body}")]
    Hub {
        operation: String,
        key: String,
        status: u16,
        body: String,
    },
    #[error("backup: {operation} failed: {message}")]
    Api { operation: String, message: String },
    #[error("backup: read {path} for upload: {source}")]
    Io {
        path: String,
        #[source]
        source: std::io::Error,
    },
    #[error("backup: xet upload failed: {0}")]
    Xet(String),
    #[error("backup: {path}: checksum mismatch after restore (wanted {want}, got {got})")]
    Verify {
        path: String,
        want: String,
        got: String,
    },
    #[error("backup: manifest {0} is malformed: {1}")]
    Manifest(String, String),
    #[error("backup: refusing to write {0}: it escapes the restore root")]
    Escape(String),
    #[error(transparent)]
    Http(#[from] reqwest::Error),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
pub struct Config {
    /// The hub token used for both the bucket API and the Xet write token.
    pub token: String,
    /// `namespace/name`, as the bucket API spells it.
    pub bucket: String,
    pub endpoint: String,
    /// The directory that is backed up. Stored paths are relative to it.
    pub root: PathBuf,
    /// Key prefix the backup owns inside the bucket.
    pub prefix: String,
    /// Exclusion patterns; see [`scan`].
    pub excludes: Vec<String>,
    /// Manifests kept before the oldest is pruned.
    pub retention: usize,
    /// Delete objects no retained manifest still references. Off by default:
    /// the bucket bills what it holds, but a pruned object cannot come back.
    pub gc: bool,
    /// Extra seconds added to every request timeout, for a slow uplink.
    pub timeout_secs: u64,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            token: String::new(),
            bucket: String::new(),
            endpoint: DEFAULT_ENDPOINT.to_string(),
            root: PathBuf::from("/root"),
            prefix: DEFAULT_PREFIX.to_string(),
            excludes: DEFAULT_EXCLUDES.iter().map(|s| s.to_string()).collect(),
            retention: DEFAULT_RETENTION,
            gc: false,
            timeout_secs: 120,
        }
    }
}

impl Config {
    pub fn validate(&self) -> Result<()> {
        if self.token.trim().is_empty() {
            return Err(Error::Config("no hub token is set".into()));
        }
        if self.bucket.trim().is_empty() {
            return Err(Error::Config("no bucket is set".into()));
        }
        if !self.bucket.contains('/') {
            return Err(Error::Config(format!(
                "bucket {:?} must be namespace/name",
                self.bucket
            )));
        }
        if self.prefix.starts_with('/') || self.prefix.contains("//") {
            return Err(Error::Config(format!("bad key prefix {:?}", self.prefix)));
        }
        if !self.root.is_dir() {
            return Err(Error::Config(format!(
                "{} is not a directory",
                self.root.display()
            )));
        }
        Ok(())
    }

    pub fn object_key(&self, sha256: &str) -> String {
        format!("{}/objects/{}/{}", self.prefix, &sha256[..2], sha256)
    }

    pub fn manifest_key(&self, name: &str) -> String {
        format!("{}/manifests/{}.json", self.prefix, name)
    }

    pub fn latest_key(&self) -> String {
        format!("{}/latest.json", self.prefix)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    File,
    Dir,
    Symlink,
}

/// One filesystem entry. `path` is relative to the backup root with `/`
/// separators and never starts with `/`, so a manifest cannot name a target
/// outside the root it was taken from.
#[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
pub struct Entry {
    pub path: String,
    pub kind: Kind,
    #[serde(default)]
    pub size: u64,
    /// Permission bits only; ownership is not portable and is not restored.
    #[serde(default)]
    pub mode: u32,
    #[serde(default)]
    pub mtime: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sha256: Option<String>,
}

impl Entry {
    pub fn object_key(&self, prefix: &str) -> Option<String> {
        let sha = self.sha256.as_deref()?;
        Some(format!("{prefix}/objects/{}/{}", &sha[..2], sha))
    }
}

#[derive(Clone, Debug, Default, serde::Serialize, serde::Deserialize)]
pub struct Counts {
    pub files: usize,
    pub dirs: usize,
    pub symlinks: usize,
    pub bytes: u64,
}

#[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
pub struct Manifest {
    pub version: u32,
    /// `20261101T053000Z`; also the manifest's name in the bucket.
    pub name: String,
    pub created_at: i64,
    pub host: String,
    pub root: String,
    #[serde(default)]
    pub counts: Counts,
    /// Bytes actually put on the wire by this run; everything else was reused.
    #[serde(default)]
    pub uploaded_bytes: u64,
    /// Paths the scan could not read, kept so the next run reports them again.
    #[serde(default)]
    pub skipped: Vec<String>,
    pub entries: Vec<Entry>,
}

impl Manifest {
    /// The object keys this manifest needs, for garbage collection.
    pub fn object_keys(&self) -> Vec<String> {
        let mut keys: Vec<String> = self
            .entries
            .iter()
            .filter_map(|e| e.sha256.clone())
            .map(|s| format!("objects/{}/{}", &s[..2], s))
            .collect();
        keys.sort();
        keys.dedup();
        keys
    }

    pub fn absolute(&self, key_prefix: &str) -> Vec<String> {
        self.object_keys()
            .into_iter()
            .map(|k| format!("{key_prefix}/{k}"))
            .collect()
    }
}

/// A short record of a stored manifest, for listing without downloading them.
#[derive(Clone, Debug, serde::Serialize)]
pub struct ManifestHead {
    pub name: String,
    pub created_at: i64,
    pub files: usize,
    pub bytes: u64,
    pub uploaded_bytes: u64,
    pub root: String,
    pub host: String,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Phase {
    #[default]
    Idle,
    Scanning,
    Uploading,
    Writing,
    Verifying,
    Pruning,
    Done,
    Failed,
}

impl Phase {
    pub fn as_str(self) -> &'static str {
        match self {
            Phase::Idle => "idle",
            Phase::Scanning => "scanning",
            Phase::Uploading => "uploading",
            Phase::Writing => "writing",
            Phase::Verifying => "verifying",
            Phase::Pruning => "pruning",
            Phase::Done => "done",
            Phase::Failed => "failed",
        }
    }
}

#[derive(Clone, Debug, Default, serde::Serialize)]
pub struct Progress {
    pub phase: Phase,
    pub operation: String,
    pub message: String,
    pub done: usize,
    pub total: usize,
    pub bytes_done: u64,
    pub bytes_total: u64,
    pub started_at: i64,
    pub updated_at: i64,
}

/// Progress a worker publishes and the HTTP layer reads. Cloning shares one
/// slot, so the handler can watch while the worker runs.
#[derive(Clone, Default)]
pub struct Reporter(Arc<Mutex<Progress>>);

impl Reporter {
    pub fn new(operation: &str) -> Self {
        let now = now_unix();
        Self(Arc::new(Mutex::new(Progress {
            phase: Phase::Scanning,
            operation: operation.to_string(),
            started_at: now,
            updated_at: now,
            ..Default::default()
        })))
    }

    pub fn snapshot(&self) -> Progress {
        self.0.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }

    pub fn set(&self, phase: Phase, message: impl Into<String>) {
        let mut p = self.0.lock().unwrap_or_else(|e| e.into_inner());
        let message = message.into();
        tracing::info!("backup {}: {} — {}", p.operation, phase.as_str(), message);
        p.phase = phase;
        p.message = message;
        p.updated_at = now_unix();
    }

    pub fn set_totals(&self, total: usize, bytes_total: u64) {
        let mut p = self.0.lock().unwrap_or_else(|e| e.into_inner());
        p.total = total;
        p.bytes_total = bytes_total;
        p.updated_at = now_unix();
    }

    pub fn advance(&self, files: usize, bytes: u64) {
        let mut p = self.0.lock().unwrap_or_else(|e| e.into_inner());
        p.done += files;
        p.bytes_done += bytes;
        p.updated_at = now_unix();
    }

    pub fn tick(&self) {
        self.advance(1, 0);
    }
}

pub fn now_unix() -> i64 {
    chrono::Utc::now().timestamp()
}

/// `20261101T053000Z` — sortable, and safe as a key segment.
pub fn stamp_utc() -> String {
    chrono::Utc::now().format("%Y%m%dT%H%M%SZ").to_string()
}

pub fn hostname() -> String {
    std::fs::read_to_string("/proc/sys/kernel/hostname")
        .map(|s| s.trim().to_string())
        .unwrap_or_else(|_| "unknown".to_string())
}

/// Percent-encode a bucket key for a URL path, keeping `/` literal.
pub(crate) fn encode_key(key: &str) -> String {
    let mut out = String::with_capacity(key.len());
    for byte in key.as_bytes() {
        let c = *byte as char;
        if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '~' | '-' | '/') {
            out.push(c);
        } else {
            out.push_str(&format!("%{byte:02X}"));
        }
    }
    out
}

/// Path safety for restore: a manifest entry must stay under the root it is
/// written to.
pub(crate) fn join_under(root: &Path, rel: &str) -> Result<PathBuf> {
    if rel.starts_with('/') || rel.contains("//") || rel.contains('\\') || rel.contains('\0') {
        return Err(Error::Escape(rel.to_string()));
    }
    for part in rel.split('/') {
        if part == ".." || part.is_empty() {
            return Err(Error::Escape(rel.to_string()));
        }
    }
    Ok(root.join(rel))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn join_under_refuses_escapes() {
        let root = Path::new("/tmp/x");
        assert!(join_under(root, "a/b").unwrap().starts_with(root));
        for bad in ["/abs", "a//b", "../out", "a/../../out", "a\\b", "a/./b/.."] {
            assert!(join_under(root, bad).is_err(), "{bad} should be refused");
        }
    }

    #[test]
    fn keys_are_content_addressed() {
        let cfg = Config {
            token: "t".into(),
            bucket: "u/b".into(),
            ..Default::default()
        };
        let sha = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899";
        assert_eq!(
            cfg.object_key(sha),
            format!("aixodia/root/objects/aa/{sha}")
        );
        assert_eq!(
            cfg.manifest_key("20261101T053000Z"),
            "aixodia/root/manifests/20261101T053000Z.json"
        );
    }

    #[test]
    fn config_needs_token_and_bucket() {
        let cfg = Config::default();
        assert!(cfg.validate().is_err());
        let cfg = Config {
            token: "hf_x".into(),
            bucket: "no-slash".into(),
            ..Default::default()
        };
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn object_keys_dedup_within_a_manifest() {
        let entry = |sha: &str| Entry {
            path: "f".into(),
            kind: Kind::File,
            size: 1,
            mode: 0o644,
            mtime: 0,
            target: None,
            sha256: Some(sha.into()),
        };
        let manifest = Manifest {
            version: 1,
            name: "n".into(),
            created_at: 0,
            host: "h".into(),
            root: "/root".into(),
            counts: Counts::default(),
            uploaded_bytes: 0,
            skipped: vec![],
            entries: vec![
                entry("1111111111111111111111111111111111111111111111111111111111111111"),
                entry("1111111111111111111111111111111111111111111111111111111111111111"),
                entry("2222222222222222222222222222222222222222222222222222222222222222"),
            ],
        };
        assert_eq!(manifest.object_keys().len(), 2);
    }
}
