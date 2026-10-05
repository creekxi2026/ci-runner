#!/usr/bin/env python3
"""Initialize fixed cache directory metadata, never package files or host trees."""
import argparse
import fcntl
import json
import os
import stat
from contextlib import ExitStack

NAMES = ('npm', 'pip', 'gomod', 'go-build', 'jest')
FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW


def initialize(root='/opt/ci-cache', uid=1001, gid=1001, max_bytes=4 * 1024**3):
    with ExitStack() as stack:
        def open_dir(name, parent=None):
            fd = os.open(name, FLAGS, dir_fd=parent)
            stack.callback(os.close, fd)
            return fd
        base = open_dir(root)
        # Admission watermark, not a filesystem quota: concurrent active writers
        # can overshoot. Traverse descriptors without following job-owned links.
        total = 0
        def measure(fd):
            nonlocal total
            for name in os.listdir(fd):
                try:
                    info = os.stat(name, dir_fd=fd, follow_symlinks=False)
                    if stat.S_ISDIR(info.st_mode):
                        child = os.open(name, FLAGS, dir_fd=fd)
                        try:
                            measure(child)
                        finally:
                            os.close(child)
                    elif stat.S_ISREG(info.st_mode):
                        total += info.st_size
                        if total >= max_bytes:
                            raise OSError('dependency cache admission watermark exceeded')
                except FileNotFoundError:
                    pass  # Package-native atomic replacement by an active job.
        measure(base)
        # Inspect every existing fixed entry before any metadata mutation.
        opened = {}
        for name in NAMES:
            try:
                opened[name] = open_dir(name, base)
            except FileNotFoundError:
                pass
        for name in NAMES:
            if name not in opened:
                try:
                    os.mkdir(name, mode=0o700, dir_fd=base)
                except FileExistsError:
                    pass
                opened[name] = open_dir(name, base)
        for fd in opened.values():
            os.fchown(fd, uid, gid)
            os.fchmod(fd, 0o700)


def _initialize_volume(root, uid=1001, gid=1001):
    """One root-owned data/tools layout for controller and standalone jobs."""
    from pathlib import Path
    root = Path(root)
    base = os.open(root, FLAGS)
    try:
        for name in ('data',):
            try: os.mkdir(name, mode=0o755, dir_fd=base)
            except FileExistsError: pass
        data = os.open('data', FLAGS, dir_fd=base)
        try:
            info = os.fstat(data)
            if info.st_uid != os.getuid() or info.st_mode & 0o022:
                raise ValueError('Cache data root must be initializer-owned and not group/world writable')
            try: os.mkdir('tools', mode=0o755, dir_fd=data)
            except FileExistsError: pass
            tools = os.open('tools', FLAGS, dir_fd=data)
            try:
                info = os.fstat(tools)
                if info.st_uid != os.getuid() or info.st_mode & 0o022:
                    raise ValueError('Tools must be initializer-owned and not group/world writable')
            finally: os.close(tools)
        finally: os.close(data)
    finally: os.close(base)
    marker = root/'data'/'ready.json'
    if marker.is_symlink(): raise ValueError('Cache marker cannot be a symlink')
    if marker.exists() and json.loads(marker.read_text()).get('schema') != 'ci-cache-v1':
        raise ValueError('Unsupported cache data schema')
    initialize(str(root/'data'), uid, gid)
    npm = root/'data'/'npm'/'_cacache'
    npm.mkdir(mode=0o700, exist_ok=True)
    fd = os.open(npm, FLAGS)
    try:
        os.fchown(fd, uid, gid)
        os.fchmod(fd, 0o700)
    finally: os.close(fd)
    if not marker.exists():
        # Exclusive create prevents concurrent helpers from truncating a marker.
        try:
            fd = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o644)
        except FileExistsError: return
        with os.fdopen(fd, 'w') as f:
            json.dump({'schema':'ci-cache-v1','cache_uid':uid,'cache_gid':gid,'trust_domain':'trusted'}, f)


def initialize_volume(root, uid=1001, gid=1001):
    base = os.open(root, FLAGS)
    try:
        lock = os.open('.initialize.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600, dir_fd=base)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX)
            _initialize_volume(root, uid, gid)
        finally: os.close(lock)
    finally: os.close(base)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--volume-root')
    args = parser.parse_args()
    if args.volume_root: initialize_volume(args.volume_root)
    else: initialize()
