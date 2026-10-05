#!/usr/bin/env python3
"""Root-only immutable templates; jobs receive private files, never seed mounts.

FICLONE is an explicit request to the filesystem. Unsupported filesystems copy
bytes and report that fallback; identical bytes alone do not imply deduplication.
"""
import argparse
import errno
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat

FICLONE = 0x40049409
KEY = re.compile(r"^[a-f0-9]{64}$")
FALLBACK = {errno.EXDEV, errno.EOPNOTSUPP, errno.ENOTTY, errno.EINVAL}


def key(value):
    if not KEY.fullmatch(value):
        raise ValueError("expected a SHA256 template identity")
    return value


def clone_tree(source, dest, uid, counts, require_clone=False, link_root=None, exclude=None):
    """No symlinks, devices or hard links enter a dependency template."""
    dest.mkdir(mode=0o700)
    for p in sorted(source.iterdir()):
        if exclude and p in exclude:
            continue
        st = p.lstat()
        target = dest / p.name
        if stat.S_ISDIR(st.st_mode):
            clone_tree(p, target, uid, counts, require_clone, link_root, exclude)
        elif stat.S_ISLNK(st.st_mode) and link_root is not None:
            link = os.readlink(p)
            if os.path.isabs(link) or not p.resolve().is_relative_to(link_root.resolve()):
                raise ValueError("runner link escapes template")
            target.symlink_to(link)
            os.chown(target, uid, uid, follow_symlinks=False)
        elif stat.S_ISREG(st.st_mode):
            # O_NOFOLLOW also protects the source against accidental symlink
            # replacement. Import sources must be idle and administrator-trusted.
            fd = os.open(p, os.O_RDONLY | os.O_NOFOLLOW)
            try:
                with os.fdopen(fd, "rb") as src, target.open("xb") as dst:
                    try:
                        fcntl.ioctl(dst.fileno(), FICLONE, src.fileno())
                        counts["cloned_bytes"] += st.st_size
                    except OSError as e:
                        if require_clone or e.errno not in FALLBACK:
                            raise
                        shutil.copyfileobj(src, dst, 1024 * 1024)
                        counts["copied_bytes"] += st.st_size
                    dst.flush()
                target.chmod((st.st_mode & 0o111) | 0o600)
                os.chown(target, uid, uid)
            except Exception:
                target.unlink(missing_ok=True)
                raise
        else:
            raise ValueError("template contains unsupported file type")
    os.chown(dest, uid, uid)


def manifest(path, runner=False):
    files = {}
    for p in sorted(path.rglob("*")):
        if p.is_symlink():
            if not runner:
                raise ValueError("template symlink")
            files[str(p.relative_to(path))] = {"link": os.readlink(p)}
            continue
        if p.is_file() and not p.is_symlink():
            with p.open("rb") as f:
                digest = hashlib.file_digest(f, "sha256").hexdigest()
            files[str(p.relative_to(path))] = {"sha256": digest, "bytes": p.stat().st_size}
    return files


def durable_manifest(directory, data):
    # Flush seed contents before the atomic directory publication; a crashed
    # publisher leaves only staging, which is removed under the same lock.
    for p in directory.rglob("*"):
        if p.is_file():
            with p.open("rb") as f:
                os.fsync(f.fileno())
    with (directory / "manifest.json").open("x") as f:
        json.dump(data, f, sort_keys=True)
        f.flush()
        os.fsync(f.fileno())


def publish(root, kind, identity, build):
    parent = root / kind
    parent.mkdir(mode=0o700, exist_ok=True)
    target = parent / key(identity)
    if target.exists():
        info = json.loads((target / "manifest.json").read_text())
        if info["identity"] != identity or info["kind"] != kind:
            raise ValueError("template identity mismatch")
        return target
    staging = parent / (identity + ".staging")
    if staging.exists():
        shutil.rmtree(staging)
    staging.mkdir(mode=0o700)
    try:
        data = build(staging)
        durable_manifest(staging, {"identity": identity, "kind": kind, **data})
        staging.rename(target)
        fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    except BaseException:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    return target


