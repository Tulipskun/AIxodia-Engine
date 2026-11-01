//! The two operations.
//!
//! [`backup`] walks the tree, reuses whatever the bucket already holds, and
//! replaces the bucket's one manifest with the result. [`restore`] reads that
//! manifest and rebuilds the tree under a root, in parallel, verifying every
//! file's sha256 afterwards and retrying a flaky download before failing.
//!
//! Both block, on purpose: nothing here is `async`, so a caller cannot hold a
//! tokio worker. Start them with `std::thread::spawn` and read progress from a
//! [`Reporter`].

use std::collections::{HashMap, HashSet, VecDeque};
use std::fs;
use std::os::unix::fs::{symlink, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, UNIX_EPOCH};

use crate::scan::{self, Scanned};
use crate::xet::{self, Job, Source};
use crate::{
    Config, Counts, Entry, Error, Hub, Kind, Manifest, Phase, Reporter, Result, RESTORE_ATTEMPTS,
    RESTORE_CONCURRENCY,
};

/// What a backup run did. Every byte count is post-exclusion.
#[derive(Clone, Debug, serde::Serialize)]
pub struct BackupReport {
    pub name: String,
    pub manifest_key: String,
    pub bucket: String,
    pub root: String,
    pub files: usize,
    pub dirs: usize,
    pub symlinks: usize,
    pub bytes: u64,
    /// Bytes this run put on the wire. Everything else was either reused
    /// unread from the previous manifest or already in the bucket.
    pub uploaded_bytes: u64,
    pub files_reused: usize,
    pub objects_reused: usize,
    pub skipped: Vec<String>,
    /// The manifest this run replaced. There is never more than one stored.
    pub deleted_manifests: usize,
    pub duration_ms: u64,
}

/// What a restore run wrote. A file whose content already matched the manifest
/// was not downloaded and counts in `unchanged`.
#[derive(Clone, Debug, serde::Serialize)]
pub struct RestoreReport {
    pub name: String,
    pub root: String,
    pub files: usize,
    pub dirs: usize,
    pub symlinks: usize,
    pub bytes: u64,
    pub unchanged: usize,
    pub duration_ms: u64,
}

/// A content object that still has to be ingested.
struct Pending {
    key: String,
    path: PathBuf,
    sha256: Option<String>,
    size: u64,
}

