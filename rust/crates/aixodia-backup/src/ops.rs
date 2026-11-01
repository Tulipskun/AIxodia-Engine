//! The two operations, plus listing.
//!
//! [`backup`] walks the tree, reuses whatever the bucket already holds, and
//! writes one manifest per run. [`restore`] reads a manifest and rebuilds the
//! tree under a root, in parallel, verifying every file's sha256 afterwards.
//!
//! Both block, on purpose: nothing here is `async`, so a caller cannot hold a
//! tokio worker. Start them with `std::thread::spawn` and read progress from a
//! [`Reporter`].

use std::collections::{HashMap, HashSet, VecDeque};
use std::fs;
use std::os::unix::fs::{symlink, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, UNIX_EPOCH};

use crate::scan::{self, Scanned};
use crate::xet::{self, Job, Source};
use crate::{
    Config, Counts, Entry, Error, Hub, Kind, Manifest, ManifestHead, Phase, Reporter, Result,
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
    pub deleted_manifests: usize,
    pub deleted_objects: usize,
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
    // A file the previous manifest already named is in the bucket unless a
    // prune removed it, so only the remaining keys are worth asking about.
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

    let mut deleted_manifests = 0usize;
    let mut deleted_objects = 0usize;
    if cfg.retention > 0 || cfg.gc {
        reporter.set(Phase::Pruning, "pruning");
        let (manifests, objects) = prune(&hub, cfg)?;
        deleted_manifests = manifests;
        deleted_objects = objects;
    }

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
        deleted_objects,
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

/// Stored backups, newest first.
pub fn list(cfg: &Config) -> Result<Vec<ManifestHead>> {
    cfg.validate()?;
    let hub = Hub::new(cfg)?;
    let mut names: Vec<String> = hub
        .list_files(&hub.manifests_prefix())?
        .into_iter()
        .filter_map(|item| {
            item.path
                .rsplit('/')
                .next()
                .and_then(|tail| tail.strip_suffix(".json"))
                .map(str::to_string)
        })
        .collect();
    names.sort();
    names.reverse();

    // A head costs the manifest it names, so fetch them together.
    let queue = Arc::new(Mutex::new(VecDeque::from(names)));
    let heads: Arc<Mutex<Vec<ManifestHead>>> = Arc::new(Mutex::new(Vec::new()));
    let errors: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    std::thread::scope(|scope| {
        for _ in 0..4 {
            let (hub, queue, heads, errors) = (&hub, &queue, &heads, &errors);
            scope.spawn(move || loop {
                let next = queue.lock().unwrap_or_else(|e| e.into_inner()).pop_front();
                let Some(name) = next else { break };
                match fetch_manifest(hub, &name) {
                    Ok(manifest) => heads
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .push(head_of(&name, &manifest)),
                    Err(err) => errors
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .push(format!("{name}: {err}")),
                }
            });
        }
    });
    fail_if_any("list manifests", &errors)?;
    let mut out = heads.lock().unwrap_or_else(|e| e.into_inner()).clone();
    out.sort_by(|a, b| b.name.cmp(&a.name));
    Ok(out)
}

/// Rebuild `manifest` (or the newest one) under `root`.
///
/// Nothing is deleted: files the manifest does not name are left exactly as
/// they are, so a restore onto a live tree only ever adds and overwrites.
pub fn restore(
    cfg: &Config,
    name: Option<&str>,
    root: &Path,
    reporter: &Reporter,
) -> Result<RestoreReport> {
    let started = std::time::Instant::now();
    cfg.validate()?;
    let hub = Hub::new(cfg)?;
    let manifest = match name {
        Some(name) => fetch_manifest(&hub, name)?,
        None => latest_manifest(&hub)?
            .ok_or_else(|| Error::Config("the bucket holds no backup yet".into()))?,
    };
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
/// restore.
fn restore_file(hub: &Hub, cfg: &Config, entry: &Entry, path: &Path) -> Result<(u64, bool)> {
    let key = entry
        .object_key(&cfg.prefix)
        .ok_or_else(|| Error::Manifest(entry.path.clone(), "no object key".into()))?;
    let want = entry.sha256.clone().unwrap_or_default();
    let meta = fs::symlink_metadata(path);
    let already_regular = meta.as_ref().map(|m| m.is_file()).unwrap_or(false);
    if already_regular
        && meta.as_ref().map(|m| m.len()).unwrap_or(0) == entry.size
        && scan::hash_file(path)? == want
    {
        set_mtime(path, entry.mtime);
        let _ = fs::set_permissions(path, fs::Permissions::from_mode(entry.mode.max(0o600)));
        return Ok((0, true));
    }
    if meta.is_ok() && !already_regular {
        // Something else occupies the path: a symlink, or a directory where a
        // file belongs.
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
    let written = hub.download_to(&key, path)?;
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

/// Drop manifests past `retention`, and with `gc` on, every object no retained
/// manifest references.
fn prune(hub: &Hub, cfg: &Config) -> Result<(usize, usize)> {
    let mut manifests: Vec<String> = hub
        .list_files(&hub.manifests_prefix())?
        .into_iter()
        .map(|i| i.path)
        .collect();
    manifests.sort();
    manifests.reverse();
    // With garbage collection off, nothing is kept by count alone; the newest
    // manifest is always the one to reference.
    let keep = if cfg.retention > 0 { cfg.retention } else { 1 };
    let mut doomed: Vec<String> = Vec::new();
    if cfg.retention > 0 && manifests.len() > cfg.retention {
        doomed = manifests[cfg.retention..].to_vec();
    }
    if !doomed.is_empty() {
        hub.delete_keys(&doomed)?;
    }
    let mut deleted_objects = 0usize;
    if cfg.gc {
        let retained = &manifests[..manifests.len().min(keep)];
        let mut referenced: HashSet<String> = HashSet::new();
        for key in retained {
            let Some(name) = short_name(key) else {
                continue;
            };
            let manifest = fetch_manifest(hub, name)?;
            referenced.extend(manifest.absolute(&cfg.prefix));
        }
        let orphans: Vec<String> = hub
            .list_files(&hub.objects_prefix())?
            .into_iter()
            .map(|i| i.path)
            .filter(|path| !referenced.contains(path))
            .collect();
        deleted_objects = orphans.len();
        if !orphans.is_empty() {
            hub.delete_keys(&orphans)?;
        }
    }
    Ok((doomed.len(), deleted_objects))
}

/// `aixodia/root/manifests/20261101T053000Z.json` -> `20261101T053000Z`.
fn short_name(key: &str) -> Option<&str> {
    key.rsplit('/')
        .next()
        .and_then(|tail| tail.strip_suffix(".json"))
        .filter(|name| !name.is_empty())
}

fn head_of(name: &str, manifest: &Manifest) -> ManifestHead {
    ManifestHead {
        name: name.to_string(),
        created_at: manifest.created_at,
        files: manifest.counts.files,
        bytes: manifest.counts.bytes,
        uploaded_bytes: manifest.uploaded_bytes,
        root: manifest.root.clone(),
        host: manifest.host.clone(),
    }
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

fn fail_if_any(operation: &str, errors: &Arc<Mutex<Vec<String>>>) -> Result<()> {
    let problems = errors.lock().unwrap_or_else(|e| e.into_inner()).clone();
    if problems.is_empty() {
        return Ok(());
    }
    Err(Error::Api {
        operation: operation.into(),
        message: problems.join("; "),
    })
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