def prepare(args, root, counts):
    def runner(staging):
        source = Path(args.runner_source)
        clone_tree(source, staging / "files", 0, counts, link_root=source)
        return {"source": "immutable runner image", "files": manifest(staging / "files", runner=True)}
    template = publish(root, "runner", args.image_id, runner)
    home = Path(args.home)
    if any(home.iterdir()):
        raise ValueError("job home must be empty before initialization")
    private = home / ".runner-staging"
    clone_tree(template / "files", private, 1001, counts, args.require_reflink, template / "files")
    for p in private.iterdir():
        p.rename(home / p.name)
    private.rmdir()
    if args.seed:
        seed = root / "dependencies" / key(args.seed)
        info = json.loads((seed / "manifest.json").read_text())
        if info["kind"] != "dependencies" or info["identity"] != args.seed:
            raise ValueError("dependency seed identity mismatch")
        # Match the toolchain/ABI, not an unrelated application or deployment name.
        if info["compatibility"] != json.loads(Path("/opt/ci/cache-compatibility.json").read_text()):
            raise ValueError("dependency seed toolchain/ABI mismatch")
        for name, destination in {"npm": ".npm/_cacache", "gomod": "go/pkg/mod", "go-build": ".cache/go-build"}.items():
            dest = home / destination
            dest.parent.mkdir(parents=True, exist_ok=True)
            clone_tree(seed / name, dest, 1001, counts, args.require_reflink)
        for p in home.rglob("*"):
            # All files were created by this root initializer before a job exists.
            os.chown(p, 1001, 1001, follow_symlinks=False)
    (home / ".ci-workspace-ready").write_text(args.image_id + "\n")
    os.chown(home / ".ci-workspace-ready", 1001, 1001)
    os.chown(home, 1001, 1001)
    home.chmod(0o700)


def import_seed(args, root, counts):
    source = Path(args.source)
    staging = root / "import.staging"
    if staging.exists():
        shutil.rmtree(staging)
    staging.mkdir(mode=0o700)
    try:
        for name, rel in {"npm": "npm/_cacache", "gomod": "gomod", "go-build": "go-build/" + args.go_namespace}.items():
            # VCS checkout/config state can contain private remotes or auth;
            # package archives/expanded modules suffice for the seed.
            clone_tree(source / rel, staging / name, 0, counts, exclude={source / "gomod/cache/vcs"})
        content = {"files": manifest(staging), "compatibility": json.loads(Path("/opt/ci/cache-compatibility.json").read_text()), "provenance": args.provenance}
        identity = hashlib.sha256(json.dumps(content, sort_keys=True).encode()).hexdigest()
        def build(dest):
            for p in list(staging.iterdir()):
                p.rename(dest / p.name)
            return content
        publish(root, "dependencies", identity, build)
        print(json.dumps({"seed": identity, **counts}), flush=True)
    finally:
        shutil.rmtree(staging)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--templates", default="/templates")
    sub = p.add_subparsers(dest="mode", required=True)
    a = sub.add_parser("prepare")
    a.add_argument("--image-id", required=True, type=key)
    a.add_argument("--home", default="/job-home")
    a.add_argument("--runner-source", default="/opt/actions-runner")
    a.add_argument("--seed", type=key)
    a.add_argument("--require-reflink", action="store_true")
    a = sub.add_parser("import")
    a.add_argument("--source", default="/source")
    a.add_argument("--go-namespace", required=True)
    a.add_argument("--provenance", required=True, help="Reviewed source revision/lock digests and source cache identity")
    args = p.parse_args()
    if os.getuid() != 0:
        raise ValueError("administrator initializer required")
    if args.mode == "import" and not re.fullmatch(r"[a-zA-Z0-9_.-]+", args.go_namespace):
        raise ValueError("invalid Go namespace")
    root = Path(args.templates)
    if root.is_symlink() or root.stat().st_uid != 0 or root.stat().st_mode & 0o077:
        raise ValueError("templates root must be root-owned 0700")
    counts = {"cloned_bytes": 0, "copied_bytes": 0}
    with (root / "template.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if args.mode == "prepare":
            prepare(args, root, counts)
            print(json.dumps({"prepared": True, **counts}), flush=True)
        else:
            import_seed(args, root, counts)


if __name__ == "__main__":
    main()