pub fn backup(cfg: &Config, reporter: &Reporter) -> Result<BackupReport> {
    let started = std::time::Instant::now();
    cfg.validate()?;
    // A backup reads the tree. Walking a root that is not there would write an
    // empty manifest and then pruning would treat the real tree's objects as
    // unreferenced, so it is refused here rather than at the HTTP layer.
    if !cfg.root.is_dir() {
        return Err(Error::Config(format!(
            "nothing to back up: {} is not a directory",
            cfg.root.display()
        )));
    }
    let hub = Hub::new(cfg)?;

    reporter.set(Phase::Scanning, "reading the previous manifest");
    let previous = latest_manifest(&hub)?;
    let mut known: HashSet<String> = HashSet::new();
    match previous.as_ref() {
        Some(manifest) => known.extend(manifest.absolute(&cfg.prefix)),
        None => reporter.set(Phase::Scanning, "no previous backup: this one is full"),
    }

    let mut skipped = Vec::new();
    let scanned: Vec<Scanned> = scan::scan(&cfg.root, &cfg.excludes, &mut skipped)?;
    reporter.set(
        Phase::Scanning,
        format!("{} entries under {}", scanned.len(), cfg.root.display()),
    );

    let mut unreadable = Vec::new();
    let plan = scan::plan(
        &cfg.root,
        scanned,
        previous.as_ref(),
        &known,
        &cfg.prefix,
        &mut unreadable,
    )?;
    skipped.extend(unreadable);
    reporter.set(
        Phase::Scanning,
        format!(
            "{} of {} file(s) must be read, {} reused unread",
            plan.uploads.len(),
            plan.entries.iter().filter(|e| e.kind == Kind::File).count(),
            plan.reused
        ),
    );

    // Which bytes belong to which object key, so an upload can be described by
    // the entry that needs it.
    let mut by_key: HashMap<String, &Entry> = HashMap::new();
    for entry in &plan.entries {
        if let Some(key) = entry.object_key(&cfg.prefix) {
            by_key.entry(key).or_insert(entry);
        }
    }
    let mut pending: Vec<Pending> = Vec::new();
    let mut objects_reused = 0usize;
    {
        let mut seen: HashSet<String> = HashSet::new();
        for (path, key) in &plan.uploads {
            if !seen.insert(key.clone()) {
                // Two new files, one content: the second reuses the first.
                objects_reused += 1;
                continue;
            }
            let entry = by_key.get(key.as_str()).copied();
            pending.push(Pending {
                key: key.clone(),
                path: path.clone(),
                sha256: entry.and_then(|e| e.sha256.clone()),
                size: entry.map(|e| e.size).unwrap_or(0),
            });
        }
    }
    // Objects are never deleted, so a file the previous manifest already
    // named is in the bucket, and only the remaining keys are worth asking
    // about.
    let keys: Vec<String> = pending.iter().map(|p| p.key.clone()).collect();
    if !keys.is_empty() {
        reporter.set(
            Phase::Uploading,
            format!("asking the bucket about {} object(s)", keys.len()),
        );
        let existing: HashSet<String> = hub.existing_keys(&keys)?.into_iter().collect();
        objects_reused += existing.len();
        pending.retain(|p| !existing.contains(&p.key));
    }

    let mut uploaded_bytes = 0u64;
    if pending.is_empty() {
        reporter.set(Phase::Uploading, "nothing new to upload");
    } else {
        reporter.set(
            Phase::Uploading,
            format!(
                "{} object(s), {} byte(s)",
                pending.len(),
                pending.iter().map(|p| p.size).sum::<u64>()
            ),
        );
        let jobs: Vec<Job> = pending
            .into_iter()
            .map(|p| Job {
                key: p.key,
                source: Source::File(p.path),
                sha256: p.sha256,
                size: p.size,
            })
            .collect();
        let done = xet::ingest(cfg, jobs, reporter.clone())?;
        let now = epoch_ms();
        let items: Vec<(String, String, i64, Option<&str>)> = done
            .iter()
            .map(|d| (d.key.clone(), d.xet_hash.clone(), now, None))
            .collect();
        hub.add_files(&items)?;
        uploaded_bytes = done.iter().map(|d| d.size).sum();
    }

    let counts = count(&plan.entries);
    let name = crate::stamp_utc();
    let manifest = Manifest {
        version: 1,
        name: name.clone(),
        created_at: crate::now_unix(),
        host: crate::hostname(),
        root: cfg.root.display().to_string(),
        counts,
        uploaded_bytes,
        skipped: skipped.clone(),
        entries: plan.entries,
    };
    let manifest_key = hub.manifest_key(&name);
    let pointer = serde_json::json!({
        "name": manifest.name,
        "created_at": manifest.created_at,
        "manifest": manifest_key,
    });

    reporter.set(Phase::Writing, format!("writing manifest {name}"));
    // One batch, manifest first: until `latest.json` names it, readers still
    // see the manifest that was good before this run.
    write_json(
        &hub,
        cfg,
        &[
            (
                manifest_key.as_str(),
                serde_json::to_vec_pretty(&manifest)?,
                Some("application/json"),
            ),
            (
                hub.latest_key().as_str(),
                serde_json::to_vec_pretty(&pointer)?,
                Some("application/json"),
            ),
        ],
    )?;

    // The bucket holds one manifest: this run's. Older ones are replaced,
    // while objects stay so the next run can reuse them unread.
    let deleted_manifests = {
        reporter.set(Phase::Pruning, "replacing the previous manifest");
        let stale: Vec<String> = hub
            .list_files(&hub.manifests_prefix())?
            .into_iter()
            .map(|item| item.path)
            .filter(|path| path != &manifest_key)
            .collect();
        if !stale.is_empty() {
            hub.delete_keys(&stale)?;
        }
        stale.len()
    };

    reporter.set(
        Phase::Done,
        format!(
            "{} file(s), {} byte(s) uploaded, {} reused",
            manifest.counts.files,
            uploaded_bytes,
            plan.reused + objects_reused
        ),
    );
    Ok(BackupReport {
        name,
        manifest_key,
        bucket: hub.bucket().to_string(),
        root: manifest.root.clone(),
        files: manifest.counts.files,
        dirs: manifest.counts.dirs,
        symlinks: manifest.counts.symlinks,
        bytes: manifest.counts.bytes,
        uploaded_bytes,
        files_reused: plan.reused,
        objects_reused,
        skipped,
        deleted_manifests,
        duration_ms: elapsed_ms(started),
    })
}

