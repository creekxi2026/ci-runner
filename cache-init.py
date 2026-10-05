#!/usr/bin/env python3
"""Initialize fixed cache directory metadata, never package files or host trees."""
import os
import stat
from contextlib import ExitStack

NAMES = ('npm', 'pip', 'gomod', 'go-build')
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


if __name__ == '__main__':
    initialize()
