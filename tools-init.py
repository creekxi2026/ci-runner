#!/usr/bin/env python3
"""Publish reviewed tools separately from job-writable package caches."""
import argparse
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import stat

spec = importlib.util.spec_from_file_location('workspace', Path(__file__).with_name('workspace-init.py'))
workspace = importlib.util.module_from_spec(spec)
spec.loader.exec_module(workspace)
DIRECTORIES = ('python', 'bin', 'wheels', 'wechat')


def validate_source(source):
    # Source must stay idle during publication. Check every selected entry;
    # never promote job-writable executables into a trusted tool snapshot.
    for name in DIRECTORIES:
        root = source / name
        if root.is_symlink() or not root.is_dir():
            raise ValueError('missing tool directory: ' + name)
        for path in [root, *root.rglob('*')]:
            info = path.lstat()
            if info.st_uid != os.geteuid():
                raise ValueError('tool source has a foreign owner')
            if path.is_symlink():
                if os.path.isabs(os.readlink(path)) or not path.resolve(strict=True).is_relative_to(root.resolve()):
                    raise ValueError('tool symlink escapes its distribution')
            elif not (stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode)) or info.st_mode & 0o022:
                raise ValueError('tool source is writable or has unsupported entries')


def publish_tools(source, root, provenance):
    validate_source(source)
    stage = root / 'tools-import.staging'
    if stage.exists(): shutil.rmtree(stage)
    stage.mkdir(mode=0o700)
    counts = {'cloned_bytes': 0, 'copied_bytes': 0}
    try:
        for name in DIRECTORIES:
            workspace.clone_tree(source / name, stage / name, os.geteuid(), counts, link_root=source / name)
        content = {'schema': 1, 'files': workspace.manifest(stage, runner=True), 'provenance': provenance,
                   'platform': 'linux-arm64-ubuntu24.04',
                   'executables': sorted(str(p.relative_to(stage)) for p in stage.rglob('*')
                                         if not p.is_symlink() and p.is_file() and p.stat().st_mode & 0o111)}
        identity = hashlib.sha256(json.dumps(content, sort_keys=True).encode()).hexdigest()
        def build(dest):
            files = dest / 'files'
            files.mkdir(mode=0o755)
            for path in list(stage.iterdir()): path.rename(files / path.name)
            for path in files.rglob('*'):
                if not path.is_symlink():
                    path.chmod(0o755 if path.is_dir() else ((path.stat().st_mode & 0o111) | 0o444))
            return content
        target = workspace.publish(root, 'tools', identity, build)
        # Verify a pre-existing content identity too; a partial/corrupt snapshot
        # cannot be accepted merely because its directory already exists.
        if workspace.manifest(target / 'files', runner=True) != content['files']:
            raise ValueError('published tool content mismatch')
        return {'tools_seed': identity, **counts}
    finally:
        shutil.rmtree(stage, ignore_errors=True)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--templates', default='/templates')
    p.add_argument('--source', default='/source/tools')
    p.add_argument('--provenance', required=True)
    args = p.parse_args()
    root = Path(args.templates)
    if os.getuid() != 0 or root.is_symlink() or root.stat().st_uid != 0 or root.stat().st_mode & 0o077:
        raise ValueError('administrator-owned 0700 templates root required')
    with (root / 'template.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        print(json.dumps(publish_tools(Path(args.source), root, args.provenance)), flush=True)


if __name__ == '__main__': main()