/// The manifest `latest.json` points at, or `None` when there is no backup yet.
pub fn latest_manifest(hub: &Hub) -> Result<Option<Manifest>> {
    let Some(bytes) = hub.get_bytes(&hub.latest_key())? else {
        return Ok(None);
    };
    let pointer: serde_json::Value = serde_json::from_slice(&bytes)
        .map_err(|e| Error::Manifest("latest.json".into(), e.to_string()))?;
    let name = pointer["name"]
        .as_str()
        .ok_or_else(|| Error::Manifest("latest.json".into(), "no name field".into()))?;
    fetch_manifest(hub, name).map(Some)
}

/// One manifest by name (`20261101T053000Z`).
pub fn fetch_manifest(hub: &Hub, name: &str) -> Result<Manifest> {
    let key = hub.manifest_key(name);
    let bytes = hub.get_bytes(&key)?.ok_or_else(|| {
        Error::Manifest(
            name.to_string(),
            "the bucket has no manifest with that name".into(),
        )
    })?;
    serde_json::from_slice(&bytes).map_err(|e| Error::Manifest(name.to_string(), e.to_string()))
}

/// Rebuild the bucket's manifest under `root`.
///
/// Nothing is deleted: files the manifest does not name are left exactly as
/// they are, so a restore onto a live tree only ever adds and overwrites.
pub fn restore(cfg: &Config, root: &Path, reporter: &Reporter) -> Result<RestoreReport> {
    let started = std::time::Instant::now();
    cfg.validate()?;
    let hub = Hub::new(cfg)?;
    let manifest = latest_manifest(&hub)?
        .ok_or_else(|| Error::Config("the bucket holds no backup yet".into()))?;
    fs::create_dir_all(root).map_err(|source| Error::Io {
        path: root.display().to_string(),
        source,
    })?;

    reporter.set(
        Phase::Writing,
        format!("restoring {} into {}", manifest.name, root.display()),
    );
    reporter.set_totals(
        manifest.entries.len(),
        manifest
            .entries
            .iter()
            .filter(|e| e.kind == Kind::File)
            .map(|e| e.size)
            .sum(),
    );

    let mut dirs: Vec<&Entry> = Vec::new();
    let mut files: Vec<(Entry, PathBuf)> = Vec::new();
    let mut links: Vec<(Entry, PathBuf)> = Vec::new();
    for entry in &manifest.entries {
        let path = crate::join_under(root, &entry.path)?;
        match entry.kind {
            Kind::Dir => dirs.push(entry),
            Kind::File => files.push((entry.clone(), path)),
            Kind::Symlink => links.push((entry.clone(), path)),
        }
    }
    // Shallowest first, so a parent directory always exists before its
    // children are written into it.
    dirs.sort_by_key(|e| depth(&e.path));
    for entry in dirs.clone() {
        let path = crate::join_under(root, &entry.path)?;
        fs::create_dir_all(&path).map_err(|source| Error::Io {
            path: path.display().to_string(),
            source,
        })?;
        let _ = fs::set_permissions(&path, fs::Permissions::from_mode(entry.mode.max(0o700)));
        reporter.tick();
    }

    links.sort_by_key(|(_, path)| path.components().count());
    for (entry, path) in &links {
        let want = entry.target.as_deref().unwrap_or_default();
        if fs::symlink_metadata(path).is_ok() {
            if read_link_text(path).as_deref() == Some(want) {
                reporter.tick();
                continue;
            }
            fs::remove_file(path).map_err(|source| Error::Io {
                path: path.display().to_string(),
                source,
            })?;
        }
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).map_err(|source| Error::Io {
                path: parent.display().to_string(),
                source,
            })?;
        }
        symlink(want, path).map_err(|source| Error::Io {
            path: path.display().to_string(),
            source,
        })?;
        reporter.tick();
    }

    let queue = Arc::new(Mutex::new(VecDeque::from(files)));
    let counters = Arc::new(Mutex::new((0usize, 0u64, 0usize)));
    let errors: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    std::thread::scope(|scope| {
        for _ in 0..RESTORE_CONCURRENCY {
            let (hub, queue, counters, errors, reporter) =
                (&hub, &queue, &counters, &errors, reporter.clone());
            scope.spawn(move || loop {
                let next = queue.lock().unwrap_or_else(|e| e.into_inner()).pop_front();
                let Some((entry, path)) = next else { break };
                match restore_file(hub, cfg, &entry, &path) {
                    Ok((bytes, unchanged)) => {
                        let mut c = counters.lock().unwrap_or_else(|e| e.into_inner());
                        c.0 += 1;
                        c.1 += bytes;
                        if unchanged {
                            c.2 += 1;
                        }
                    }
                    Err(err) => errors
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .push(format!("{}: {err}", entry.path)),
                }
                reporter.tick();
            });
        }
    });
    let problems = errors.lock().unwrap_or_else(|e| e.into_inner()).clone();
    if !problems.is_empty() {
        reporter.set(Phase::Failed, format!("{} file(s) failed", problems.len()));
        return Err(Error::Api {
            operation: "restore".into(),
            message: problems[..problems.len().min(5)].join("; "),
        });
    }
    let (files_out, bytes, unchanged) = *counters.lock().unwrap_or_else(|e| e.into_inner());

    reporter.set(
        Phase::Done,
        format!("{files_out} file(s) restored under {}", root.display()),
    );
    Ok(RestoreReport {
        name: manifest.name.clone(),
        root: root.display().to_string(),
        files: files_out,
        dirs: dirs.len(),
        symlinks: links.len(),
        bytes,
        unchanged,
        duration_ms: elapsed_ms(started),
    })
}

