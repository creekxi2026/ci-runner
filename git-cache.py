#!/usr/bin/env python3
"""Controller-only Git object cache. No worker data is ever imported.

Snapshots contain only objects plus a generated bare-repository configuration.
All Git commands run with isolated configuration; credentials exist only in a
fetch child's environment. Jobs receive independent files and normal checkout
still fetches/verifies its requested source. The cached HEAD is never authority.
"""
import argparse
import base64
import errno
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import sys
import time
import uuid

FICLONE = 0x40049409
FALLBACK = {errno.EXDEV, errno.EOPNOTSUPP, errno.ENOTTY, errno.EINVAL}
REPOSITORY = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\Z")
SHA = re.compile(r"[a-f0-9]{40}\Z")
SNAPSHOT = re.compile(r"[a-f0-9]{32}\Z")
CONFIG = b"[core]\n\trepositoryformatversion = 0\n\tbare = true\n"


class CacheCancelled(BaseException):
    """Cancellation must never be mistaken for an optional refresh failure."""


class GitCache:
    def __init__(self, root, repository, refs, token="", *, max_bytes=2 << 30,
                 timeout=180, uid=0, job_uid=1001):
        if not REPOSITORY.fullmatch(repository) or any(
                p in (".", "..") for p in repository.split("/")):
            raise ValueError("invalid repository")
        if not refs or len(refs) > 8:
            raise ValueError("one to eight cache refs required")
        for ref in refs:
            if (not re.fullmatch(r"refs/heads/[A-Za-z0-9_./-]+", ref)
                    or ".." in ref or "//" in ref or "@{" in ref
                    or ref.endswith(("/", ".", ".lock"))):
                raise ValueError("invalid cache ref")
        self.root, self.repository, self.refs = Path(root), repository, refs
        self.url = "https://github.com/" + repository
        self.token, self.timeout, self.max_bytes = token, timeout, max_bytes
        self.uid, self.job_uid = uid, job_uid
        self.compact_packs = 32
        self.metrics = {"cloned_bytes": 0, "copied_bytes": 0}

    def protected(self, path, directory=False):
        st = path.lstat()
        kind = stat.S_ISDIR(st.st_mode) if directory else stat.S_ISREG(st.st_mode)
        if (not kind or st.st_uid != self.uid or st.st_mode & 0o077
                or (not directory and st.st_nlink != 1)):
            raise ValueError("unsafe Git cache permissions or file type")
        return st

    def git(self, directory, *args, auth=False):
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": "/nonexistent",
               "LC_ALL": "C", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
               "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_COUNT": "0"}
        for name in ("HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"):
            if name in os.environ:
                env[name] = os.environ[name]
        if auth and self.token:
            encoded = base64.b64encode(("x-access-token:" + self.token).encode()).decode()
            env.update(GIT_CONFIG_COUNT="1", GIT_CONFIG_KEY_0="http.https://github.com/.extraHeader",
                       GIT_CONFIG_VALUE_0="AUTHORIZATION: basic " + encoded)
        command = ["git", "-c", "core.hooksPath=/dev/null", "-c", "credential.helper=",
                   "-c", "http.followRedirects=false", "-c", "protocol.allow=never",
                   "-c", "protocol.https.allow=always", "-c", "gc.auto=0",
                   "-c", "maintenance.auto=false", "-c", "pack.threads=1",
                   "-c", "pack.windowMemory=32m", "-c", "fetch.fsckObjects=true",
                   "-c", "fetch.unpackLimit=0",
                   "-C", str(directory), *args]
        # One process group is killed on timeout, including remote-https/index-pack.
        # Defer SIGTERM until the child is registered in the cleanup guard.
        # Otherwise cancellation between fork/Popen.__enter__ and try can orphan
        # the separate Git session when the controller later kills the helper.
        previous_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM})
        proc = None
        try:
            proc = subprocess.Popen(command, env=env, stdout=subprocess.PIPE,
                                    stderr=subprocess.DEVNULL, start_new_session=True)
            try:
                signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
                deadline = time.monotonic() + self.timeout
                while True:
                    try:
                        output, _ = proc.communicate(timeout=min(0.25, max(0.001, deadline - time.monotonic())))
                        break
                    except subprocess.TimeoutExpired:
                        if time.monotonic() >= deadline:
                            raise
                        if args[0] == "fetch":
                            size = sum(p.stat().st_size for p in (Path(directory) / "objects").rglob("*") if p.is_file())
                            if size > self.max_bytes or shutil.disk_usage(directory).free < 1 << 30:
                                raise ValueError("Git fetch storage budget exceeded")
            except BaseException:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass  # Preserve the original cancellation if Git just exited.
                proc.wait()
                raise
            if proc.returncode:
                raise ValueError("Git operation failed: " + args[0])
        finally:
            if proc is not None:
                proc.stdout.close()
            signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
        return output.decode().strip()

    def copy_objects(self, source, target, uid):
        total, count = 0, 0
        target.mkdir(mode=0o700)
        for parent, dirs, files in os.walk(source, followlinks=False):
            parent = Path(parent)
            self.protected(parent, directory=True)
            dest = target / parent.relative_to(source)
            for name in dirs:
                self.protected(parent / name, directory=True)
                (dest / name).mkdir(mode=0o700)
            for name in files:
                src, dst = parent / name, dest / name
                relative = str(src.relative_to(source))
                # Neither alternates nor commit-graph/config/hook metadata is imported.
                if not re.fullmatch(r"(?:[a-f0-9]{2}/[a-f0-9]{38}|pack/pack-[a-f0-9]{40}\.(?:pack|idx|rev))", relative):
                    raise ValueError("unexpected Git object metadata")
                st = self.protected(src)
                total += st.st_size
                count += 1
                if total > self.max_bytes or count > 100000:
                    raise ValueError("Git object cache budget exceeded")
                with os.fdopen(os.open(src, os.O_RDONLY | os.O_NOFOLLOW), "rb") as inp, dst.open("xb") as out:
                    try:
                        fcntl.ioctl(out.fileno(), FICLONE, inp.fileno())
                        self.metrics["cloned_bytes"] += st.st_size
                    except OSError as exc:
                        if exc.errno not in FALLBACK:
                            raise
                        shutil.copyfileobj(inp, out, 1024 * 1024)
                        self.metrics["copied_bytes"] += st.st_size
                dst.chmod(0o600)
                os.chown(dst, uid, uid if uid != os.getuid() else os.getgid())
        for parent, _, _ in os.walk(target):
            os.chown(parent, uid, uid if uid != os.getuid() else os.getgid())
        return total

    def current(self):
        index = self.root / "index.json"
        if not index.exists():
            return None
        self.protected(index)
        if index.stat().st_size > 4096:
            raise ValueError("invalid Git cache index")
        data = json.loads(index.read_text())
        if not SNAPSHOT.fullmatch(data["snapshot"]) or data["repository"] != self.repository:
            raise ValueError("Git cache identity mismatch")
        source = self.root / data["snapshot"]
        self.protected(source, directory=True)
        self.protected(source / "config")
        if (source / "config").read_bytes() != CONFIG:
            raise ValueError("unexpected Git cache configuration")
        # fsck reads configuration/refs, so validate the whole tree first.
        for parent, dirs, files in os.walk(source, followlinks=False):
            for name in dirs:
                self.protected(Path(parent) / name, directory=True)
            for name in files:
                self.protected(Path(parent) / name)
        if (any((source / "objects/info").iterdir()) or (source / "hooks").exists()
                or (source / "shallow").exists()):
            raise ValueError("unexpected Git cache indirection")
        self.git(source, "fsck", "--full", "--no-reflogs")
        return source, data

    def init_bare(self, path):
        path.mkdir(mode=0o700)
        self.git(path, "init", "--bare", "--template=")
        (path / "config").write_bytes(CONFIG)

    def heads(self, source):
        lines = self.git(source, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/").splitlines()
        heads = dict(line.split(" ", 1)[::-1] for line in lines)
        if not heads or any(ref not in self.refs or not SHA.fullmatch(sha) for ref, sha in heads.items()):
            raise ValueError("unexpected Git cache refs")
        return heads

    def update(self, previous):
        staging = self.root / "staging"
        if staging.exists():
            self.protected(staging, directory=True)
            shutil.rmtree(staging)
        self.init_bare(staging)
        try:
            before = 0
            if previous:
                shutil.rmtree(staging / "objects")
                before = self.copy_objects(previous[0] / "objects", staging / "objects", self.uid)
                for ref, sha in self.heads(previous[0]).items():
                    self.git(staging, "update-ref", ref, sha)
            start = time.monotonic()
            packs_before = {p.name for p in (staging / "objects/pack").glob("*.pack")}
            self.git(staging, "fetch", "--no-tags", "--prune", "--no-write-fetch-head",
                     self.url, *["+" + ref + ":" + ref for ref in self.refs], auth=True)
            self.metrics["fetch_seconds"] = time.monotonic() - start
            self.metrics["received_pack_bytes"] = sum(
                p.stat().st_size for p in (staging / "objects/pack").glob("*.pack")
                if p.name not in packs_before)
            # Compact only unpublished data under the lock. Private job copies
            # stay valid even when a force-pushed source becomes unreachable.
            if len(list((staging / "objects/pack").glob("*.pack"))) > self.compact_packs:
                gc_start = time.monotonic()
                self.git(staging, "repack", "-ad", "-n", "--no-write-bitmap-index")
                self.git(staging, "prune", "--expire=now")
                self.metrics["gc_seconds"] = time.monotonic() - gc_start
            # Check content, not a cached success flag. No jobs consume partial packs.
            self.git(staging, "fsck", "--full", "--no-reflogs")
            heads = self.heads(staging)
            self.git(staging, "symbolic-ref", "HEAD", next(iter(heads)))
            after = sum(p.stat().st_size for p in (staging / "objects").rglob("*") if p.is_file())
            if after > self.max_bytes:
                raise ValueError("Git object cache budget exceeded")
            # Git follows the private umask. Never copy config from a job or remote.
            for p in staging.rglob("*"):
                p.chmod(0o700 if p.is_dir() else 0o600)
            identity = uuid.uuid4().hex
            staging.rename(self.root / identity)
            data = {"snapshot": identity, "repository": self.repository,
                    "updated": time.time(), "object_bytes": after, "heads": heads}
            index = self.root / "index.new"
            with index.open("w") as output:
                json.dump(data, output, sort_keys=True)
                output.flush()
                os.fsync(output.fileno())
            index.replace(self.root / "index.json")
            fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
            self.metrics.update(object_bytes=after, added_object_bytes=max(0, after - before), heads=heads)
            return self.root / identity, data
        finally:
            if staging.exists():
                shutil.rmtree(staging)

    def collect(self, keep):
        # Copies are self-contained; no live job references these directories.
        for path in self.root.iterdir():
            if SNAPSHOT.fullmatch(path.name) and path != keep:
                self.protected(path, directory=True)
                shutil.rmtree(path)

    def prepare(self, home, refresh_seconds=60):
        self.metrics = {"cloned_bytes": 0, "copied_bytes": 0, "received_pack_bytes": 0, "fetch_seconds": 0}
        self.protected(self.root, directory=True)
        start = time.monotonic()
        if shutil.disk_usage(self.root).free < 5 << 30:
            raise ValueError("less than 5 GiB free on Git cache filesystem")
        lock_path = self.root / "cache.lock"
        with os.fdopen(os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600), "w") as lock:
            self.protected(lock_path)
            fcntl.flock(lock, fcntl.LOCK_EX)
            old = self.current()  # Corruption fails cold; it is never propagated.
            hit = old is not None
            if old is None or time.time() - old[1]["updated"] >= refresh_seconds:
                try:
                    old = self.update(old)
                except Exception:
                    if old is None:
                        raise
                    self.metrics["refresh_failed"] = True
            self.collect(old[0])
            home = Path(home)
            workspace = home / "_work" / self.repository.split("/")[1] / self.repository.split("/")[1]
            if workspace.exists():
                raise ValueError("Git cache target must not already exist")
            workspace.mkdir(mode=0o700, parents=True)
            try:
                self.git(workspace, "init", "--template=")
                private = workspace / ".git"
                # --template= omits info/. actions/checkout writes its non-cone
                # sparse-checkout file here directly, before invoking Git.
                (private / "info").mkdir(mode=0o700)
                shutil.rmtree(private / "objects")
                self.copy_objects(old[0] / "objects", private / "objects", self.uid)
                for ref, sha in self.heads(old[0]).items():
                    self.git(workspace, "update-ref", ref.replace("refs/heads/", "refs/remotes/origin/", 1), sha)
                self.git(workspace, "remote", "add", "origin", self.url)
                # A valid detached HEAD lets actions/checkout clean/reset this
                # repository instead of deleting it as an invalid partial init.
                # No fetched source is executed here. The job still selects SHA.
                # Do not expand unbounded/compressible blobs in the controller.
                # checkout's reset/checkout materializes files inside the worker.
                self.git(workspace, "update-ref", "--no-deref", "HEAD", next(iter(self.heads(old[0]).values())))
                for parent, dirs, files in os.walk(workspace, followlinks=False):
                    for p in [Path(parent), *[Path(parent) / name for name in dirs + files]]:
                        os.chown(p, self.job_uid, self.job_uid if self.job_uid != os.getuid() else os.getgid(), follow_symlinks=False)
                for p in [workspace.parent, workspace.parent.parent]:
                    os.chown(p, self.job_uid, self.job_uid if self.job_uid != os.getuid() else os.getgid())
            except BaseException:
                shutil.rmtree(workspace)
                raise
        self.metrics.update(cache_hit=hit, prepare_seconds=time.monotonic() - start,
                            repository=self.repository, snapshot=old[0].name)
        return dict(self.metrics)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True)
    parser.add_argument("--home", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--refs", default="refs/heads/main")
    args = parser.parse_args()
    if os.getuid() != 0:
        raise ValueError("controller root identity required")
    def terminate(_signal, _frame):
        raise CacheCancelled("Git cache preparation cancelled")
    signal.signal(signal.SIGTERM, terminate)
    os.umask(0o077)
    token = sys.stdin.read(16384).strip()
    cache = GitCache(args.root, args.repository, args.refs.split(","), token)
    print(json.dumps(cache.prepare(args.home), sort_keys=True), flush=True)


if __name__ == "__main__":
    try:
        main()
    except (Exception, CacheCancelled):
        # Do not emit argv, remote responses, environment or credentials.
        print("Git cache unavailable; use normal cold checkout", file=sys.stderr)
        sys.exit(1)
