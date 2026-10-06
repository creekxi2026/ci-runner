#!/usr/bin/env python3
"""Initialize a new, private job disk; no recursive ownership or shared inputs."""
import os
from pathlib import Path


def initialize(root):
    root = Path(root)
    if root.is_symlink() or any(root.iterdir()):
        raise ValueError('A new empty job disk is required')
    # Leave a small host reserve; this is admission control, not a disk quota.
    usage = os.statvfs(root)
    if usage.f_bavail * usage.f_frsize < 5 * 1024**3:
        raise ValueError('Less than 5 GiB free on the job disk filesystem')
    for name, uid, mode in [('home', 1001, 0o700), ('tmp', 1001, 0o1777)]:
        p = root / name
        p.mkdir(mode=mode)
        os.chown(p, uid, uid)
        p.chmod(mode)


if __name__ == '__main__':
    initialize('/job-disk')