/// Download, verify, then set mode and times. A file that already holds the
/// manifest's content is left alone — that is the whole cost of a repeat
/// restore. Its mtime matching is enough to know it does: a restore is the
/// only writer of that stamp, so an untouched file keeps it.
fn restore_file(hub: &Hub, cfg: &Config, entry: &Entry, path: &Path) -> Result<(u64, bool)> {
    let key = entry
        .object_key(&cfg.prefix)
        .ok_or_else(|| Error::Manifest(entry.path.clone(), "no object key".into()))?;
    let want = entry.sha256.clone().unwrap_or_default();
    let meta = fs::symlink_metadata(path);
    if let Ok(meta) = meta.as_ref() {
        if meta.is_file() && meta.len() == entry.size {
            let fresh = entry.mtime > 0 && meta.mtime() == entry.mtime;
            if fresh || (!want.is_empty() && scan::hash_file(path)? == want) {
                set_mtime(path, entry.mtime);
                let _ =
                    fs::set_permissions(path, fs::Permissions::from_mode(entry.mode.max(0o600)));
                return Ok((0, true));
            }
        }
    }
    if meta.is_ok() && !meta.as_ref().map(|m| m.is_file()).unwrap_or(false) {
        // Something else occupies the path: a symlink, or a directory where a
        // file belongs. A half-restored or hand-made tree can present either,
        // and refusing to replace a directory is how a restore dies at the
        // exact moment the machine needs it most.
        let removed = if meta.as_ref().map(|m| m.is_dir()).unwrap_or(false) {
            fs::remove_dir_all(path)
        } else {
            fs::remove_file(path)
        };
        removed.map_err(|source| Error::Io {
            path: path.display().to_string(),
            source,
        })?;
    }
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(|source| Error::Io {
            path: parent.display().to_string(),
            source,
        })?;
    }
    let written = download_with_retry(hub, &key, path)?;
    if !want.is_empty() {
        let got = scan::hash_file(path)?;
        if got != want {
            return Err(Error::Verify {
                path: path.display().to_string(),
                want,
                got,
            });
        }
    }
    fs::set_permissions(path, fs::Permissions::from_mode(entry.mode.max(0o600))).map_err(
        |source| Error::Io {
            path: path.display().to_string(),
            source,
        },
    )?;
    set_mtime(path, entry.mtime);
    Ok((written, false))
}

