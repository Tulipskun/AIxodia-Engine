//! The two operations.
//!
//! [`backup`] walks the tree, reuses whatever the bucket already holds, and
//! replaces the bucket's one manifest with the result. [`restore`] reads that
//! manifest and rebuilds the tree under a root, in parallel, verifying each
//! file's sha256 as it lands. A file that never arrives is named in the report
//! and the rest of the tree is still written: one bad object must not abandon
//! the four hundred good ones behind it.
//!
//! Both block, on purpose: nothing here is `async`, so a caller cannot hold a
//! tokio worker. Start them with `std::thread::spawn` and read progress from a
//! [`Reporter`].

use std::collections::{HashMap, HashSet, VecDeque};
use std::fs;
use std::os::unix::fs::{symlink, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::{
    atomic::{AtomicUsize, Ordering},
    Arc, Mutex,
};
use std::time::{Duration, UNIX_EPOCH};

use crate::scan::{self, Scanned};
use crate::xet::{self, Job, Source};
use crate::{
    Config, Counts, Entry, Error, Hub, Kind, Manifest, Phase, Reporter, Resolver, Result,
    RESTORE_ATTEMPTS, RESTORE_CONCURRENCY,
};
/// A file at or above this size is fetched as several range requests.
///
/// The bucket serves roughly a fifth of a megabyte per second to one connection
/// and is generous with many, so the split point is where a single stream stops
/// being the fastest answer: a ninety-megabyte binary left whole is a
/// seven-minute tail on a restore that has otherwise finished.
const RANGE_MIN_SIZE: u64 = 8 * 1024 * 1024;
/// The bytes each range request asks for.
const RANGE_TARGET: u64 = 4 * 1024 * 1024;
/// The most a single file is cut into, so one huge object cannot eat the run.
const RANGE_MAX_PIECES: usize = 12;
/// A file slower than this gets a line of its own, because it is the only way a
/// restore that took minutes can say afterwards where they went.
const SLOW_FILE_MS: u64 = 2_000;

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
    /// What the run could not write, each one named with its reason: a path it
    /// refused, a directory it could not create, a file whose bytes never
    /// arrived. Everything else in the manifest was still written, so this is
    /// the whole distance the tree is short, not a note that the run stopped.
    pub failed: Vec<String>,
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

    // A manifest naming an object the bucket does not hold is how a restore
    // comes back with holes in the tree, so the bucket is asked whether it holds
    // every key before the manifest is published. Objects are never deleted, so
    // a healthy bucket answers this in one request and nothing else happens.
    let mut wanted: Vec<String> = plan
        .entries
        .iter()
        .filter_map(|e| e.object_key(&cfg.prefix))
        .collect();
    wanted.sort();
    wanted.dedup();
    let present: HashSet<String> = hub.existing_keys(&wanted)?.into_iter().collect();
    let mut absent: Vec<String> = wanted
        .into_iter()
        .filter(|key| !present.contains(key))
        .collect();
    if !absent.is_empty() {
        // Trusting the previous manifest's key list was what let a hole through:
        // an object the last run believed was there is re-sent here, once, and
        // then believed only if the bucket says so.
        tracing::warn!(
            "backup: {} object(s) the bucket did not hold: {}",
            absent.len(),
            absent[..absent.len().min(5)].join(", ")
        );
        reporter.set(
            Phase::Uploading,
            format!(
                "{} object(s) were missing from the bucket; sending them",
                absent.len()
            ),
        );
        uploaded_bytes += upload_objects(&hub, cfg, reporter, &absent, &by_key)?;
        let now: HashSet<String> = hub.existing_keys(&absent)?.into_iter().collect();
        absent.retain(|key| !now.contains(key));
        if !absent.is_empty() {
            // A committed object can be slow to show up, so one more look is
            // cheaper than failing a run over a lag.
            std::thread::sleep(Duration::from_secs(1));
            let late: HashSet<String> = hub.existing_keys(&absent)?.into_iter().collect();
            absent.retain(|key| !late.contains(key));
        }
        if !absent.is_empty() {
            // Refuse to publish. `latest.json` still names the manifest that was
            // good before this run, so the next restore gets a tree that is
            // whole; a half-true manifest is the thing that broke the machine.
            return Err(Error::Api {
                operation: "backup".into(),
                message: format!(
                    "{} object(s) are still not in the bucket after upload, so this manifest was not published: {}",
                    absent.len(),
                    absent[..absent.len().min(5)].join(", ")
                ),
            });
        }
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
/// they are, so a restore onto a live tree only ever adds and overwrites. The
/// run always goes through the whole manifest and reports what it could not
/// write, so a partial tree is never the surprise ending of a failed call.
pub fn restore(cfg: &Config, root: &Path, reporter: &Reporter) -> Result<RestoreReport> {
    let started = std::time::Instant::now();
    cfg.validate()?;
    let hub = Hub::new(cfg)?;
    let reading = std::time::Instant::now();
    let manifest = latest_manifest(&hub)?
        .ok_or_else(|| Error::Config("the bucket holds no backup yet".into()))?;
    tracing::info!(
        "restore: manifest {} holds {} entries, read in {}ms",
        manifest.name,
        manifest.entries.len(),
        elapsed_ms(reading),
    );
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

    let mut problems: Vec<String> = Vec::new();
    let mut dirs: Vec<&Entry> = Vec::new();
    let mut files: Vec<(Entry, PathBuf)> = Vec::new();
    let mut links: Vec<(Entry, PathBuf)> = Vec::new();
    for entry in &manifest.entries {
        // An entry whose path cannot be honoured is that entry's own problem.
        // The rest of the manifest is still worth restoring.
        let path = match crate::join_under(root, &entry.path) {
            Ok(path) => path,
            Err(err) => {
                problems.push(format!("{}: {err}", entry.path));
                continue;
            }
        };
        match entry.kind {
            Kind::Dir => dirs.push(entry),
            Kind::File => files.push((entry.clone(), path)),
            Kind::Symlink => links.push((entry.clone(), path)),
        }
    }
    // Shallowest first, so a parent directory always exists before its
    // children are written into it.
    dirs.sort_by_key(|e| depth(&e.path));
    let mut dirs_out = 0usize;
    for entry in dirs.clone() {
        let Ok(path) = crate::join_under(root, &entry.path) else {
            continue;
        };
        match fs::create_dir_all(&path) {
            Ok(()) => {
                let _ =
                    fs::set_permissions(&path, fs::Permissions::from_mode(entry.mode.max(0o700)));
                dirs_out += 1;
            }
            Err(source) => problems.push(format!("{}: create directory: {source}", entry.path)),
        }
        reporter.tick();
    }

    links.sort_by_key(|(_, path)| path.components().count());
    let mut links_out = 0usize;
    for (entry, path) in &links {
        let want = entry.target.as_deref().unwrap_or_default();
        if fs::symlink_metadata(path).is_ok() {
            if read_link_text(path).as_deref() == Some(want) {
                links_out += 1;
                reporter.tick();
                continue;
            }
            if let Err(source) = fs::remove_file(path) {
                problems.push(format!("{}: replace what is there: {source}", entry.path));
                reporter.tick();
                continue;
            }
        }
        if let Some(parent) = path.parent() {
            if let Err(source) = fs::create_dir_all(parent) {
                problems.push(format!("{}: {}: {source}", entry.path, parent.display()));
                reporter.tick();
                continue;
            }
        }
        match symlink(want, path) {
            Ok(()) => links_out += 1,
            Err(source) => problems.push(format!("{}: link to {want}: {source}", entry.path)),
        }
        reporter.tick();
    }

    let wanted_files = files.len();
    let counters = Arc::new(Mutex::new((0usize, 0u64, 0usize)));
    let errors: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(problems));
    let slowest: Arc<Mutex<(u64, String)>> = Arc::new(Mutex::new((0, String::new())));

    // The unit of work is a range request, not a file. The bucket answers a
    // download at a fraction of a megabyte per second per connection, so one
    // ninety-megabyte binary is a seven-minute tail on a restore that is
    // otherwise finished — and a single request cannot use more than one
    // connection. A large file is therefore cut into pieces that land in the
    // same temporary sibling at their own offsets, and any worker may take any
    // piece of any file.
    let mut slots: Vec<Slot> = Vec::new();
    let mut queue: VecDeque<Piece> = VecDeque::new();
    for (entry, path) in files {
        let label = entry.path.clone();
        let want = entry.sha256.clone().unwrap_or_default();
        let Some(key) = entry.object_key(&cfg.prefix) else {
            note(&errors, format!("{label}: no object key"));
            reporter.tick();
            continue;
        };
        if let Ok(found) = fs::symlink_metadata(&path) {
            if found.is_file() && found.len() == entry.size {
                // A restore is the only writer of a restored file's stamp, so an
                // mtime that matches is enough to know the bytes are current.
                let fresh = entry.mtime > 0 && found.mtime() == entry.mtime;
                let same = !want.is_empty() && scan::hash_file(&path).is_ok_and(|got| got == want);
                if fresh || same {
                    if let Err(source) = fs::set_permissions(
                        &path,
                        fs::Permissions::from_mode(entry.mode.max(0o600)),
                    ) {
                        note(&errors, format!("{label}: set mode: {source}"));
                    } else {
                        set_mtime(&path, entry.mtime);
                        counters.lock().unwrap_or_else(|e| e.into_inner()).2 += 1;
                    }
                    reporter.tick();
                    continue;
                }
            } else if !found.is_file() {
                // Something else occupies the path: a symlink, or a directory
                // where a file belongs. Refusing to replace a directory is how a
                // restore dies at the moment the machine needs it most.
                if found.is_dir() {
                    tracing::warn!(
                        "restore: {label} is a directory in a file's place; replacing it"
                    );
                }
                let removing = if found.is_dir() {
                    fs::remove_dir_all(&path)
                } else {
                    fs::remove_file(&path)
                };
                if let Err(source) = removing {
                    note(&errors, format!("{label}: replace what is there: {source}"));
                    reporter.tick();
                    continue;
                }
            }
        }
        if let Some(parent) = path.parent() {
            if let Err(source) = fs::create_dir_all(parent) {
                note(&errors, format!("{label}: {}: {source}", parent.display()));
                reporter.tick();
                continue;
            }
        }
        if entry.size == 0 {
            // Nothing to fetch: an empty file is entirely its own content.
            if let Err(source) = fs::File::create(&path) {
                note(&errors, format!("{label}: create: {source}"));
                reporter.tick();
                continue;
            }
            let _ = fs::set_permissions(&path, fs::Permissions::from_mode(entry.mode.max(0o600)));
            set_mtime(&path, entry.mtime);
            counters.lock().unwrap_or_else(|e| e.into_inner()).0 += 1;
            reporter.tick();
            continue;
        }
        let temp = crate::hub::temp_path(&path);
        if let Err(source) = sized_temp(&temp, entry.size) {
            note(&errors, format!("{label}: {}: {source}", temp.display()));
            reporter.tick();
            continue;
        }
        let ranges = ranges_for(entry.size);
        let index = slots.len();
        slots.push(Slot {
            index,
            label,
            key,
            path,
            temp,
            want,
            size: entry.size,
            mode: entry.mode,
            mtime: entry.mtime,
            outstanding: AtomicUsize::new(ranges.len()),
            failed: Mutex::new(None),
            verifies: AtomicUsize::new(0),
            started: std::time::Instant::now(),
        });
        queue.extend(ranges.into_iter().map(|(first, last)| Piece {
            slot: index,
            first,
            last,
            attempts: 0,
        }));
    }
    let slots = Arc::new(slots);
    let queue = Arc::new(Mutex::new(queue));
    // One window on the bucket per worker. A `reqwest` blocking client drives
    // every request it is handed from a single internal thread, so workers that
    // share one client queue behind each other — and behind whatever one of them
    // happens to be stuck on.
    let downloaders = (0..RESTORE_CONCURRENCY)
        .map(|_| hub.downloader())
        .collect::<Result<Vec<_>>>()?;
    let transferring = std::time::Instant::now();
    std::thread::scope(|scope| {
        for downloader in &downloaders {
            let (slots, queue, counters, errors, slowest, reporter) = (
                &slots,
                &queue,
                &counters,
                &errors,
                &slowest,
                reporter.clone(),
            );
            scope.spawn(move || loop {
                let next = queue.lock().unwrap_or_else(|e| e.into_inner()).pop_front();
                let Some(piece) = next else { break };
                let slot = &slots[piece.slot];
                if let Some(again) = run_piece(downloader, slot, piece) {
                    queue
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .push_back(again);
                    continue;
                }
                // Only the worker that takes the last piece of a file finalises
                // it, so a file is hashed and renamed exactly once however many
                // hands carried its bytes.
                if slot.outstanding.fetch_sub(1, Ordering::AcqRel) == 1 {
                    finish_slot(slot, queue, counters, errors, slowest, &reporter);
                }
            });
        }
    });
    let problems = errors.lock().unwrap_or_else(|e| e.into_inner()).clone();
    let (files_out, bytes, unchanged) = *counters.lock().unwrap_or_else(|e| e.into_inner());
    let (slow_ms, slow_path) = slowest.lock().unwrap_or_else(|e| e.into_inner()).clone();
    tracing::info!(
        "restore: {} of {wanted_files} file(s) written in {}ms, {unchanged} already current, {} failed",
        files_out,
        elapsed_ms(transferring),
        problems.len(),
    );
    if !slow_path.is_empty() {
        tracing::warn!("restore: slowest file {slow_path} took {slow_ms}ms");
    }

    if !problems.is_empty() {
        // Whatever the run could write, it wrote, and then it says which files
        // it could not. Refusing the whole run over one of them is what left a
        // tree half-restored with no record of which half.
        reporter.set(
            Phase::Failed,
            format!(
                "{files_out} file(s) written, {} could not be",
                problems.len()
            ),
        );
    } else {
        reporter.set(
            Phase::Done,
            format!("{files_out} file(s) restored under {}", root.display()),
        );
    }
    Ok(RestoreReport {
        name: manifest.name.clone(),
        root: root.display().to_string(),
        files: files_out,
        dirs: dirs_out,
        symlinks: links_out,
        bytes,
        unchanged,
        failed: problems,
        duration_ms: elapsed_ms(started),
    })
}

/// A file waiting for its bytes, and how much of them is still on the wire.
///
/// Pieces of one file are written into `temp`, the sibling the manifest's own
/// name is renamed over at the end, so no partial file is ever the one a user
/// sees at the path.
struct Slot {
    index: usize,
    /// The manifest path, for the report.
    label: String,
    key: String,
    path: PathBuf,
    temp: PathBuf,
    want: String,
    size: u64,
    mode: u32,
    mtime: i64,
    /// Pieces that have not settled yet. Reaching zero is what finalises the
    /// file, and only the worker that takes it to zero runs that.
    outstanding: AtomicUsize,
    /// The first error any piece of this file met. Later ones are the same
    /// broken object seen again, and one line names it.
    failed: Mutex<Option<String>>,
    /// Whole-file refetches already spent on a body that did not hash right.
    verifies: AtomicUsize,
    started: std::time::Instant,
}

impl Slot {
    fn fail(&self, reason: &str) {
        let mut failed = self.failed.lock().unwrap_or_else(|e| e.into_inner());
        if failed.is_none() {
            *failed = Some(reason.to_string());
        }
    }
}

/// One range request for one slot: the inclusive byte span, and how many times
/// it has been asked for.
struct Piece {
    slot: usize,
    first: u64,
    last: u64,
    attempts: usize,
}

/// The byte ranges a file of `size` is fetched in. Below the split point the
/// answer is one span covering the object, which is asked for with no `Range`
/// header at all — a plain GET is what every server answers correctly and what
/// the bucket serves without the range bookkeeping.
fn ranges_for(size: u64) -> Vec<(u64, u64)> {
    if size < RANGE_MIN_SIZE {
        return vec![(0, size - 1)];
    }
    let pieces = (size.div_ceil(RANGE_TARGET) as usize).min(RANGE_MAX_PIECES);
    let step = size.div_ceil(pieces as u64);
    let mut out = Vec::with_capacity(pieces);
    let mut first = 0u64;
    while first < size {
        let last = (first + step - 1).min(size - 1);
        out.push((first, last));
        first = last + 1;
    }
    out
}

/// The sibling every piece of one file lands in, already the length the
/// manifest names so a piece can seek and write without anyone owning the end
/// of the file.
fn sized_temp(temp: &Path, size: u64) -> std::io::Result<()> {
    let file = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .open(temp)?;
    file.set_len(size)?;
    Ok(())
}

fn note(errors: &Mutex<Vec<String>>, message: String) {
    errors
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .push(message);
}

fn report_speed(slowest: &Mutex<(u64, String)>, label: &str, took_ms: u64) {
    if took_ms < SLOW_FILE_MS {
        return;
    }
    let mut top = slowest.lock().unwrap_or_else(|e| e.into_inner());
    if took_ms > top.0 {
        *top = (took_ms, label.to_string());
    }
}

/// Ask a bucket for one piece. Returns the piece to queue again when this
/// attempt should be repeated, and nothing once the piece has settled —
/// successfully or into the slot's recorded failure.
///
/// Verification is not here: it needs the whole file, so it happens in
/// [`finish_slot`], and a file that comes back wrong is fetched again from
/// nothing. A CDN can hand a stale or truncated body for a good key, which is
/// why an answer that will not change (a missing or refused object) is never
/// asked for again, and why four attempts on one file cost under two seconds.
fn run_piece(downloader: &Resolver, slot: &Slot, piece: Piece) -> Option<Piece> {
    let whole = piece.first == 0 && piece.last + 1 == slot.size;
    let range = (!whole).then_some((piece.first, piece.last));
    let expected = piece.last - piece.first + 1;
    match downloader.download_piece(&slot.key, range, expected, &slot.temp) {
        Ok(_) => None,
        Err(err) => {
            if err.is_permanent() || piece.attempts + 1 >= RESTORE_ATTEMPTS {
                slot.fail(&err.to_string());
                return None;
            }
            let redo = if matches!(err, Error::Api { .. }) && !whole {
                // The bucket answered a range request with the whole object,
                // which cannot be written at an offset. One stream can be.
                Piece {
                    slot: piece.slot,
                    first: 0,
                    last: slot.size - 1,
                    attempts: piece.attempts + 1,
                }
            } else {
                Piece {
                    attempts: piece.attempts + 1,
                    ..piece
                }
            };
            tracing::warn!(
                "restore: {} bytes {}-{} attempt {} failed ({err}); retrying",
                slot.label,
                piece.first,
                piece.last,
                piece.attempts + 1,
            );
            std::thread::sleep(retry_delay(piece.attempts + 1));
            Some(redo)
        }
    }
}

/// The last piece of one file has landed: check the bytes, then put the file in
/// place with its mode and its stamp. Anything that goes wrong names the file
/// and removes the half of it, because a restore that leaves a wrong file at the
/// path is worse than one that leaves the path empty.
fn finish_slot(
    slot: &Slot,
    queue: &Mutex<VecDeque<Piece>>,
    counters: &Mutex<(usize, u64, usize)>,
    errors: &Mutex<Vec<String>>,
    slowest: &Mutex<(u64, String)>,
    reporter: &Reporter,
) {
    let took = elapsed_ms(slot.started);
    if let Some(reason) = slot.failed.lock().unwrap_or_else(|e| e.into_inner()).take() {
        let _ = fs::remove_file(&slot.temp);
        note(errors, format!("{}: {reason}", slot.label));
        report_speed(slowest, &slot.label, took);
        reporter.tick();
        return;
    }
    let wrong = if slot.want.is_empty() {
        None
    } else {
        match scan::hash_file(&slot.temp) {
            Ok(got) if got == slot.want => None,
            Ok(got) => Some(format!(
                "content is not what the manifest names (want {}, got {got})",
                slot.want
            )),
            Err(err) => Some(err.to_string()),
        }
    };
    if let Some(problem) = wrong {
        if slot.verifies.fetch_add(1, Ordering::AcqRel) < RESTORE_ATTEMPTS - 1 {
            // Start the file over from nothing, so a second stale body cannot be
            // read as part of the first one.
            tracing::warn!(
                "restore: {} came back wrong ({problem}); fetching it again",
                slot.label
            );
            let _ = fs::remove_file(&slot.temp);
            if sized_temp(&slot.temp, slot.size).is_ok() {
                slot.outstanding.store(1, Ordering::Release);
                queue
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .push_back(Piece {
                        slot: slot.index,
                        first: 0,
                        last: slot.size - 1,
                        attempts: 0,
                    });
                return;
            }
        }
        let _ = fs::remove_file(&slot.temp);
        note(errors, format!("{}: {problem}", slot.label));
        report_speed(slowest, &slot.label, took);
        reporter.tick();
        return;
    }
    if let Err(source) =
        fs::set_permissions(&slot.temp, fs::Permissions::from_mode(slot.mode.max(0o600)))
    {
        let _ = fs::remove_file(&slot.temp);
        note(errors, format!("{}: set mode: {source}", slot.label));
        report_speed(slowest, &slot.label, took);
        reporter.tick();
        return;
    }
    set_mtime(&slot.temp, slot.mtime);
    if let Err(source) = fs::rename(&slot.temp, &slot.path) {
        let _ = fs::remove_file(&slot.temp);
        note(errors, format!("{}: put in place: {source}", slot.label));
        report_speed(slowest, &slot.label, took);
        reporter.tick();
        return;
    }
    let mut c = counters.lock().unwrap_or_else(|e| e.into_inner());
    c.0 += 1;
    c.1 += slot.size;
    drop(c);
    report_speed(slowest, &slot.label, took);
    reporter.tick();
}

/// 200ms, 400ms, 800ms: long enough for a CDN hop to come back, short enough
/// that four attempts on one file cost under two seconds rather than minutes.
fn retry_delay(attempt: usize) -> Duration {
    Duration::from_millis(200 * 2u64.pow(attempt.saturating_sub(1) as u32).min(8))
}

fn read_link_text(path: &Path) -> Option<String> {
    fs::read_link(path)
        .ok()
        .map(|t| t.to_string_lossy().to_string())
}

/// Send the content `keys` name and register it under them, reading each file
/// out of the tree again. This is the repair path for an object the bucket was
/// believed to hold and did not.
fn upload_objects(
    hub: &Hub,
    cfg: &Config,
    reporter: &Reporter,
    keys: &[String],
    by_key: &HashMap<String, &Entry>,
) -> Result<u64> {
    let mut jobs = Vec::with_capacity(keys.len());
    for key in keys {
        let entry = *by_key.get(key.as_str()).ok_or_else(|| {
            Error::Manifest(key.clone(), "no entry of this manifest names it".into())
        })?;
        jobs.push(Job {
            key: key.clone(),
            source: Source::File(crate::join_under(&cfg.root, &entry.path)?),
            sha256: entry.sha256.clone(),
            size: entry.size,
        });
    }
    let done = xet::ingest(cfg, jobs, reporter.clone())?;
    let now = epoch_ms();
    let items: Vec<(String, String, i64, Option<&str>)> = done
        .iter()
        .map(|d| (d.key.clone(), d.xet_hash.clone(), now, None))
        .collect();
    hub.add_files(&items)?;
    Ok(done.iter().map(|d| d.size).sum())
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpListener;

    fn sha(bytes: &[u8]) -> String {
        use sha2::{Digest, Sha256};
        hex::encode(Sha256::digest(bytes))
    }

    enum Route {
        Bytes(Vec<u8>),
        /// An error the first time, the bytes after: a CDN hop that dropped.
        Flaky(Vec<u8>),
        Gone,
        /// Bytes that answer a `Range` header the way a CDN does: the slice, and
        /// a 206 saying so.
        Ranged(Vec<u8>),
        /// Bytes that answer a `Range` header with the whole object, because the
        /// server never implemented it.
        Rangeless(Vec<u8>),
    }

    /// A bucket that answers from a fixed table on a loopback port, counting
    /// how many times each path was asked for. The count is the point: a
    /// restore's cost is the number of requests it makes per file.
    fn fake_bucket(routes: HashMap<String, Route>) -> (String, Arc<Mutex<HashMap<String, usize>>>) {
        let listener = TcpListener::bind("127.0.0.1:0").expect("a free port");
        let addr = listener.local_addr().expect("the address it took");
        let routes = Arc::new(Mutex::new(routes));
        let hits: Arc<Mutex<HashMap<String, usize>>> = Arc::new(Mutex::new(HashMap::new()));
        let (server_routes, server_hits) = (routes.clone(), hits.clone());
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(mut stream) = stream else { break };
                let (routes, hits) = (server_routes.clone(), server_hits.clone());
                std::thread::spawn(move || {
                    let mut buffer = [0u8; 4096];
                    let read = match stream.read(&mut buffer) {
                        Ok(0) | Err(_) => return,
                        Ok(read) => read,
                    };
                    let request = String::from_utf8_lossy(&buffer[..read]).to_string();
                    let Some(path) = request.split_whitespace().nth(1) else {
                        return;
                    };
                    let path = path.to_string();
                    let range: Option<(usize, usize)> = request.lines().find_map(|line| {
                        let spec = line.trim().strip_prefix("Range: bytes=")?;
                        let (first, last) = spec.split_once('-')?;
                        Some((first.parse().ok()?, last.parse().ok()?))
                    });
                    let found = {
                        let routes = routes.lock().unwrap_or_else(|e| e.into_inner());
                        routes.get(&path).map(|route| match route {
                            Route::Bytes(b) => (200, "OK", b.clone()),
                            Route::Flaky(b) => (500, "Internal Server Error", b.clone()),
                            Route::Gone => (404, "Not Found", Vec::new()),
                            Route::Ranged(b) => match range {
                                Some((first, last)) => (
                                    206,
                                    "Partial Content",
                                    b[first..last.min(b.len() - 1) + 1].to_vec(),
                                ),
                                None => (200, "OK", b.clone()),
                            },
                            Route::Rangeless(b) => (200, "OK", b.clone()),
                        })
                    };
                    let Some((status, reason, body)) = found else {
                        let _ = stream.write_all(
                            b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                        );
                        return;
                    };
                    let seen = {
                        let mut hits = hits.lock().unwrap_or_else(|e| e.into_inner());
                        *hits.entry(path.clone()).or_insert(0) += 1;
                        hits[&path]
                    };
                    let (status, reason, body) = if status == 500 && seen > 1 {
                        // A `Flaky` route drops the first attempt and answers the
                        // second, which is how a retry is proved without a network
                        // to break.
                        let routes = routes.lock().unwrap_or_else(|e| e.into_inner());
                        match routes.get(&path) {
                            Some(Route::Flaky(b)) => (200, "OK", b.clone()),
                            _ => (status, reason, body),
                        }
                    } else {
                        (status, reason, body)
                    };
                    let head = format!(
                        "HTTP/1.1 {status} {reason}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        body.len()
                    );
                    let _ = stream.write_all(head.as_bytes());
                    let _ = stream.write_all(&body);
                });
            }
        });
        (format!("http://{addr}"), hits)
    }

    struct Tree {
        cfg: Config,
        dir: std::path::PathBuf,
        urls: HashMap<String, String>,
        hits: Arc<Mutex<HashMap<String, usize>>>,
    }

    /// A manifest of four files, of which the fake bucket serves three: one
    /// only after a retry, and one that is not there at all.
    fn tree(name: &str) -> Tree {
        tree_with(name, false)
    }

    /// `escape` adds an entry naming a path outside the root, which the run
    /// must refuse on its own and not for the whole manifest.
    fn tree_with(name: &str, escape: bool) -> Tree {
        let dir = std::env::temp_dir().join(format!("aix-restore-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).expect("a restore root");
        let prefix = "aixodia/root";
        let files = [
            ("a.txt", &b"first"[..]),
            ("sub/b.txt", &b"second"[..]),
            ("flaky.txt", &b"retried"[..]),
            ("gone.txt", &b"never served"[..]),
        ];
        let mut entries = Vec::new();
        let mut routes: HashMap<String, Route> = HashMap::new();
        let mut urls = HashMap::new();
        for (path, bytes) in files {
            let sha = sha(bytes);
            entries.push(Entry {
                path: path.to_string(),
                kind: Kind::File,
                size: bytes.len() as u64,
                mode: 0o644,
                mtime: 0,
                target: None,
                sha256: Some(sha.clone()),
            });
            let url = format!(
                "/buckets/u/b/resolve/{prefix}/objects/{}/{}",
                &sha[..2],
                sha
            );
            routes.insert(
                url.clone(),
                if path.starts_with("gone") {
                    Route::Gone
                } else if path.starts_with("flaky") {
                    Route::Flaky(bytes.to_vec())
                } else {
                    Route::Bytes(bytes.to_vec())
                },
            );
            urls.insert(path.to_string(), url);
        }
        if escape {
            entries.push(Entry {
                path: "../evil.txt".into(),
                kind: Kind::File,
                size: 1,
                mode: 0o644,
                mtime: 0,
                target: None,
                sha256: None,
            });
        }
        let manifest = Manifest {
            version: 1,
            name: "test".into(),
            created_at: 0,
            host: "test".into(),
            root: dir.display().to_string(),
            counts: Counts::default(),
            uploaded_bytes: 0,
            skipped: vec![],
            entries,
        };
        routes.insert(
            format!("/buckets/u/b/resolve/{prefix}/latest.json"),
            Route::Bytes(b"{\"name\":\"test\"}".to_vec()),
        );
        routes.insert(
            format!("/buckets/u/b/resolve/{prefix}/manifests/test.json"),
            Route::Bytes(serde_json::to_vec(&manifest).expect("a manifest")),
        );
        let (endpoint, hits) = fake_bucket(routes);
        Tree {
            cfg: Config {
                token: "t".into(),
                bucket: "u/b".into(),
                endpoint,
                root: dir.clone(),
                prefix: prefix.into(),
                excludes: vec![],
                ..Default::default()
            },
            dir,
            urls,
            hits,
        }
    }

    fn hit(hits: &Arc<Mutex<HashMap<String, usize>>>, url: &str) -> usize {
        *hits
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(url)
            .unwrap_or(&0)
    }

    /// The whole point of the failure rules: a file the bucket does not have
    /// must not cost the other three their place on disk, must not be asked for
    /// four times, and must be named afterwards.
    #[test]
    fn a_missing_object_ends_the_run_with_the_rest_of_the_tree_written() {
        let tree = tree("partial");
        let reporter = Reporter::new("restore");
        let report = restore(&tree.cfg, &tree.dir, &reporter).expect("the run goes on");

        assert_eq!(report.files, 3, "every object the hub served landed");
        assert_eq!(report.failed.len(), 1, "one file is named as missing");
        assert!(
            report.failed[0].contains("gone.txt") && report.failed[0].contains("404"),
            "the failure says which file and why: {}",
            report.failed[0]
        );
        assert_eq!(fs::read(tree.dir.join("a.txt")).unwrap(), b"first");
        assert_eq!(fs::read(tree.dir.join("sub/b.txt")).unwrap(), b"second");
        assert_eq!(
            fs::read(tree.dir.join("flaky.txt")).unwrap(),
            b"retried",
            "a dropped stream is asked for again"
        );
        assert_eq!(
            hit(&tree.hits, &tree.urls["gone.txt"]),
            1,
            "an object that is not there is not chased"
        );
        assert_eq!(hit(&tree.hits, &tree.urls["flaky.txt"]), 2);
        assert_eq!(reporter.snapshot().phase, Phase::Failed);
    }

    /// A second restore onto the tree it just wrote is the ordinary case — the
    /// machine came back and only some of it was wrong — so it must ask for
    /// nothing it already holds.
    #[test]
    fn a_repeat_restore_asks_for_nothing_it_already_holds() {
        let tree = tree("repeat");
        let reporter = Reporter::new("restore");
        restore(&tree.cfg, &tree.dir, &reporter).expect("the first run");
        let before: HashMap<String, usize> = tree
            .urls
            .iter()
            .map(|(path, url)| (path.clone(), hit(&tree.hits, url)))
            .collect();

        let report = restore(&tree.cfg, &tree.dir, &reporter).expect("the second run");

        assert_eq!(report.unchanged, 3, "the tree is already current");
        assert_eq!(report.bytes, 0, "and it cost no bytes to learn that");
        for (path, url) in &tree.urls {
            if path == "gone.txt" {
                continue;
            }
            assert_eq!(
                hit(&tree.hits, url),
                before[path],
                "{path} was not fetched again"
            );
        }
    }

    /// A manifest naming a path outside the root is refused entry by entry, not
    /// run by run: the other files are still what the machine needs.
    #[test]
    fn one_refused_path_does_not_stop_the_rest() {
        let tree = tree_with("escape", true);
        let report = restore(&tree.cfg, &tree.dir, &Reporter::new("restore")).expect("the run");
        assert_eq!(report.files, 3, "the writable entries were still written");
        assert!(
            report.failed.iter().any(|f| f.contains("evil.txt")),
            "and the refused one is named: {:?}",
            report.failed
        );
    }

    #[test]
    fn a_retry_waits_a_short_and_bounded_time() {
        assert_eq!(retry_delay(1), Duration::from_millis(200));
        assert_eq!(retry_delay(2), Duration::from_millis(400));
        assert_eq!(retry_delay(4), Duration::from_millis(1600));
        assert_eq!(
            retry_delay(40),
            Duration::from_millis(1600),
            "it stops growing"
        );
    }

    /// A 404 will be a 404 when asked again, and a dead stream might not be.
    #[test]
    fn a_refusal_is_not_asked_for_again_but_a_drop_is() {
        let gone = Error::Hub {
            operation: "download".into(),
            key: "k".into(),
            status: 404,
            body: String::new(),
        };
        assert!(gone.is_permanent());
        let rate_limited = Error::Hub {
            operation: "download".into(),
            key: "k".into(),
            status: 429,
            body: String::new(),
        };
        assert!(
            !rate_limited.is_permanent(),
            "a 429 is a moment, not a fact"
        );
        assert!(!Error::Verify {
            path: "p".into(),
            want: "a".into(),
            got: "b".into(),
        }
        .is_permanent());
    }

    /// A manifest of one file big enough to be cut into range requests, and the
    /// bytes the fake bucket holds for it. `honours_range` is the difference
    /// between a CDN and a server that never implemented `Range`.
    fn big_tree(name: &str, honours_range: bool) -> (Tree, Vec<u8>) {
        let dir = std::env::temp_dir().join(format!("aix-range-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).expect("a restore root");
        let prefix = "aixodia/root";
        let mut body = Vec::new();
        while (body.len() as u64) < RANGE_MIN_SIZE + 1024 * 1024 {
            body.extend((0..65_536usize).map(|i| (i % 251) as u8));
        }
        let sha = sha(&body);
        let entries = vec![Entry {
            path: "big.bin".into(),
            kind: Kind::File,
            size: body.len() as u64,
            mode: 0o644,
            mtime: 0,
            target: None,
            sha256: Some(sha.clone()),
        }];
        let url = format!(
            "/buckets/u/b/resolve/{prefix}/objects/{}/{}",
            &sha[..2],
            sha
        );
        let mut routes: HashMap<String, Route> = HashMap::new();
        routes.insert(
            url.clone(),
            if honours_range {
                Route::Ranged(body.clone())
            } else {
                Route::Rangeless(body.clone())
            },
        );
        routes.insert(
            format!("/buckets/u/b/resolve/{prefix}/latest.json"),
            Route::Bytes(b"{\"name\":\"test\"}".to_vec()),
        );
        let manifest = Manifest {
            version: 1,
            name: "test".into(),
            created_at: 0,
            host: "test".into(),
            root: dir.display().to_string(),
            counts: Counts::default(),
            uploaded_bytes: 0,
            skipped: vec![],
            entries,
        };
        routes.insert(
            format!("/buckets/u/b/resolve/{prefix}/manifests/test.json"),
            Route::Bytes(serde_json::to_vec(&manifest).expect("a manifest")),
        );
        let (endpoint, hits) = fake_bucket(routes);
        let mut urls = HashMap::new();
        urls.insert("big.bin".to_string(), url);
        let tree = Tree {
            cfg: Config {
                token: "t".into(),
                bucket: "u/b".into(),
                endpoint,
                root: dir.clone(),
                prefix: prefix.into(),
                excludes: vec![],
                ..Default::default()
            },
            dir,
            urls,
            hits,
        };
        (tree, body)
    }

    /// A twenty-minute restore of a tree full of toolchain binaries is not slow
    /// because of its thirty thousand small files: it is the largest file,
    /// carried by one connection at a fifth of a megabyte a second, that decides
    /// when the run ends. So a big object arrives as several ranges at once.
    #[test]
    fn a_large_file_is_carried_by_several_ranges() {
        let (tree, body) = big_tree("split", true);
        let reporter = Reporter::new("restore");
        let report = restore(&tree.cfg, &tree.dir, &reporter).expect("the run finishes");

        assert!(
            report.failed.is_empty(),
            "one file, served: {:?}",
            report.failed
        );
        assert_eq!(report.files, 1);
        assert_eq!(report.bytes, body.len() as u64);
        assert_eq!(fs::read(tree.dir.join("big.bin")).expect("the file"), body);
        assert!(
            hit(&tree.hits, &tree.urls["big.bin"]) >= 3,
            "a nine-megabyte object is fetched in several ranges, not one stream"
        );
    }

    /// A server that answers a range request with the whole object cannot have
    /// its body written at an offset. Refusing it is only right if the file still
    /// comes across, which is the point of the fallback in `run_piece`.
    #[test]
    fn a_bucket_that_ignores_ranges_still_delivers_the_file() {
        let (tree, body) = big_tree("ignorant", false);
        let reporter = Reporter::new("restore");
        let report = restore(&tree.cfg, &tree.dir, &reporter).expect("the run finishes");

        assert!(report.failed.is_empty(), "{:?}", report.failed);
        assert_eq!(fs::read(tree.dir.join("big.bin")).expect("the file"), body);
    }
}
