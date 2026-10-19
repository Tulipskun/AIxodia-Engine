#!/usr/bin/env python3
"""Environment snapshot for a Kaggle notebook session.

The machine is ephemeral: /root, /opt, /etc, /var and /usr are wiped when the
session ends, so the engine keeps them in a private Hugging Face dataset and
restores them at the start of the next session.

    python3 kgat_sync.py restore    # download the snapshot and unpack it (notebook start)
    python3 kgat_sync.py backup     # snapshot this machine (engine, before a restart)
    python3 kgat_sync.py restart    # push a fresh version of the notebook

The notebook only calls `restore`, starts the engine and waits. The engine owns
the session: it decides when the session is over, calls `backup`, calls
`restart`, and exits.
"""

import gzip
import heapq
import json
import os
import queue
import random
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path


REPOS = ("kyomu53n/kgat-snapshot-a", "kyomu53n/kgat-snapshot-b")


def _secret(name, *envs):
    """A credential from the environment, else from ~/.aixodia/<name>.

    Secrets are never in this file: the notebook that starts the engine holds
    them, exports them to the restore process and leaves them in ~/.aixodia so
    a backup started by the engine (which inherits only the engine's own
    environment) can read them.
    """
    for key in envs:
        v = os.environ.get(key, "").strip()
        if v:
            return v
    try:
        return (Path.home() / ".aixodia" / name).read_text().strip()
    except OSError:
        return ""


HF_TOKEN = _secret("hf_token", "HF_TOKEN", "AIXODIA_HF_TOKEN")
ARCHIVE = "env_backup.part"
MANIFEST = "manifest.json"
INDEX = "image_index.json.gz"
SAMPLE = 128
WORKERS = 32
LEVEL = 3
TAR = shutil.which("tar")
FIND = shutil.which("find")

ROOTS = ["/root", "/opt", "/etc", "/var", "/usr"]
SKIP = (
    "/proc", "/sys", "/dev", "/run", "/tmp", "/mnt", "/media", "/kaggle/input",
    "/var/cache", "/var/log", "/var/lib/docker", "/root/.cache", "/root/.npm",
    "/etc/hosts", "/etc/resolv.conf", "/etc/hostname", "/.dockerenv",
    "/etc/mtab", "/etc/secrets", "/kaggle/src", "/kaggle/nbdev", "/kaggle/huggingface",
    "/kaggle/lib", "/root/.julia", "/root/.gradle",
    "/usr/local/lib", "/usr/local/go", "/usr/local/share",
)
SKIP_NAMES = {"__pycache__"}

_BASE = None
_IMG = None
_FRESH_BASE = False
_BASE_SOURCE = None
_PREV = None
_EXISTS = False
_SCAN = None
_REPO = None


def _skipped(p):
    return any(p == s or p.startswith(s + "/") for s in SKIP)