/// A CDN hop that drops is normal on a flaky uplink, so one failed request is
/// retried with a short backoff before it is called a restore failure.
fn download_with_retry(hub: &Hub, key: &str, path: &Path) -> Result<u64> {
    let mut attempts = 0usize;
    loop {
        attempts += 1;
        match hub.download_to(key, path) {
            Ok(written) => return Ok(written),
            Err(err) if attempts < RESTORE_ATTEMPTS => {
                tracing::warn!("restore: {key} attempt {attempts} failed ({err}); retrying");
                std::thread::sleep(Duration::from_millis(
                    250 * 2u64.pow(attempts as u32 - 1) * 2,
                ));
            }
            Err(err) => return Err(err),
        }
    }
}

fn read_link_text(path: &Path) -> Option<String> {
    fs::read_link(path)
        .ok()
        .map(|t| t.to_string_lossy().to_string())
}

/// Ingest then register small JSON objects, in the order given.
fn write_json(hub: &Hub, cfg: &Config, items: &[(&str, Vec<u8>, Option<&str>)]) -> Result<()> {
    let mut jobs = Vec::new();
    let mut order: Vec<(&str, Option<&str>)> = Vec::new();
    for (key, bytes, content_type) in items {
        let size = bytes.len() as u64;
        jobs.push(Job {
            key: key.to_string(),
            source: Source::Bytes(bytes.clone()),
            sha256: None,
            size,
        });
        order.push((*key, *content_type));
    }
    let done = xet::ingest(cfg, jobs, Reporter::default())?;
    let hashes: HashMap<String, String> = done.into_iter().map(|d| (d.key, d.xet_hash)).collect();
    let now = epoch_ms();
    let mut registered = Vec::new();
    for (key, content_type) in &order {
        let hash = hashes
            .get(*key)
            .ok_or_else(|| Error::Xet(format!("{key}: the bucket did not acknowledge it")))?;
        registered.push((key.to_string(), hash.clone(), now, *content_type));
    }
    hub.add_files(&registered)
}

fn count(entries: &[Entry]) -> Counts {
    let mut counts = Counts::default();
    for entry in entries {
        match entry.kind {
            Kind::File => {
                counts.files += 1;
                counts.bytes += entry.size;
            }
            Kind::Dir => counts.dirs += 1,
            Kind::Symlink => counts.symlinks += 1,
        }
    }
    counts
}

fn depth(path: &str) -> usize {
    path.matches('/').count()
}

fn set_mtime(path: &Path, mtime: i64) {
    if mtime <= 0 {
        return;
    }
    let when = UNIX_EPOCH + Duration::from_secs(mtime as u64);
    if let Ok(file) = fs::OpenOptions::new().write(true).open(path) {
        let _ = file.set_times(fs::FileTimes::new().set_modified(when));
    }
}

fn epoch_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or_default()
}

fn elapsed_ms(started: std::time::Instant) -> u64 {
    started.elapsed().as_millis() as u64
}