def _scan_py():
    q = queue.Queue()
    for r in ROOTS:
        q.put(r)
    found, lock = {}, threading.Lock()

    def worker():
        while True:
            d = q.get()
            if d is None:
                q.task_done()
                return
            local = {}
            try:
                with os.scandir(d) as it:
                    for e in it:
                        p = e.path
                        if e.name in SKIP_NAMES or _skipped(p):
                            continue
                        try:
                            if e.is_dir(follow_symlinks=False):
                                q.put(p)
                                continue
                            st = e.stat(follow_symlinks=False)
                        except OSError:
                            continue
                        if stat.S_ISREG(st.st_mode) or stat.S_ISLNK(st.st_mode):
                            local[p] = (st.st_size, st.st_mtime_ns // 10**9)
            except OSError:
                pass
            if local:
                with lock:
                    found.update(local)
            q.task_done()

    threads = [threading.Thread(target=worker, daemon=True) for _ in range(WORKERS)]
    for t in threads:
        t.start()
    q.join()
    for _ in threads:
        q.put(None)
    for t in threads:
        t.join()
    return found


def _scan():
    if FIND is None:
        return _scan_py()
    pairs = []
    for s in SKIP:
        pairs += [["-path", s], ["-path", s + "/*"]]
    for n in SKIP_NAMES:
        pairs.append(["-name", n])
    group = []
    for pr in pairs:
        if group:
            group.append("-o")
        group += pr
    found = {}
    for r in ROOTS:
        cmd = [FIND, r]
        if group:
            cmd += ["(", *group, ")", "-prune", "-o"]
        cmd += ["(", "-type", "f", "-o", "-type", "l", ")",
                "-printf", "%p\\0%s\\0%T@\\0"]
        out = subprocess.run(cmd, capture_output=True).stdout.split(b"\0")
        for i in range(0, len(out) - 2, 3):
            if out[i]:
                found[out[i].decode()] = (int(out[i + 1]), int(float(out[i + 2])))
    return found


def _baseline_scan(ready=None):
    global _BASE, _IMG, _FRESH_BASE, _BASE_SOURCE
    t = time.time()
    _IMG = _scan()
    _BASE, _FRESH_BASE, _BASE_SOURCE = _IMG, True, "scan"
    print(f"[sync] baseline: {len(_BASE)} image files indexed ({time.time() - t:.1f}s)")


def _key(sig):
    return [sig[0], sig[1]]


def _snapshot_files(cur, base, prev):
    changed = {p: s for p, s in cur.items() if base.get(p) != s}
    carried = {p: s for p, s in cur.items() if prev and prev.get(p) == _key(s)}
    return changed, carried


def _validate_index(idx, n=SAMPLE):
    paths = list(idx)
    if len(paths) > n:
        paths = random.sample(paths, n)
    bad = 0
    for p in paths:
        try:
            st = os.stat(p, follow_symlinks=False)
        except OSError:
            bad += 1
            continue
        if (st.st_size, st.st_mtime_ns // 10**9) != (idx[p][0], idx[p][1]):
            bad += 1
    return bad, len(paths)


def _index_is_stale(bad, n):
    return bad > min(max(2, int(n * 0.02)), max(0, n - 1))


def _fix_symlink_mtimes(prev):
    n = 0
    for p, k in (prev or {}).items():
        if os.path.islink(p):
            try:
                os.utime(p, ns=(k[1] * 10**9, k[1] * 10**9), follow_symlinks=False)
                n += 1
            except OSError:
                pass
    return n


def _load_index(tmp):
    ip = Path(tmp, INDEX)
    if not ip.exists():
        print("[sync] no image index in the snapshot yet - scanning the image (this session only)")
        return None
    try:
        idx = json.loads(gzip.decompress(ip.read_bytes()))
    except Exception as ex:
        print(f"[sync] image index unreadable ({type(ex).__name__}) - scanning the image")
        return None
    if not idx:
        print("[sync] image index is empty - scanning the image")
        return None
    bad, n = _validate_index(idx)
    if _index_is_stale(bad, n):
        print(f"[sync] image index looks stale (sample {n}: {bad} mismatch) - scanning the image")
        return None
    print(f"[sync] image index reused: {len(idx)} files, sample {n} -> {bad} mismatch - no scan needed")
    return {p: (v[0], v[1]) for p, v in idx.items()}


def _shard(files, k):
    k = max(1, min(k, len(files)))
    heap = [(0, i) for i in range(k)]
    bins = [[] for _ in range(k)]
    for p, size in sorted(files, key=lambda x: -x[1]):
        total, i = heapq.heappop(heap)
        bins[i].append(p)
        heapq.heappush(heap, (total + size, i))
    return bins


def _zstd():
    try:
        import zstandard
    except ImportError:
        import subprocess

        subprocess.run([sys.executable, "-m", "pip", "install", "-q", "zstandard"], check=True)
        import zstandard
    return zstandard


def _pack_py(args):
    i, paths, tmp = args
    ok, bad = [], []
    with open(f"{tmp}/{ARCHIVE}{i:02d}.tar.zst", "wb") as f:
        with _zstd().ZstdCompressor(level=LEVEL).stream_writer(f) as z:
            with tarfile.open(fileobj=z, mode="w|") as tar:
                for p in paths:
                    try:
                        tar.add(p, arcname=p.lstrip("/"), recursive=False)
                        ok.append(p)
                    except OSError as ex:
                        bad.append((p, str(ex)))
    return ok, bad


def _tar_names(archive):
    fd, t = tempfile.mkstemp(suffix=".tar")
    with os.fdopen(fd, "wb") as out, open(archive, "rb") as fin:
        _zstd().ZstdDecompressor().copy_stream(fin, out)
    try:
        with tarfile.open(t) as tar:
            return {"/" + m.name for m in tar}
    finally:
        os.unlink(t)


def _pack(args):
    i, paths, tmp = args
    if TAR is None:
        return _pack_py(args)
    archive = f"{tmp}/{ARCHIVE}{i:02d}.tar.zst"
    lst = f"{tmp}/{ARCHIVE}{i:02d}.list"
    Path(lst).write_bytes(b"\0".join(p.lstrip("/").encode() for p in paths) + b"\0")
    try:
        p = subprocess.Popen([TAR, "-C", "/", "--null", "-T", lst, "-cf", "-"],
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        with open(archive, "wb") as f, _zstd().ZstdCompressor(level=LEVEL).stream_writer(f) as z:
            shutil.copyfileobj(p.stdout, z)
        rc = p.wait()
    finally:
        os.unlink(lst)
    if rc == 0:
        return list(paths), []
    landed = _tar_names(archive)
    ok = [x for x in paths if x in landed]
    bad = [(x, "tar skipped it") for x in paths if x not in landed]
    return ok, bad


def _unpack(archive):
    if TAR is None:
        fd, tmp = tempfile.mkstemp(suffix=".tar")
        with os.fdopen(fd, "wb") as out, open(archive, "rb") as fin:
            _zstd().ZstdDecompressor().copy_stream(fin, out)
        try:
            n = 0
            with tarfile.open(tmp) as tar:
                for m in tar:
                    if m.name.startswith("/") or ".." in Path(m.name).parts:
                        raise RuntimeError(f"unsafe path in archive: {m.name}")
                    os.makedirs(os.path.dirname("/" + m.name), exist_ok=True)
                    try:
                        tar.extract(m, "/", filter="fully_trusted")
                    except TypeError:
                        tar.extract(m, "/")
                    n += 1
            return n
        finally:
            os.unlink(tmp)
    errf = tempfile.TemporaryFile()
    p = subprocess.Popen([TAR, "-x", "-C", "/", "--no-same-owner"],
                         stdin=subprocess.PIPE, stderr=errf)
    try:
        with open(archive, "rb") as fin:
            _zstd().ZstdDecompressor().copy_stream(fin, p.stdin)
    finally:
        try:
            p.stdin.close()
        except OSError:
            pass
    rc = p.wait()
    if rc != 0:
        errf.seek(0)
        raise RuntimeError(f"unreadable archive {os.path.basename(archive)}: "
                           f"{errf.read().decode(errors='replace').strip()[:200]}")
    errf.close()
    return 0


def _hf():
    if not HF_TOKEN:
        raise RuntimeError("no Hugging Face token: export HF_TOKEN or write ~/.aixodia/hf_token")
    try:
        from huggingface_hub import HfApi
    except ImportError:
        import subprocess

        subprocess.run([sys.executable, "-m", "pip", "install", "-q", "huggingface_hub"], check=True)
        from huggingface_hub import HfApi
    return HfApi(token=HF_TOKEN)


def _current():
    api = _hf()
    best = None
    for r in REPOS:
        try:
            if MANIFEST not in api.list_repo_files(r, repo_type="dataset"):
                continue
            when = api.dataset_info(r).lastModified
        except Exception:
            continue
        if best is None or when > best[0]:
            best = (when, r)
    return best[1] if best else None


def _download(repo, tmp):
    from huggingface_hub import snapshot_download

    return snapshot_download(
        repo, repo_type="dataset", local_dir=tmp, token=HF_TOKEN,
        allow_patterns=[ARCHIVE + "*.tar.zst", MANIFEST, INDEX],
    )


def restore():
    global _PREV, _EXISTS, _SCAN, _BASE, _FRESH_BASE, _BASE_SOURCE, _IMG, _REPO
    t0 = time.time()
    ready = threading.Event()
    try:
        from huggingface_hub.utils import HfHubHTTPError
    except ImportError:
        _hf()
        from huggingface_hub.utils import HfHubHTTPError

    repo = _current()
    if repo is None:
        print(f"[sync] no snapshot in {REPOS} yet - starting fresh")
        _SCAN = threading.Thread(target=_baseline_scan, args=(ready,), daemon=True)
        _SCAN.start()
        return

    try:
        with tempfile.TemporaryDirectory() as tmp:
            try:
                _download(repo, tmp)
            except HfHubHTTPError as ex:
                code = getattr(ex.response, "status_code", 0)
                raise RuntimeError(
                    f"cannot read {repo} (HTTP {code}) - aborting so the snapshot is not overwritten"
                ) from ex
            except Exception as ex:
                raise RuntimeError(f"cannot read {repo}: {ex}") from ex
            if not Path(tmp, MANIFEST).exists():
                raise RuntimeError(f"{repo} has no {MANIFEST} - aborting so the snapshot is not overwritten")
            _REPO, _EXISTS = repo, True
            _PREV = json.loads(Path(tmp, MANIFEST).read_text())

            idx = _load_index(tmp)
            if idx is not None:
                _BASE, _IMG, _FRESH_BASE, _BASE_SOURCE = idx, None, False, "index"
                _SCAN = None
            else:
                _BASE = None
                _SCAN = threading.Thread(target=_baseline_scan, args=(ready,), daemon=True)
                _SCAN.start()

            parts = sorted(Path(tmp).glob(ARCHIVE + "*.tar.zst"))
            gone = time.time() - t0

            _zstd()
            t1 = time.time()
            with ThreadPoolExecutor(min(WORKERS, max(1, len(parts)))) as ex:
                list(ex.map(_unpack, parts))
            extract = time.time() - t1
            total = len(_PREV)

            missing = [p for p in _PREV if not os.path.lexists(p)]
            if missing:
                raise RuntimeError(f"{len(missing)} file(s) of the snapshot are missing, e.g. {missing[0]}")

            links = _fix_symlink_mtimes(_PREV)

            ssh = Path.home() / ".ssh"
            if ssh.is_dir():
                os.chmod(ssh, 0o700)
                for f in ssh.iterdir():
                    if f.is_file() and not f.name.endswith(".pub"):
                        os.chmod(f, 0o600)

            print(
                f"[sync] restore ready in {time.time() - t0:.1f}s "
                f"({total} files from {len(parts)} archive(s); download {gone:.1f}s + extract {extract:.1f}s; "
                f"image index: {_BASE_SOURCE or 'scanning'}"
                f"{f'; {links} symlink mtime(s) restored' if links else ''})"
            )
    finally:
        ready.set()


def backup(force=False):
    global _BASE, _PREV, _EXISTS, _FRESH_BASE, _BASE_SOURCE, _REPO
    t0 = time.time()
    if _SCAN is not None:
        _SCAN.join()
    if _BASE is None:
        raise RuntimeError(
            "kgat_sync: call restore() at the start of the session first - it loads or scans the image "
            "index, which is how backup() knows what not to upload"
        )

    cur = _scan()
    changed, carried = _snapshot_files(cur, _BASE, _PREV)
    user = dict(carried)
    user.update(changed)
    now = {p: _key(s) for p, s in user.items()}
    print(f"[sync] scan: {len(cur)} files on disk, {len(user)} belong in the snapshot "
          f"({len(changed)} changed/new, {len(carried)} unchanged from the last snapshot) "
          f"({time.time() - t0:.1f}s)")

    if not user:
        print("[sync] nothing to back up")
        return []
    if not force and _EXISTS and now == _PREV:
        print("[sync] nothing changed since the last snapshot")
        return []

    t1 = time.time()
    with tempfile.TemporaryDirectory() as tmp:
        shards = _shard([(p, s[0]) for p, s in user.items()], WORKERS)
        with ThreadPoolExecutor(min(WORKERS, len(shards))) as ex:
            results = list(ex.map(_pack, [(i, b, tmp) for i, b in enumerate(shards)]))
        ok = [p for good, _ in results for p in good]
        bad = [x for _, err in results for x in err]
        manifest = {p: _key(user[p]) for p in ok}
        Path(tmp, MANIFEST).write_text(json.dumps(manifest))
        src = _IMG if _IMG is not None else _BASE
        idx = {p: [s[0], s[1]] for p, s in src.items()}
        Path(tmp, INDEX).write_bytes(gzip.compress(json.dumps(idx).encode(), 6))
        stored = f" + image index ({len(idx)} files)"

        api = _hf()
        target = REPOS[1] if _REPO == REPOS[0] else REPOS[0]
        api.delete_repo(target, repo_type="dataset", missing_ok=True)
        api.create_repo(target, repo_type="dataset", private=True, exist_ok=True)
        api.upload_folder(
            folder_path=tmp,
            repo_id=target,
            repo_type="dataset",
            commit_message=f"snapshot: {len(ok)} files {time.strftime('%Y-%m-%d %H:%M')}",
        )
        if _REPO and _REPO != target:
            api.delete_repo(_REPO, repo_type="dataset", missing_ok=True)
    _REPO = target
    _PREV, _EXISTS = manifest, True
    if stored:
        _FRESH_BASE, _BASE_SOURCE = False, "index"
    print(f"[sync] backed up {len(ok)} files{stored} -> {target} ({time.time() - t1:.1f}s)")
    for p, err in bad:
        print(f"[sync]   unreadable: {p} ({err})")
    return bad

# ---------------------------------------------------------------------------
# Entry points used by the engine and the notebook.

KERNEL = os.environ.get("AIXODIA_KERNEL", "kyomusen/aixodia-engine")
KAGGLE_TOKEN = _secret("kaggle_token", "AIXODIA_KAGGLE_TOKEN", "KAGGLE_API_TOKEN")


def load_state():
    """Load the manifest and the image index without touching the filesystem.

    restore() extracts the snapshot, so it can only run before the engine is
    started. backup() needs the manifest (what the last snapshot holds) and the
    image index (which files are the base image), and both are a few megabytes:
    they are fetched here so a backup can run in a process of its own.
    """
    global _PREV, _EXISTS, _REPO, _BASE, _IMG, _BASE_SOURCE
    repo = _current()
    if repo is None:
        print("[sync] no snapshot yet - this machine becomes the baseline")
        _BASE, _IMG, _BASE_SOURCE = _scan(), None, "scan"
        _PREV, _EXISTS, _REPO = None, False, None
        return False

    _hf()  # installs huggingface_hub first when the image has none
    from huggingface_hub import hf_hub_download

    with tempfile.TemporaryDirectory() as tmp:
        try:
            hf_hub_download(repo, MANIFEST, repo_type="dataset", local_dir=tmp, token=HF_TOKEN)
        except Exception as ex:
            raise RuntimeError(f"{repo} has no readable {MANIFEST} ({type(ex).__name__}) - "
                               "refusing to overwrite the snapshot") from ex
        try:
            hf_hub_download(repo, INDEX, repo_type="dataset", local_dir=tmp, token=HF_TOKEN)
        except Exception as ex:
            print(f"[sync] no {INDEX} in the snapshot ({type(ex).__name__})")
        _PREV = json.loads(Path(tmp, MANIFEST).read_text())
        _REPO, _EXISTS = repo, True
        idx = _load_index(tmp)
        if idx is not None:
            _BASE, _IMG, _BASE_SOURCE = idx, None, "index"
        else:
            _BASE, _IMG, _BASE_SOURCE = _scan(), None, "scan"
    print(f"[sync] loaded {len(_PREV)} manifest entries from {_REPO}")
    return True


def restart():
    """Push a fresh version of this notebook; Kaggle starts it as a new session."""
    env = dict(os.environ)
    env.pop("KAGGLE_API_V1_TOKEN", None)
    if KAGGLE_TOKEN:
        env["KAGGLE_API_TOKEN"] = KAGGLE_TOKEN
    where = os.environ.get("AIXODIA_RESTART_DIR", "/kaggle/working/restart")
    Path(where).mkdir(parents=True, exist_ok=True)
    for argv in (["kaggle", "kernels", "pull", "-m", "-p", where, KERNEL],
                 ["kaggle", "kernels", "push", "-p", where]):
        print(f"[sync] {' '.join(argv)}", flush=True)
        if subprocess.run(argv, env=env).returncode != 0:
            raise RuntimeError(f"{' '.join(argv)} failed")


def main(argv):
    if len(argv) < 2:
        print("usage: kgat_sync.py {restore|backup|restart|status}", file=sys.stderr)
        return 2
    cmd = argv[1]
    if cmd == "restore":
        restore()
    elif cmd == "backup":
        load_state()
        bad = backup(force="--force" in argv[2:])
        if bad:
            print(f"[sync] {len(bad)} file(s) could not be packed", flush=True)
    elif cmd == "restart":
        restart()
    elif cmd == "status":
        repo = _current()
        print(f"[sync] snapshot: {repo or 'none'}")
    else:
        print("usage: kgat_sync.py {restore|backup|restart|status}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
